package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newVisitsClient() *http.Client {
	return &http.Client{Timeout: 3 * time.Second}
}

// TestFetchVisitsOnline: upstream returns a JSON payload → success map.
func TestFetchVisitsOnline(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/visits" || r.URL.Query().Get("pin") != "pinabc" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"total":45,"today":0,"uv":34}`))
	}))
	defer backend.Close()

	out, err := fetchVisits(newVisitsClient(), backend.URL, "pinabc")
	if err != nil {
		t.Fatalf("在线应无 error，得到 %v", err)
	}
	if v, _ := out["total"].(float64); v != 45 {
		t.Fatalf("total 应 45，得到 %v", out["total"])
	}
	if v, _ := out["uv"].(float64); v != 34 {
		t.Fatalf("uv 应 34，得到 %v", out["uv"])
	}
}

// TestFetchVisitsUpstream5xx: upstream (nginx) answers 502 → fetchVisits must
// report the HTTP status, not swallow it into a nil error.
func TestFetchVisitsUpstream5xx(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		http.Error(w, "<html>502 Bad Gateway</html>", http.StatusBadGateway)
	}))
	defer backend.Close()

	out, err := fetchVisits(newVisitsClient(), backend.URL, "pinabc")
	if out != nil {
		t.Fatalf("5xx 应返回 nil 载荷，得到 %v", out)
	}
	if err == nil {
		t.Fatalf("5xx 应返回 error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("error 应含 502，得到 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "nginx") {
		t.Fatalf("error 应提示 nginx 反代故障，得到 %q", err.Error())
	}
}

// TestFetchVisitsUnreachable: upstream is down (closed server) → fetchVisits
// reports an "不可达" error, not a 5xx.
func TestFetchVisitsUnreachable(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := backend.URL
	backend.Close() // socket now refuses connections

	out, err := fetchVisits(newVisitsClient(), dead, "pinabc")
	if out != nil {
		t.Fatalf("不可达应返回 nil 载荷，得到 %v", out)
	}
	if err == nil {
		t.Fatalf("不可达应返回 error")
	}
	if !strings.Contains(err.Error(), "不可达") {
		t.Fatalf("error 应含「不可达」，得到 %q", err.Error())
	}
}
