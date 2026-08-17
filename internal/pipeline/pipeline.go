package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/erishen/cicdkit/internal/build"
	"github.com/erishen/cicdkit/internal/config"
	"github.com/erishen/cicdkit/internal/deploy"
	"github.com/erishen/cicdkit/internal/probe"
	"github.com/erishen/cicdkit/internal/store"
)

// Runner orchestrates pipeline executions with bounded concurrency and
// supports cancellation and live log persistence.
type Runner struct {
	cfg    *config.Config
	store  store.Store
	sem    chan struct{}
	active sync.Map // runID -> *runCtl
}

// New creates a Runner bound to the store and configuration.
func New(cfg *config.Config, st store.Store) *Runner {
	return &Runner{
		cfg:   cfg,
		store: st,
		sem:   make(chan struct{}, cfg.Runner.MaxConcurrent),
	}
}

// TriggerBuild builds (and pushes, if configured) an image without deploying.
func (r *Runner) TriggerBuild(projectID, trigger, tag, method, targetName string) (*store.Run, error) {
	return r.start(projectID, trigger, tag, false, method, targetName)
}

// TriggerPipeline builds, pushes (if configured) and deploys.
func (r *Runner) TriggerPipeline(projectID, trigger, tag, method, targetName string) (*store.Run, error) {
	return r.start(projectID, trigger, tag, true, method, targetName)
}

// TriggerDeploy deploys an already-built image reference without building.
func (r *Runner) TriggerDeploy(projectID, trigger, imageRef, method, targetName string) (*store.Run, error) {
	p, ok := r.store.GetProject(projectID)
	if !ok {
		return nil, fmt.Errorf("项目 %s 不存在", projectID)
	}
	if imageRef == "" {
		return nil, fmt.Errorf("部署需要 image_ref 或 tag")
	}
	run := store.Run{
		ID:          store.GenID("run"),
		ProjectID:   p.ID,
		ProjectName: p.Name,
		Trigger:     trigger,
		ImageRef:    imageRef,
		ImageTag:    tagFromRef(imageRef),
		Status:      store.StatusQueued,
		CreatedAt:   time.Now(),
	}
	return r.launch(run, p, false, true, method, targetName)
}

// buildContextError returns a friendly message when the build context path will
// not resolve on the server, or "" when it looks fine. A bare directory name
// (e.g. from a browser directory picker that cannot expose the real path) would
// otherwise surface as the cryptic docker error "path <name> not found".
func buildContextError(ctxPath, workDir string) string {
	ctxPath = strings.TrimSpace(ctxPath)
	if ctxPath == "" {
		return "构建上下文为空：请在项目设置里填写 build.context（服务器可访问的绝对路径），用浏览器目录选择器时无法自动获取真实路径。"
	}
	if abs, err := filepath.Abs(ctxPath); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return ""
		}
	}
	if workDir != "" {
		if abs, err := filepath.Abs(filepath.Join(workDir, ctxPath)); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return ""
			}
		}
	}
	return fmt.Sprintf("构建上下文不存在: %q（请填写服务器可访问的绝对路径，而非目录名；浏览器目录选择器无法自动获取路径，需在保存前手动补全）", ctxPath)
}

func (r *Runner) start(projectID, trigger, tag string, withDeploy bool, method, targetName string) (*store.Run, error) {
	p, ok := r.store.GetProject(projectID)
	if !ok {
		return nil, fmt.Errorf("项目 %s 不存在", projectID)
	}
	t, err := r.resolveTag(p, tag)
	if err != nil {
		return nil, err
	}
	run := store.Run{
		ID:          store.GenID("run"),
		ProjectID:   p.ID,
		ProjectName: p.Name,
		Trigger:     trigger,
		ImageTag:    t,
		ImageRef:    repoImage(p, t),
		Status:      store.StatusQueued,
		CreatedAt:   time.Now(),
	}
	return r.launch(run, p, true, withDeploy, method, targetName)
}

// launch registers the run before returning, so a Cancel arriving immediately
// after the trigger response can always find it (previously the registration
// happened inside the goroutine, leaving a window where Cancel reported
// "not found").
func (r *Runner) launch(run store.Run, p store.Project, doBuild, doDeploy bool, method, targetName string) (*store.Run, error) {
	ctl := newRunCtl(run, r.store)
	if err := r.store.SaveRun(run); err != nil {
		return nil, err
	}
	r.active.Store(run.ID, ctl)
	go r.execute(ctl, p, doBuild, doDeploy, method, targetName)
	return &run, nil
}

