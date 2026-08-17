// Package check implements a dry-run validation of a project: it verifies the
// project configuration, probes whether the docker daemon and (when the deploy
// method touches a cluster) the kubeconfig are reachable, and checks that the
// build context exists on disk. It never builds or deploys anything — its only
// side effect is running a couple of read-only `docker`/`kubectl` probes.
package check

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/erishen/cicdkit/internal/config"
	"github.com/erishen/cicdkit/internal/probe"
	"github.com/erishen/cicdkit/internal/store"
)

// Status values for an individual check.
const (
	StatusOK    = "ok"
	StatusWarn  = "warn"
	StatusError = "error"
)

// Result is one validation check's outcome.
type Result struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Run performs a dry-run validation of a project and returns one Result per
// check performed. The order is stable: config, docker, kubeconfig, build
// context.
func Run(ctx context.Context, p store.Project, cfg *config.Config) []Result {
	results := make([]Result, 0, 4)
	results = append(results, configCheck(p, cfg))
	results = append(results, dockerCheck(ctx, cfg))
	if needsCluster(p.Deploy.Method) {
		results = append(results, kubeconfigCheck(ctx, p, cfg))
	}
	results = append(results, buildContextCheck(p, cfg))
	results = append(results, serviceProbeCheck(ctx, p, cfg))
	return results
}

// Summary reports whether every check passed. A warning is not fatal; only an
// explicit error fails the summary.
func Summary(results []Result) bool {
	for _, r := range results {
		if r.Status == StatusError {
			return false
		}
	}
	return true
}

func configCheck(p store.Project, cfg *config.Config) Result {
	if err := p.Validate(cfg.Runner.WorkDir); err != nil {
		return Result{"配置校验", StatusError, err.Error()}
	}
	return Result{"配置校验", StatusOK, "项目配置合法（部署方式、导入命令白名单、路径均在允许目录内）"}
}

// dockerCheck probes the docker daemon with `docker info`. DOCKER_HOST is
// honoured so a remote docker socket is checked the same way deploys would use.
func dockerCheck(ctx context.Context, cfg *config.Config) Result {
	out, err := runProbe(ctx, cfg, "docker", "info")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Result{"Docker daemon", StatusError, "未找到 docker 命令（请安装 Docker / OrbStack 并确保其在 PATH 中）"}
		}
		detail := "docker daemon 不可达"
		if line := firstLine(out); line != "" {
			detail = "docker daemon 不可达: " + line
		}
		return Result{"Docker daemon", StatusError, detail}
	}
	ver := extractField(out, "Server Version:")
	if ver != "" {
		return Result{"Docker daemon", StatusOK, "可达（Server " + ver + "）"}
	}
	return Result{"Docker daemon", StatusOK, "可达"}
}

// kubeconfigCheck probes the cluster with `kubectl cluster-info`. A project
// kubeconfig wins over the configured default; if neither is set kubectl falls
// back to its own default (~/.kube/config), which we still attempt.
func kubeconfigCheck(ctx context.Context, p store.Project, cfg *config.Config) Result {
	kc := resolveKubeconfig(p.Deploy.Kubeconfig, cfg.Runner.DefaultKubeconfig)
	args := []string{}
	if kc != "" {
		args = append(args, "--kubeconfig", kc)
	}
	args = append(args, "cluster-info")
	out, err := runProbe(ctx, nil, "kubectl", args...)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return Result{"Kubeconfig / 集群", StatusError, "未找到 kubectl 命令（请安装 kubectl 并确保其在 PATH 中）"}
		}
		detail := "集群不可达"
		if line := firstLine(out); line != "" {
			detail = "集群不可达: " + line
		}
		if kc == "" {
			detail += "（未配置 kubeconfig，已尝试 kubectl 默认配置）"
		} else {
			detail += "（kubeconfig: " + kc + "）"
		}
		return Result{"Kubeconfig / 集群", StatusError, detail}
	}
	return Result{"Kubeconfig / 集群", StatusOK, "集群可达" + kubeControlPlane(out)}
}

