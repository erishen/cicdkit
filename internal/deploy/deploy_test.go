package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/erishen/cicdkit/internal/store"
)

func TestBuildCmdArgs(t *testing.T) {
	cfg := KubeConfig{DefaultKubeconfig: "/home/u/.kube/config"}

	cases := []struct {
		name       string
		method     string
		spec       store.DeploySpec
		wantBin    string
		wantArgs   []string // 必须依序出现（允许中间夹其他参数）
		wantAbsent []string
		wantErr    bool
	}{
		{
			name:     "apply 带命名空间与等待",
			method:   "kubectl-apply",
			spec:     store.DeploySpec{ManifestPath: "k8s/app.yaml", Namespace: "prod", Wait: true, Timeout: "3m"},
			wantBin:  "kubectl",
			wantArgs: []string{"--kubeconfig", "/home/u/.kube/config", "-n", "prod", "apply", "-f", "k8s/app.yaml", "--wait", "--timeout", "3m"},
		},
		{
			name:    "apply 缺 manifest_path 报错",
			method:  "kubectl-apply",
			spec:    store.DeploySpec{},
			wantErr: true,
		},
		{
			name:       "项目级 kubeconfig 覆盖默认值",
			method:     "kubectl-apply",
			spec:       store.DeploySpec{ManifestPath: "a.yaml", Kubeconfig: "/custom/kubeconfig"},
			wantBin:    "kubectl",
			wantArgs:   []string{"--kubeconfig", "/custom/kubeconfig"},
			wantAbsent: []string{"/home/u/.kube/config"},
		},
		{
			name:     "set-image 组合 container=image",
			method:   "kubectl-set-image",
			spec:     store.DeploySpec{Deployment: "web", Container: "app"},
			wantBin:  "kubectl",
			wantArgs: []string{"set", "image", "deployment/web", "app=repo/app:v1"},
		},
		{
			name:    "set-image 缺 container 报错",
			method:  "kubectl-set-image",
			spec:    store.DeploySpec{Deployment: "web"},
			wantErr: true,
		},
		{
			name:     "helm upgrade --install 并注入镜像",
			method:   "helm",
			spec:     store.DeploySpec{ReleaseName: "web", ChartPath: "./chart", HelmSetImage: true, HelmImageKey: "image.full"},
			wantBin:  "helm",
			wantArgs: []string{"upgrade", "--install", "web", "./chart", "--set", "image.full=repo/app:v1"},
		},
		{
			name:     "helm 未指定 image key 时用默认 image",
			method:   "helm",
			spec:     store.DeploySpec{ReleaseName: "web", ChartPath: "./chart", HelmSetImage: true},
			wantBin:  "helm",
			wantArgs: []string{"--set", "image=repo/app:v1"},
		},
		{
			name:    "helm 缺 chart_path 报错",
			method:  "helm",
			spec:    store.DeploySpec{ReleaseName: "web"},
			wantErr: true,
		},
		{
			name:    "未支持的方式报错",
			method:  "ssh",
			spec:    store.DeploySpec{},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := store.Project{Deploy: c.spec}
			cmd, err := buildCmd(context.Background(), c.method, cfg, p, "repo/app:v1")
			if c.wantErr {
				if err == nil {
					t.Fatal("期望报错，但成功了")
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错: %v", err)
			}
			if got := cmd.Args[0]; !strings.HasSuffix(got, c.wantBin) {
				t.Fatalf("可执行文件应为 %s，得到 %s", c.wantBin, got)
			}
			joined := strings.Join(cmd.Args, " ")
			idx := 0
			for _, want := range c.wantArgs {
				found := -1
				for i := idx; i < len(cmd.Args); i++ {
					if cmd.Args[i] == want {
						found = i
						break
					}
				}
				if found < 0 {
					t.Fatalf("参数中缺少 %q（按序）：%s", want, joined)
				}
				idx = found + 1
			}
			for _, absent := range c.wantAbsent {
				if strings.Contains(joined, absent) {
					t.Fatalf("参数中不应出现 %q：%s", absent, joined)
				}
			}
		})
	}
}

// buildCmd 必须绑定 context，否则取消一次运行时 kubectl/helm 进程不会被终止。
func TestBuildCmdIsBoundToContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消

	p := store.Project{Deploy: store.DeploySpec{ManifestPath: "a.yaml"}}
	cmd, err := buildCmd(ctx, "kubectl-apply", KubeConfig{}, p, "img:1")
	if err != nil {
		t.Fatal(err)
	}
	// 已取消的 context 下启动进程必须失败，这正是「取消生效」的证据
	if err := cmd.Start(); err == nil {
		_ = cmd.Wait()
		t.Fatal("context 已取消，命令仍被启动：说明未绑定 context，Cancel 将失效")
	}
}

