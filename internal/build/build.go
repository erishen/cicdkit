package build

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/erishen/cicdkit/internal/store"
)

// DockerConfig carries the docker environment (e.g. DOCKER_HOST).
type DockerConfig struct {
	DockerHost string
}

// Run executes `docker build` for the project and returns the stage result.
// Output is streamed to log (and buffered for the result) as it arrives.
func Run(ctx context.Context, cfg DockerConfig, p store.Project, tag string, log io.Writer) (store.StageResult, error) {
	start := time.Now()
	res := store.StageResult{Name: "build", Status: store.StatusRunning, StartedAt: start}
	if p.Build.ImageRepo == "" {
		res.Status = store.StatusFailed
		res.Error = "build.image_repo 未配置"
		res.EndedAt = time.Now()
		return res, fmt.Errorf("%s", res.Error)
	}
	imageRef := fmt.Sprintf("%s:%s", strings.TrimSuffix(p.Build.ImageRepo, "/"), tag)

	// Private-registry auth (needed if base images or the push target are
	// private). Done up front so both the single- and multi-arch paths below
	// can rely on it. Resolve from CICD_REGISTRY_* env first so the secret need
	// not live in the on-disk project JSON.
	reg := store.ResolveRegistrySpec(p.Build.Registry)
	if reg.Username != "" {
		if err := dockerLogin(ctx, reg, log); err != nil {
			res.Status = store.StatusFailed
			res.Error = fmt.Sprintf("docker login 失败: %v", err)
			res.EndedAt = time.Now()
			return res, err
		}
	}

	// Decide the build platform. For an ssh_transfer deploy we ship a tar to a
	// SINGLE host, so we must build a SINGLE-arch image for that host. A
	// multi-arch OCI index cannot be ingested by the remote `docker load`
	// (it only accepts single-image docker-format or single-image OCI tars),
	// which is exactly why a multi-arch transfer previously failed with
	// "docker-import .../blobs/json: no such file or directory" and then fell
	// back to a Docker Hub pull. So: prefer the ssh-specific platforms (the
	// target host's arch), fall back to Build.Platforms, and cap to one.
	platforms := p.Build.Platforms
	if p.Deploy.Method == "ssh" && p.Deploy.SSHTransfer {
		if len(p.Deploy.SSHBuildPlatforms) > 0 {
			platforms = p.Deploy.SSHBuildPlatforms
		}
		if len(platforms) > 1 {
			platforms = platforms[:1]
		}
		// ssh_transfer targets a single bare VM. When no platform is configured
		// (neither Build.Platforms nor the CICD_SSH_BUILD_PLATFORMS env), default
		// to linux/amd64 so the image runs natively on the overwhelmingly-common
		// x86 cloud host — instead of being built for the runner's own arch (e.g.
		// an arm64 Mac) and then failing under QEMU emulation on the remote. This
		// also prevents a stale arm64 "last successful build" from being reused
		// and streamed to an amd64 host.
		if len(platforms) == 0 {
			platforms = []string{"linux/amd64"}
		}
	}

	// Multi-arch build: a classic `docker build` can only emit a single
	// platform, so a multi-arch image requires `docker buildx build
	// --platform a,b --output type=oci` which writes a portable tar. The
	// ssh_transfer stage ships this tar and the remote's `docker run` picks its
	// own arch automatically — no manual single-arch guess, no QEMU warning.
	if len(platforms) > 1 {
		// A multi-arch OCI tar needs a container-backed buildx builder (the
		// default `docker` driver cannot export OCI). Ensure one exists and pin
		// the build to it explicitly.
		builder, err := ensureMultiArchBuilder(ctx, cfg, log)
		if err != nil {
			res.Status = store.StatusFailed
			res.Error = err.Error()
			res.EndedAt = time.Now()
			return res, err
		}
		tarPath := filepath.Join(os.TempDir(), "cicd-multi-"+tag+".tar")
		args := []string{"buildx", "build", "--builder", builder,
			"--platform", strings.Join(platforms, ","),
			"-t", imageRef,
			"--output", "type=oci,dest=" + tarPath,
		}
		if p.Build.Dockerfile != "" {
			args = append(args, "-f", p.Build.Dockerfile)
		}
		if p.Build.Target != "" {
			args = append(args, "--target", p.Build.Target)
		}
		for k, v := range p.Build.BuildArgs {
			args = append(args, "--build-arg", fmt.Sprintf("%s=%s", k, v))
		}
		ctxDir := p.Build.Context
		if ctxDir == "" {
			ctxDir = "."
		}
		args = append(args, ctxDir)

		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Env = dockerEnv(cfg)
		var buf bytes.Buffer
		cmd.Stdout = io.MultiWriter(log, &buf)
		cmd.Stderr = io.MultiWriter(log, &buf)
		if err := cmd.Run(); err != nil {
			res.Status = store.StatusFailed
			res.Error = err.Error()
			res.Log = buf.String()
			res.EndedAt = time.Now()
			return res, err
		}
		res.ArtifactPath = tarPath
		res.Status = store.StatusSuccess
		res.Log = buf.String()
		res.EndedAt = time.Now()
		return res, nil
	}

	// Single-platform (or native) build via classic `docker build`.
	args := []string{"build"}
	if p.Build.Dockerfile != "" {
		args = append(args, "-f", p.Build.Dockerfile)
	}
	if p.Build.Target != "" {
		args = append(args, "--target", p.Build.Target)
	}
	for k, v := range p.Build.BuildArgs {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", k, v))
	}
	for _, plat := range platforms {
		args = append(args, "--platform", plat)
	}
	args = append(args, "-t", imageRef)
	ctxDir := p.Build.Context
	if ctxDir == "" {
		ctxDir = "."
	}
	args = append(args, ctxDir)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = dockerEnv(cfg)
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(log, &buf)
	cmd.Stderr = io.MultiWriter(log, &buf)
	if err := cmd.Run(); err != nil {
		res.Status = store.StatusFailed
		res.Error = err.Error()
		res.Log = buf.String()
		res.EndedAt = time.Now()
		return res, err
	}
	res.Status = store.StatusSuccess
	res.Log = buf.String()
	res.EndedAt = time.Now()
	return res, nil
}

