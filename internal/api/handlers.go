package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/erishen/cicdkit/internal/check"
	"github.com/erishen/cicdkit/internal/build"
	"github.com/erishen/cicdkit/internal/deploy"
	"github.com/erishen/cicdkit/internal/generate"
	"github.com/erishen/cicdkit/internal/kb"
	"github.com/erishen/cicdkit/internal/llm"
	"github.com/erishen/cicdkit/internal/probe"
	"github.com/erishen/cicdkit/internal/scan"
	"github.com/erishen/cicdkit/internal/store"
)

func jsonEncoder(w http.ResponseWriter) *json.Encoder {
	return json.NewEncoder(w)
}

// repoImage builds the full image reference from a project's image_repo + tag.
func repoImage(p store.Project, tag string) string {
	return strings.TrimSuffix(p.Build.ImageRepo, "/") + ":" + tag
}

// redactAll masks stored credentials in a list response.
func redactAll(ps []store.Project) []store.Project {
	out := make([]store.Project, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Redacted())
	}
	return out
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "cicdkit"})
}

// handleVersion 返回后端构建戳，供前端 footer 展示，便于确认浏览器连的是否为最新二进制。
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "cicdkit", "build": BuildStamp})
}

// ---- Projects ----

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit, offset := parsePage(r)
	page := s.store.ListProjectsPaged(q, limit, offset)
	page.Projects = redactAll(page.Projects)
	writeJSON(w, http.StatusOK, page)
}

// handleScanProject derives a best-effort project draft from a local directory.
// The UI's "import from directory" flow uses server-side path mode:
//
//	{"path": "/abs/dir"}  — the server walks the directory on disk (via
//	                        GET /api/fs/* it lets the user pick a directory that
//	                        is guaranteed reachable from the server, avoiding the
//	                        browser's inability to reveal real local paths).
//
// A legacy {"root","files"} shape is still accepted for callers that read file
// contents in the browser, but the UI no longer uses it. Either way the response
// is a prefilled draft the UI feeds into the create-project form; nothing is
// persisted here.
func (s *Server) handleScanProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path  string `json:"path"`
		Root  string `json:"root"`
		Files []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		} `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}

	var draft *scan.Draft
	if strings.TrimSpace(body.Path) != "" {
		d, err := scan.ScanPath(strings.TrimSpace(body.Path))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		draft = d
	} else if len(body.Files) > 0 {
		files := make(map[string]string, len(body.Files))
		for _, f := range body.Files {
			rel := strings.TrimSpace(f.Path)
			if rel == "" {
				continue
			}
			files[rel] = f.Content
		}
		if len(files) == 0 {
			writeError(w, http.StatusBadRequest, "未提供任何文件")
			return
		}
		draft = scan.ScanFiles(strings.TrimSpace(body.Root), files)
	} else {
		writeError(w, http.StatusBadRequest, "请提供 path（本地目录）或 files（目录内的文件列表）")
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var p store.Project
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if err := p.Validate(s.cfg.Runner.WorkDir); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now()
	if p.ID == "" {
		p.ID = store.GenID("proj")
	}
	p.CreatedAt = now
	p.UpdatedAt = now
	if err := s.store.CreateProject(p); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p.Redacted())
}

func (s *Server) handleProjectByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/projects/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少项目 id")
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.getProject(w, r, id)
		case http.MethodPut:
			s.updateProject(w, r, id)
		case http.MethodDelete:
			s.deleteProject(w, r, id)
		default:
			writeError(w, http.StatusMethodNotAllowed, "方法不被允许")
		}
		return
	}

	action := parts[1]
	switch action {
	case "build":
		if r.Method == http.MethodPost {
			s.trigger(w, r, id, "build")
			return
		}
	case "pipeline":
		if r.Method == http.MethodPost {
			s.trigger(w, r, id, "pipeline")
			return
		}
	case "deploy":
		if r.Method == http.MethodPost {
			s.triggerDeploy(w, r, id)
			return
		}
	case "validate":
		if r.Method == http.MethodPost || r.Method == http.MethodGet {
			s.handleValidateProject(w, r, id)
			return
		}
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	case "probe":
		if r.Method == http.MethodPost || r.Method == http.MethodGet {
			s.handleProbeProject(w, r, id)
			return
		}
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	case "generate":
		switch r.Method {
		case http.MethodGet:
			s.handleGeneratePlan(w, r, id)
		case http.MethodPost:
			s.handleGenerateApply(w, r, id)
		default:
			writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
		}
		return
	}
	writeError(w, http.StatusNotFound, "未找到该资源")
}

// handleValidateProject runs a dry-run validation of a project (config sanity,
// docker daemon and kubeconfig reachability) without building or deploying.
func (s *Server) handleValidateProject(w http.ResponseWriter, r *http.Request, id string) {
	p, ok := s.store.GetProject(id)
	if !ok {
		writeError(w, http.StatusNotFound, "项目不存在: "+id)
		return
	}
	results := check.Run(r.Context(), p, s.cfg)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":     check.Summary(results),
		"checks": results,
	})
}

// handleProbeProject runs the service-availability probe on demand (Postman
// style) and returns the full result — status code, latency, headers and body
// — so the UI can present it like a request/response console.
func (s *Server) handleProbeProject(w http.ResponseWriter, r *http.Request, id string) {
	p, ok := s.store.GetProject(id)
	if !ok {
		writeError(w, http.StatusNotFound, "项目不存在: "+id)
		return
	}
	// An optional target name probes that named DeployTarget's address
	// (probe.urls[target]); otherwise the project's primary Deploy is used.
	var body struct {
		Target string `json:"target"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	res := probe.Run(r.Context(), p, s.cfg, strings.TrimSpace(body.Target))
	writeJSON(w, http.StatusOK, res)
}

