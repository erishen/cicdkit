package check

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/erishen/cicdkit/internal/config"
	"github.com/erishen/cicdkit/internal/store"
)

func TestConfigCheck(t *testing.T) {
	cfg := &config.Config{}
	cfg.Runner.WorkDir = t.TempDir()

	valid := store.Project{Name: "ok", Deploy: store.DeploySpec{Method: "local-k3s"}}
	if r := configCheck(valid, cfg); r.Status != StatusOK {
		t.Fatalf("合法项目应 ok，得到 %s: %s", r.Status, r.Detail)
	}

	// 任意导入命令必须被白名单挡下
	bad := store.Project{Name: "evil", Deploy: store.DeploySpec{Method: "local-k3s", K3sImportCmd: "touch /tmp/pwned"}}
	if r := configCheck(bad, cfg); r.Status != StatusError {
		t.Fatalf("非法导入命令应 error，得到 %s", r.Status)
	}
	if r := configCheck(store.Project{Name: ""}, cfg); r.Status != StatusError {
		t.Fatalf("空 name 应 error，得到 %s", r.Status)
	}
}

func TestBuildContextCheck(t *testing.T) {
	wd := t.TempDir()
	cfg := &config.Config{}
	cfg.Runner.WorkDir = wd

	// 与真实用法一致：context 与 dockerfile 都相对 WorkDir（仓库根）解析，
	// dockerfile 并不是相对 context 目录。这正好覆盖 “examples/hello-cicd” +
	// “examples/hello-cicd/Dockerfile” 这种会被错误拼接成 ctx/ctx/Dockerfile 的场景。
	ctxRel := "ctx"
	dfRel := filepath.Join("ctx", "Dockerfile") // 相对 WorkDir，不是相对 context
	ctxDir := filepath.Join(wd, ctxRel)
	if err := os.Mkdir(ctxDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if r := buildContextCheck(store.Project{}, cfg); r.Status != StatusWarn {
		t.Fatalf("空 context 应 warn，得到 %s", r.Status)
	}
	// 缺失的 context（相对 WorkDir）
	if r := buildContextCheck(store.Project{Build: store.BuildSpec{Context: "nope"}}, cfg); r.Status != StatusError {
		t.Fatalf("缺失 context 应 error，得到 %s", r.Status)
	}
	// context 存在、未配 dockerfile → ok
	if r := buildContextCheck(store.Project{Build: store.BuildSpec{Context: ctxRel}}, cfg); r.Status != StatusOK {
		t.Fatalf("context 存在应 ok，得到 %s: %s", r.Status, r.Detail)
	}
	// dockerfile 配了但相对 WorkDir 下找不到 → error
	if r := buildContextCheck(store.Project{Build: store.BuildSpec{Context: ctxRel, Dockerfile: filepath.Join("ctx", "missing")}}, cfg); r.Status != StatusError {
		t.Fatalf("缺失 dockerfile 应 error，得到 %s", r.Status)
	}
	// dockerfile 存在（相对 WorkDir，与 context 同级）→ ok；关键：不能拼成 ctx/ctx/Dockerfile
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := buildContextCheck(store.Project{Build: store.BuildSpec{Context: ctxRel, Dockerfile: dfRel}}, cfg); r.Status != StatusOK {
		t.Fatalf("dockerfile 存在应 ok，得到 %s: %s", r.Status, r.Detail)
	}
}

func TestNeedsCluster(t *testing.T) {
	cases := map[string]bool{
		"": false, "kubectl-apply": true, "kubectl-set-image": true,
		"helm": true, "local-k3s": true, "bogus": false,
	}
	for m, want := range cases {
		if got := needsCluster(m); got != want {
			t.Fatalf("needsCluster(%q)=%v, want %v", m, got, want)
		}
	}
}

func TestRunReturnsExpectedChecks(t *testing.T) {
	cfg := &config.Config{}
	cfg.Runner.WorkDir = t.TempDir()
	p := store.Project{Name: "demo", Deploy: store.DeploySpec{Method: "local-k3s"}}

	results := Run(context.Background(), p, cfg)
	names := map[string]bool{}
	for _, r := range results {
		names[r.Name] = true
	}
	for _, need := range []string{"配置校验", "Docker daemon", "Kubeconfig / 集群", "构建上下文"} {
		if !names[need] {
			t.Fatalf("缺少检查项 %q，实际 %v", need, results)
		}
	}

	// 不触碰集群的部署方式不应出现 kubeconfig 检查
	noCluster := store.Project{Name: "x"} // method 为空
	names2 := map[string]bool{}
	for _, r := range Run(context.Background(), noCluster, cfg) {
		names2[r.Name] = true
	}
	if names2["Kubeconfig / 集群"] {
		t.Fatalf("method 为空不应检查 kubeconfig，实际 %v", names2)
	}
}

func TestSummary(t *testing.T) {
	if !Summary([]Result{{Status: StatusOK}, {Status: StatusWarn}}) {
		t.Fatal("ok+warn 应通过")
	}
	if Summary([]Result{{Status: StatusOK}, {Status: StatusError}}) {
		t.Fatal("含 error 不应通过")
	}
}
