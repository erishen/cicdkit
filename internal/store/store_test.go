package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *jsonStore {
	t.Helper()
	st, err := NewJsonStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	return st
}

func TestLastSuccessfulBuild(t *testing.T) {
	st := newTestStore(t)
	base := time.Now()

	// 只有 deploy 成功、没有 build 阶段的 run 不算「构建过镜像」
	mustSave(t, st, Run{
		ID: "r1", ProjectID: "p1", Status: StatusSuccess, ImageRef: "app:deploy-only",
		CreatedAt: base,
		Stages:    []StageResult{{Name: "deploy", Status: StatusSuccess}},
	})
	// build 失败的不算
	mustSave(t, st, Run{
		ID: "r2", ProjectID: "p1", Status: StatusFailed, ImageRef: "app:failed",
		CreatedAt: base.Add(time.Minute),
		Stages:    []StageResult{{Name: "build", Status: StatusFailed}},
	})
	// 成功构建
	mustSave(t, st, Run{
		ID: "r3", ProjectID: "p1", Status: StatusSuccess, ImageRef: "app:good-old",
		CreatedAt: base.Add(2 * time.Minute),
		Stages:    []StageResult{{Name: "build", Status: StatusSuccess}},
	})
	// 更新的一次成功构建，应当胜出
	mustSave(t, st, Run{
		ID: "r4", ProjectID: "p1", Status: StatusSuccess, ImageRef: "app:good-new",
		CreatedAt: base.Add(3 * time.Minute),
		Stages:    []StageResult{{Name: "build", Status: StatusSuccess}},
	})
	// 其他项目的成功构建不能串用
	mustSave(t, st, Run{
		ID: "r5", ProjectID: "p2", Status: StatusSuccess, ImageRef: "other:newest",
		CreatedAt: base.Add(4 * time.Minute),
		Stages:    []StageResult{{Name: "build", Status: StatusSuccess}},
	})

	got, ok := st.LastSuccessfulBuild("p1")
	if !ok {
		t.Fatal("应当找到 p1 的成功构建")
	}
	if got.ImageRef != "app:good-new" {
		t.Fatalf("应当取最近一次成功构建，得到 %q", got.ImageRef)
	}

	if _, ok := st.LastSuccessfulBuild("no-such-project"); ok {
		t.Fatal("不存在的项目不应返回构建")
	}
}

func TestTrimRunsKeepsNewestAndInFlight(t *testing.T) {
	st := newTestStore(t)
	st.SetMaxRuns(5)
	base := time.Now()

	// 一个仍在运行的老 run：即使最旧也不能被淘汰
	mustSave(t, st, Run{ID: "live", ProjectID: "p", Status: StatusRunning, CreatedAt: base})
	for i := 0; i < 20; i++ {
		mustSave(t, st, Run{
			ID:        fmt.Sprintf("done-%02d", i),
			ProjectID: "p",
			Status:    StatusSuccess,
			CreatedAt: base.Add(time.Duration(i+1) * time.Minute),
		})
	}

	all := st.ListRuns("")
	if len(all) != 5 {
		t.Fatalf("应当只保留 5 条，实际 %d", len(all))
	}
	ids := map[string]bool{}
	for _, r := range all {
		ids[r.ID] = true
	}
	if !ids["live"] {
		t.Fatal("仍在运行的 run 不应被淘汰")
	}
	// 剩余 4 个名额给最新的已完成 run
	for _, want := range []string{"done-19", "done-18", "done-17", "done-16"} {
		if !ids[want] {
			t.Fatalf("应保留最新的已完成 run %s，实际保留: %v", want, ids)
		}
	}
	if ids["done-00"] {
		t.Fatal("最旧的已完成 run 应被淘汰")
	}
}

