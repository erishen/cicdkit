package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is the persistence contract used by the API and runner.
type Store interface {
	ListProjects() []Project
	GetProject(id string) (Project, bool)
	CreateProject(p Project) error
	UpdateProject(p Project) error
	DeleteProject(id string) error

	ListRuns(projectID string) []Run
	GetRun(id string) (Run, bool)
	SaveRun(r Run) error
	// LastSuccessfulBuild returns the most recent successful run for the project
	// that actually built an image (has a "build" stage with status success).
	// Used to let a standalone Deploy reuse the last built image tag.
	LastSuccessfulBuild(projectID string) (Run, bool)
	// ClearRuns wipes the entire run history (all projects). Intended for the
	// UI "清空运行记录" action; as a safety net it never removes an in-flight
	// run's eventual SaveRun (the runCtl re-appends it on finish).
	ClearRuns() error

	ListDeployments(projectID string) []Deployment
	SaveDeployment(d Deployment) error
	GetDeployment(id string) (Deployment, bool)
	// ClearDeployments wipes the entire deployment history (all projects).
	ClearDeployments() error

	// Paged variants back the list endpoints. limit<=0 means "all"; offset is
	// zero-based. Total is the count before slicing (honouring the project_id
	// filter) so the UI can render a pager / "load more".
	ListRunsPaged(projectID string, limit, offset int) RunPage
	ListDeploymentsPaged(projectID string, limit, offset int) DeploymentPage
	ListProjectsPaged(q string, limit, offset int) ProjectPage
}

// RunPage is a page of run history plus the pre-slice total count.
type RunPage struct {
	Runs  []Run `json:"runs"`
	Total int   `json:"total"`
}

// DeploymentPage is a page of deployment history plus the pre-slice total.
type DeploymentPage struct {
	Deployments []Deployment `json:"deployments"`
	Total       int          `json:"total"`
}

// ProjectPage is a page of projects plus the pre-slice total count. q filters
// by case-insensitive substring on name + repository; limit<=0 means "all".
type ProjectPage struct {
	Projects []Project `json:"projects"`
	Total    int       `json:"total"`
}

// jsonStore is a thread-safe, file-backed implementation of Store. It keeps the
// full dataset in memory and rewrites the JSON file on every mutation. This is
// dependency-free (no cgo/sqlite) and adequate for a single-node management
// platform; swap in BoltDB/SQLite behind this same interface for scale.
type jsonStore struct {
	mu         sync.RWMutex
	persistMu  sync.Mutex    // guards flushTimer
	flushTimer *time.Timer  // pending delayed flush, nil when idle
	path       string
	projects   []Project
	runs       []Run
	deployments []Deployment
	maxRuns    int
}

// DefaultMaxRuns is the retained run history size (each run embeds its log).
const DefaultMaxRuns = 200

