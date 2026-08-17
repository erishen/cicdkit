package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/erishen/cicdkit/internal/api"
	"github.com/erishen/cicdkit/internal/config"
	"github.com/erishen/cicdkit/internal/pipeline"
	"github.com/erishen/cicdkit/internal/store"
)

//go:embed all:web/dist
var webFS embed.FS

func main() {
	configPath := flag.String("config", "", "配置文件路径 (JSON)")
	addr := flag.String("addr", "", "监听地址，覆盖配置文件 (如 :8080)")
	initCmd := flag.Bool("init", false, "写出默认配置文件后退出")
	flag.Parse()

	// Load .env (and .env.local) so SSH connection secrets can live in a file
	// instead of the project JSON / UI form. Real environment variables win over
	// the file. Must run before config.Load so applyEnv can also see these vars.
	if err := config.LoadDotEnv(); err != nil {
		log.Printf("加载 .env 失败 (忽略): %v", err)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if *addr != "" {
		cfg.Server.Addr = *addr
	}

	if *initCmd {
		path := *configPath
		if path == "" {
			path = "config.json"
		}
		if err := cfg.Save(path); err != nil {
			log.Fatalf("写入配置失败: %v", err)
		}
		fmt.Printf("已写出默认配置到 %s\n", path)
		return
	}

	st, err := store.NewJsonStore(cfg.StoreFile)
	if err != nil {
		log.Fatalf("打开存储失败: %v", err)
	}
	runner := pipeline.New(cfg, st)
	sub, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		log.Fatalf("加载前端资源失败: %v", err)
	}

	// AUTO_TOKEN: when enabled and no explicit API_TOKEN is set, generate a
	// one-time random token so the API is protected without manual setup. The
	// token is injected into the served web UI (local browser authenticates
	// transparently) and printed to the log for out-of-band access. It is
	// regenerated on every restart and never written to disk.
	var autoToken string
	autoTokenUsed := false
	if cfg.Server.AutoToken {
		if cfg.Server.APIToken != "" {
			log.Printf("AUTO_TOKEN 已启用，但检测到已显式配置 API_TOKEN，将使用显式 Token（不自动生成）。")
		} else {
			tok, gerr := config.GenerateToken()
			if gerr != nil {
				log.Fatalf("生成 AUTO_TOKEN 失败: %v", gerr)
			}
			cfg.Server.APIToken = tok
			autoToken = tok
			autoTokenUsed = true
			log.Printf("AUTO_TOKEN 已启用：已自动生成一次性 API Token 并注入 Web UI。\n"+
				"  本机浏览器打开页面即自动鉴权，无需手动输入。\n"+
				"  如需从其他设备 / 脚本访问，本次启动的 Token 为：%s", tok)
		}
	}

	// This server shells out to docker/kubectl on the host, so an
	// unauthenticated (or only-auto-token-protected) listener on a routable
	// address is effectively remote command execution. Refuse to start rather
	// than merely warn, so a misconfigured bind can never silently expose the
	// host. AUTO_TOKEN's token is embedded in the served page, so it does NOT
	// protect a non-loopback bind — an explicit API_TOKEN is required there.
	if !cfg.Server.IsLoopback() {
		switch {
		case cfg.Server.APIToken == "":
			log.Fatalf("安全错误: 监听地址 %s 不限于本机，但未设置 API_TOKEN。本平台会在宿主机执行 docker/kubectl，等同于把命令执行开放给全网。请设置 API_TOKEN 环境变量，或把地址改为 127.0.0.1。", cfg.Server.Addr)
		case autoTokenUsed:
			log.Fatalf("安全错误: 监听地址 %s 不限于本机，但使用的是 AUTO_TOKEN（令牌已写入前端页面，对外网无效）。请改用显式 API_TOKEN 环境变量后再启动。", cfg.Server.Addr)
		}
	}

	// 后端启动戳：每次启动写入真实时间，供前端 footer 显示，用于一眼确认浏览器
	// 连的是否为最新启动的后端进程（排查「改了代码却没生效」时比对用）。用 time.Now()
	// 而非硬编码，避免「戳不变」误以为旧进程没被杀掉——其实戳是写死的常量。
	// 格式用「空格分隔、无时区」的本地时间（如 2026-08-16 12:18:27），与前端
	// UI_BUILD 的样式统一，避免 footer 出现 RFC3339 的 T 与 +08:00 后缀造成不一致。
	api.BuildStamp = time.Now().Format("2006-01-02 15:04:05")

	srv := api.New(cfg, st, runner, sub, autoToken)

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Deliberately generous: run logs can be large.
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	// Graceful shutdown so in-flight requests aren't cut mid-response.
	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		fmt.Println("\n正在关闭服务…")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("关闭超时: %v", err)
		}
		// Force the last coalesced persist window to disk so a clean shutdown
		// never loses the most recent mutation.
		st.Flush()
		close(done)
	}()

	fmt.Printf("CI/CD 发布管理平台已启动: http://%s\n", cfg.Server.Addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("服务异常: %v", err)
	}
	<-done
}
