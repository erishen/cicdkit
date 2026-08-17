package pipeline

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erishen/cicdkit/internal/store"
)

// countingStore 记录 SaveRun 调用次数，用来证明日志持久化被批量化了。
type countingStore struct {
	mu    sync.Mutex
	saves int64
	last  store.Run
}

func (c *countingStore) SaveRun(r store.Run) error {
	atomic.AddInt64(&c.saves, 1)
	c.mu.Lock()
	c.last = r
	c.mu.Unlock()
	return nil
}
func (c *countingStore) saveCount() int64 { return atomic.LoadInt64(&c.saves) }
func (c *countingStore) lastRun() store.Run {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// 其余方法仅为满足接口。
func (c *countingStore) ListProjects() []store.Project                { return nil }
func (c *countingStore) GetProject(string) (store.Project, bool)      { return store.Project{}, false }
func (c *countingStore) CreateProject(store.Project) error            { return nil }
func (c *countingStore) UpdateProject(store.Project) error            { return nil }
func (c *countingStore) DeleteProject(string) error                   { return nil }
func (c *countingStore) ListRuns(string) []store.Run                  { return nil }
func (c *countingStore) GetRun(string) (store.Run, bool)              { return store.Run{}, false }
func (c *countingStore) LastSuccessfulBuild(string) (store.Run, bool) { return store.Run{}, false }
func (c *countingStore) ListDeployments(string) []store.Deployment    { return nil }
func (c *countingStore) SaveDeployment(store.Deployment) error        { return nil }
func (c *countingStore) GetDeployment(string) (store.Deployment, bool) { return store.Deployment{}, false }
func (c *countingStore) ClearRuns() error                              { return nil }
func (c *countingStore) ClearDeployments() error                      { return nil }
func (c *countingStore) ListRunsPaged(string, int, int) store.RunPage            { return store.RunPage{} }
func (c *countingStore) ListDeploymentsPaged(string, int, int) store.DeploymentPage { return store.DeploymentPage{} }
func (c *countingStore) ListProjectsPaged(string, int, int) store.ProjectPage { return store.ProjectPage{} }

// 日志写入曾经是「每来一行就重写整个 store.json」，对于动辄上千行的
// docker build 输出是二次放大的磁盘 I/O。现在应当被节流成极少次写入。
func TestLogWritesAreThrottled(t *testing.T) {
	cs := &countingStore{}
	ctl := newRunCtl(store.Run{ID: "r1"}, cs)

	const lines = 500
	for i := 0; i < lines; i++ {
		if _, err := ctl.Write([]byte("Step 1/10 : FROM golang\n")); err != nil {
			t.Fatal(err)
		}
	}
	// 等待节流窗口内的延迟落盘完成
	time.Sleep(2 * logFlushDelay)

	if n := cs.saveCount(); n >= lines {
		t.Fatalf("日志写入未被批量化：%d 行触发了 %d 次持久化", lines, n)
	}
	if got := cs.lastRun().Log; strings.Count(got, "\n") != lines {
		t.Fatalf("日志内容丢失：期望 %d 行，得到 %d 行", lines, strings.Count(got, "\n"))
	}
}

// 超长日志必须被截断，否则单个 run 会把整个存储文件撑爆。
func TestLogIsTruncatedKeepingTail(t *testing.T) {
	ctl := newRunCtl(store.Run{ID: "r1"}, &countingStore{})
	chunk := strings.Repeat("x", 64<<10)
	for i := 0; i < 8; i++ { // 512KB > maxRunLogBytes
		_, _ = ctl.Write([]byte(chunk))
	}
	_, _ = ctl.Write([]byte("TAIL-MARKER"))

	got := ctl.snapshot().Log
	if len(got) > maxRunLogBytes+len(truncNotice) {
		t.Fatalf("日志未被截断，长度 %d", len(got))
	}
	if !strings.HasSuffix(got, "TAIL-MARKER") {
		t.Fatal("截断应保留日志尾部（最新内容）")
	}
	if !strings.Contains(got, "截断") {
		t.Fatal("截断后应留有提示")
	}
}

// 取消是显式的用户意图，不能被随后的失败状态覆盖成 failed。
func TestCancelIsNotOverwrittenByFinish(t *testing.T) {
	cs := &countingStore{}
	ctl := newRunCtl(store.Run{ID: "r1", Status: store.StatusQueued}, cs)

	ctl.cancel()
	if got := ctl.snapshot().Status; got != store.StatusCanceled {
		t.Fatalf("取消后状态应为 canceled，得到 %s", got)
	}

	// 取消导致底层命令失败，execute 会走 finish(failed)
	ctl.finish(store.StatusFailed)
	if got := ctl.snapshot().Status; got != store.StatusCanceled {
		t.Fatalf("取消状态被覆盖成了 %s", got)
	}
}

// 排队期间就被取消的运行，拿到 cancelFn 时应立即触发取消。
func TestCancelBeforeCancelFnIsRegistered(t *testing.T) {
	ctl := newRunCtl(store.Run{ID: "r1"}, &countingStore{})
	ctl.cancel()

	fired := false
	ctl.setCancelFn(func() { fired = true })
	if !fired {
		t.Fatal("排队期间的取消应在 cancelFn 注册后立即生效")
	}
}

// 日志写入来自 os/exec 的拷贝 goroutine，而阶段记录、状态变更、取消来自其他
// goroutine：这些必须共用同一把锁。本测试在 -race 下运行才有意义。
func TestConcurrentLogStageAndCancelAreRaceFree(t *testing.T) {
	ctl := newRunCtl(store.Run{ID: "r1"}, &countingStore{})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = ctl.Write([]byte("log line\n"))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			ctl.addStage(store.StageResult{Name: "build", Status: store.StatusSuccess})
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			_ = ctl.snapshot()
			_ = ctl.imageRef()
			_ = ctl.isCanceled()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctl.cancel()
	}()
	wg.Wait()
	ctl.finish(store.StatusSuccess)

	if got := ctl.snapshot().Status; got != store.StatusCanceled {
		t.Fatalf("并发取消后状态应为 canceled，得到 %s", got)
	}
}