// execute runs the pipeline stages for a single run. method, when non-empty and
// no targetName is given, overrides the project's stored deploy method for this
// run only — this is how a single project can be published to a different method
// without persisting a change. targetName, when non-empty, selects a named
// DeployTarget whose full DeploySpec is used for the run. Both are resolved into
// the local p.Deploy copy so every downstream stage (deploy.Run, probe.Run,
// SaveDeployment) sees the correct spec without further changes.
func (r *Runner) execute(ctl *runCtl, p store.Project, doBuild, doDeploy bool, method, targetName string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl.setCancelFn(cancel)
	defer r.active.Delete(ctl.id())

	// Bounded concurrency: a run may sit here while others finish.
	r.sem <- struct{}{}
	defer func() { <-r.sem }()

	// It may have been canceled while queued.
	if ctl.isCanceled() {
		ctl.finish(store.StatusCanceled)
		return
	}

	// Apply a per-run deploy target / method override. A named target wins and
	// uses that target's full DeploySpec; otherwise the project's primary Deploy
	// is used with an optional method override. The resolved spec is written onto
	// the local p.Deploy copy so deploy.Run / probe.Run / SaveDeployment below
	// operate on the right configuration unchanged.
	if strings.TrimSpace(targetName) != "" {
		spec, rerr := p.ResolveDeploySpec(targetName, method)
		if rerr != nil {
			now := time.Now()
			ctl.addStage(store.StageResult{Name: "deploy", Status: store.StatusFailed, Error: rerr.Error(), Log: rerr.Error(), EndedAt: now})
			ctl.finish(store.StatusFailed)
			return
		}
		p.Deploy = spec
	} else if strings.TrimSpace(method) != "" {
		p.Deploy.Method = method
	}

	// When an override switches to ssh but no registry path is configured
	// (push=false and no explicit ssh_pull) and transfer wasn't already enabled,
	// default to registry-free transfer (docker save -> scp -> load). Without
	// this the remote `docker run` silently falls back to a Docker Hub pull and
	// times out, which is exactly the "Unable to find image ... locally" failure.
	if p.Deploy.Method == "ssh" && !p.Build.Push && !p.Deploy.SSHPull && !p.Deploy.SSHTransfer {
		p.Deploy.SSHTransfer = true
	}

	// For an ssh_transfer deploy to a host of a different architecture, default
	// the build platform to the configured target arch (CICD_SSH_BUILD_PLATFORMS,
	// comma-separated) so the transferred image is native and avoids QEMU
	// emulation on the remote (and the "platform does not match host" warning).
	// Only fills it when ssh_transfer is in effect and no explicit per-project
	// ssh_build_platforms was given — keeping local-k3s builds on the native
	// (Apple-Silicon) arch.
	if p.Deploy.Method == "ssh" && p.Deploy.SSHTransfer && len(p.Deploy.SSHBuildPlatforms) == 0 {
		if envPlats := strings.TrimSpace(os.Getenv("CICD_SSH_BUILD_PLATFORMS")); envPlats != "" {
			for _, s := range strings.Split(envPlats, ",") {
				if s = strings.TrimSpace(s); s != "" {
					p.Deploy.SSHBuildPlatforms = append(p.Deploy.SSHBuildPlatforms, s)
				}
			}
		}
		// No explicit override (config or CICD_SSH_BUILD_PLATFORMS env): probe
		// the target host's real architecture over SSH and build for it, so the
		// transferred image is native and runs without QEMU emulation — no
		// "platform does not match host" warning, no slow/failed startup under
		// emulation. A probe failure falls back silently to the build.go default
		// (linux/amd64 for ssh_transfer), so SSH reachability never blocks a build.
		if len(p.Deploy.SSHBuildPlatforms) == 0 {
			if arch := deploy.DetectRemoteArch(ctx, p.Deploy); arch != "" {
				p.Deploy.SSHBuildPlatforms = []string{arch}
			}
		}
	}

	ctl.begin()

	dockerCfg := build.DockerConfig{DockerHost: r.cfg.Runner.DockerHost}
	kubeCfg := deploy.KubeConfig{
		DefaultKubeconfig: r.cfg.Runner.DefaultKubeconfig,
		// The image precheck must query the same daemon that built the image.
		DockerHost: r.cfg.Runner.DockerHost,
	}

	if doBuild {
		if cerr := buildContextError(p.Build.Context, r.cfg.Runner.WorkDir); cerr != "" {
			now := time.Now()
			ctl.addStage(store.StageResult{Name: "build", Status: store.StatusFailed, Error: cerr, Log: cerr, EndedAt: now})
			ctl.finish(store.StatusFailed)
			return
		}
		br, err := build.Run(ctx, dockerCfg, p, ctl.imageTag(), ctl)
		ctl.addStage(br)
		if err != nil {
			ctl.finish(store.StatusFailed)
			return
		}
		// Carry a build-produced artifact (e.g. a multi-arch OCI tar) forward to
		// the deploy stage so ssh_transfer can ship it instead of `docker save`.
		if br.ArtifactPath != "" {
			p.Deploy.TransferArtifact = br.ArtifactPath
		}
		if p.Build.Push {
			pr, perr := build.Push(ctx, dockerCfg, p, ctl.imageRef(), ctl)
			ctl.addStage(pr)
			if perr != nil {
				ctl.finish(store.StatusFailed)
				return
			}
		}
	}

	// probeFailed escalates a failed post-deploy service probe to a failed run.
	// A container may have started (deploy stage succeeded) yet the service is
	// not actually serving — that is a broken rollout, so the run must read as
	// 失败 rather than 成功, matching what the 服务探测 step already shows.
	probeFailed := false

	if doDeploy {
		dr, derr := deploy.Run(ctx, kubeCfg, p, ctl.imageRef(), ctl)
		ctl.addStage(dr)
		if derr != nil {
			ctl.finish(store.StatusFailed)
			return
		}

		// Post-deploy service probe (opt-in). Its outcome is recorded on the
		// deployment and surfaced in the UI; a failed/skipped probe does NOT
		// fail the deploy stage itself, but a failed/skipped-because-unreachable
		// probe escalates the whole run to 失败 (the rollout is not healthy).
		var probeRes store.ProbeResult
		if p.Probe.Enabled {
			probeRes = probe.Run(ctx, p, r.cfg, targetName)
			ctl.Write([]byte("服务探测: " + probeRes.Detail + "\n"))
			ctl.setProbe(probeRes)
			if probeRes.Status == "fail" || probeRes.Status == "err" {
				probeFailed = true
			}
		}

		_ = r.store.SaveDeployment(store.Deployment{
			ID:          store.GenID("dep"),
			ProjectID:   p.ID,
			ProjectName: p.Name,
			ImageRef:    ctl.imageRef(),
			Cluster:     p.Deploy.Kubeconfig,
			Namespace:   p.Deploy.Namespace,
			Method:      p.Deploy.Method,
			Target:      strings.TrimSpace(targetName),
			Status:      "deployed",
			Probe:       probeRes,
			CreatedAt:   time.Now(),
			RunID:       ctl.id(),
		})
	}

	if probeFailed {
		ctl.finish(store.StatusFailed)
	} else {
		ctl.finish(store.StatusSuccess)
	}
}

// Cancel aborts a running pipeline. Returns false if it is not active.
func (r *Runner) Cancel(runID string) bool {
	v, ok := r.active.Load(runID)
	if !ok {
		return false
	}
	v.(*runCtl).cancel()
	return true
}

func (r *Runner) resolveTag(p store.Project, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	switch p.Build.TagStrategy {
	case "git-sha":
		if p.Build.Context != "" && isLocalGitDir(p.Build.Context) {
			out, err := exec.Command("git", "-C", p.Build.Context, "rev-parse", "--short", "HEAD").Output()
			if err == nil {
				return strings.TrimSpace(string(out)), nil
			}
		}
		return defaultTag(), nil
	case "manual":
		return "", fmt.Errorf("tag_strategy=manual 必须显式指定 tag")
	default:
		return defaultTag(), nil
	}
}

func defaultTag() string { return time.Now().Format("20060102-150405") }

func isLocalGitDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}

func repoImage(p store.Project, tag string) string {
	return fmt.Sprintf("%s:%s", strings.TrimSuffix(p.Build.ImageRepo, "/"), tag)
}

func tagFromRef(ref string) string {
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}
