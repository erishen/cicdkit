// a2a.go — A2A 标准面：Agent Card + JSON-RPC + 任务状态机 + peer 签名面。
// 从 tsm-hub 同源拷贝，适配 cicdkit（CI/CD 发布管理）场景。
package a2a

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ── 状态值域 ─────────────────────────────────────────────────────────

const (
	StateSubmitted     = "submitted"
	StateWorking       = "working"
	StateCompleted     = "completed"
	StateFailed        = "failed"
	StateRejected      = "rejected"
	StateInputRequired = "input-required"
	StateCanceled      = "canceled"
)

func IsTerminal(state string) bool {
	switch state {
	case StateCompleted, StateFailed, StateCanceled, StateRejected:
		return true
	default:
		return false
	}
}

// ── 任务模型 ────────────────────────────────────────────────────────

type message struct {
	Role      string   `json:"role"`
	MessageID string   `json:"messageId"`
	Kind      string   `json:"kind"`
	Parts     []part   `json:"parts"`
}

type part struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type artifact struct {
	ArtifactID string `json:"artifactId"`
	Parts      []part `json:"parts"`
}

type taskStatus struct {
	State     string   `json:"state"`
	Timestamp string   `json:"timestamp"`
	Message   *message `json:"message,omitempty"`
}

type taskTraceStep struct {
	Node string `json:"node"`
	At   int64  `json:"at"`
	Note string `json:"note,omitempty"`
	Cap  string `json:"cap,omitempty"`
}

type taskMeta struct {
	Capability string         `json:"capability"`
	Hops       int            `json:"hops"`
	Trace      []taskTraceStep `json:"trace"`
	Args       map[string]any `json:"args,omitempty"`
}

type Task struct {
	ID        string     `json:"id"`
	ContextID string     `json:"contextId"`
	Kind      string     `json:"kind"`
	Status    taskStatus `json:"status"`
	Artifacts []artifact `json:"artifacts"`
	History   []message  `json:"history"`
	Metadata  taskMeta   `json:"metadata"`
	Node      string     `json:"node"`
	Created   int64      `json:"created"`
	Updated   int64      `json:"updated"`
}

func validTaskID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	return !strings.ContainsAny(id, `\/\:*?"<>|`)
}

func newTaskID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newMessageID() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func nowText() string { return time.Now().Format(time.RFC3339) }

func newTask(node, ctxID, capability, text string, hops int) *Task {
	if ctxID == "" {
		ctxID = newMessageID()
	}
	now := time.Now().Unix()
	return &Task{
		ID:        newTaskID(),
		ContextID: ctxID,
		Kind:      "task",
		Status:    taskStatus{State: StateSubmitted, Timestamp: nowText()},
		Artifacts: []artifact{},
		History: []message{{Role: "user", MessageID: newMessageID(), Kind: "message",
			Parts: []part{{Kind: "text", Text: text}}}},
		Metadata: taskMeta{Capability: capability, Hops: hops,
			Trace: []taskTraceStep{{Node: node, At: now, Cap: capability, Note: "accepted"}}},
		Node:    node,
		Created: now,
		Updated: now,
	}
}

func (t *Task) advance(state, note string, artifacts []artifact) {
	t.Status.State = state
	t.Status.Timestamp = nowText()
	if artifacts != nil {
		t.Artifacts = artifacts
	}
	if note != "" {
		t.Metadata.Trace = append(t.Metadata.Trace, taskTraceStep{
			Node: t.Node, At: time.Now().Unix(), Note: note,
			Cap: t.Metadata.Capability,
		})
	}
	t.Updated = time.Now().Unix()
}

func (t *Task) advanceWithMessage(state, question string) {
	t.advance(state, "", nil)
	t.Status.Message = &message{
		Role: "agent", MessageID: newMessageID(), Kind: "message",
		Parts: []part{{Kind: "text", Text: question}},
	}
	t.Updated = time.Now().Unix()
}

func mergedArgs(prev, current map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range prev {
		out[k] = v
	}
	for k, v := range current {
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		out[k] = v
	}
	return out
}

func firstArtifactBody(arts []artifact) string {
	if len(arts) == 0 {
		return ""
	}
	for _, p := range arts[0].Parts {
		if p.Kind == "text" {
			return p.Text
		}
	}
	return ""
}

// ── 任务存储 ────────────────────────────────────────────────────────

type TaskStore struct {
	dir   string
	mu    sync.Mutex
	index map[string]*Task
}

