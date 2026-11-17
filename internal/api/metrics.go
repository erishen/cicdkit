package api

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/erishen/cicdkit/internal/store"
)

// durationBuckets 是构建/部署耗时直方图固定的桶边界（秒）。无 per-sample
// 标签，序列量恒定（桶数 × 1）。
var durationBuckets = []float64{10, 30, 60, 120, 300, 600, 1800, 3600}

// handleMetrics 暴露 Prometheus 文本格式指标（/metrics，不鉴权，语义同 /api/health）。
//
// 与 tsm-hub 的工程护栏一致：标签只用「有限枚举」——
//
//	status ∈ {queued, running, success, failed, canceled}
//	result ∈ {ok, fail, err, skip}
//
// 绝不把项目名 / 镜像名 / 日志 / 错误文本 / client_ip 写进标签，避免未鉴权
// 端点的高基数泄露（cicdkit 的 /metrics 由 tsm-hub 的 Prometheus 容器走容器
// DNS 抓取，不发布宿主端口、不改绑 0.0.0.0）。
//
// 指标全部从 store 现场派生，复用已有内存聚合、不新增常驻状态：
//
//	cicdkit_active_runs          当前 running/queued 运行数（gauge）
//	cicdkit_max_concurrent       并发上限（gauge）
//	cicdkit_runs_total{status}   保留历史里按状态计数的运行（counter）
//	cicdkit_deploys_total        保留的部署记录数（counter）
//	cicdkit_probe_total{result}  部署后服务探测结果计数（counter）
//	cicdkit_build_seconds        构建 stage 耗时直方图（histogram）
//	cicdkit_deploy_seconds       部署 stage 耗时直方图（histogram）
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	runs := s.store.ListRuns("")
	deps := s.store.ListDeployments("")

	active := 0
	statusCounts := make(map[string]int)
	for i := range runs {
		st := string(runs[i].Status)
		statusCounts[st]++
		if st == string(store.StatusRunning) || st == string(store.StatusQueued) {
			active++
		}
	}
	probeCounts := make(map[string]int)
	for i := range deps {
		if deps[i].Probe.Status != "" {
			probeCounts[deps[i].Probe.Status]++
		}
	}
	buildSamples := stageDurations(runs, "build")
	deploySamples := stageDurations(runs, "deploy")

	var b strings.Builder
	b.WriteString("# HELP cicdkit_active_runs Number of pipeline runs currently running or queued.\n")
	b.WriteString("# TYPE cicdkit_active_runs gauge\n")
	fmt.Fprintf(&b, "cicdkit_active_runs %d\n", active)

	b.WriteString("# HELP cicdkit_max_concurrent Maximum number of concurrent pipeline runs.\n")
	b.WriteString("# TYPE cicdkit_max_concurrent gauge\n")
	fmt.Fprintf(&b, "cicdkit_max_concurrent %d\n", s.cfg.Runner.MaxConcurrent)

	b.WriteString("# HELP cicdkit_runs_total Retained pipeline runs by status (history window level, not lifetime).\n")
	b.WriteString("# TYPE cicdkit_runs_total gauge\n")
	for _, st := range []string{"queued", "running", "success", "failed", "canceled"} {
		fmt.Fprintf(&b, "cicdkit_runs_total{status=%q} %d\n", st, statusCounts[st])
	}

	b.WriteString("# HELP cicdkit_deploys_total Retained deployment records (history window level).\n")
	b.WriteString("# TYPE cicdkit_deploys_total gauge\n")
	fmt.Fprintf(&b, "cicdkit_deploys_total %d\n", len(deps))

	b.WriteString("# HELP cicdkit_probe_total Post-deploy service probe outcomes (retained deployments).\n")
	b.WriteString("# TYPE cicdkit_probe_total gauge\n")
	for _, res := range []string{"ok", "fail", "err", "skip"} {
		fmt.Fprintf(&b, "cicdkit_probe_total{result=%q} %d\n", res, probeCounts[res])
	}

	writeHistogram(&b, "cicdkit_build_seconds", "Duration of build stages in seconds.", buildSamples)
	writeHistogram(&b, "cicdkit_deploy_seconds", "Duration of deploy stages in seconds.", deploySamples)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, b.String())
}

// stageDurations collects the durations (seconds) of every stage named `name`
// that ran to completion, across the retained runs. Terminal-but-failed stages
// are included (their duration is still meaningful); zero-timestamp stages are
// skipped so a just-started stage does not skew the histogram.
func stageDurations(runs []store.Run, name string) []float64 {
	var out []float64
	for i := range runs {
		for j := range runs[i].Stages {
			sg := &runs[i].Stages[j]
			if sg.Name != name || sg.StartedAt.IsZero() || sg.EndedAt.IsZero() {
				continue
			}
			d := sg.EndedAt.Sub(sg.StartedAt).Seconds()
			if d < 0 {
				continue
			}
			out = append(out, d)
		}
	}
	return out
}

// writeHistogram emits a Prometheus histogram in text exposition format from a
// slice of samples, using the fixed durationBuckets and no per-sample labels.
func writeHistogram(b *strings.Builder, name, help string, samples []float64) {
	b.WriteString("# HELP " + name + " " + help + "\n")
	b.WriteString("# TYPE " + name + " histogram\n")
	total := 0.0
	for _, v := range samples {
		total += v
	}
	for _, le := range durationBuckets {
		cum := 0
		for _, v := range samples {
			if v <= le {
				cum++
			}
		}
		fmt.Fprintf(b, "%s_bucket{le=%q} %d\n", name, floatStr(le), cum)
	}
	fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n", name, len(samples))
	fmt.Fprintf(b, "%s_sum %s\n", name, floatStr(total))
	fmt.Fprintf(b, "%s_count %d\n", name, len(samples))
}

// floatStr formats a histogram boundary / sum the way Prometheus expects:
// integral values without a decimal point, otherwise shortest round-trip form.
func floatStr(v float64) string {
	if v == float64(int64(v)) && v < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
