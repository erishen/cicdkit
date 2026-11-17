package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erishen/cicdkit/internal/store"
)

// TestMetricsExposesStoreDerivedSeries 验证 /metrics 不鉴权、输出 Prometheus
// 文本格式，且指标确实从 store 现场派生（不新增常驻状态）。
func TestMetricsExposesStoreDerivedSeries(t *testing.T) {
	srv, st := newTestServer(t, "")

	// 一条成功的 run：build + deploy 两个 stage（各 20s 耗时）。
	now := time.Now()
	r := store.Run{
		ID: "r1", ProjectID: "p1", Status: store.StatusSuccess,
		StartedAt: now.Add(-40 * time.Second), EndedAt: now,
		Stages: []store.StageResult{
			{Name: "build", Status: store.StatusSuccess,
				StartedAt: now.Add(-40 * time.Second), EndedAt: now.Add(-20 * time.Second)},
			{Name: "deploy", Status: store.StatusSuccess,
				StartedAt: now.Add(-20 * time.Second), EndedAt: now},
		},
	}
	if err := st.SaveRun(r); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveDeployment(store.Deployment{
		ID: "d1", ProjectID: "p1", Status: "deployed",
		Probe: store.ProbeResult{Status: "ok", URL: "http://x/"},
	}); err != nil {
		t.Fatal(err)
	}
	// 一条 running 的 run，用于 active gauge。
	if err := st.SaveRun(store.Run{ID: "r2", ProjectID: "p1", Status: store.StatusRunning}); err != nil {
		t.Fatal(err)
	}

	w := do(t, srv, http.MethodGet, "/metrics", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
	body := w.Body.String()
	for _, want := range []string{
		"cicdkit_active_runs 1",
		`cicdkit_runs_total{status="success"} 1`,
		`cicdkit_runs_total{status="running"} 1`,
		"cicdkit_deploys_total 1",
		`cicdkit_probe_total{result="ok"} 1`,
		"cicdkit_build_seconds_bucket{le=\"+Inf\"} 1",
		"cicdkit_build_seconds_sum 20",
		"cicdkit_deploy_seconds_count 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("缺少指标 %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestMetricsNoHighCardinalityLabels 守住 tsm-hub 同款护栏：标签值只能是
// 有限枚举，绝不出现项目名 / 镜像名 / 错误文本。
func TestMetricsNoHighCardinalityLabels(t *testing.T) {
	srv, st := newTestServer(t, "")
	if err := st.SaveRun(store.Run{
		ID: "r1", ProjectID: "secret-project-name", ProjectName: "secret-project-name",
		ImageRef: "registry.example.com/secret-image:tag", Status: store.StatusFailed,
		Stages: []store.StageResult{
			{Name: "build", Status: store.StatusFailed,
				StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now(),
				Error:   "build failed: some secret error text"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	w := do(t, srv, http.MethodGet, "/metrics", "", nil)
	body := w.Body.String()
	for _, leak := range []string{"secret-project-name", "registry.example.com", "secret error"} {
		if strings.Contains(body, leak) {
			t.Errorf("标签/指标泄露了高基数内容 %q:\n%s", leak, body)
		}
	}
}