func NewTaskStore(baseDir, node string) *TaskStore {
	dir := filepath.Join(baseDir, "a2a-tasks", node)
	_ = os.MkdirAll(dir, 0o755)
	s := &TaskStore{dir: dir, index: map[string]*Task{}}
	s.restore()
	return s
}

func (s *TaskStore) path(id string) string {
	if !validTaskID(id) {
		return ""
	}
	return filepath.Join(s.dir, id+".json")
}

func (s *TaskStore) save(t *Task) error {
	p := s.path(t.ID)
	if p == "" {
		return fmt.Errorf("invalid task id %q", t.ID)
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	s.mu.Lock()
	s.index[t.ID] = t
	s.mu.Unlock()
	return nil
}

func (s *TaskStore) load(id string) (*Task, error) {
	s.mu.Lock()
	if t, ok := s.index[id]; ok {
		s.mu.Unlock()
		return t, nil
	}
	s.mu.Unlock()
	p := s.path(id)
	if p == "" {
		return nil, os.ErrNotExist
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("task %s: %w", id, err)
	}
	s.mu.Lock()
	s.index[id] = &t
	s.mu.Unlock()
	return &t, nil
}

func (s *TaskStore) ActiveCount() int {
	s.mu.Lock()
	n := 0
	for _, t := range s.index {
		if !IsTerminal(t.Status.State) {
			n++
		}
	}
	s.mu.Unlock()
	return n
}

func (s *TaskStore) recent(n int) []*Task {
	s.mu.Lock()
	out := make([]*Task, 0, len(s.index))
	for _, t := range s.index {
		out = append(out, t)
	}
	s.mu.Unlock()
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Updated > out[j-1].Updated; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func (s *TaskStore) restore() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if _, err := s.load(id); err != nil {
			fmt.Printf("[a2a] restore task %s failed: %v\n", id, err)
		}
	}
}

// ── 能力注册表 ──────────────────────────────────────────────────────

type Capability struct {
	Name      string
	Title     string
	Desc      string
	Tag       string
	ArgsExample string
	WhenToUse string
}

type DispatchResult struct {
	State     string
	Artifacts []artifact
	Note      string
}

type DispatchFunc func(args map[string]any) DispatchResult

type Capabilities struct {
	ordered []Capability
	table   map[string]DispatchFunc
}

func NewCapabilities() *Capabilities {
	return &Capabilities{table: map[string]DispatchFunc{}}
}

func (c *Capabilities) Register(cap Capability, fn DispatchFunc) {
	if _, exists := c.table[cap.Name]; exists {
		return
	}
	c.ordered = append(c.ordered, cap)
	c.table[cap.Name] = fn
}

func (c *Capabilities) Get(name string) (Capability, DispatchFunc, bool) {
	fn, ok := c.table[name]
	if !ok {
		return Capability{}, nil, false
	}
	for _, cap := range c.ordered {
		if cap.Name == name {
			return cap, fn, true
		}
	}
	return Capability{}, nil, false
}

func (c *Capabilities) All() []Capability { return c.ordered }

func (c *Capabilities) Names() []string {
	out := make([]string, len(c.ordered))
	for i, cap := range c.ordered {
		out[i] = cap.Name
	}
	return out
}

func completedArtifact(v any) DispatchResult {
	raw, err := json.Marshal(v)
	if err != nil {
		return DispatchResult{State: StateFailed, Note: "serialize result: " + err.Error()}
	}
	return DispatchResult{
		State:     StateCompleted,
		Artifacts: []artifact{{ArtifactID: newMessageID(),
			Parts: []part{{Kind: "text", Text: string(raw)}}}},
		Note: "ok",
	}
}

// ── Agent Card ──────────────────────────────────────────────────────

func Card(nodeName, base, desc string, caps *Capabilities) map[string]any {
	skills := make([]map[string]any, 0, len(caps.ordered))
	for _, cap := range caps.ordered {
		tags := []string{cap.Tag}
		if cap.Tag == "" {
			tags = []string{"general"}
		}
		skills = append(skills, map[string]any{
			"id":          cap.Name,
			"name":        cap.Title,
			"description": cap.Desc,
			"tags":        tags,
			"examples":    []string{cap.ArgsExample},
		})
	}
	return map[string]any{
		"name":             nodeName,
		"description":    desc,
		"version":         "1.0.0",
		"protocolVersion":  "1.0",
		"supportedInterfaces": []map[string]any{
			{"protocolVersion": "1.0", "protocolBinding": "JSONRPC", "url": base + "/a2a"},
		},
		"url":                base + "/a2a",
		"capabilities":       map[string]any{"streaming": false, "pushNotifications": false},
		"defaultInputModes":  []string{"text/plain", "application/json"},
		"defaultOutputModes": []string{"text/plain", "application/json"},
		"skills":             skills,
	}
}