// handleGeneratePlan previews the files that would be scaffolded for a project
// (a missing Dockerfile and/or k8s manifest) without writing anything. The UI
// shows the content and the config changes before the user confirms.
func (s *Server) handleGeneratePlan(w http.ResponseWriter, r *http.Request, id string) {
	p, ok := s.store.GetProject(id)
	if !ok {
		writeError(w, http.StatusNotFound, "项目不存在: "+id)
		return
	}
	plan, err := generate.PlanProject(p, s.cfg.Runner.WorkDir)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// handleGenerateApply writes the scaffolded files into the project's build
// context and updates the project config (dockerfile / manifest_path /
// deployment / container / image_repo) so it is immediately publishable.
func (s *Server) handleGenerateApply(w http.ResponseWriter, r *http.Request, id string) {
	p, ok := s.store.GetProject(id)
	if !ok {
		writeError(w, http.StatusNotFound, "项目不存在: "+id)
		return
	}
	plan, err := generate.Apply(p, s.cfg.Runner.WorkDir, s.store)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !plan.NeedsAny {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"needs_any": false,
			"message":   "Dockerfile 与 k8s 清单均已存在，无需生成",
		})
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request, id string) {
	p, ok := s.store.GetProject(id)
	if !ok {
		writeError(w, http.StatusNotFound, "项目不存在")
		return
	}
	writeJSON(w, http.StatusOK, p.Redacted())
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request, id string) {
	var p store.Project
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	old, ok := s.store.GetProject(id)
	if !ok {
		writeError(w, http.StatusNotFound, "项目不存在")
		return
	}
	p.ID = id
	// The UI only ever sees a masked password, so preserve the stored secret
	// unless a genuinely new one was supplied.
	p = p.MergeSecrets(old)
	if err := p.Validate(s.cfg.Runner.WorkDir); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.UpdatedAt = time.Now()
	if err := s.store.UpdateProject(p); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p.Redacted())
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.store.DeleteProject(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// ---- Triggers ----

func (s *Server) trigger(w http.ResponseWriter, r *http.Request, id string, kind string) {
	var body struct {
		Tag    string `json:"tag"`
		Method string `json:"method"`
		Target string `json:"target"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	p, ok := s.store.GetProject(id)
	if !ok {
		writeError(w, http.StatusNotFound, "项目不存在")
		return
	}
	// A per-run method override lets one project publish to different targets
	// (e.g. local k3s vs. ssh to a bare host) from the UI without editing it.
	if !store.IsValidDeployMethod(body.Method) {
		writeError(w, http.StatusBadRequest, "非法的部署方式覆盖: "+body.Method)
		return
	}
	// A named target selects one of the project's DeployTargets. It must exist
	// (when set) so we never silently deploy the wrong destination.
	if tn := strings.TrimSpace(body.Target); tn != "" {
		found := false
		for _, t := range p.Targets {
			if t.Name == tn {
				found = true
				break
			}
		}
		if !found {
			writeError(w, http.StatusBadRequest, "未找到发布目标: "+tn)
			return
		}
	}
	var (
		run *store.Run
		err error
	)
	if kind == "pipeline" {
		run, err = s.runner.TriggerPipeline(id, "manual", body.Tag, body.Method, body.Target)
	} else {
		run, err = s.runner.TriggerBuild(id, "manual", body.Tag, body.Method, body.Target)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) triggerDeploy(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Tag      string `json:"tag"`
		ImageRef string `json:"image_ref"`
		Target   string `json:"target"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	p, ok := s.store.GetProject(id)
	if !ok {
		writeError(w, http.StatusNotFound, "项目不存在")
		return
	}
	if tn := strings.TrimSpace(body.Target); tn != "" {
		found := false
		for _, t := range p.Targets {
			if t.Name == tn {
				found = true
				break
			}
		}
		if !found {
			writeError(w, http.StatusBadRequest, "未找到发布目标: "+tn)
			return
		}
	}
	imageRef := body.ImageRef
	reusedLast := false
	if imageRef == "" {
		if body.Tag != "" {
			imageRef = repoImage(p, body.Tag)
		} else if last, ok := s.store.LastSuccessfulBuild(id); ok {
			// Reuse the most recently built image instead of inventing a fresh
			// timestamp that was never built (which would make the deploy fail
			// at the cluster with ImagePullBackOff).
			imageRef = last.ImageRef
			reusedLast = true
		} else {
			writeError(w, http.StatusBadRequest,
				"尚无成功构建的镜像，请先执行 Build / Pipeline，或在请求中指定 image_ref 或 tag")
			return
		}
	}
	// When we auto-reused a previously-built image, verify it is still present
	// in the local docker daemon. A daemon restart, `docker image prune`, or a
	// different machine would silently drop it, and the deploy would then fail
	// at `docker save` with "reference does not exist". Fall back to a fresh
	// build+deploy so the action still succeeds instead of erroring out.
	if reusedLast {
		dockerCfg := build.DockerConfig{DockerHost: s.cfg.Runner.DockerHost}
		if !build.ImagePresent(r.Context(), dockerCfg, imageRef) {
			run, err := s.runner.TriggerPipeline(id, "manual", "", "", body.Target)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, http.StatusAccepted, run)
			return
		}
		// Guard against shipping a cached build whose architecture does not
		// match the target host: a previous build on an Apple-Silicon runner
		// (or a stale arm64 image) would otherwise be pushed to an amd64 cloud
		// host and fail under QEMU emulation with a "platform does not match
		// host" warning. If the local image's arch differs from the host's,
		// rebuild fresh instead of reusing. Only applies to ssh targets (k3s /
		// local builds stay on the native runner arch). A host-probe failure
		// (unreachable) falls through to reuse, so reachability never blocks a
		// deploy.
		if spec, rerr := p.ResolveDeploySpec(body.Target, ""); rerr == nil && spec.Method == "ssh" {
			if remoteArch := deploy.DetectRemoteArch(r.Context(), spec); remoteArch != "" {
				wantArch := strings.TrimPrefix(remoteArch, "linux/")
				if localArch := build.ImageArch(r.Context(), dockerCfg, imageRef); localArch != "" && localArch != wantArch {
					run, err := s.runner.TriggerPipeline(id, "manual", "", "", body.Target)
					if err != nil {
						writeError(w, http.StatusBadRequest, err.Error())
						return
					}
					writeJSON(w, http.StatusAccepted, run)
					return
				}
			}
		}
	}
	run, err := s.runner.TriggerDeploy(id, "manual", imageRef, "", body.Target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

// ---- Runs ----

// parsePage reads limit/offset from the query string. limit defaults to 20 and
// is capped at 100; offset defaults to 0. limit<=0 (explicit ?limit=0) means
// "return everything", which the store honours.
func parsePage(r *http.Request) (limit, offset int) {
	limit = 20
	offset = 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n > 0 {
				if n > 100 {
					n = 100
				}
				limit = n
			} else {
				limit = 0
			}
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	limit, offset := parsePage(r)
	writeJSON(w, http.StatusOK, s.store.ListRunsPaged(projectID, limit, offset))
}

// handleClearRuns wipes the entire run history. Wired as DELETE /api/runs via
// methodSwitch; the UI asks for confirmation before calling it.
func (s *Server) handleClearRuns(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ClearRuns(); err != nil {
		writeError(w, http.StatusInternalServerError, "清空运行记录失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"cleared": "runs"})
}

func (s *Server) handleRunByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/runs/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少运行 id")
		return
	}
	if len(parts) > 1 && parts[1] == "cancel" && r.Method == http.MethodPost {
		if !s.runner.Cancel(id) {
			writeError(w, http.StatusNotFound, "运行不存在或已结束")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"canceled": id})
		return
	}
	if len(parts) > 1 && parts[1] == "diagnose" && r.Method == http.MethodPost {
		run, ok := s.store.GetRun(id)
		if !ok {
			writeError(w, http.StatusNotFound, "运行不存在")
			return
		}
		s.handleDiagnoseRun(w, r, run)
		return
	}
	if len(parts) >= 3 && parts[1] == "diagnosis" && parts[2] == "adopt" && r.Method == http.MethodPost {
		s.handleAdoptDiagnosis(w, r, id)
		return
	}
	run, ok := s.store.GetRun(id)
	if !ok {
		writeError(w, http.StatusNotFound, "运行不存在")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// ---- Deployments ----

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	limit, offset := parsePage(r)
	writeJSON(w, http.StatusOK, s.store.ListDeploymentsPaged(projectID, limit, offset))
}

// handleClearDeployments wipes the entire deployment history. Wired as DELETE
// /api/deployments via methodSwitch; the UI asks for confirmation first.
func (s *Server) handleClearDeployments(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ClearDeployments(); err != nil {
		writeError(w, http.StatusInternalServerError, "清空部署历史失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"cleared": "deployments"})
}

// handleDeploymentByID serves per-deployment detail. The only action today is
// "diagnose", which jumps to the originating Run's logs (via RunID) and asks
// the LLM for a failure analysis.
func (s *Server) handleDeploymentByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/deployments/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少部署 id")
		return
	}
	if len(parts) > 1 && parts[1] == "diagnose" && r.Method == http.MethodPost {
		dep, ok := s.store.GetDeployment(id)
		if !ok {
			writeError(w, http.StatusNotFound, "部署记录不存在")
			return
		}
		run, ok := s.store.GetRun(dep.RunID)
		if !ok {
			writeError(w, http.StatusNotFound, "找不到该部署对应的运行记录（无法读取日志）")
			return
		}
		s.handleDiagnoseRun(w, r, run)
		return
	}
	dep, ok := s.store.GetDeployment(id)
	if !ok {
		writeError(w, http.StatusNotFound, "部署记录不存在")
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

// handleConfigLLM reads (GET, masked) or writes (PUT) the optional LLM config
// used by the AI diagnosis feature. The API key is never returned in clear.
func (s *Server) handleConfigLLM(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, s.llm.Masked())
		return
	}
	// PUT
	var body struct {
		Enabled  bool   `json:"enabled"`
		BaseURL  string `json:"base_url"`
		APIKey   string `json:"api_key"`
		Model    string `json:"model"`
		System   string `json:"system"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "配置解析失败: "+err.Error())
		return
	}
	cfg := *s.llm
	cfg.Enabled = body.Enabled
	cfg.BaseURL = strings.TrimSpace(body.BaseURL)
	cfg.Model = strings.TrimSpace(body.Model)
	cfg.System = body.System
	// Keep the existing key when the client sends an empty one (it does so
	// because GET never returns the real value — we only expose api_key_set).
	if body.APIKey != "" {
		cfg.APIKey = body.APIKey
	}
	if cfg.BaseURL != "" && cfg.Model == "" {
		writeError(w, http.StatusBadRequest, "已填写 BaseURL，请同时填写 Model")
		return
	}
	if err := llm.Save(s.llmPath, &cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "保存 LLM 配置失败: "+err.Error())
		return
	}
	s.llm = &cfg
	writeJSON(w, http.StatusOK, cfg.Masked())
}

// buildRunLogs assembles the most relevant failure context from a run: its
// overall status plus every failed stage's error and log. This is what gets
// sent to the model.
func (s *Server) buildRunLogs(run store.Run) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("项目: %s\n运行ID: %s\n状态: %s\n镜像: %s\n触发: %s\n",
		run.ProjectName, run.ID, run.Status, run.ImageRef, run.Trigger))
	if run.Log != "" {
		b.WriteString("\n--- 总日志 ---\n" + run.Log + "\n")
	}
	for _, st := range run.Stages {
		if st.Status == store.StatusFailed || st.Error != "" {
			b.WriteString(fmt.Sprintf("\n--- 阶段: %s (状态: %s) ---\n", st.Name, st.Status))
			if st.Error != "" {
				b.WriteString("错误: " + st.Error + "\n")
			}
			if st.Log != "" {
				b.WriteString(st.Log + "\n")
			}
		}
	}
	logs := b.String()
	// Keep the tail: build/deploy failures are usually at the end, and we must
	// stay within model context limits.
	const maxLen = 12000
	if len(logs) > maxLen {
		logs = logs[len(logs)-maxLen:]
	}
	return logs
}

// redactSecrets matches the kinds of internal detail a run log can leak: IPv4/
// IPv6 addresses, ssh-style user@host targets, and absolute paths under the
// common home directories. They are replaced with opaque placeholders before
// the log leaves the machine (e.g. sent to an external LLM for diagnosis).
var (
	reIPV4      = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	reIPV6      = regexp.MustCompile(`\b(?:[A-Fa-f0-9]{1,4}:){3,}[A-Fa-f0-9]{0,4}\b|\b[A-Fa-f0-9]{0,4}:(?::[A-Fa-f0-9]{0,4}){2,}\b`)
	reUserHost  = regexp.MustCompile(`\b[\w.+-]+@[\w.-]+\b`)
	reLocalPath = regexp.MustCompile(`(?:/Users/|/home/|/root/)[^\s"'<>]*`)
)

func redactForExternal(logs string) string {
	// Order matters: mask user@host before bare IPs, so an IP inside a
	// user@host target is collapsed into the host placeholder rather than
	// leaving "user@<redacted-ip>".
	s := reLocalPath.ReplaceAllString(logs, "<redacted-path>")
	s = reUserHost.ReplaceAllString(s, "<redacted-host>")
	s = reIPV4.ReplaceAllString(s, "<redacted-ip>")
	s = reIPV6.ReplaceAllString(s, "<redacted-ip>")
	return s
}

// handleDiagnoseRun runs the AI failure analysis for a run and returns the
// model's text. It is the shared backend for both /api/runs/:id/diagnose and
// /api/deployments/:id/diagnose (the latter resolves the run via RunID first).
func (s *Server) handleDiagnoseRun(w http.ResponseWriter, r *http.Request, run store.Run) {
	if s.llm == nil || !s.llm.Enabled || s.llm.APIKey == "" {
		writeError(w, http.StatusBadRequest,
			"LLM 未配置：请先在「设置」中填写 BaseURL / API Key / Model 并启用")
		return
	}
	if run.Status != store.StatusFailed {
		writeError(w, http.StatusBadRequest, "该运行并未失败，无需诊断")
		return
	}
	// Cache hit: a previous diagnosis is stored on the run, so we return it
	// without calling the model again.
	if run.Diagnosis != nil && run.Diagnosis.Text != "" {
		run.Diagnosis.FromCache = true
		s.store.SaveRun(run)
		similar := s.kb.Match(s.buildRunLogs(run))
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"analysis": run.Diagnosis.Text,
			"run_id":   run.ID,
			"cached":   true,
			"adopted":  run.Diagnosis.Adopted,
			"rejected": run.Diagnosis.Rejected,
			"similar":  similar,
		})
		return
	}
	logs := s.buildRunLogs(run)
	similar := s.kb.Match(logs)
	// 命中知识库同类失败：直接复用历史结论，省一次 LLM 调用。
	if len(similar) > 0 && strings.TrimSpace(similar[0].Diagnosis) != "" {
		analysis := similar[0].Diagnosis
		now := time.Now().Format(time.RFC3339)
		run.Diagnosis = &store.RunDiagnosis{Text: analysis, CreatedAt: now, FromCache: true}
		s.store.SaveRun(run)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"analysis":       analysis,
			"run_id":         run.ID,
			"cached":         true,
			"cached_from_kb": true,
			"adopted":        false,
			"rejected":       false,
			"similar":        similar,
		})
		return
	}
	cli := llm.NewClient(*s.llm)
	// 日志含内网 IP / user@host / 本地路径，外发前脱敏，避免把内部基建信息发到第三方 LLM。
	analysis, err := cli.Diagnose(r.Context(), redactForExternal(logs))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	now := time.Now().Format(time.RFC3339)
	run.Diagnosis = &store.RunDiagnosis{Text: analysis, CreatedAt: now}
	s.store.SaveRun(run)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"analysis": analysis,
		"run_id":   run.ID,
		"cached":   false,
		"adopted":  false,
		"rejected": false,
		"similar":  similar,
	})
}