// buildContextCheck verifies the build context path exists on disk. The docker
// build context is resolved relative to the server's working directory, but we
// also accept it being relative to Runner.WorkDir for convenience.
func buildContextCheck(p store.Project, cfg *config.Config) Result {
	ctxPath := strings.TrimSpace(p.Build.Context)
	if ctxPath == "" {
		return Result{"构建上下文", StatusWarn, "未配置 build.context，跳过构建上下文检查"}
	}
	resolved := resolveContext(ctxPath, cfg.Runner.WorkDir)
	if resolved == "" {
		return Result{"构建上下文", StatusError, fmt.Sprintf("build.context %q 不存在（已尝试相对于工作目录与 %s）", ctxPath, cfg.Runner.WorkDir)}
	}
	detail := fmt.Sprintf("build.context %q 存在", ctxPath)
	if df := strings.TrimSpace(p.Build.Dockerfile); df != "" {
		// The dockerfile path is resolved the same way as the context: relative
		// to the server cwd (and then WorkDir). It is NOT relative to the
		// context dir. This matches build.Run, which passes `-f <dockerfile>`
		// and the context dir to `docker build` as two independent arguments
		// (e.g. `docker build -f examples/hello-cicd/Dockerfile examples/hello-cicd`).
		dfResolved := resolveContext(df, cfg.Runner.WorkDir)
		if dfResolved == "" {
			return Result{"构建上下文", StatusError, fmt.Sprintf("dockerfile %q 不存在（已尝试相对于工作目录与 %s）", df, cfg.Runner.WorkDir)}
		}
		detail += "，dockerfile " + df + " 存在"
	}
	return Result{"构建上下文", StatusOK, detail}
}

// serviceProbeCheck verifies the deployed service is actually reachable. When
// the project has not enabled a probe it is reported as a warning (not an
// error) so the dry-run still passes — service probing is opt-in.
func serviceProbeCheck(ctx context.Context, p store.Project, cfg *config.Config) Result {
	if !p.Probe.Enabled {
		return Result{"服务可用性", StatusWarn, "未启用服务探测（在项目设置中启用 probe 以在部署前/后检查服务是否可用）"}
	}
	res := probe.Run(ctx, p, cfg, "")
	switch res.Status {
	case "ok":
		return Result{"服务可用性", StatusOK, "服务可达: " + res.Detail}
	case "skip":
		return Result{"服务可用性", StatusWarn, res.Detail}
	case "err":
		return Result{"服务可用性", StatusError, "探测出错: " + res.Error}
	default: // fail
		return Result{"服务可用性", StatusError, "服务探测未达标: " + res.Detail}
	}
}

// runProbe runs a read-only command and returns its combined output. When cfg
// is non-nil and the probe is docker, DOCKER_HOST is injected so remote docker
// sockets are probed identically to how deploys use them.
func runProbe(ctx context.Context, cfg *config.Config, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if cfg != nil && name == "docker" && cfg.Runner.DockerHost != "" {
		cmd.Env = append(cmd.Environ(), "DOCKER_HOST="+cfg.Runner.DockerHost)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func resolveKubeconfig(projectKc, defaultKc string) string {
	if strings.TrimSpace(projectKc) != "" {
		return projectKc
	}
	return defaultKc
}

// resolveContext tries the path relative to the server cwd and relative to
// WorkDir, returning the first that exists (absolutized), else "".
func resolveContext(ctxPath, workDir string) string {
	if abs, err := filepath.Abs(ctxPath); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	if workDir != "" {
		joined := filepath.Join(workDir, ctxPath)
		if abs, err := filepath.Abs(joined); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	return ""
}

func needsCluster(method string) bool {
	switch method {
	case "kubectl-apply", "kubectl-set-image", "helm", "local-k3s":
		return true
	default:
		return false
	}
}

func firstLine(out string) string {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func extractField(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func kubeControlPlane(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Kubernetes control plane is running at") {
			return "（" + strings.TrimPrefix(line, "Kubernetes control plane is running at") + "）"
		}
	}
	return ""
}
