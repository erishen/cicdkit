// a2a 包测试：
//   1) 静态 HMAC 向量 —— 与 Lume 的 hmac_sha256（hex 输出）+ key_for() 派生
//      公式做字节级对齐锚点（向量独立用 Python 计算）；
//   2) peer 验签行为 —— 冒充 / 未登记节点 / 时间窗外**必须被拒**；
//   3) A2A JSON-RPC 行为 —— 未知能力不猜（rejected / input-required）；
//   4) peer/invoke 白名单 + 脱敏。
package a2a

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/erishen/cicdkit/internal/store"
)

// 静态向量（Python 独立计算，root = "ROOT-SECRET-2026"）：
const (
	staticRoot   = "ROOT-SECRET-2026"
	staticShared = "SHARED-KEY"
	fxTs         = "1762200000"
	fxNonce      = "abc123"
	invBody      = `{"tool":"cicd.overview"}`

	keyCicd   = "35cb07611294951103ce1ee498705facb1543325ef1c08540941c79b4501df6f"
	keyLume   = "ef4bfe675fe420676d85f4ce17b6adb4026011a149dfe27ad42819cc7e24201c"
	sigCicdG  = "caa0dd07602d3d097661ddb621e0fad130446f430e7c6567afc5cd662cdc7a20"
	sigCicdP  = "033e808c91cfd103d9793488ecaa7786aa433d3e554b6fa6884612b2ff993be3"
	sigLumeG  = "c32f6a192bc4ba05c2f6382385b5afcbfcd33f4858f53354391787bd8168601a"
	sigShared = "94ab88fea0e070e4ab4e2ade01f50eca65957b72d8c19d5c3698c99b081f1267"
)

func TestStaticVectors(t *testing.T) {
	peer := New("cicdkit", "", staticRoot, "", 0)
	if got := peer.deriveKey("cicdkit"); got != keyCicd {
		t.Fatalf("deriveKey(cicdkit) = %s, want %s", got, keyCicd)
	}
	if got := peer.deriveKey("lume-a2a"); got != keyLume {
		t.Fatalf("deriveKey(lume-a2a) = %s, want %s", got, keyLume)
	}
	if got := signWith(keyCicd, "cicdkit", fxTs, fxNonce, scopeGET("/peer/health")); got != sigCicdG {
		t.Fatalf("GET sig mismatch")
	}
	if got := signWith(keyCicd, "cicdkit", fxTs, fxNonce, scopePOST("/peer/invoke", invBody)); got != sigCicdP {
		t.Fatalf("POST sig mismatch")
	}
	if got := signWith(keyLume, "lume-a2a", fxTs, fxNonce, scopeGET("/peer/health")); got != sigLumeG {
		t.Fatalf("lume sig mismatch")
	}
	if got := signWith(staticShared, "cicdkit", fxTs, fxNonce, scopeGET("/peer/health")); got != sigShared {
		t.Fatalf("shared sig mismatch")
	}
}

