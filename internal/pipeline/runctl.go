package pipeline

import (
	"context"
	"sync"
	"time"

	"github.com/erishen/cicdkit/internal/store"
)

const (
	// maxRunLogBytes caps a single run's stored log. A docker build can emit
	// megabytes; for diagnosis only the tail matters, and an unbounded log gets
	// rewritten into store.json on every flush.
	maxRunLogBytes = 256 << 10
	truncNotice    = "\n… (日志过长，已截断，仅保留最近内容) …\n"

	// logFlushDelay batches log persistence. Writing through on every chunk made
	// the store rewrite the whole JSON file per output line — quadratic I/O that
	// grows with run history.
	logFlushDelay = 400 * time.Millisecond
)

// runCtl owns a single Run. Every read and write of the Run goes through this
// one mutex, which is what makes the log writer (called from os/exec's copy
// goroutines), the stage bookkeeping and Cancel safe to use concurrently.
// It also throttles persistence instead of saving on every log chunk.
type runCtl struct {
	mu       sync.Mutex
	run      store.Run
	store    store.Store
	cancelFn context.CancelFunc
	canceled bool
	dirty    bool
	timer    *time.Timer
}

func newRunCtl(run store.Run, st store.Store) *runCtl {
	return &runCtl{run: run, store: st}
}

func (c *runCtl) id() string { return c.run.ID }

// snapshot returns a copy of the current run state.
func (c *runCtl) snapshot() store.Run {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.run
}

func (c *runCtl) imageRef() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.run.ImageRef
}

func (c *runCtl) imageTag() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.run.ImageTag
}

// Write implements io.Writer so build/deploy engines can stream logs here.
func (c *runCtl) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.run.Log += string(p)
	if len(c.run.Log) > maxRunLogBytes {
		keep := maxRunLogBytes - len(truncNotice)
		c.run.Log = truncNotice + c.run.Log[len(c.run.Log)-keep:]
	}
	c.touchLocked()
	return len(p), nil
}

// touchLocked marks state as unsaved and schedules a delayed flush, so a burst
// of log lines results in one write instead of hundreds.
func (c *runCtl) touchLocked() {
	c.dirty = true
	if c.timer == nil {
		c.timer = time.AfterFunc(logFlushDelay, c.flush)
	}
}

func (c *runCtl) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushLocked()
}

func (c *runCtl) flushLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if !c.dirty {
		return
	}
	c.dirty = false
	_ = c.store.SaveRun(c.run)
}

// save persists immediately (used at state transitions, where the UI should see
// the change without waiting for the throttle window).
func (c *runCtl) save() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dirty = true
	c.flushLocked()
}

func (c *runCtl) setCancelFn(fn context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelFn = fn
	// Cancel may have arrived while the run was still queued.
	if c.canceled && c.cancelFn != nil {
		c.cancelFn()
	}
}

// cancel aborts the run. It records the canceled state immediately so the UI
// reflects it, and finish() will not overwrite it with a failure caused by the
// cancellation itself.
func (c *runCtl) cancel() {
	c.mu.Lock()
	c.canceled = true
	c.run.Status = store.StatusCanceled
	c.run.EndedAt = time.Now()
	fn := c.cancelFn
	c.dirty = true
	c.flushLocked()
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (c *runCtl) isCanceled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.canceled
}

func (c *runCtl) begin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.run.Status = store.StatusRunning
	c.run.StartedAt = time.Now()
	c.dirty = true
	c.flushLocked()
}

func (c *runCtl) addStage(sr store.StageResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.run.Stages = append(c.run.Stages, sr)
	c.dirty = true
	c.flushLocked()
}

// setProbe records the post-deploy service-availability result on the run so
// it is persisted and the UI can surface 服务探测 as a run step. A nil/empty
// probe (probe disabled or skipped) leaves the field unset.
func (c *runCtl) setProbe(pr store.ProbeResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pr.Status == "" {
		return
	}
	cp := pr
	c.run.Probe = &cp
	c.dirty = true
	c.flushLocked()
}

// finish records the terminal status, preserving an explicit cancellation.
func (c *runCtl) finish(status store.RunStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.canceled {
		status = store.StatusCanceled
	}
	c.run.Status = status
	c.run.EndedAt = time.Now()
	c.dirty = true
	c.flushLocked()
}