// NewJsonStore opens (or initializes) the store at path.
func NewJsonStore(path string) (*jsonStore, error) {
	s := &jsonStore{path: path, maxRuns: DefaultMaxRuns}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *jsonStore) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var data struct {
		Projects    []Project    `json:"projects"`
		Runs        []Run        `json:"runs"`
		Deployments []Deployment `json:"deployments"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return err
	}
	s.projects = data.Projects
	s.runs = data.Runs
	s.deployments = data.Deployments
	s.backfillLastDeploy()
	s.backfillRunProbe()
	return nil
}

// backfillLastDeploy derives each project's LastDeploy from its newest
// successful deployment in memory. This makes prior rollouts (including ssh/腾讯云
// overrides) visible on the project card immediately on startup, without waiting
// for a fresh deploy. It only mutates the in-memory slice; persistence happens
// on the next mutation (e.g. a new deployment), so a stale store.json is never
// needlessly rewritten at load time.
func (s *jsonStore) backfillLastDeploy() {
	newest := make(map[string]Deployment)
	for _, d := range s.deployments {
		if d.Status != "deployed" {
			continue
		}
		cur, ok := newest[d.ProjectID]
		if !ok || d.CreatedAt.After(cur.CreatedAt) {
			newest[d.ProjectID] = d
		}
	}
	for i := range s.projects {
		if d, ok := newest[s.projects[i].ID]; ok {
			s.projects[i].LastDeploy = &LastDeployInfo{
				Method:      d.Method,
				Target:      d.Target,
				ImageRef:    d.ImageRef,
				At:          d.CreatedAt,
				ProbeStatus: d.Probe.Status,
			}
		}
	}
}

// backfillRunProbe attaches each run's post-deploy service probe (stored on the
// matching Deployment, keyed by RunID) so runs created before the Run.Probe
// field existed still display the 服务探测 step in the UI without being re-run.
// It also reconciles the run's overall status: if a probe attached during a
// run that completed before the "probe-fail → run-fail" escalation policy
// landed, the run is upgraded to failed so the top-level badge matches the
// 服务探测 step instead of contradicting it. It only mutates the in-memory
// slice; persistence happens on the next run save, so a stale store.json is
// never needlessly rewritten at load time.
func (s *jsonStore) backfillRunProbe() {
	best := make(map[string]Deployment, len(s.deployments))
	for _, d := range s.deployments {
		if d.RunID == "" || d.Probe.Status == "" {
			continue
		}
		if cur, ok := best[d.RunID]; !ok || d.CreatedAt.After(cur.CreatedAt) {
			best[d.RunID] = d
		}
	}
	for i := range s.runs {
		d, ok := best[s.runs[i].ID]
		if !ok {
			continue
		}
		if s.runs[i].Probe == nil {
			p := d.Probe
			s.runs[i].Probe = &p
		}
		// Status reconciliation: a stored "success" run that has a backfilled
		// failure probe is logically a bad publish. Only upgrade when the run
		// is already in a terminal state (the policy says a probe failure
		// cannot resurrect a still-running or canceled run).
		if s.runs[i].Status == StatusSuccess && (d.Probe.Status == "fail" || d.Probe.Status == "err") {
			s.runs[i].Status = StatusFailed
		}
	}
}

// persistDelay is the coalescing window: rapid mutations (e.g. a build run
// whose SaveRun fires on every stage) collapse into a single disk write.
const persistDelay = 200 * time.Millisecond

// persist schedules a delayed, coalesced flush. It never returns an error: the
// actual write happens asynchronously in flushNow, so a caller can't observe a
// transient disk failure — acceptable for a local single-node store. Call
// Flush() at process exit to guarantee the last window is written.
func (s *jsonStore) persist() {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if s.flushTimer == nil {
		s.flushTimer = time.AfterFunc(persistDelay, s.flushNow)
	}
}

// Flush forces any pending mutation to disk immediately (stopping the pending
// timer). Safe to call from the shutdown path.
func (s *jsonStore) Flush() {
	s.persistMu.Lock()
	if s.flushTimer != nil {
		s.flushTimer.Stop()
		s.flushTimer = nil
	}
	s.persistMu.Unlock()
	s.flushNow()
}

// flushNow snapshots the in-memory dataset and writes it atomically (tmp +
// rename). It is safe to call concurrently: it only takes a read lock for the
// snapshot, and rename is atomic. Errors go to stderr so they're visible
// without failing the in-memory mutation.
func (s *jsonStore) flushNow() {
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	s.mu.RLock()
	// Snapshot into a fresh slice so masking the registry password (when the
	// operator supplies it via CICD_REGISTRY_PASSWORD) never mutates the live
	// in-memory project. The operator's env-provided secret stays out of disk.
	ps := make([]Project, len(s.projects))
	copy(ps, s.projects)
	if regEnv := strings.TrimSpace(os.Getenv("CICD_REGISTRY_PASSWORD")); regEnv != "" {
		for i := range ps {
			if ps[i].Build.Registry.Password != "" {
				ps[i].Build.Registry.Password = MaskedSecret
			}
		}
	}
	data := struct {
		Projects    []Project    `json:"projects"`
		Runs        []Run        `json:"runs"`
		Deployments []Deployment `json:"deployments"`
	}{ps, s.runs, s.deployments}
	s.mu.RUnlock()
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "store persist marshal error: %v\n", err)
		return
	}
	tmp := s.path + ".tmp"
	// 0600: the store can hold secrets (registry password, ssh key paths); only
	// the owning user should be able to read it even when a secret is persisted.
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "store persist write error: %v\n", err)
		return
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		// best-effort; the freshly written tmp already has 0600 and will be the
		// canonical file after the atomic rename below.
		_ = err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		fmt.Fprintf(os.Stderr, "store persist rename error: %v\n", err)
	}
}

func (s *jsonStore) ListProjects() []Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]Project(nil), s.projects...)
	sortProjectsByCreatedAtDesc(out)
	return out
}

// ListProjectsPaged mirrors ListProjects but honours an optional case-insensitive
// substring query (q, matched against name + repository) and limit/offset paging.
// Total is the count before slicing so the UI can render "load more".
func (s *jsonStore) ListProjectsPaged(q string, limit, offset int) ProjectPage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]Project(nil), s.projects...)
	if q != "" {
		ql := strings.ToLower(q)
		kept := out[:0]
		for _, p := range out {
			hay := strings.ToLower(p.Name + " " + p.Repository)
			if strings.Contains(hay, ql) {
				kept = append(kept, p)
			}
		}
		out = kept
	}
	sortProjectsByCreatedAtDesc(out)
	total := len(out)
	if limit > 0 {
		if offset < 0 {
			offset = 0
		}
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}
		out = out[offset:end]
	}
	return ProjectPage{Projects: out, Total: total}
}

func (s *jsonStore) GetProject(id string) (Project, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.projects {
		if p.ID == id {
			return p, true
		}
	}
	return Project{}, false
}

func (s *jsonStore) CreateProject(p Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.projects {
		if e.ID == p.ID {
			return fmt.Errorf("项目 %s 已存在", p.ID)
		}
	}
	s.projects = append(s.projects, p)
	s.persist()
	return nil
}

func (s *jsonStore) UpdateProject(p Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.projects {
		if e.ID == p.ID {
			p.CreatedAt = e.CreatedAt
			s.projects[i] = p
			s.persist()
	return nil
		}
	}
	return fmt.Errorf("项目 %s 不存在", p.ID)
}

func (s *jsonStore) DeleteProject(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.projects[:0]
	found := false
	for _, e := range s.projects {
		if e.ID == id {
			found = true
			continue
		}
		out = append(out, e)
	}
	s.projects = out
	if !found {
		return fmt.Errorf("项目 %s 不存在", id)
	}
	s.persist()
	return nil
}

func (s *jsonStore) ListRuns(projectID string) []Run {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Run
	for _, r := range s.runs {
		if projectID == "" || r.ProjectID == projectID {
			out = append(out, r)
		}
	}
	sortRunsByCreatedAtDesc(out)
	return out
}

// ListRunsPaged returns a single page of runs (newest first) plus the total
// matching count. limit<=0 disables slicing (returns everything); offset is
// clamped to [0, total] so out-of-range requests never panic.
func (s *jsonStore) ListRunsPaged(projectID string, limit, offset int) RunPage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var filtered []Run
	for _, r := range s.runs {
		if projectID == "" || r.ProjectID == projectID {
			filtered = append(filtered, r)
		}
	}
	sortRunsByCreatedAtDesc(filtered)
	total := len(filtered)
	if limit > 0 {
		if offset < 0 {
			offset = 0
		}
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}
		filtered = filtered[offset:end]
	}
	return RunPage{Runs: filtered, Total: total}
}

func (s *jsonStore) GetRun(id string) (Run, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.runs {
		if r.ID == id {
			return r, true
		}
	}
	return Run{}, false
}

func (s *jsonStore) GetDeployment(id string) (Deployment, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.deployments {
		if d.ID == id {
			return d, true
		}
	}
	return Deployment{}, false
}

func (s *jsonStore) SaveRun(r Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for i, e := range s.runs {
		if e.ID == r.ID {
			s.runs[i] = r
			found = true
			break
		}
	}
	if !found {
		s.runs = append(s.runs, r)
		s.trimRunsLocked()
	}
	s.persist()
	return nil
}

// SetMaxRuns bounds how much run history is retained. Runs carry their full log,
// so unbounded history means store.json grows forever and every save rewrites
// all of it. Non-terminal runs are never evicted.
func (s *jsonStore) SetMaxRuns(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxRuns = n
	s.trimRunsLocked()
}

func (s *jsonStore) trimRunsLocked() {
	if s.maxRuns <= 0 || len(s.runs) <= s.maxRuns {
		return
	}
	// Keep everything still in flight, then backfill with the newest finished
	// runs until the cap is reached.
	var live, done []Run
	for _, r := range s.runs {
		switch r.Status {
		case StatusQueued, StatusRunning:
			live = append(live, r)
		default:
			done = append(done, r)
		}
	}
	sortRunsByCreatedAtDesc(done)
	budget := s.maxRuns - len(live)
	if budget < 0 {
		budget = 0
	}
	if budget > len(done) {
		budget = len(done)
	}
	kept := append(live, done[:budget]...)
	sort.Slice(kept, func(i, j int) bool {
		if !kept[i].CreatedAt.Equal(kept[j].CreatedAt) {
			return kept[i].CreatedAt.Before(kept[j].CreatedAt)
		}
		return kept[i].ID < kept[j].ID
	})
	s.runs = kept
}

// LastSuccessfulBuild returns the most recent successful run that built an
// image (a "build" stage succeeded). Used so a standalone Deploy can reuse the
// last built tag instead of inventing a fresh timestamp that was never built.
func (s *jsonStore) LastSuccessfulBuild(projectID string) (Run, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best Run
	found := false
	for _, r := range s.runs {
		if r.ProjectID != projectID || r.Status != StatusSuccess {
			continue
		}
		built := false
		for _, st := range r.Stages {
			if st.Name == "build" && st.Status == StatusSuccess {
				built = true
				break
			}
		}
		if !built {
			continue
		}
		if !found || r.CreatedAt.After(best.CreatedAt) {
			best = r
			found = true
		}
	}
	return best, found
}

func (s *jsonStore) ListDeployments(projectID string) []Deployment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Deployment
	for _, d := range s.deployments {
		if projectID == "" || d.ProjectID == projectID {
			out = append(out, d)
		}
	}
	sortDeploymentsByCreatedAtDesc(out)
	return out
}

// ListDeploymentsPaged mirrors ListRunsPaged for deployment history.
func (s *jsonStore) ListDeploymentsPaged(projectID string, limit, offset int) DeploymentPage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var filtered []Deployment
	for _, d := range s.deployments {
		if projectID == "" || d.ProjectID == projectID {
			filtered = append(filtered, d)
		}
	}
	sortDeploymentsByCreatedAtDesc(filtered)
	total := len(filtered)
	if limit > 0 {
		if offset < 0 {
			offset = 0
		}
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}
		filtered = filtered[offset:end]
	}
	return DeploymentPage{Deployments: filtered, Total: total}
}

func (s *jsonStore) SaveDeployment(d Deployment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deployments = append(s.deployments, d)
	// Keep the project's LastDeploy in sync with the latest successful rollout.
	// Only successful deploys count, so a failed run never clobbers a good one.
	if d.Status == "deployed" {
		for i := range s.projects {
			if s.projects[i].ID == d.ProjectID {
				s.projects[i].LastDeploy = &LastDeployInfo{
					Method:      d.Method,
					Target:      d.Target,
					ImageRef:    d.ImageRef,
					At:          d.CreatedAt,
					ProbeStatus: d.Probe.Status,
				}
				break
			}
		}
	}
	s.persist()
	return nil
}

func (s *jsonStore) ClearRuns() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = nil
	s.persist()
	return nil
}

func (s *jsonStore) ClearDeployments() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deployments = nil
	// Drop each project's LastDeploy so the card no longer claims a rollout
	// whose history was just wiped.
	for i := range s.projects {
		s.projects[i].LastDeploy = nil
	}
	s.persist()
	return nil
}

// GenID returns a short, collision-resistant identifier with the given prefix.
func GenID(prefix string) string {
	b := make([]byte, 5)
	rand.Read(b)
	return fmt.Sprintf("%s-%s%s", prefix, time.Now().Format("020106"), hex.EncodeToString(b))
}

// 以下三个排序器按创建时间倒序；当 created_at 为空（旧数据）或相同时以 ID 倒序兜底，
// 保证排序「确定性」——否则 sort.Slice 在不稳定比较 + 全零值下，不同分页请求返回的顺序
// 不一致、相邻页互相重叠，前端按 id 去重后可见条数会少于总数（如 50 条只显示 30 条）。
func sortProjectsByCreatedAtDesc(ps []Project) {
	sort.Slice(ps, func(i, j int) bool {
		if !ps[i].CreatedAt.Equal(ps[j].CreatedAt) {
			return ps[i].CreatedAt.After(ps[j].CreatedAt)
		}
		return ps[i].ID > ps[j].ID
	})
}
func sortRunsByCreatedAtDesc(rs []Run) {
	sort.Slice(rs, func(i, j int) bool {
		if !rs[i].CreatedAt.Equal(rs[j].CreatedAt) {
			return rs[i].CreatedAt.After(rs[j].CreatedAt)
		}
		return rs[i].ID > rs[j].ID
	})
}
func sortDeploymentsByCreatedAtDesc(ds []Deployment) {
	sort.Slice(ds, func(i, j int) bool {
		if !ds[i].CreatedAt.Equal(ds[j].CreatedAt) {
			return ds[i].CreatedAt.After(ds[j].CreatedAt)
		}
		return ds[i].ID > ds[j].ID
	})
}
