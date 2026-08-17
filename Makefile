.PHONY: build run test vet tidy init clean image web dev example k3s-import build-linux build-linux-arm64

# 完整构建：先构建前端 (Vite → dist)，再编译 Go 后端（嵌入 dist）
# 注意：macOS 15 (Sequoia) 的 dyld 要求二进制带 LC_UUID 加载命令，而 Go 1.22 内部链接器
# 不产出它（golang #68678），会导致后端一绑定本地端口即 Abort。修法：CGO_ENABLED=1 +
# -linkmode=external 强制走系统外部链接器（clang），由它补上 LC_UUID。Linux 构建无此问题。
build:
	cd cmd/server/web && npm install && npm run build
	CGO_ENABLED=1 go build -ldflags="-linkmode=external -s -w" -o ./bin/server ./cmd/server

# 交叉编译 Linux 静态单文件（CGO_ENABLED=0 → 不链 glibc，拷到任何同架构 Linux 直接跑，无需装运行时）
# 先靠 web 产出 dist，再 GOOS=linux 编出 ELF 静态二进制。macOS 的 dyld/LC_UUID 坑在交叉编译时不存在（走内部链接器）。
build-linux: web
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o ./bin/cicdkit-linux-amd64 ./cmd/server

# 同上，目标架构 arm64（如 AWS Graviton / 树莓派 4）。输出文件名带架构以便同目录共存。
build-linux-arm64: web
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o ./bin/cicdkit-linux-arm64 ./cmd/server

# 仅构建前端
web:
	cd cmd/server/web && npm install && npm run build

# 本地运行（默认 127.0.0.1:8080，数据存 ./data）。前端未构建时显示占位页
# 只绑本机：平台会在宿主机执行 docker/kubectl，绑到全网卡且无 Token 等于开放命令执行。
# 需要局域网访问时：make run ADDR=0.0.0.0:8080 API_TOKEN=<你的令牌>
# 同样用外部链接器以规避 macOS 15 的 LC_UUID / dyld 问题（go run 的临时二进制也会中招）
ADDR ?= 127.0.0.1:8080
run:
	@bash ./scripts/kill-ports.sh 8080
	@sleep 0.3
	CGO_ENABLED=1 go build -ldflags="-linkmode=external -s -w" -o ./bin/server ./cmd/server && ./bin/server -addr $(ADDR)

# 前端开发模式（Vite HMR，代理 /api 到 :8080）。需另开一个终端跑 `make run`
dev-web:
	cd cmd/server/web && npm install && npm run dev

# 开发模式：先启动后端 (Go :8080)，等其 /api/health 就绪后再启动前端 (Vite :5173)
# 后端要先 go build 再监听，Vite 启动极快；若两者同时拉起，Vite 会在后端就绪前
# 狂报 ECONNREFUSED（proxy error）。这里用 health 门控，消除这个竞态。
# Ctrl-C 会一并终止前后端进程
# dev 模式启动后端前用 env -u API_TOKEN 屏蔽 shell 里可能残留的旧 API_TOKEN，
# 确保 .env（已 gitignore）中固化的 API_TOKEN 一定生效；浏览器弹框时填 .env 的值即可。
# 生产 make run 不带 env -u：以 API_TOKEN 环境变量或 .env 为准。
dev:
	@echo "▶ 清理占用 8080/5173 的残留进程..."
	@bash ./scripts/kill-ports.sh 8080 5173
	@echo "▶ 启动后端 (Go :8080)..."
	@trap 'kill 0 2>/dev/null' EXIT INT TERM; \
	(CGO_ENABLED=1 go build -ldflags="-linkmode=external -s -w" -o ./bin/server ./cmd/server && env -u API_TOKEN ./bin/server -addr $(ADDR)) & \
	echo "▶ 等待后端就绪 (/api/health)..."; \
	n=0; \
	until curl -sf --noproxy '*' http://127.0.0.1:8080/api/health >/dev/null 2>&1; do \
	  n=$$((n+1)); \
	  if [ $$n -ge 60 ]; then echo "✗ 后端 30s 内未就绪，可能 go build 失败，请检查上方输出"; exit 1; fi; \
	  sleep 0.5; \
	done; \
	echo "▶ 后端已就绪，启动前端 (Vite :5173)..."; \
	(cd cmd/server/web && npm install --registry https://registry.npmmirror.com && npm run dev) & \
	wait

# 生成示例配置文件到 ./data/config.json
init:
	go run ./cmd/server -init -config ./data/config.json

# 静态检查
vet:
	go vet ./...

# 单元测试（-race：本平台有并发日志写入与取消，竞态必须被测出来）
# -linkmode=external 同 build 目标：否则 macOS 15 的 dyld 会因缺 LC_UUID 直接 abort 测试二进制
test:
	CGO_ENABLED=1 go test -race -ldflags="-linkmode=external" ./...

# 方案 B（纯本地）：把平台最新构建出的本地镜像导入 k3s containerd
k3s-import:
	bash ./scripts/import-to-k3s.sh

# 把示例项目 (examples/hello-cicd/project.json) 导入正在运行的服务
example:
	./scripts/seed-example.sh

# 生成 vendor（本平台纯标准库，通常不需要）
tidy:
	go mod tidy

# 构建 docker 镜像
image:
	docker build -t cicdkit:latest .

clean:
	rm -rf ./bin ./data/store.json