// ── JSON-RPC 外壳 ───────────────────────────────────────────────────

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Method  string `json:"method"`
	Params  struct {
		Message struct {
			Role    string `json:"role"`
			Parts   []struct {
				Kind string `json:"kind"`
				Text string `json:"text"`
			} `json:"parts"`
			TaskID string `json:"taskId"`
		} `json:"message"`
		ContextID string `json:"contextId"`
		TaskID    string `json:"id"`
		Metadata  struct {
			Capability string         `json:"capability"`
			Args       map[string]any `json:"args"`
			Hops       int            `json:"hops"`
		} `json:"metadata"`
	} `json:"params"`
}

func rpcResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id any, code int, msg string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	}
}

func (p *rpcRequest) messageText() string {
	var out []string
	for _, pt := range p.Params.Message.Parts {
		if pt.Kind == "text" || pt.Kind == "" {
			out = append(out, pt.Text)
		}
	}
	return strings.Join(out, "\n")
}

// ── A2A 服务端 ──────────────────────────────────────────────────────

type Server struct {
	Node  string
	Caps  *Capabilities
	Tasks *TaskStore
	PEER  *Peer
	Uptime func() time.Duration
}

func (s *Server) AdminOverview() map[string]any {
	out := map[string]any{
		"node": s.Node,
		"caps": len(s.Caps.All()),
	}
	if s.Tasks != nil {
		out["tasks_active"] = s.Tasks.ActiveCount()
	}
	if s.PEER == nil {
		out["peer_mode"] = "off"
	} else {
		out["peer_mode"] = string(s.PEER.Mode())
	}
	if s.Uptime != nil {
		out["uptime_secs"] = int64(s.Uptime().Seconds())
	}
	return out
}

func (s *Server) RecentTasks(n int) []*Task {
	if s.Tasks == nil {
		return nil
	}
	return s.Tasks.recent(n)
}

func (s *Server) InvokeCapability(tool string, args map[string]any) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	if _, _, ok := s.Caps.Get(tool); !ok {
		return map[string]any{
			"ok": false, "tool": tool, "error": "unknown tool '" + tool + "'",
			"known": s.Caps.Names(),
		}
	}
	_, fn, _ := s.Caps.Get(tool)
	res := fn(args)
	return map[string]any{
		"ok":     res.State == StateCompleted,
		"tool":   tool,
		"state":  res.State,
		"result": firstArtifactBody(res.Artifacts),
		"note":   res.Note,
	}
}

// HandleAgentCard
func (s *Server) HandleAgentCard(w http.ResponseWriter, r *http.Request) {
	writeJSONBody(w, http.StatusOK, Card(s.Node, s.baseURL(r),
		"cicdkit 能力节点：CI/CD 发布管理平台的项目/构建/部署/探测/知识库查询。只读，不提供写入。", s.Caps))
}

// HandleA2A
func (s *Server) HandleA2A(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONBody(w, http.StatusOK, rpcError(nil, -32700, "parse error: "+err.Error()))
		return
	}
	switch req.Method {
	case "message/send", "tasks/send":
		s.handleSend(w, &req)
	case "tasks/get", "GetTask":
		s.handleTasksGet(w, &req)
	case "tasks/cancel", "CancelTask":
		s.handleTasksCancel(w, &req)
	default:
		writeJSONBody(w, http.StatusOK, rpcError(req.ID, -32601, "method not found: "+req.Method))
	}
}