func TestSaveRunUpdateDoesNotDuplicate(t *testing.T) {
	st := newTestStore(t)
	r := Run{ID: "r1", ProjectID: "p", Status: StatusRunning, CreatedAt: time.Now()}
	mustSave(t, st, r)
	r.Status = StatusSuccess
	r.Log = "done"
	mustSave(t, st, r)

	all := st.ListRuns("")
	if len(all) != 1 {
		t.Fatalf("同 ID 应更新而非追加，实际 %d 条", len(all))
	}
	if all[0].Status != StatusSuccess || all[0].Log != "done" {
		t.Fatalf("更新未生效: %+v", all[0])
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	st, err := NewJsonStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProject(Project{ID: "p1", Name: "app", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// persist() is deferred (debounced), so flush before reopening to ensure
	// the project is actually on disk when the second store reads it.
	st.Flush()

	reopened, err := NewJsonStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.GetProject("p1"); !ok {
		t.Fatal("重新打开后项目应当还在")
	}
}

// TestBackfillRunProbeFromDeployment 验证：Run.Probe 字段引入前产出的运行记录，
// 在 store 加载时能通过 Deployment.RunID 回填探测结果，使旧数据也能在 UI 显示
// 服务探测步骤，无需重新跑流水线。
func TestBackfillRunProbeFromDeployment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	st, err := NewJsonStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// 旧 run：没有 Probe 字段（模拟 Run.Probe 字段引入前的历史数据）
	if err := st.SaveRun(Run{ID: "r-old", ProjectID: "p1", Status: StatusSuccess, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// 对应的部署带探测结果，并通过 RunID 关联
	if err := st.SaveDeployment(Deployment{
		ID: "dep-1", ProjectID: "p1", Status: "deployed", RunID: "r-old", CreatedAt: time.Now(),
		Probe: ProbeResult{Status: "ok", StatusCode: 200, DurationMs: 12, Detail: "HTTP 200，12ms"},
	}); err != nil {
		t.Fatal(err)
	}
	st.Flush()

	// 重新打开触发 load() -> backfillRunProbe()
	reopened, err := NewJsonStore(path)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := reopened.GetRun("r-old")
	if !ok {
		t.Fatal("重新打开后 run 应当还在")
	}
	if r.Probe == nil {
		t.Fatal("旧 run 应通过 Deployment.RunID 回填 Probe")
	}
	if r.Probe.Status != "ok" || r.Probe.StatusCode != 200 {
		t.Fatalf("回填的 Probe 内容不正确: %+v", r.Probe)
	}

	// 没有关联部署的 run 不应被错误回填
	if err := reopened.SaveRun(Run{ID: "r-orphan", ProjectID: "p1", Status: StatusSuccess, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	reopened.Flush()
	again, err := NewJsonStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ro, _ := again.GetRun("r-orphan")
	if ro.Probe != nil {
		t.Fatal("无关联部署的 run 不应被回填 Probe")
	}
}

// TestBackfillRunProbeEscalatesSuccessRunToFailed 验证：服务探测失败→run 整体
// 升级为 failed 的策略上线之前产出的"success + 探测失败"的旧 run，在 store
// 加载时会被回填升级为 failed，与「服务探测 未通过」步骤保持一致。
func TestBackfillRunProbeEscalatesSuccessRunToFailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	st, err := NewJsonStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// 旧 run：策略升级前完成 → 仍记 success。
	mustSave(t, st, Run{
		ID:        "r-escalate",
		ProjectID: "p1",
		Status:    StatusSuccess,
		CreatedAt: now,
		StartedAt: now,
		EndedAt:   now,
		Stages:    []StageResult{{Name: "deploy", Status: StatusSuccess}},
	})
	// 对应部署带 fail 探测（关键：RunID 要能匹配到 run）。
	if err := st.SaveDeployment(Deployment{
		ID: "dep-escalate", ProjectID: "p1", Status: "deployed", RunID: "r-escalate",
		CreatedAt: now.Add(time.Second),
		Probe:     ProbeResult{Status: "fail", StatusCode: 502, DurationMs: 3951, Detail: "HTTP 502, 期望 200, 3951ms"},
	}); err != nil {
		t.Fatal(err)
	}
	st.Flush()

	reopened, err := NewJsonStore(path)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := reopened.GetRun("r-escalate")
	if !ok {
		t.Fatal("run 不应消失")
	}
	if r.Status != StatusFailed {
		t.Fatalf("旧 run 应被升级为 failed，实际 %q", r.Status)
	}
	if r.Probe == nil || r.Probe.Status != "fail" {
		t.Fatalf("同时应回填 Probe 字段: %+v", r.Probe)
	}

	// 反向用例：探测 ok 的 run 永远不会被改成 failed。
	mustSave(t, reopened, Run{
		ID: "r-ok", ProjectID: "p1", Status: StatusSuccess,
		CreatedAt: now, StartedAt: now, EndedAt: now,
		Stages: []StageResult{{Name: "deploy", Status: StatusSuccess}},
	})
	if err := reopened.SaveDeployment(Deployment{
		ID: "dep-ok", ProjectID: "p1", Status: "deployed", RunID: "r-ok",
		CreatedAt: now.Add(time.Second),
		Probe:     ProbeResult{Status: "ok", StatusCode: 200, DurationMs: 12, Detail: "HTTP 200, 12ms"},
	}); err != nil {
		t.Fatal(err)
	}
	reopened.Flush()
	final, err := NewJsonStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rok, _ := final.GetRun("r-ok")
	if rok.Status != StatusSuccess {
		t.Fatalf("探测 ok 不应改 status，实际 %q", rok.Status)
	}
}

func mustSave(t *testing.T, st *jsonStore, r Run) {
	t.Helper()
	if err := st.SaveRun(r); err != nil {
		t.Fatalf("保存 run 失败: %v", err)
	}
}

// TestFlushMasksRegistryPasswordOnDisk 验证：配置了 CICD_REGISTRY_PASSWORD 时，
// 落盘 JSON 不含真实密码（磁盘不再存明文密钥），且文件权限为 0600。
func TestFlushMasksRegistryPasswordOnDisk(t *testing.T) {
	t.Setenv("CICD_REGISTRY_PASSWORD", "env-secret")
	path := filepath.Join(t.TempDir(), "store.json")
	st, err := NewJsonStore(path)
	if err != nil {
		t.Fatal(err)
	}
	p := Project{
		ID:    "p1",
		Name:  "app",
		Build: BuildSpec{ImageRepo: "app", Registry: RegistryAuth{Server: "https://reg", Username: "u", Password: "REAL-SECRET"}},
		Deploy: DeploySpec{Method: "local-k3s", Deployment: "app", Container: "app"},
	}
	if err := st.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	st.Flush()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "REAL-SECRET") {
		t.Fatalf("落盘 JSON 不应含真实 registry 密码: %s", string(b))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("store.json 权限应为 0600，实际 %o", mode)
	}
}

func TestDeploymentPersistsProbe(t *testing.T) {
	st := newTestStore(t)
	dep := Deployment{
		ID:          "dep1",
		ProjectID:   "p1",
		ProjectName: "app",
		ImageRef:    "app:1",
		Method:      "local-k3s",
		Status:      "deployed",
		Probe: ProbeResult{
			Status:     "ok",
			URL:        "http://localhost:8080/healthz",
			Method:     "GET",
			StatusCode: 200,
			DurationMs: 12,
			Matched:    true,
			Detail:     "HTTP 200，12ms",
		},
		CreatedAt: time.Now(),
	}
	if err := st.SaveDeployment(dep); err != nil {
		t.Fatalf("保存部署失败: %v", err)
	}
	got := st.ListDeployments("")
	if len(got) != 1 {
		t.Fatalf("应有一条部署记录，得到 %d", len(got))
	}
	if got[0].Probe.Status != "ok" {
		t.Fatalf("部署的探测状态应为 ok，得到 %q", got[0].Probe.Status)
	}
	if got[0].Probe.StatusCode != 200 || !got[0].Probe.Matched {
		t.Fatalf("探测明细未持久化: %+v", got[0].Probe)
	}
}

func TestListProjectsPaged(t *testing.T) {
	st := newTestStore(t)
	seed := []Project{
		{ID: "p-hello", Name: "hello-java", Repository: "git@a.com:hello/java.git", CreatedAt: time.Now().Add(-3 * time.Hour)},
		{ID: "p-node", Name: "hello-node", Repository: "git@a.com:hello/node.git", CreatedAt: time.Now().Add(-2 * time.Hour)},
		{ID: "p-cicd", Name: "hello-cicd", Repository: "git@a.com:cicd/go.git", CreatedAt: time.Now().Add(-1 * time.Hour)},
	}
	for _, p := range seed {
		if err := st.CreateProject(p); err != nil {
			t.Fatal(err)
		}
	}

	// 默认（无 q）按创建时间倒序，第一页 limit=2。
	page1 := st.ListProjectsPaged("", 2, 0)
	if page1.Total != 3 {
		t.Fatalf("total 应为 3，得到 %d", page1.Total)
	}
	if len(page1.Projects) != 2 {
		t.Fatalf("第一页应 2 条，得到 %d", len(page1.Projects))
	}
	// 倒序：最新（hello-cicd）应在最前。
	if page1.Projects[0].ID != "p-cicd" {
		t.Fatalf("倒序首条应为 p-cicd，得到 %s", page1.Projects[0].ID)
	}

	// 第二页剩余 1 条。
	page2 := st.ListProjectsPaged("", 2, 2)
	if len(page2.Projects) != 1 || page2.Projects[0].ID != "p-hello" {
		t.Fatalf("第二页应只剩 p-hello，得到 %+v", page2.Projects)
	}

	// q 过滤（大小写不敏感，匹配 name 或 repository）。
	q := st.ListProjectsPaged("NODE", 10, 0)
	if q.Total != 1 || q.Projects[0].ID != "p-node" {
		t.Fatalf("q=node 应只命中 hello-node，得到 %+v (total=%d)", q.Projects, q.Total)
	}
	q2 := st.ListProjectsPaged("cicd/go", 10, 0)
	if q2.Total != 1 || q2.Projects[0].ID != "p-cicd" {
		t.Fatalf("q=repository 应命中 hello-cicd，得到 %+v (total=%d)", q2.Projects, q2.Total)
	}

	// limit<=0 表示全部。
	all := st.ListProjectsPaged("", 0, 0)
	if all.Total != 3 || len(all.Projects) != 3 {
		t.Fatalf("limit=0 应返回全部 3 条，得到 total=%d len=%d", all.Total, len(all.Projects))
	}
}

// 回归：旧数据的 created_at 可能全为空（time.Time 零值）。排序比较器退化时，
// sort.Slice 在不同分页请求间返回不一致顺序，相邻页互相重叠，前端按 id 去重后
// 可见条数 < 总数（如 50 条只显示 30 条）。ID 兜底 tiebreaker 必须保证分页不重叠。
func TestListDeploymentsPagedDisjointWithNullCreatedAt(t *testing.T) {
	st := newTestStore(t)
	const n = 50
	for i := 0; i < n; i++ {
		st.deployments = append(st.deployments, Deployment{
			ID:        fmt.Sprintf("dep-%03d", i),
			ProjectID: "proj-x",
			// CreatedAt 故意留零值，复现排序退化导致的分页重叠
		})
	}
	seen := map[string]bool{}
	total := 0
	for off := 0; off < n; off += 20 {
		page := st.ListDeploymentsPaged("", 20, off)
		if off == 0 {
			total = page.Total
		}
		for _, d := range page.Deployments {
			if seen[d.ID] {
				t.Fatalf("分页重叠：id=%s 在 offset=%d 重复出现", d.ID, off)
			}
			seen[d.ID] = true
		}
	}
	if total != n {
		t.Fatalf("total=%d, want %d", total, n)
	}
	if len(seen) != n {
		t.Fatalf("去重后可见 %d 条, want %d（分页重叠导致丢失）", len(seen), n)
	}
}