// multiArchBuilderName is the buildx builder used for multi-arch OCI exports.
// The default `docker` driver cannot emit an OCI tar (it reports "OCI exporter
// is not supported for the docker driver"), so a container-backed builder is
// required. We create/reuse one named here.
const multiArchBuilderName = "cicd-multiarch"

// ensureMultiArchBuilder makes sure a container-backed buildx builder exists and
// returns its name. The default `docker` driver cannot export a multi-arch OCI
// tar, so we use a `docker-container` driver. Reuse an existing builder of that
// name if present; otherwise create it.
func ensureMultiArchBuilder(ctx context.Context, cfg DockerConfig, log io.Writer) (string, error) {
	// Best-effort: register QEMU binfmt for cross-arch emulation — but ONLY if
	// the helper image is already present locally. We must not trigger a
	// docker.io pull here (it can fail in restricted networks and pollutes the
	// build log). On Docker Desktop / OrbStack cross-arch emulation is provided
	// by the platform already, so this is purely an opt-in fallback for bare
	// environments, and its output is discarded.
	if ImagePresent(ctx, cfg, "tonistiigin/qemu-user-static:latest") {
		qemu := exec.CommandContext(ctx, "docker", "run", "--rm", "--privileged",
			"tonistiigin/qemu-user-static", "--reset", "-p", "yes")
		qemu.Env = dockerEnv(cfg)
		qemu.Stdout = io.Discard
		qemu.Stderr = io.Discard
		_ = qemu.Run()
	}

	// Create the container-backed builder only if it does not already exist.
	// Calling `buildx create` on an existing builder errors with "existing
	// instance ... but no append mode", which is harmless but pollutes the log,
	// so we probe first. We never change the user's *default* builder (builds
	// always pass --builder explicitly), so classic `docker build` is untouched.
	if !builderExists(ctx, cfg, multiArchBuilderName) {
		create := exec.CommandContext(ctx, "docker", "buildx", "create",
			"--name", multiArchBuilderName, "--driver", "docker-container")
		create.Env = dockerEnv(cfg)
		create.Stdout = log
		create.Stderr = log
		if err := create.Run(); err != nil {
			// Any failure to create surfaces when the build actually runs.
			fmt.Fprintln(log, "（创建 buildx builder "+multiArchBuilderName+" 失败: "+err.Error()+"）")
		}
	}
	return multiArchBuilderName, nil
}

// builderExists reports whether a named buildx builder is already present.
func builderExists(ctx context.Context, cfg DockerConfig, name string) bool {
	cmd := exec.CommandContext(ctx, "docker", "buildx", "inspect", name)
	cmd.Env = dockerEnv(cfg)
	return cmd.Run() == nil
}

// ImagePresent reports whether a docker image is already in the local store.
func ImagePresent(ctx context.Context, cfg DockerConfig, ref string) bool {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", ref)
	cmd.Env = dockerEnv(cfg)
	return cmd.Run() == nil
}

// ImageArch returns the architecture of a locally built image (e.g. "amd64",
// "arm64"), or "" when it cannot be determined. Used by the deploy guard that
// refuses to ship a cached build whose arch does not match the target host.
func ImageArch(ctx context.Context, cfg DockerConfig, ref string) string {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.Architecture}}", ref)
	cmd.Env = dockerEnv(cfg)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(buf.String())
}

// Push executes `docker push` for an already-built image reference.
func Push(ctx context.Context, cfg DockerConfig, p store.Project, imageRef string, log io.Writer) (store.StageResult, error) {
	start := time.Now()
	res := store.StageResult{Name: "push", Status: store.StatusRunning, StartedAt: start}
	cmd := exec.CommandContext(ctx, "docker", "push", imageRef)
	cmd.Env = dockerEnv(cfg)
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(log, &buf)
	cmd.Stderr = io.MultiWriter(log, &buf)
	if err := cmd.Run(); err != nil {
		res.Status = store.StatusFailed
		res.Error = err.Error()
		res.Log = buf.String()
		res.EndedAt = time.Now()
		return res, err
	}
	res.Status = store.StatusSuccess
	res.Log = buf.String()
	res.EndedAt = time.Now()
	return res, nil
}

func dockerLogin(ctx context.Context, reg store.RegistryAuth, log io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "login", reg.Server, "-u", reg.Username, "--password-stdin")
	cmd.Env = os.Environ()
	cmd.Stdin = strings.NewReader(reg.Password)
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(log, &buf)
	cmd.Stderr = io.MultiWriter(log, &buf)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, buf.String())
	}
	return nil
}

func dockerEnv(cfg DockerConfig) []string {
	env := os.Environ()
	if cfg.DockerHost != "" {
		env = append(env, "DOCKER_HOST="+cfg.DockerHost)
	}
	return env
}
