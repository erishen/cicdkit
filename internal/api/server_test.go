package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/erishen/cicdkit/internal/config"
	"github.com/erishen/cicdkit/internal/pipeline"
	"github.com/erishen/cicdkit/internal/store"
)

func newTestServer(t *testing.T, token string) (*Server, store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Server.APIToken = token
	cfg.StoreFile = filepath.Join(t.TempDir(), "store.json")
	st, err := store.NewJsonStore(cfg.StoreFile)
	if err != nil {
		t.Fatal(err)
	}
	runner := pipeline.New(cfg, st)
	web := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html><head></head><body>ui</body></html>")}}
	return New(cfg, st, runner, web, ""), st
}

func do(t *testing.T, srv *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

func TestAuthTokenGuardsAPI(t *testing.T) {
	srv, _ := newTestServer(t, "s3cret")

	// 无凭据：拒绝
	if got := do(t, srv, http.MethodGet, "/api/projects", "", nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("无 Token 应 401，得到 %d", got)
	}
	// 错误凭据：拒绝
	if got := do(t, srv, http.MethodGet, "/api/projects", "", map[string]string{"X-API-Token": "wrong"}).Code; got != http.StatusUnauthorized {
		t.Fatalf("错误 Token 应 401，得到 %d", got)
	}
	// 创建项目这类写操作同样受保护（否则等于开放命令执行）
	body := `{"name":"x","deploy":{"method":"local-k3s"}}`
	if got := do(t, srv, http.MethodPost, "/api/projects", body, nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("无 Token 创建项目应 401，得到 %d", got)
	}
	// X-API-Token 头
	if got := do(t, srv, http.MethodGet, "/api/projects", "", map[string]string{"X-API-Token": "s3cret"}).Code; got != http.StatusOK {
		t.Fatalf("正确 Token 应 200，得到 %d", got)
	}
	// Bearer 形式
	if got := do(t, srv, http.MethodGet, "/api/projects", "", map[string]string{"Authorization": "Bearer s3cret"}).Code; got != http.StatusOK {
		t.Fatalf("Bearer Token 应 200，得到 %d", got)
	}
	// health 与静态资源不需要 Token，否则 UI 无法加载并输入 Token
	if got := do(t, srv, http.MethodGet, "/api/health", "", nil).Code; got != http.StatusOK {
		t.Fatalf("health 应免鉴权，得到 %d", got)
	}
	if got := do(t, srv, http.MethodGet, "/", "", nil).Code; got != http.StatusOK {
		t.Fatalf("静态页面应免鉴权，得到 %d", got)
	}
}

func TestNoTokenConfiguredMeansOpen(t *testing.T) {
	srv, _ := newTestServer(t, "")
	if got := do(t, srv, http.MethodGet, "/api/projects", "", nil).Code; got != http.StatusOK {
		t.Fatalf("未配置 Token 时应放行（默认仅绑本机），得到 %d", got)
	}
}

func TestAutoTokenInjectedIntoUI(t *testing.T) {
	cfg := config.Default()
	cfg.StoreFile = filepath.Join(t.TempDir(), "store.json")
	st, err := store.NewJsonStore(cfg.StoreFile)
	if err != nil {
		t.Fatal(err)
	}
	runner := pipeline.New(cfg, st)
	web := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html><head></head><body>ui</body></html>")},
		"app.js":     &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	srv := New(cfg, st, runner, web, "auto-generated-token")

	// 根路径应注入 token 全局变量，供前端自动鉴权。
	w := do(t, srv, http.MethodGet, "/", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("/ 应 200，得到 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `window.__CICD_API_TOKEN__="auto-generated-token"`) {
		t.Fatalf("index.html 未注入 auto token: %s", w.Body.String())
	}

	// 真实静态资源应正常返回且不应注入 token。
	wa := do(t, srv, http.MethodGet, "/app.js", "", nil)
	if wa.Code != http.StatusOK {
		t.Fatalf("/app.js 应 200，得到 %d", wa.Code)
	}
	if strings.Contains(wa.Body.String(), "__CICD_API_TOKEN__") {
		t.Fatalf("/app.js 不应注入 token")
	}

	// 不存在的客户端路由应回退到 index.html 并注入 token。
	wr := do(t, srv, http.MethodGet, "/some/spa/route", "", nil)
	if wr.Code != http.StatusOK {
		t.Fatalf("SPA 回退应 200，得到 %d", wr.Code)
	}
	if !strings.Contains(wr.Body.String(), "__CICD_API_TOKEN__") {
		t.Fatalf("SPA 回退应注入 token")
	}
}

func TestRegistryPasswordNeverLeaves(t *testing.T) {
	srv, st := newTestServer(t, "")

	body := `{"name":"app","build":{"image_repo":"app","registry":{"server":"r","username":"u","password":"REAL-SECRET"}},"deploy":{"method":"local-k3s"}}`
	w := do(t, srv, http.MethodPost, "/api/projects", body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("创建失败: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "REAL-SECRET") {
		t.Fatal("创建响应泄露了明文密码")
	}
	var created store.Project
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// 列表与单个查询都不能泄露
	for _, path := range []string{"/api/projects", "/api/projects/" + created.ID} {
		got := do(t, srv, http.MethodGet, path, "", nil).Body.String()
		if strings.Contains(got, "REAL-SECRET") {
			t.Fatalf("%s 泄露了明文密码", path)
		}
	}

	// 但服务端仍然保有真实密码，否则 docker login 会失败
	stored, _ := st.GetProject(created.ID)
	if stored.Build.Registry.Password != "REAL-SECRET" {
		t.Fatalf("服务端应保留真实密码，实际 %q", stored.Build.Registry.Password)
	}

	// UI 把掩码回写时不能把密码擦掉
	upd := `{"name":"app","build":{"image_repo":"app","registry":{"server":"r","username":"u","password":"` + store.MaskedSecret + `"}},"deploy":{"method":"local-k3s"}}`
	if w := do(t, srv, http.MethodPut, "/api/projects/"+created.ID, upd, nil); w.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", w.Code, w.Body.String())
	}
	stored, _ = st.GetProject(created.ID)
	if stored.Build.Registry.Password != "REAL-SECRET" {
		t.Fatalf("掩码回写后密码被破坏: %q", stored.Build.Registry.Password)
	}
}

func TestCreateProjectRejectsArbitraryImportCommand(t *testing.T) {
	srv, _ := newTestServer(t, "")
	body := `{"name":"evil","build":{"image_repo":"x"},"deploy":{"method":"local-k3s","deployment":"d","container":"c","k3s_import_cmd":"/usr/bin/touch /tmp/pwned"}}`
	w := do(t, srv, http.MethodPost, "/api/projects", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("任意导入命令应被拒 400，得到 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "不允许") {
		t.Fatalf("应说明命令不被允许: %s", w.Body.String())
	}
}

func TestDeployWithoutAnyBuildIsRejected(t *testing.T) {
	srv, _ := newTestServer(t, "")
	body := `{"name":"app","build":{"image_repo":"app"},"deploy":{"method":"local-k3s","deployment":"d","container":"c"}}`
	w := do(t, srv, http.MethodPost, "/api/projects", body, nil)
	var p store.Project
	_ = json.Unmarshal(w.Body.Bytes(), &p)

	// 从未构建过：不能凭空造一个时间戳 tag 去部署（那会导致 ImagePullBackOff 假成功）
	w = do(t, srv, http.MethodPost, "/api/projects/"+p.ID+"/deploy", `{}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("无成功构建时部署应 400，得到 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "尚无成功构建") {
		t.Fatalf("错误信息应提示先 Build: %s", w.Body.String())
	}
}

func TestWebhookSecretIsEnforced(t *testing.T) {
	srv, st := newTestServer(t, "")
	srv.cfg.Server.WebhookSecret = "hook-secret"
	if err := st.CreateProject(store.Project{ID: "p1", Name: "app"}); err != nil {
		t.Fatal(err)
	}

	if got := do(t, srv, http.MethodPost, "/api/webhook/p1", `{}`, nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("缺 secret 应 401，得到 %d", got)
	}
	if got := do(t, srv, http.MethodPost, "/api/webhook/p1?secret=wrong", `{}`, nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("错误 secret 应 401，得到 %d", got)
	}
	if got := do(t, srv, http.MethodGet, "/api/webhook/p1?secret=hook-secret", "", nil).Code; got != http.StatusMethodNotAllowed {
		t.Fatalf("GET webhook 应 405，得到 %d", got)
	}
}

func TestRepoImage(t *testing.T) {
	cases := []struct{ repo, tag, want string }{
		{"app", "v1", "app:v1"},
		{"registry.example.com/team/app", "v1", "registry.example.com/team/app:v1"},
		{"app/", "v1", "app:v1"}, // 末尾斜杠应被规整
	}
	for _, c := range cases {
		p := store.Project{Build: store.BuildSpec{ImageRepo: c.repo}}
		if got := repoImage(p, c.tag); got != c.want {
			t.Fatalf("repoImage(%q,%q)=%q，期望 %q", c.repo, c.tag, got, c.want)
		}
	}
}

func TestValidateEndpoint(t *testing.T) {
	srv, _ := newTestServer(t, "")
	create := do(t, srv, http.MethodPost, "/api/projects",
		`{"name":"demo","build":{"image_repo":"demo"},"deploy":{"method":"local-k3s","deployment":"d","container":"c"}}`, nil)
	if create.Code != http.StatusCreated {
		t.Fatalf("创建项目应 201，得到 %d: %s", create.Code, create.Body.String())
	}
	var created store.Project
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("无法解析创建响应: %v / %s", err, create.Body.String())
	}

	w := do(t, srv, http.MethodPost, "/api/projects/"+created.ID+"/validate", `{}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("校验应 200，得到 %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析校验响应失败: %v / %s", err, w.Body.String())
	}
	if len(resp.Checks) == 0 {
		t.Fatalf("校验应返回检查项，得到 %s", w.Body.String())
	}

	// GET 也应可用（只读校验）
	if g := do(t, srv, http.MethodGet, "/api/projects/"+created.ID+"/validate", "", nil); g.Code != http.StatusOK {
		t.Fatalf("GET 校验应 200，得到 %d", g.Code)
	}
	// 未知项目应 404
	if nf := do(t, srv, http.MethodPost, "/api/projects/nope/validate", `{}`, nil); nf.Code != http.StatusNotFound {
		t.Fatalf("未知项目校验应 404，得到 %d", nf.Code)
	}
}

func TestProbeEndpoint(t *testing.T) {
	// 被测服务自身起一个 httptest 后端，作为探测目标。
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"healthy"}`))
	}))
	defer backend.Close()

	srv, _ := newTestServer(t, "")
	create := do(t, srv, http.MethodPost, "/api/projects",
		`{"name":"demo","build":{"image_repo":"demo"},"deploy":{"method":"local-k3s","deployment":"d","container":"c"},"probe":{"enabled":true,"url":"`+backend.URL+`","body_contains":"healthy"}}`, nil)
	if create.Code != http.StatusCreated {
		t.Fatalf("创建项目应 201，得到 %d: %s", create.Code, create.Body.String())
	}
	var created store.Project
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("无法解析创建响应: %v / %s", err, create.Body.String())
	}

	w := do(t, srv, http.MethodPost, "/api/projects/"+created.ID+"/probe", `{}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("探测应 200，得到 %d: %s", w.Code, w.Body.String())
	}
	var res store.ProbeResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("解析探测响应失败: %v / %s", err, w.Body.String())
	}
	if res.Status != "ok" {
		t.Fatalf("探测应 ok，得到 %s (%s)", res.Status, res.Detail)
	}
	if res.StatusCode != 200 {
		t.Fatalf("StatusCode 应 200，得到 %d", res.StatusCode)
	}
	if !res.Matched {
		t.Fatalf("body_contains 应匹配")
	}

	// 未知项目应 404
	if nf := do(t, srv, http.MethodPost, "/api/projects/nope/probe", `{}`, nil); nf.Code != http.StatusNotFound {
		t.Fatalf("未知项目探测应 404，得到 %d", nf.Code)
	}
}

