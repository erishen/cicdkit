package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erishen/cicdkit/internal/config"
	"github.com/erishen/cicdkit/internal/store"
)

func projWithProbe(spec store.ProbeSpec) store.Project {
	return store.Project{Name: "demo", Deploy: store.DeploySpec{Method: "local-k3s"}, Probe: spec}
}

func TestDisabledProbeSkips(t *testing.T) {
	p := projWithProbe(store.ProbeSpec{Enabled: false})
	r := Run(context.Background(), p, &config.Config{}, "")
	if r.Status != "skip" {
		t.Fatalf("disabled probe 应 skip，得到 %s", r.Status)
	}
}

func TestAutoDeriveSkipsWhenNoClusterAndNoURL(t *testing.T) {
	// method "" is not a cluster method, url empty -> skip with explanation.
	p := store.Project{Name: "demo", Deploy: store.DeploySpec{Method: ""}, Probe: store.ProbeSpec{Enabled: true}}
	r := Run(context.Background(), p, &config.Config{}, "")
	if r.Status != "skip" {
		t.Fatalf("非集群且无 URL 应 skip，得到 %s (%s)", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "不涉及集群") {
		t.Fatalf("skip 说明应提及不涉及集群，得到 %q", r.Detail)
	}
}

func TestProbeOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	p := projWithProbe(store.ProbeSpec{Enabled: true, Method: "GET", URL: srv.URL, ExpectedStatus: 200})
	r := Run(context.Background(), p, &config.Config{}, "")
	if r.Status != "ok" {
		t.Fatalf("期望 ok，得到 %s (%s)", r.Status, r.Detail)
	}
	if r.StatusCode != 200 {
		t.Fatalf("StatusCode 应 200，得到 %d", r.StatusCode)
	}
	if r.DurationMs < 0 {
		t.Fatalf("DurationMs 不应为负，得到 %d", r.DurationMs)
	}
}

func TestProbeStatusMismatchFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	p := projWithProbe(store.ProbeSpec{Enabled: true, URL: srv.URL, ExpectedStatus: 200})
	r := Run(context.Background(), p, &config.Config{}, "")
	if r.Status != "fail" {
		t.Fatalf("期望 fail，得到 %s (%s)", r.Status, r.Detail)
	}
}