func TestWaitForRolloutArgs(t *testing.T) {
	// 通过 deploymentExists/waitForRollout 共用的参数拼装验证命名空间与超时透传。
	spec := store.DeploySpec{Deployment: "web", Namespace: "staging", Timeout: "90s"}
	var sb strings.Builder
	// kubectl 不一定存在，这里只关心不 panic 且把参数写进日志
	_ = waitForRollout(context.Background(), KubeConfig{DefaultKubeconfig: "kc"}, spec, &sb)
	out := sb.String()
	for _, want := range []string{"rollout", "status", "deployment/web", "-n staging", "--timeout 90s", "--kubeconfig kc"} {
		if !strings.Contains(out, want) {
			t.Fatalf("等待滚动更新的命令应包含 %q，实际日志: %s", want, out)
		}
	}
}

func TestSSHDeployParts(t *testing.T) {
	cases := []struct {
		name      string
		spec      store.DeploySpec
		imageRef  string
		wantBin   string
		wantArgs  []string
		wantScriptContains []string
		wantErr   bool
	}{
		{
			name:     "最小配置：默认容器名 cicd-app + 先 rm 旧容器（端口不冲突）",
			spec:     store.DeploySpec{SSHHost: "1.2.3.4", SSHUser: "root"},
			imageRef: "registry/app:v1",
			wantBin:  "ssh",
			wantArgs: []string{"-p", "22", "root@1.2.3.4"},
			wantScriptContains: []string{"docker rm -f 'cicd-app' 2>/dev/null || true", "docker run -d --name 'cicd-app' -p 8080:8080 --restart unless-stopped 'registry/app:v1'"},
		},
		{
			name:     "带私钥 + pull + 命名 + 运行参数",
			spec:     store.DeploySpec{SSHHost: "h", SSHUser: "u", SSHKeyPath: "/key", SSHPull: true, SSHContainer: "c", SSHRunArgs: "-p 8080:8080"},
			imageRef: "img:1",
			wantArgs: []string{"-i", "/key", "-p", "22", "u@h"},
			wantScriptContains: []string{"docker pull 'img:1'", "docker rm -f 'c' 2>/dev/null || true", "docker run -d --name 'c' -p 8080:8080 --restart unless-stopped 'img:1'"},
		},
		{
			name:     "显式 ssh_image 覆盖 image_ref",
			spec:     store.DeploySpec{SSHHost: "h", SSHUser: "u", SSHImage: "explicit:tag"},
			imageRef: "ignored:1",
			wantScriptContains: []string{"docker rm -f 'cicd-app' 2>/dev/null || true", "docker run -d --name 'cicd-app' -p 8080:8080 --restart unless-stopped 'explicit:tag'"},
		},
		{
			name:     "缺镜像报错",
			spec:     store.DeploySpec{SSHHost: "h", SSHUser: "u"},
			imageRef: "",
			wantErr:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin, args, err := sshDeployParts(store.Project{Deploy: c.spec}, c.imageRef)
			if c.wantErr {
				if err == nil {
					t.Fatal("期望报错，但成功")
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错: %v", err)
			}
			if c.wantBin != "" && bin != c.wantBin {
				t.Fatalf("可执行应为 %s，得到 %s", c.wantBin, bin)
			}
			joined := strings.Join(args, " ")
			for _, want := range c.wantArgs {
				if !strings.Contains(joined, want) {
					t.Fatalf("ssh 参数应含 %q，实际: %s", want, joined)
				}
			}
			script := args[len(args)-1]
			for _, want := range c.wantScriptContains {
				if !strings.Contains(script, want) {
					t.Fatalf("远端脚本应含 %q，实际: %s", want, script)
				}
			}
		})
	}
}

func TestSanitizeImageName(t *testing.T) {
	got := sanitizeImageName("registry.cn-1/ns/hello-cicd:20260814")
	// 不应含路径/标签/域名分隔符
	for _, bad := range []rune{'/', ':', '.', '@'} {
		if strings.ContainsRune(got, bad) {
			t.Fatalf("sanitizeImageName 不应含 %q，得到 %s", bad, got)
		}
	}
	if got == "" {
		t.Fatal("sanitizeImageName 不应为空")
	}
	if sanitizeImageName("!!!") != "img" {
		t.Fatal("全非法字符应回退为 img")
	}
}

