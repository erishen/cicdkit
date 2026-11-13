// caps_cicd.go — cicdkit 的 5 个只读 A2A 能力。
//
// 设计约束（与 tsm-hub 的 caps_tsm.go 同构）：
//   * 全部只读，不暴露任何写入/执行原语（trigger pipeline / delete project /
//     SSH 执行 / registry 操作绝不进 A2A 面）；
//   * 数据脱敏：Project 走 Redacted()（registry 密码 → ********，
//     SSH key path → ********），Run 不含原始日志（只报状态/阶段）；
//   * 有参数能力（如按 project_id 查单个 run）不注册进 Lume agent 工具池
//     （Lume 框架 tool schema 不向模型暴露参数定义，模型会猜，违反「不猜」
//     原则）；但通过 /peer/invoke 白名单仍可带参调用。
package a2a

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/erishen/cicdkit/internal/kb"
	"github.com/erishen/cicdkit/internal/store"
)

// Deps 是能力实现需要的只读依赖注入。
type Deps struct {
	Store store.Store
	KB    *kb.KB
}

// RegisterCICDCapabilities 注册 5 个只读能力到 Capabilities 注册表。
func RegisterCICDCapabilities(c *Capabilities, d Deps) {
	c.Register(Capability{
		Name: "cicd.overview", Title: "项目总览",
		Desc: "当前有多少个受管项目、最近一次构建/部署状态、活跃 run 数",
		Tag: "status",
		ArgsExample: "{}",
		WhenToUse: "问『cicdkit 管着几个项目』『最近发布成功没』时用。",
	}, capOverview(d))

	c.Register(Capability{
		Name: "cicd.recent_runs", Title: "最近构建记录",
		Desc: "最近 N 条 pipeline run 的状态/阶段/耗时（不含原始日志）",
		Tag: "history",
		ArgsExample: "{\"limit\":5}",
		WhenToUse: "问『最近 5 次构建什么状态』『哪个阶段挂了』时用。",
	}, capRecentRuns(d))

	c.Register(Capability{
		Name: "cicd.deployments", Title: "最近部署记录",
		Desc: "最近 N 条部署历史（镜像版本/目标集群/探测结果）",
		Tag: "history",
		ArgsExample: "{\"limit\":5}",
		WhenToUse: "问『最近部署到了哪个集群』『镜像版本』时用。",
	}, capDeployments(d))

	c.Register(Capability{
		Name: "cicd.knowledge", Title: "AI 诊断知识库",
		Desc: "已采纳的 AI 故障诊断条目（错误模式 → 修复方案）",
		Tag: "diagnosis",
		ArgsExample: "{}",
		WhenToUse: "问『之前遇到过类似报错』『知识库有几条采纳的诊断』时用。",
	}, capKnowledge(d))

	c.Register(Capability{
		Name: "cicd.capabilities", Title: "能力清单",
		Desc: "本节点当前注册的所有 A2A 能力（meta 自描述）",
		Tag: "meta",
		ArgsExample: "{}",
		WhenToUse: "问『cicdkit 能做什么』时用。",
	}, capCapabilities(c))
}

// ── 实现 ─────────────────────────────────────────────────────────────

// capOverview: 项目数 + 最近 run 状态 + 活跃任务数。
func capOverview(d Deps) DispatchFunc {
	return func(args map[string]any) DispatchResult {
		projects := d.Store.ListProjects()
		// 取每个项目最近一条 run，汇总状态分布。
		runStats := map[string]int{"success": 0, "failed": 0, "running": 0, "canceled": 0}
		var lastRuns []map[string]any
		for _, p := range projects {
			runs := d.Store.ListRuns(p.ID)
			if len(runs) > 0 {
				last := runs[0]
				status := string(last.Status)
				switch status {
				case string(store.StatusSuccess):
					runStats["success"]++
				case string(store.StatusFailed):
					runStats["failed"]++
				case string(store.StatusRunning):
					runStats["running"]++
				case string(store.StatusCanceled):
					runStats["canceled"]++
				}
				stageSummary := make([]map[string]any, 0, len(last.Stages))
				for _, s := range last.Stages {
					stageSummary = append(stageSummary, map[string]any{
						"name":   s.Name,
						"status": string(s.Status),
					})
				}
				lastRuns = append(lastRuns, map[string]any{
					"project":   p.Name,
					"status":    status,
					"image_tag": last.ImageTag,
					"stages":    stageSummary,
				})
			}
		}
		names := make([]string, 0, len(projects))
		for _, p := range projects {
			names = append(names, p.Name)
		}
		return completedArtifact(map[string]any{
			"project_count": len(projects),
			"projects":      names,
			"run_status":    runStats,
			"last_runs":     lastRuns,
		})
	}
}