func (s *Server) handleSend(w http.ResponseWriter, req *rpcRequest) {
	text := req.messageText()
	resume := req.Params.TaskID
	if resume == "" {
		resume = req.Params.Message.TaskID
	}
	if resume != "" {
		s.resumeTask(w, req, resume, text, req.Params.Metadata.Args)
		return
	}
	capName := req.Params.Metadata.Capability
	args := req.Params.Metadata.Args
	if args == nil {
		args = map[string]any{}
	}
	if capName == "" {
		t := newTask(s.Node, req.Params.ContextID, "unknown", text, req.Params.Metadata.Hops)
		t.Metadata.Args = args
		_ = s.Tasks.save(t)
		t.advanceWithMessage(StateInputRequired, "请指定 capability: "+strings.Join(s.Caps.Names(), ", "))
		_ = s.Tasks.save(t)
		writeJSONBody(w, http.StatusOK, rpcResult(req.ID, t))
		return
	}
	if _, _, ok := s.Caps.Get(capName); !ok {
		t := newTask(s.Node, req.Params.ContextID, capName, text, req.Params.Metadata.Hops)
		t.Metadata.Args = args
		_ = s.Tasks.save(t)
		t.advance(StateRejected, "unknown capability '"+capName+"'", []artifact{})
		_ = s.Tasks.save(t)
		writeJSONBody(w, http.StatusOK, rpcResult(req.ID, t))
		return
	}
	_, fn, _ := s.Caps.Get(capName)
	t := newTask(s.Node, req.Params.ContextID, capName, text, req.Params.Metadata.Hops)
	t.Metadata.Args = args
	_ = s.Tasks.save(t)
	t.advance(StateWorking, "dispatch "+capName, nil)
	_ = s.Tasks.save(t)
	res := fn(args)
	if res.State == StateInputRequired {
		t.advanceWithMessage(res.State, res.Note)
		_ = s.Tasks.save(t)
	} else {
		t.advance(res.State, res.Note, res.Artifacts)
		_ = s.Tasks.save(t)
	}
	writeJSONBody(w, http.StatusOK, rpcResult(req.ID, t))
}

func (s *Server) resumeTask(w http.ResponseWriter, req *rpcRequest, taskID, text string, args map[string]any) {
	t, err := s.Tasks.load(taskID)
	if err != nil {
		writeJSONBody(w, http.StatusOK, rpcError(req.ID, -32001, "task not found: "+taskID))
		return
	}
	state := t.Status.State
	if IsTerminal(state) {
		writeJSONBody(w, http.StatusOK, rpcError(req.ID, -32004,
			"任务已终结("+state+"), 不可恢复"))
		return
	}
	if state != StateInputRequired {
		writeJSONBody(w, http.StatusOK, rpcError(req.ID, -32005,
			"任务正在处理中("+state+"), 无需补充"))
		return
	}
	t.History = append(t.History, message{
		Role: "user", MessageID: newMessageID(), Kind: "message",
		Parts: []part{{Kind: "text", Text: text}},
	})
	merged := mergedArgs(t.Metadata.Args, args)
	t.Metadata.Args = merged
	capName := req.Params.Metadata.Capability
	if capName == "" || capName == "unknown" {
		capName = t.Metadata.Capability
	}
	t.Metadata.Capability = capName
	_ = s.Tasks.save(t)
	_, fn, ok := s.Caps.Get(capName)
	if !ok {
		t.advance(StateRejected, "unknown capability '"+capName+"'", []artifact{})
		_ = s.Tasks.save(t)
	} else {
		t.advance(StateWorking, "resumed", nil)
		_ = s.Tasks.save(t)
		res := fn(merged)
		if res.State == StateInputRequired {
			t.advanceWithMessage(res.State, res.Note)
		} else {
			t.advance(res.State, res.Note, res.Artifacts)
		}
		_ = s.Tasks.save(t)
	}
	writeJSONBody(w, http.StatusOK, rpcResult(req.ID, t))
}

func (s *Server) handleTasksGet(w http.ResponseWriter, req *rpcRequest) {
	id := req.Params.TaskID
	if id == "" {
		id = req.Params.Message.TaskID
	}
	t, err := s.Tasks.load(id)
	if err != nil {
		writeJSONBody(w, http.StatusOK, rpcError(req.ID, -32001, "task not found: "+id))
		return
	}
	writeJSONBody(w, http.StatusOK, rpcResult(req.ID, t))
}

func (s *Server) handleTasksCancel(w http.ResponseWriter, req *rpcRequest) {
	id := req.Params.TaskID
	if id == "" {
		id = req.Params.Message.TaskID
	}
	t, err := s.Tasks.load(id)
	if err != nil {
		writeJSONBody(w, http.StatusOK, rpcError(req.ID, -32001, "task not found: "+id))
		return
	}
	if IsTerminal(t.Status.State) {
		writeJSONBody(w, http.StatusOK, rpcError(req.ID, -32002,
			"task not cancelable (state="+t.Status.State+")"))
		return
	}
	t.advance(StateCanceled, "canceled", nil)
	_ = s.Tasks.save(t)
	writeJSONBody(w, http.StatusOK, rpcResult(req.ID, t))
}