// handleAdoptDiagnosis marks a run's diagnosis as adopted (or rejected) and,
// when adopted, persists it into the knowledge base so similar failures can be
// matched later. The diagnosis must already exist (the operator must have run
// the analysis at least once) before it can be adopted.
func (s *Server) handleAdoptDiagnosis(w http.ResponseWriter, r *http.Request, runID string) {
	run, ok := s.store.GetRun(runID)
	if !ok {
		writeError(w, http.StatusNotFound, "运行不存在")
		return
	}
	if run.Diagnosis == nil || run.Diagnosis.Text == "" {
		writeError(w, http.StatusBadRequest, "请先执行一次 AI 诊断，再决定是否采纳")
		return
	}
	var body struct {
		Adopt bool `json:"adopt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求解析失败: "+err.Error())
		return
	}
	now := time.Now().Format(time.RFC3339)
	kbID := ""
	if body.Adopt {
		stage, keyword, excerpt := extractFailure(run)
		e := kb.Entry{
			ProjectID:    run.ProjectID,
			ProjectName:  run.ProjectName,
			Stage:        stage,
			ErrorKeyword: keyword,
			ErrorExcerpt: excerpt,
			Diagnosis:    run.Diagnosis.Text,
		}
		s.kbMu.Lock()
		saved := s.kb.Add(e)
		_ = s.kb.Save()
		s.kbMu.Unlock()
		kbID = saved.ID
		run.Diagnosis.Adopted = true
		run.Diagnosis.Rejected = false
		run.Diagnosis.AdoptedAt = now
	} else {
		run.Diagnosis.Rejected = true
		run.Diagnosis.Adopted = false
	}
	if err := s.store.SaveRun(run); err != nil {
		writeError(w, http.StatusInternalServerError, "保存采纳状态失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"adopted":   body.Adopt,
		"rejected":  !body.Adopt,
		"kb_id":     kbID,
		"diagnosis": run.Diagnosis,
	})
}

// extractFailure pulls the failing stage name and a normalized error snippet
// from a run, used to seed the knowledge base entry.
func extractFailure(run store.Run) (stage, keyword, excerpt string) {
	for _, st := range run.Stages {
		if st.Status == store.StatusFailed || st.Error != "" {
			stage = st.Name
			excerpt = st.Error
			if excerpt == "" {
				excerpt = st.Log
			}
			if len(excerpt) > 280 {
				excerpt = excerpt[:280]
			}
			keyword = kb.Normalize(st.Error)
			if keyword == "" {
				keyword = kb.Normalize(st.Log)
			}
			return
		}
	}
	excerpt = run.Log
	if len(excerpt) > 280 {
		excerpt = excerpt[:280]
	}
	keyword = kb.Normalize(run.Log)
	return
}

// handleKBList returns the knowledge base entries (adopted diagnoses) ordered
// by most-recently-adopted first. An optional ?q= filters by case-insensitive
// substring across project / stage / error / diagnosis text.
func (s *Server) handleKBList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "方法不被允许")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	entries := s.kb.List()
	if q != "" {
		lower := strings.ToLower(q)
		filtered := entries[:0]
		for _, e := range entries {
			hay := strings.ToLower(e.ProjectName + " " + e.Stage + " " + e.ErrorKeyword +
				" " + e.ErrorExcerpt + " " + e.Diagnosis)
			if strings.Contains(hay, lower) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"entries": entries})
}

// handleKBItem serves a single knowledge-base entry. Today only DELETE is
// supported, which removes an adopted diagnosis from the KB.
func (s *Server) handleKBItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/kb/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少知识库条目 id")
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "方法不被允许")
		return
	}
	s.kbMu.Lock()
	removed := s.kb.Remove(id)
	var err error
	if removed {
		err = s.kb.Save()
	}
	s.kbMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "删除知识库条目失败: "+err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "知识库条目不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// handleTestLLM pings the configured (or form-provided) LLM endpoint with a
// minimal prompt so operators can verify connectivity before relying on it for
// real failure diagnoses. The request body mirrors the LLM config so the test
// can run even on unsaved edits; an empty api_key falls back to the stored one.
func (s *Server) handleTestLLM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "方法不被允许")
		return
	}
	var body struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		Model   string `json:"model"`
		System  string `json:"system"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "配置解析失败: "+err.Error())
		return
	}
	cfg := *s.llm
	if v := strings.TrimSpace(body.BaseURL); v != "" {
		cfg.BaseURL = v
	}
	if body.APIKey != "" {
		cfg.APIKey = body.APIKey
	}
	if v := strings.TrimSpace(body.Model); v != "" {
		cfg.Model = v
	}
	if body.System != "" {
		cfg.System = body.System
	}
	if cfg.BaseURL == "" || cfg.APIKey == "" || cfg.Model == "" {
		writeError(w, http.StatusBadRequest, "配置不完整：需 BaseURL / API Key / Model")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cli := llm.NewClient(cfg)
	reply, err := cli.Test(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "连接测试失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1", "reply": reply})
}