// capRecentRuns: 取全项目最近 N 条 run（按 created_at 倒序）。
func capRecentRuns(d Deps) DispatchFunc {
	return func(args map[string]any) DispatchResult {
		limit := 5
		if v, ok := args["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		if limit > 50 {
			limit = 50
		}
		type runSummary struct {
			ID          string         `json:"id"`
			ProjectName string         `json:"project"`
			Trigger     string         `json:"trigger"`
			ImageTag    string         `json:"image_tag"`
			Status      string         `json:"status"`
			Stages      []map[string]any `json:"stages"`
			CreatedAt   string         `json:"created_at"`
			DurationMs  int64          `json:"duration_ms,omitempty"`
		}
		projects := d.Store.ListProjects()
		var all []runSummary
		for _, p := range projects {
			runs := d.Store.ListRuns(p.ID)
			for i, r := range runs {
				if i >= limit {
					break
				}
				stages := make([]map[string]any, 0, len(r.Stages))
				for _, s := range r.Stages {
					stages = append(stages, map[string]any{
						"name":   s.Name,
						"status": string(s.Status),
					})
				}
				var durMs int64
				if !r.StartedAt.IsZero() && !r.EndedAt.IsZero() {
					durMs = r.EndedAt.Sub(r.StartedAt).Milliseconds()
				}
				all = append(all, runSummary{
					ID:          r.ID,
					ProjectName: p.Name,
					Trigger:     r.Trigger,
					ImageTag:    r.ImageTag,
					Status:      string(r.Status),
					Stages:      stages,
					CreatedAt:   r.CreatedAt.Format(time.RFC3339),
					DurationMs:  durMs,
				})
			}
		}
		// 按 created_at 倒序
		for i := 1; i < len(all); i++ {
			for j := i; j > 0 && all[j].CreatedAt > all[j-1].CreatedAt; j-- {
				all[j], all[j-1] = all[j-1], all[j]
			}
		}
		if len(all) > limit {
			all = all[:limit]
		}
		return completedArtifact(map[string]any{
			"count": len(all),
			"runs":  all,
		})
	}
}

// capDeployments: 最近 N 条部署历史。
func capDeployments(d Deps) DispatchFunc {
	return func(args map[string]any) DispatchResult {
		limit := 5
		if v, ok := args["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		if limit > 50 {
			limit = 50
		}
		type deploySummary struct {
			ID          string `json:"id"`
			ProjectName string `json:"project"`
			ImageRef    string `json:"image_ref"`
			Cluster     string `json:"cluster"`
			Namespace   string `json:"namespace"`
			Method      string `json:"method"`
			Target      string `json:"target,omitempty"`
			Status      string `json:"status"`
			ProbeStatus string `json:"probe_status,omitempty"`
			ProbeURL    string `json:"probe_url,omitempty"`
			CreatedAt   string `json:"created_at"`
		}
		projects := d.Store.ListProjects()
		var all []deploySummary
		for _, p := range projects {
			depl := d.Store.ListDeployments(p.ID)
			for i, dp := range depl {
				if i >= limit {
					break
				}
				all = append(all, deploySummary{
					ID:          dp.ID,
					ProjectName: p.Name,
					ImageRef:    dp.ImageRef,
					Cluster:     dp.Cluster,
					Namespace:   dp.Namespace,
					Method:      dp.Method,
					Target:      dp.Target,
					Status:      dp.Status,
					ProbeStatus: dp.Probe.Status,
					ProbeURL:    dp.Probe.URL,
					CreatedAt:   dp.CreatedAt.Format(time.RFC3339),
				})
			}
		}
		for i := 1; i < len(all); i++ {
			for j := i; j > 0 && all[j].CreatedAt > all[j-1].CreatedAt; j-- {
				all[j], all[j-1] = all[j-1], all[j]
			}
		}
		if len(all) > limit {
			all = all[:limit]
		}
		return completedArtifact(map[string]any{
			"count":       len(all),
			"deployments": all,
		})
	}
}

// capKnowledge: 知识库条目摘要（不含完整日志，只报签名/错误关键词/诊断摘要）。
func capKnowledge(d Deps) DispatchFunc {
	return func(args map[string]any) DispatchResult {
		if d.KB == nil {
			return completedArtifact(map[string]any{
				"count": 0,
				"entries": []any{},
				"note":  "知识库未配置",
			})
		}
		entries := d.KB.List()
		summaries := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			// 只暴露摘要，不暴露完整日志/error_excerpt（可能含 IP/路径）
			summaries = append(summaries, map[string]any{
				"id":             e.ID,
				"project":        e.ProjectName,
				"stage":         e.Stage,
				"error_keyword":  e.ErrorKeyword,
				"diagnosis_len":  len(e.Diagnosis),
				"adopted_count":  e.AdoptedCount,
				"adopted_at":     e.AdoptedAt,
			})
		}
		return completedArtifact(map[string]any{
			"count":   len(entries),
			"entries": summaries,
		})
	}
}

// capCapabilities: 自描述（meta）。
func capCapabilities(c *Capabilities) DispatchFunc {
	return func(args map[string]any) DispatchResult {
		out := make([]map[string]any, 0, len(c.All()))
		for _, cap := range c.All() {
			out = append(out, map[string]any{
				"name":        cap.Name,
				"title":       cap.Title,
				"desc":        cap.Desc,
				"tag":         cap.Tag,
				"when_to_use": cap.WhenToUse,
			})
		}
		return completedArtifact(map[string]any{
			"count":      len(out),
			"capabilities": out,
		})
	}
}

// ── 辅助 ─────────────────────────────────────────────────────────────

// RedactedProject 把 Project 做脱敏后 JSON 序列化（供需要完整 project 结构的能力用）。
func RedactedProject(p store.Project) string {
	redacted := p.Redacted()
	b, err := json.Marshal(redacted)
	if err != nil {
		return fmt.Sprintf("{\"error\":%q}", err.Error())
	}
	return string(b)
}