func TestSSHRunScriptTransfer(t *testing.T) {
	p := store.Project{Deploy: store.DeploySpec{SSHContainer: "c", SSHRunArgs: "-p 8080:8080"}}
	script := sshRunScript(p, "img:1", "/tmp/cicd-x.tar", true)
	for _, want := range []string{"docker load -i /tmp/cicd-x.tar", "docker rm -f 'c' 2>/dev/null || true", "docker run -d --name 'c' -p 8080:8080 --restart unless-stopped 'img:1'"} {
		if !strings.Contains(script, want) {
			t.Fatalf("transfer 脚本应含 %q，实际: %s", want, script)
		}
	}
	if strings.Contains(script, "docker pull") {
		t.Fatal("transfer 模式不应出现 docker pull")
	}
}

func TestSSHRunScriptDefaultName(t *testing.T) {
	p := store.Project{ID: "hello-java", Deploy: store.DeploySpec{SSHRunArgs: "-p 8080:8080"}}
	script := sshRunScript(p, "img:1", "", false)
	for _, want := range []string{
		"for cid in $(docker ps -q --filter \"publish=8080\"); do [ -n \"$cid\" ] && docker rm -f \"$cid\"; done",
		"for cid in $(docker ps --format '{{.ID}} {{.Ports}}' | awk -v p=8080 '$0 ~ \"[: ]\"p\"($|->|/)\" {print $1}'); do [ -n \"$cid\" ] && docker rm -f \"$cid\"; done",
		"docker rm -f 'hello-java' 2>/dev/null || true",
		"docker run -d --name 'hello-java' -p 8080:8080 --restart unless-stopped 'img:1'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("期望包含 %q，实际脚本:\n%s", want, script)
		}
	}
	// 显式 SSHContainer 应覆盖派生名
	p2 := store.Project{ID: "hello-java", Deploy: store.DeploySpec{SSHContainer: "my-app", SSHRunArgs: "-p 8080:8080"}}
	if !strings.Contains(sshRunScript(p2, "img:1", "", false), "docker run -d --name 'my-app' -p 8080:8080 --restart unless-stopped 'img:1'") {
		t.Fatal("显式 SSHContainer 应覆盖派生名")
	}
}

func TestSSHRunScriptDefaultPortWhenEmpty(t *testing.T) {
	// 空 SSHRunArgs 时，部署脚本应自动发布 <ssh_probe_port>:8080（默认 8080），
	// 否则探测目标（宿主 8080）不可达会 502。
	p := store.Project{ID: "demo", Deploy: store.DeploySpec{}}
	script := sshRunScript(p, "img:1", "", false)
	for _, want := range []string{
		"for cid in $(docker ps -q --filter \"publish=8080\"); do [ -n \"$cid\" ] && docker rm -f \"$cid\"; done",
		"for cid in $(docker ps --format '{{.ID}} {{.Ports}}' | awk -v p=8080 '$0 ~ \"[: ]\"p\"($|->|/)\" {print $1}'); do [ -n \"$cid\" ] && docker rm -f \"$cid\"; done",
		"docker rm -f 'demo' 2>/dev/null || true",
		"docker run -d --name 'demo' -p 8080:8080 --restart unless-stopped 'img:1'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("期望包含 %q，实际脚本:\n%s", want, script)
		}
	}

	// 设置了 ssh_probe_port 时，宿主侧端口应与探测端口对齐。
	p2 := store.Project{ID: "demo", Deploy: store.DeploySpec{SSHProbePort: "9090"}}
	if !strings.Contains(sshRunScript(p2, "img:1", "", false), "docker run -d --name 'demo' -p 9090:8080 --restart unless-stopped 'img:1'") {
		t.Fatal("ssh_probe_port 应作为宿主发布端口")
	}
}