func TestProbeBodyContains(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"healthy"}`))
	}))
	defer srv.Close()

	// match
	ok := projWithProbe(store.ProbeSpec{Enabled: true, URL: srv.URL, BodyContains: "healthy"})
	if r := Run(context.Background(), ok, &config.Config{}, ""); r.Status != "ok" {
		t.Fatalf("body 含 healthy 应 ok，得到 %s", r.Status)
	}
	// mismatch
	no := projWithProbe(store.ProbeSpec{Enabled: true, URL: srv.URL, BodyContains: "nope"})
	if r := Run(context.Background(), no, &config.Config{}, ""); r.Status != "fail" {
		t.Fatalf("body 不含 nope 应 fail，得到 %s", r.Status)
	}
}

func TestProbeMethodAndHeaders(t *testing.T) {
	var gotMethod, gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotHeader = r.Header.Get("X-Test")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	p := projWithProbe(store.ProbeSpec{
		Enabled: true, Method: "POST", URL: srv.URL, Headers: map[string]string{"X-Test": "hi"}, Body: `{"a":1}`,
	})
	r := Run(context.Background(), p, &config.Config{}, "")
	if r.Status != "ok" {
		t.Fatalf("期望 ok，得到 %s", r.Status)
	}
	if gotMethod != "POST" {
		t.Fatalf("应收到 POST，得到 %s", gotMethod)
	}
	if gotHeader != "hi" {
		t.Fatalf("应收到 X-Test=hi，得到 %q", gotHeader)
	}
}

func TestProbeEmptyBodyNoPanic(t *testing.T) {
	// 回归：spec.Body=="" 时，旧代码 bodyReader = nil（*strings.Reader 类型）
	// 会产生「持有 nil 指针的非 nil 接口」，传给 http.NewRequestWithContext
	// 后在 case *strings.Reader 分支 v.Len() 解引用 nil 指针 panic。
	// 即便目标地址不可达（此处用死地址），也必须返回 err 而非崩溃。
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("空 Body 探活不应 panic，得到 %v", rec)
		}
	}()
	res := probeURL(context.Background(), "GET", "http://127.0.0.1:1", store.ProbeSpec{Enabled: true}, 500*time.Millisecond)
	if res.Status == "" {
		t.Fatalf("空 Body 探活应返回非空的 Status（err/skip），得到空")
	}
}

func TestProbeTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	p := projWithProbe(store.ProbeSpec{Enabled: true, URL: srv.URL, Timeout: "50ms"})
	r := Run(context.Background(), p, &config.Config{}, "")
	if r.Status != "err" {
		t.Fatalf("超时应 err，得到 %s (%s)", r.Status, r.Error)
	}
}

func TestFirstNodeExternalIP(t *testing.T) {
	with := `{"items":[{"status":{"addresses":[{"type":"InternalIP","address":"192.168.0.1"},{"type":"ExternalIP","address":"1.2.3.4"}]}}]}`
	if got := firstNodeExternalIP(with); got != "1.2.3.4" {
		t.Fatalf("应返回 1.2.3.4，得到 %q", got)
	}
	none := `{"items":[{"status":{"addresses":[{"type":"InternalIP","address":"192.168.0.1"},{"type":"ExternalIP","address":"<none>"}]}}]}`
	if got := firstNodeExternalIP(none); got != "" {
		t.Fatalf("ExternalIP 为 <none> 应返回空，得到 %q", got)
	}
	empty := `{"items":[]}`
	if got := firstNodeExternalIP(empty); got != "" {
		t.Fatalf("无节点应返回空，得到 %q", got)
	}
}

func TestResolveURLSSHRequiresManualURL(t *testing.T) {
	// ssh 不是集群方法，未配置 probe.url，且未设置 CICD_SSH_HOST 环境变量
	// 时，无法推导探测地址，应跳过并提示在 .env 配置 CICD_SSH_HOST（而非
	// 旧版的「手动填写 URL」，现在优先从环境变量推导，避免把真实主机 IP
	// 写死在项目 JSON 里）。
	t.Setenv("CICD_SSH_HOST", "")
	p := store.Project{Name: "demo", Deploy: store.DeploySpec{Method: "ssh"}, Probe: store.ProbeSpec{Enabled: true}}
	urls, note := resolveURL(context.Background(), p, &config.Config{}, "")
	if len(urls) != 0 {
		t.Fatalf("ssh 无 URL 且无 CICD_SSH_HOST 时 resolveURL 应返回空地址，得到 %v", urls)
	}
	if !strings.Contains(note, "CICD_SSH_HOST") {
		t.Fatalf("resolveURL 说明应提示配置 CICD_SSH_HOST，得到 %q", note)
	}
}

func TestDeriveFromSSHUsesEnvHost(t *testing.T) {
	// ssh 部署的探针 URL 应由 CICD_SSH_HOST + 默认端口 8080 推导，
	// 无需在项目 JSON 里硬编码真实主机 IP。
	t.Setenv("CICD_SSH_HOST", "203.0.113.10")
	p := store.Project{Name: "demo", Deploy: store.DeploySpec{Method: "ssh"}, Probe: store.ProbeSpec{Enabled: true}}
	urls, note := resolveURL(context.Background(), p, &config.Config{}, "")
	if len(urls) != 1 || urls[0] != "http://203.0.113.10:8080" {
		t.Fatalf("ssh 应推导出 http://203.0.113.10:8080，得到 %v (note=%q)", urls, note)
	}
}

func TestDeriveFromSSHHonorsProbePort(t *testing.T) {
	// 若设置了 ssh_probe_port（或 CICD_SSH_PROBE_PORT），推导出的 URL 应使用该端口。
	t.Setenv("CICD_SSH_HOST", "203.0.113.10")
	t.Setenv("CICD_SSH_PROBE_PORT", "9090")
	p := store.Project{Name: "demo", Deploy: store.DeploySpec{Method: "ssh", SSHProbePort: "9090"}, Probe: store.ProbeSpec{Enabled: true}}
	urls, _ := resolveURL(context.Background(), p, &config.Config{}, "")
	if len(urls) != 1 || urls[0] != "http://203.0.113.10:9090" {
		t.Fatalf("ssh 应推导出 http://203.0.113.10:9090，得到 %v", urls)
	}
}

func TestResolveURLPerMethodOverrideFallback(t *testing.T) {
	// ssh 方法下，若未配置 CICD_SSH_HOST（.env），且配置了 urls.ssh，
	// 则回退到 urls.ssh 作为探测地址（.env 推导失败时的兜底）。
	t.Setenv("CICD_SSH_HOST", "")
	p := store.Project{
		Name:   "demo",
		Deploy: store.DeploySpec{Method: "ssh"},
		Probe:  store.ProbeSpec{Enabled: true, URLs: map[string]string{"ssh": "http://1.2.3.4:8080"}},
	}
	urls, note := resolveURL(context.Background(), p, &config.Config{}, "")
	if len(urls) != 1 || urls[0] != "http://1.2.3.4:8080" {
		t.Fatalf("ssh 未配置 CICD_SSH_HOST 且配置了 urls.ssh 时应回退到该地址，得到 %v", urls)
	}
	if note != "" {
		t.Fatalf("命中兜底地址时不应有说明，得到 %q", note)
	}
}

func TestResolveURLSSHEnvBeatsPerMethodOverride(t *testing.T) {
	// 当同时配置了 CICD_SSH_HOST（.env）与 urls.ssh 时，应优先使用 .env 推导的
	// 地址，避免在项目 JSON 里使用硬编码的真实主机 IP；urls.ssh 仅作兜底。
	t.Setenv("CICD_SSH_HOST", "203.0.113.10")
	p := store.Project{
		Name:   "demo",
		Deploy: store.DeploySpec{Method: "ssh"},
		Probe:  store.ProbeSpec{Enabled: true, URLs: map[string]string{"ssh": "http://1.2.3.4:8080"}},
	}
	urls, note := resolveURL(context.Background(), p, &config.Config{}, "")
	if len(urls) != 1 || urls[0] != "http://203.0.113.10:8080" {
		t.Fatalf("ssh 应优先使用 CICD_SSH_HOST 推导的地址，得到 %v (note=%q)", urls, note)
	}
	if note != "" {
		t.Fatalf("命中 .env 推导地址时不应有说明，得到 %q", note)
	}
}

func TestFirstNodeInternalIP(t *testing.T) {
	with := `{"items":[{"status":{"addresses":[{"type":"InternalIP","address":"192.168.0.1"},{"type":"ExternalIP","address":"1.2.3.4"}]}}]}`
	if got := firstNodeInternalIP(with); got != "192.168.0.1" {
		t.Fatalf("应返回 192.168.0.1，得到 %q", got)
	}
	empty := `{"items":[]}`
	if got := firstNodeInternalIP(empty); got != "" {
		t.Fatalf("无节点应返回空，得到 %q", got)
	}
}

func TestProbeUsesPerMethodURL(t *testing.T) {
	// 未配置 CICD_SSH_HOST 时，ssh 方法应回退到 urls.ssh 覆盖并实际探测该地址。
	t.Setenv("CICD_SSH_HOST", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// ssh 方法 + urls.ssh 覆盖，应实际探测该地址而非跳过。
	p := store.Project{
		Name:   "demo",
		Deploy: store.DeploySpec{Method: "ssh"},
		Probe:  store.ProbeSpec{Enabled: true, URLs: map[string]string{"ssh": srv.URL}},
	}
	r := Run(context.Background(), p, &config.Config{}, "")
	if r.Status != "ok" {
		t.Fatalf("ssh+urls.ssh 应探测成功，得到 %s (%s)", r.Status, r.Detail)
	}
	if r.URL != srv.URL {
		t.Fatalf("探测 URL 应为覆盖地址，得到 %q", r.URL)
	}
}