func (s *Server) baseURL(r *http.Request) string {
	if v := firstNonEmptyEnv("CICDKIT_A2A_PUBLIC_URL", "A2A_PUBLIC_URL"); v != "" {
		return v
	}
	host := r.Host
	if i := strings.LastIndex(host, ":"); i > 0 {
		if h := host[:i]; h == "0.0.0.0" || h == "::" {
			host = "127.0.0.1:" + host[i+1:]
		}
	}
	return "http://" + host
}

// ── peer 签名面 ─────────────────────────────────────────────────────

func (s *Server) HandlePeerHealth(w http.ResponseWriter, r *http.Request) {
	if s.PEER == nil {
		writePeerUnauthorized(w, "peer disabled: CICDKIT_PEER_SHARED / CICDKIT_PEER_ROOT not set")
		return
	}
	v := s.PEER.Verify(r, "GET", scopeGET(r.URL.Path))
	if !v.OK {
		writePeerUnauthorized(w, v.Err)
		return
	}
	writeJSONBody(w, http.StatusOK, map[string]any{
		"ok":   true,
		"node": s.Node,
		"site": "cicdkit",
		"peer": v.Node,
		"ts":   time.Now().Unix(),
		"mode": string(s.PEER.Mode()),
		"caps": len(s.Caps.All()),
	})
}

func (s *Server) HandlePeerInvoke(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if s.PEER == nil {
		writePeerUnauthorized(w, "peer disabled: CICDKIT_PEER_SHARED / CICDKIT_PEER_ROOT not set")
		return
	}
	v := s.PEER.Verify(r, "POST", scopePOST(r.URL.Path, string(body)))
	if !v.OK {
		writePeerUnauthorized(w, v.Err)
		return
	}
	var req struct {
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONBody(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "body 不是合法 JSON"})
		return
	}
	if req.Tool == "" {
		writeJSONBody(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 tool 字段"})
		return
	}
	if req.Arguments == nil {
		req.Arguments = map[string]any{}
	}
	if _, _, ok := s.Caps.Get(req.Tool); !ok {
		writeJSONBody(w, http.StatusOK, map[string]any{
			"ok": false, "tool": req.Tool, "error": "unknown tool '" + req.Tool + "'",
		})
		return
	}
	_, fn, _ := s.Caps.Get(req.Tool)
	res := fn(req.Arguments)
	writeJSONBody(w, http.StatusOK, map[string]any{
		"ok":     res.State == StateCompleted,
		"node":   s.Node,
		"tool":   req.Tool,
		"state":  res.State,
		"result": structuredResult(res.Artifacts),
		"note":   res.Note,
	})
}

func structuredResult(arts []artifact) any {
	raw := firstArtifactBody(arts)
	if raw == "" {
		return nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		return parsed
	}
	return raw
}

func (s *Server) HandlePeerTools(w http.ResponseWriter, r *http.Request) {
	if s.PEER == nil {
		writePeerUnauthorized(w, "peer disabled: CICDKIT_PEER_SHARED / CICDKIT_PEER_ROOT not set")
		return
	}
	v := s.PEER.Verify(r, "GET", scopeGET(r.URL.Path))
	if !v.OK {
		writePeerUnauthorized(w, v.Err)
		return
	}
	tools := make([]map[string]any, 0, len(s.Caps.All()))
	for _, cap := range s.Caps.All() {
		tools = append(tools, map[string]any{
			"name":        cap.Name,
			"desc":        cap.Desc,
			"when_to_use": cap.WhenToUse,
		})
	}
	writeJSONBody(w, http.StatusOK, map[string]any{
		"ok":      true,
		"node":    s.Node,
		"site":    "cicdkit",
		"summary": "CI/CD 发布平台的项目/构建/部署/探测/知识库查询（只读）。问『最近发了什么』『构建挂了没』『探测通不通』用这里。",
		"tools":   tools,
	})
}

func (s *Server) HandlePeerRelay(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if s.PEER == nil {
		writePeerUnauthorized(w, "peer disabled: CICDKIT_PEER_SHARED / CICDKIT_PEER_ROOT not set")
		return
	}
	v := s.PEER.Verify(r, "POST", scopePOST(r.URL.Path, string(body)))
	if !v.OK {
		writePeerUnauthorized(w, v.Err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusGone)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": 410,
		"ok":     false,
		"node":   s.Node,
		"error":  "relay 已退役",
		"hint":   "请改用 POST /peer/invoke 直接调用本节点的能力",
	})
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func writePeerUnauthorized(w http.ResponseWriter, err string) {
	writeJSONBody(w, http.StatusOK, map[string]any{
		"ok": false, "unauthorized": true, "error": err,
	})
}

func writeJSONBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