func TestPublishedHostPorts(t *testing.T) {
	cases := []struct{ in, want string }{
		{"-p 8080:8080 --restart unless-stopped", "8080"},
		{"-p 8080", "8080"},
		{"-p 127.0.0.1:9090:80", "9090"},
		{"--publish=8080:8080", "8080"},
		{"--restart unless-stopped", ""},
	}
	for _, c := range cases {
		got := publishedHostPorts(c.in)
		if c.want == "" {
			if len(got) != 0 {
				t.Fatalf("publishedHostPorts(%q) 应无端口，得到 %v", c.in, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != c.want {
			t.Fatalf("publishedHostPorts(%q) = %v, want [%s]", c.in, got, c.want)
		}
	}
}

func TestSSHRunScriptProbePortAuthoritative(t *testing.T) {
	// 探针端口应作为宿主发布端口的唯一权威来源，即使 run_args 里写了别的 -p。
	p := store.Project{ID: "demo", Deploy: store.DeploySpec{SSHProbePort: "9090", SSHRunArgs: "-p 9999:8080 -e FOO=bar"}}
	script := sshRunScript(p, "img:1", "", false)
	if !strings.Contains(script, "docker run -d --name 'demo' -p 9090:8080 --restart unless-stopped -e FOO=bar 'img:1'") {
		t.Fatalf("探针端口应覆盖 run_args 里的 -p，实际脚本:\n%s", script)
	}
	if strings.Contains(script, "9999") {
		t.Fatalf("用户写在 run_args 的 -p 9999 应被剥离，实际:\n%s", script)
	}

	// 空 run_args 时，探针端口仍决定宿主端口
	p2 := store.Project{ID: "demo", Deploy: store.DeploySpec{SSHProbePort: "9090"}}
	if !strings.Contains(sshRunScript(p2, "img:1", "", false), "docker run -d --name 'demo' -p 9090:8080 --restart unless-stopped 'img:1'") {
		t.Fatal("空 run_args 时探针端口应决定宿主端口")
	}
}

func TestDefaultContainerName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "cicd-app"},
		{"hello-java", "hello-java"},
		{"foo/bar:baz", "foo-bar-baz"},
		{"@#$%", "c----"},
	}
	for _, c := range cases {
		if got := defaultContainerName(c.in); got != c.want {
			t.Fatalf("defaultContainerName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMapUnameArch(t *testing.T) {
	cases := []struct{ in, want string }{
		{"x86_64", "linux/amd64"},
		{"amd64", "linux/amd64"},
		{"X86_64\n", "linux/amd64"},
		{"aarch64", "linux/arm64"},
		{"arm64", "linux/arm64"},
		{"AARCH64", "linux/arm64"},
		{"ppc64le", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := mapUnameArch(c.in); got != c.want {
			t.Fatalf("mapUnameArch(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSSHBaseArgs(t *testing.T) {	// 带私钥：sshBaseArgs 只产出连接前缀，远程命令由调用方追加
	spec := store.DeploySpec{SSHHost: "h", SSHUser: "u", SSHKeyPath: "/key", SSHPort: "2222"}
	args := sshBaseArgs(spec)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-i", "/key", "-p", "2222", "StrictHostKeyChecking=accept-new", "LogLevel=ERROR", "u@h"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("sshBaseArgs 应含 %q，实际: %s", want, joined)
		}
	}
	// 末尾应是目标主机（远程命令由调用方追加）
	if args[len(args)-1] != "u@h" {
		t.Fatalf("sshBaseArgs 末尾应为目标主机，实际: %q", args[len(args)-1])
	}
	// 默认端口 22
	def := sshBaseArgs(store.DeploySpec{SSHHost: "h", SSHUser: "u"})
	if !strings.Contains(strings.Join(def, " "), "-p 22") {
		t.Fatal("未设端口时应默认 -p 22")
	}
}

func TestRemotePartitionFreeKB(t *testing.T) {
	// df 输出第二行第四列即为可用 1024-blocks；awk 取 NR==2{print $4}
	cmd := "df -P -k /var/lib/docker 2>/dev/null | awk 'NR==2{print $4}'"
	if !strings.Contains(cmd, "NR==2{print $4}") {
		t.Fatal("df 解析应取第二行第四列可用块数")
	}
}

func TestSSHCheckRemoteDiskThreshold(t *testing.T) {
	// 阈值逻辑纯函数化测试：本地镜像 800MB -> 需要 800*1.5+512 = 1.7GB，
	// 下限 1GB；远端只剩 500MB 应报错，>=1.7GB 应通过。
	root := "/var/lib/docker"

	// 远端只剩 500MB（512000 KB），需求约 1.7GB -> 应报错
	if err := sshCheckDiskThreshold(root, 512_000, 800*1024*1024); err == nil {
		t.Fatal("远端空间不足时应报错")
	}

	// 远端有 4GB（4194304 KB），需求约 1.7GB -> 应通过
	if err := sshCheckDiskThreshold(root, 4_194_304, 800*1024*1024); err != nil {
		t.Fatalf("空间充足时应通过: %v", err)
	}

	// 远端有 600MB，但本地镜像为 0（probe 不到尺寸）-> 用 1GB 下限 -> 应报错
	if err := sshCheckDiskThreshold(root, 600_000, 0); err == nil {
		t.Fatal("空间低于 1GB 下限时应报错")
	}

	// 远端有 2GB，本地镜像为 0 -> 用 1GB 下限 -> 应通过
	if err := sshCheckDiskThreshold(root, 2_000_000, 0); err != nil {
		t.Fatalf("空间高于 1GB 下限时应通过: %v", err)
	}
}