// ---- Webhook ----

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "方法不被允许")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/webhook/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少项目 id")
		return
	}
	if s.cfg.Server.WebhookSecret != "" {
		secret := r.URL.Query().Get("secret")
		if secret == "" {
			secret = r.Header.Get("X-Webhook-Secret")
		}
		if secret != s.cfg.Server.WebhookSecret {
			writeError(w, http.StatusUnauthorized, "secret 校验失败")
			return
		}
	}
	if _, ok := s.store.GetProject(id); !ok {
		writeError(w, http.StatusNotFound, "项目不存在")
		return
	}
	// Allow an optional override body: {"tag": "...", "method": "...", "target": "..."}.
	var body struct {
		Tag    string `json:"tag"`
		Method string `json:"method"`
		Target string `json:"target"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !store.IsValidDeployMethod(body.Method) {
		writeError(w, http.StatusBadRequest, "非法的部署方式覆盖: "+body.Method)
		return
	}
	if tn := strings.TrimSpace(body.Target); tn != "" {
		p, _ := s.store.GetProject(id)
		found := false
		if p.Name != "" {
			for _, t := range p.Targets {
				if t.Name == tn {
					found = true
					break
				}
			}
		}
		if !found {
			writeError(w, http.StatusBadRequest, "未找到发布目标: "+tn)
			return
		}
	}
	run, err := s.runner.TriggerPipeline(id, "webhook", body.Tag, body.Method, body.Target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}