func writeRegistry(t *testing.T, dir string, peers ...string) string {
	t.Helper()
	p := filepath.Join(dir, "peers-identity.json")
	raw, _ := json.Marshal(map[string]any{"peers": peers})
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func signedRequest(method, url, node, ts, nonce, sig, body string) *http.Request {
	var r *http.Request
	var err error
	if body != "" {
		r, err = http.NewRequest(method, url, strings.NewReader(body))
	} else {
		r, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		panic(err)
	}
	q := r.URL.Query()
	q.Set("node", node)
	q.Set("ts", ts)
	q.Set("nonce", nonce)
	if sig != "" {
		q.Set("sig", sig)
	}
	r.URL.RawQuery = q.Encode()
	return r
}

// dynGetSig 用动态时间戳算 GET 签名（derived key）。
func dynGetSig(node, root, scope string) (ts, nonce, sig string) {
	ts = strconv.FormatInt(time.Now().Unix(), 10)
	nonce = strconv.FormatInt(time.Now().UnixNano(), 10)
	key := hmac.New(sha256.New, []byte(root))
	key.Write([]byte("peer:" + node))
	keyHex := hex.EncodeToString(key.Sum(nil))
	return ts, nonce, signWith(keyHex, node, ts, nonce, scope)
}

// dynPostSig 用动态时间戳算 POST 签名（derived key）。
func dynPostSig(node, root, path, body string) (ts, nonce, sig string) {
	ts = strconv.FormatInt(time.Now().Unix(), 10)
	nonce = strconv.FormatInt(time.Now().UnixNano(), 10)
	key := hmac.New(sha256.New, []byte(root))
	key.Write([]byte("peer:" + node))
	keyHex := hex.EncodeToString(key.Sum(nil))
	return ts, nonce, signWith(keyHex, node, ts, nonce, scopePOST(path, body))
}

func timeNowTs() string { return strconv.FormatInt(time.Now().Unix(), 10) }

func deriveKeyForPeer(root, node string) string {
	m := hmac.New(sha256.New, []byte(root))
	m.Write([]byte("peer:" + node))
	return hex.EncodeToString(m.Sum(nil))
}

func TestPeerVerifyDerived(t *testing.T) {
	dir := t.TempDir()
	reg := writeRegistry(t, dir, "lume-a2a")
	peer := New("cicdkit", "", staticRoot, reg, 120)

	// 正确签名（lume-a2a，derived key）通过。
	ts, nonce, sig := dynGetSig("lume-a2a", staticRoot, scopeGET("/peer/health"))
	r := signedRequest("GET", "/peer/health", "lume-a2a", ts, nonce, sig, "")
	if v := peer.Verify(r, "GET", scopeGET("/peer/health")); !v.OK {
		t.Fatalf("valid derived sig rejected: %s", v.Err)
	}

	// 冒充：用 cicdkit 的 key 签成 lume-a2a → 必须被拒。
	ts2 := timeNowTs()
	nonce2 := timeNowTs()
	badSig := signWith(deriveKeyForPeer(staticRoot, "cicdkit"), "lume-a2a", ts2, nonce2, scopeGET("/peer/health"))
	r2 := signedRequest("GET", "/peer/health", "lume-a2a", ts2, nonce2, badSig, "")
	if v := peer.Verify(r2, "GET", scopeGET("/peer/health")); v.OK {
		t.Fatalf("impersonation must be rejected")
	}

	// 未登记节点（ghost）必须被拒。
	ts3, nonce3, sig3 := dynGetSig("ghost", staticRoot, scopeGET("/peer/health"))
	r3 := signedRequest("GET", "/peer/health", "ghost", ts3, nonce3, sig3, "")
	if v := peer.Verify(r3, "GET", scopeGET("/peer/health")); v.OK {
		t.Fatalf("ghost node must be rejected")
	}

	// 时间窗外必须被拒。
	oldTs := strconv.FormatInt(time.Now().Unix()-9999, 10)
	r4 := signedRequest("GET", "/peer/health", "lume-a2a", oldTs, "n", "x", "")
	if v := peer.Verify(r4, "GET", scopeGET("/peer/health")); v.OK {
		t.Fatalf("stale ts must be rejected")
	}
}

func TestPeerVerifySharedAndDisabled(t *testing.T) {
	// shared 模式：正确 key 通过。
	peer := New("cicdkit", staticShared, "", "", 120)
	ts := timeNowTs()
	sig := signWith(staticShared, "lume-a2a", ts, "n", scopeGET("/peer/health"))
	r := signedRequest("GET", "/peer/health", "lume-a2a", ts, "n", sig, "")
	if v := peer.Verify(r, "GET", scopeGET("/peer/health")); !v.OK {
		t.Fatalf("shared sig rejected: %s", v.Err)
	}

	// 未配凭据 → 整面关闭。
	off := New("cicdkit", "", "", "", 0)
	if off.Enabled() {
		t.Fatalf("peer must be disabled without creds")
	}
	r2 := signedRequest("GET", "/peer/health", "lume-a2a", timeNowTs(), "n", "x", "")
	if v := off.Verify(r2, "GET", scopeGET("/peer/health")); v.OK {
		t.Fatalf("disabled peer must reject everything")
	}
}

// ── fakeStore ──────────────────────────────────────────────────────

type fakeStore struct {
	projects    []store.Project
	runs        map[string][]store.Run
	deployments map[string][]store.Deployment
}

func (f *fakeStore) seed() {
	f.projects = []store.Project{
		{ID: "p1", Name: "lume-crm"},
		{ID: "p2", Name: "tsm-hub"},
	}
	now := time.Now()
	f.runs = map[string][]store.Run{
		"p1": {{ID: "r1", ProjectID: "p1", ProjectName: "lume-crm", ImageTag: "v1.2.0",
			Status: store.StatusSuccess,
			Stages: []store.StageResult{
				{Name: "build", Status: store.StatusSuccess},
				{Name: "deploy", Status: store.StatusSuccess}},
			CreatedAt: now.Add(-time.Hour)}},
		"p2": {{ID: "r2", ProjectID: "p2", ProjectName: "tsm-hub", ImageTag: "v0.9.0",
			Status: store.StatusFailed,
			Stages: []store.StageResult{
				{Name: "build", Status: store.StatusSuccess},
				{Name: "deploy", Status: store.StatusFailed}},
			CreatedAt: now.Add(-2 * time.Hour)}},
	}
	f.deployments = map[string][]store.Deployment{
		"p1": {{ID: "d1", ProjectID: "p1", ProjectName: "lume-crm", ImageRef: "lume-crm:v1.2.0",
			Cluster: "orbstack", Namespace: "default", Method: "local-k3s",
			Status: "success", Probe: store.ProbeResult{Status: "ok", URL: "http://localhost:30408"},
			CreatedAt: now.Add(-time.Hour)}},
	}
}

func (f *fakeStore) ListProjects() []store.Project { return f.projects }
func (f *fakeStore) GetProject(id string) (store.Project, bool) {
	for _, p := range f.projects {
		if p.ID == id {
			return p, true
		}
	}
	return store.Project{}, false
}
func (f *fakeStore) CreateProject(store.Project) error { return nil }
func (f *fakeStore) UpdateProject(store.Project) error  { return nil }
func (f *fakeStore) DeleteProject(string) error        { return nil }
func (f *fakeStore) ListRuns(id string) []store.Run    { return f.runs[id] }
func (f *fakeStore) GetRun(id string) (store.Run, bool) {
	for _, rs := range f.runs {
		for _, r := range rs {
			if r.ID == id {
				return r, true
			}
		}
	}
	return store.Run{}, false
}
func (f *fakeStore) SaveRun(store.Run) error { return nil }
func (f *fakeStore) LastSuccessfulBuild(string) (store.Run, bool) {
	return store.Run{}, false
}
func (f *fakeStore) ClearRuns() error                              { return nil }
func (f *fakeStore) MarkInterruptedRuns(string) (int, error)       { return 0, nil }
func (f *fakeStore) ListDeployments(id string) []store.Deployment { return f.deployments[id] }
func (f *fakeStore) SaveDeployment(store.Deployment) error        { return nil }
func (f *fakeStore) GetDeployment(id string) (store.Deployment, bool) {
	for _, ds := range f.deployments {
		for _, d := range ds {
			if d.ID == id {
				return d, true
			}
		}
	}
	return store.Deployment{}, false
}
func (f *fakeStore) ClearDeployments() error { return nil }
func (f *fakeStore) ListRunsPaged(id string, limit, offset int) store.RunPage {
	rs := f.runs[id]
	if limit <= 0 || offset >= len(rs) {
		return store.RunPage{Runs: rs, Total: len(rs)}
	}
	end := offset + limit
	if end > len(rs) {
		end = len(rs)
	}
	return store.RunPage{Runs: rs[offset:end], Total: len(rs)}
}
func (f *fakeStore) ListDeploymentsPaged(id string, limit, offset int) store.DeploymentPage {
	ds := f.deployments[id]
	if limit <= 0 || offset >= len(ds) {
		return store.DeploymentPage{Deployments: ds, Total: len(ds)}
	}
	end := offset + limit
	if end > len(ds) {
		end = len(ds)
	}
	return store.DeploymentPage{Deployments: ds[offset:end], Total: len(ds)}
}
func (f *fakeStore) ListProjectsPaged(q string, limit, offset int) store.ProjectPage {
	if limit <= 0 || offset >= len(f.projects) {
		return store.ProjectPage{Projects: f.projects, Total: len(f.projects)}
	}
	end := offset + limit
	if end > len(f.projects) {
		end = len(f.projects)
	}
	return store.ProjectPage{Projects: f.projects[offset:end], Total: len(f.projects)}
}
func (f *fakeStore) Flush() {}

// newTestServer 构造测试用 a2a.Server。
func newTestServer(t *testing.T, st store.Store) *Server {
	t.Helper()
	caps := NewCapabilities()
	RegisterCICDCapabilities(caps, Deps{Store: st, KB: nil})
	return &Server{
		Node:  "cicdkit",
		Caps:  caps,
		Tasks: NewTaskStore(t.TempDir(), "cicdkit"),
	}
}

func TestA2ARPC(t *testing.T) {
	fs := &fakeStore{}
	fs.seed()
	srv := newTestServer(t, fs)
	mux := http.NewServeMux()
	mux.HandleFunc("/a2a", srv.HandleA2A)
	mux.HandleFunc("/.well-known/agent-card.json", srv.HandleAgentCard)
	srvh := httptest.NewServer(mux)
	defer srvh.Close()

	// Agent Card 公开未认证。
	resp, err := http.Get(srvh.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	cardRaw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var card map[string]any
	_ = json.Unmarshal(cardRaw, &card)
	if card["name"] != "cicdkit" {
		t.Fatalf("agent card name = %v", card["name"])
	}
	if skills := card["skills"].([]any); len(skills) != 5 {
		t.Fatalf("expected 5 skills, got %d", len(skills))
	}

	// 未指定 capability → input-required。
	rpc := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"hi"}]}}}`
	resp2, _ := http.Post(srvh.URL+"/a2a", "application/json", strings.NewReader(rpc))
	r2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	var out map[string]any
	_ = json.Unmarshal(r2, &out)
	status := out["result"].(map[string]any)["status"].(map[string]any)
	if status["state"] != StateInputRequired {
		t.Fatalf("no capability should be input-required, got %v", status["state"])
	}

	// 指定 cicd.overview → completed。
	rpc2 := `{"jsonrpc":"2.0","id":2,"method":"message/send","params":{"metadata":{"capability":"cicd.overview"}}}`
	resp3, _ := http.Post(srvh.URL+"/a2a", "application/json", strings.NewReader(rpc2))
	r3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	_ = json.Unmarshal(r3, &out)
	status = out["result"].(map[string]any)["status"].(map[string]any)
	if status["state"] != StateCompleted {
		t.Fatalf("cicd.overview should complete, got %v (raw %s)", status["state"], string(r3))
	}
	// artifact 正文应是 JSON 对象。
	arts := out["result"].(map[string]any)["artifacts"].([]any)
	art := arts[0].(map[string]any)
	bodyStr, _ := art["parts"].([]any)[0].(map[string]any)["text"].(string)
	var overview map[string]any
	if err := json.Unmarshal([]byte(bodyStr), &overview); err != nil {
		t.Fatalf("artifact body not JSON: %v\n%s", err, bodyStr)
	}
	if overview["project_count"] != float64(2) {
		t.Fatalf("project_count = %v, want 2", overview["project_count"])
	}

	// 未知 capability → rejected。
	rpc4 := `{"jsonrpc":"2.0","id":4,"method":"message/send","params":{"metadata":{"capability":"tsm.overview"}}}`
	resp5, _ := http.Post(srvh.URL+"/a2a", "application/json", strings.NewReader(rpc4))
	r5, _ := io.ReadAll(resp5.Body)
	resp5.Body.Close()
	_ = json.Unmarshal(r5, &out)
	status = out["result"].(map[string]any)["status"].(map[string]any)
	if status["state"] != StateRejected {
		t.Fatalf("unknown capability should be rejected, got %v", status["state"])
	}
}

func TestPeerInvokeWhitelist(t *testing.T) {
	fs := &fakeStore{}
	fs.seed()
	srv := newTestServer(t, fs)
	dir := t.TempDir()
	reg := writeRegistry(t, dir, "cicdkit", "lume-a2a")
	srv.PEER = New("cicdkit", "", staticRoot, reg, 120)

	mux := http.NewServeMux()
	mux.HandleFunc("/peer/invoke", srv.HandlePeerInvoke)
	mux.HandleFunc("/peer/tools", srv.HandlePeerTools)
	mux.HandleFunc("/peer/health", srv.HandlePeerHealth)
	srvh := httptest.NewServer(mux)
	defer srvh.Close()

	// 正确签名调 cicd.overview。
	node := "cicdkit"
	body := `{"tool":"cicd.overview","arguments":{}}`
	ts, nonce, sig := dynPostSig(node, staticRoot, "/peer/invoke", body)
	r := signedRequest("POST", srvh.URL+"/peer/invoke", node, ts, nonce, sig, body)
	resp, err := srvh.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "\"ok\":true") {
		t.Fatalf("peer/invoke ok:true expected, got %s", string(b))
	}

	// 未知名工具 → ok:false。
	body2 := `{"tool":"cicd.dangerous_delete","arguments":{}}`
	ts2, nonce2, sig2 := dynPostSig(node, staticRoot, "/peer/invoke", body2)
	r2 := signedRequest("POST", srvh.URL+"/peer/invoke", node, ts2, nonce2, sig2, body2)
	resp2, _ := srvh.Client().Do(r2)
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !strings.Contains(string(b2), "unknown tool") {
		t.Fatalf("unknown tool must be rejected, got %s", string(b2))
	}

	// /peer/tools 应列 5 个能力。
	ts3, nonce3, sig3 := dynGetSig(node, staticRoot, scopeGET("/peer/tools"))
	r3 := signedRequest("GET", srvh.URL+"/peer/tools", node, ts3, nonce3, sig3, "")
	resp3, _ := srvh.Client().Do(r3)
	b3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if !strings.Contains(string(b3), "cicd.overview") || !strings.Contains(string(b3), "when_to_use") {
		t.Fatalf("/peer/tools missing fields: %s", string(b3))
	}
}

// TestCapsSanitize 验证 RedactedProject 不泄露敏感字段。
func TestCapsSanitize(t *testing.T) {
	p := store.Project{
		ID: "p9", Name: "secret-proj",
		Build: store.BuildSpec{
			Registry: store.RegistryAuth{Server: "reg.example.com", Username: "u", Password: "s3cr3t-pw"},
		},
		Deploy: store.DeploySpec{SSHKeyPath: "/Users/x/.ssh/id_rsa", SSHHost: "1.2.3.4"},
	}
	red := p.Redacted()
	// Redacted 把敏感值替换为占位符 MaskedSecret("********")，不是空串。
	if red.Build.Registry.Password != store.MaskedSecret {
		t.Fatalf("Redacted must mask registry password with %q, got %q", store.MaskedSecret, red.Build.Registry.Password)
	}
	if red.Deploy.SSHKeyPath != store.MaskedSecret {
		t.Fatalf("Redacted must mask ssh key path with %q, got %q", store.MaskedSecret, red.Deploy.SSHKeyPath)
	}
	// 原文明密绝不出现。
	out := RedactedProject(p)
	if strings.Contains(out, "s3cr3t-pw") || strings.Contains(out, "id_rsa") {
		t.Fatalf("sanitized output leaked secret: %s", out)
	}
}