func TestRedactForExternal(t *testing.T) {
	in := `连接 user@203.0.113.10 失败
scp 到 /Users/me/.ssh/id_ed25519 权限不足
kubectl --kubeconfig /home/ci/.kube/config get pods
节点 192.168.1.20:6443 不可达
IPv6 fe80::1ff:fe23:4567:890a 超时
image: registry.internal:5000/app:1.2.3.4`
	out := redactForExternal(in)
	checks := []struct{ name, needle, mustContain, mustNotContain string }{
		{"IPv4", "203.0.113.10", "<redacted-ip>", "203.0.113.10"},
		{"IPv4 内网", "192.168.1.20", "<redacted-ip>", "192.168.1.20"},
		{"IPv6", "fe80::1ff:fe23:4567:890a", "<redacted-ip>", "fe80::"},
		{"user@host", "user@203.0.113.10", "<redacted-host>", "user@203"},
		{"mac 路径", "/Users/me/.ssh/id_ed25519", "<redacted-path>", "/Users/me"},
		{"linux 路径", "/home/ci/.kube/config", "<redacted-path>", "/home/ci"},
	}
	for _, c := range checks {
		if !strings.Contains(out, c.mustContain) {
			t.Fatalf("[%s] 期望含 %q，实际：\n%s", c.name, c.mustContain, out)
		}
		if strings.Contains(out, c.mustNotContain) {
			t.Fatalf("[%s] 不应仍含 %q，实际：\n%s", c.name, c.mustNotContain, out)
		}
	}
	// 内部镜像仓库主机名（非 IP、无 @）可保留；其标签里的 1.2.3.4 形如点分
	// 四段会被当作 IP 脱敏，这是预期行为，只需确认主机名本身未被误伤。
	if !strings.Contains(out, "registry.internal:5000/app:") {
		t.Fatalf("内部镜像仓库主机名不应被误脱敏：\n%s", out)
	}
	if strings.Contains(out, "1.2.3.4") {
		t.Fatalf("镜像标签里的点分四段应被脱敏：\n%s", out)
	}
}
