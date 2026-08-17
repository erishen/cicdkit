package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/erishen/cicdkit/internal/store"
)

// KubeConfig carries the default kubeconfig path used when a project does not
// specify its own (typical for a single shared k3s context), plus the docker
// endpoint so local image prechecks talk to the same daemon that built the image.
type KubeConfig struct {
	DefaultKubeconfig string
	DockerHost        string
}

func resolveKubeconfig(spec store.DeploySpec, def string) string {
	if spec.Kubeconfig != "" {
		return spec.Kubeconfig
	}
	return def
}

// Run rolls the image out using the method configured on the project:
// kubectl-apply, kubectl-set-image, helm, local-k3s, or ssh.
func Run(ctx context.Context, cfg KubeConfig, p store.Project, imageRef string, log io.Writer) (store.StageResult, error) {
	start := time.Now()
	res := store.StageResult{Name: "deploy", Status: store.StatusRunning, StartedAt: start}
	spec := p.Deploy
	var buf bytes.Buffer
	logw := io.MultiWriter(log, &buf)

	switch spec.Method {
	case "kubectl-apply", "kubectl-set-image", "helm":
		cmd, err := buildCmd(ctx, spec.Method, cfg, p, imageRef)
		if err != nil {
			res.Status = store.StatusFailed
			res.Error = err.Error()
			res.Log = buf.String()
			res.EndedAt = time.Now()
			return res, err
		}
		cmd.Stdout = logw
		cmd.Stderr = logw
		cmd.Env = os.Environ()
		if err := cmd.Run(); err != nil {
			res.Status = store.StatusFailed
			res.Error = err.Error()
			res.Log = buf.String()
			res.EndedAt = time.Now()
			return res, err
		}
		// For set-image, wait for the rollout so a fake "image updated" success
		// is replaced by a real readiness check.
		if spec.Method == "kubectl-set-image" && spec.Wait {
			if err := waitForRollout(ctx, cfg, spec, logw); err != nil {
				res.Status = store.StatusFailed
				res.Error = err.Error()
				res.Log = buf.String()
				res.EndedAt = time.Now()
				return res, err
			}
		}
	case "local-k3s":
		if err := runLocalK3s(ctx, cfg, p, imageRef, logw); err != nil {
			res.Status = store.StatusFailed
			res.Error = err.Error()
			res.Log = buf.String()
			res.EndedAt = time.Now()
			return res, err
		}
	case "ssh":
		if err := runSSH(ctx, cfg, p, imageRef, logw); err != nil {
			res.Status = store.StatusFailed
			res.Error = err.Error()
			res.Log = buf.String()
			res.EndedAt = time.Now()
			return res, err
		}
	default:
		return fail(res, fmt.Sprintf("未知的部署方式: %q", spec.Method))
	}

	res.Status = store.StatusSuccess
	res.Log = buf.String()
	res.EndedAt = time.Now()
	return res, nil
}

// buildCmd assembles the exec.Cmd for the single-command deploy methods. It is
// bound to ctx so cancelling a run actually kills the kubectl/helm process.
func buildCmd(ctx context.Context, method string, cfg KubeConfig, p store.Project, imageRef string) (*exec.Cmd, error) {
	spec := p.Deploy
	switch method {
	case "kubectl-apply":
		if spec.ManifestPath == "" {
			return nil, fmt.Errorf("deploy.manifest_path 未配置 (kubectl-apply)")
		}
		args := []string{"--kubeconfig", resolveKubeconfig(spec, cfg.DefaultKubeconfig)}
		if spec.Namespace != "" {
			args = append(args, "-n", spec.Namespace)
		}
		args = append(args, "apply", "-f", spec.ManifestPath)
		if spec.Wait {
			args = append(args, "--wait")
		}
		if spec.Timeout != "" {
			args = append(args, "--timeout", spec.Timeout)
		}
		return exec.CommandContext(ctx, "kubectl", args...), nil
	case "kubectl-set-image":
		if spec.Deployment == "" || spec.Container == "" {
			return nil, fmt.Errorf("deploy.deployment 与 deploy.container 为必填 (kubectl-set-image)")
		}
		target := fmt.Sprintf("deployment/%s", spec.Deployment)
		image := fmt.Sprintf("%s=%s", spec.Container, imageRef)
		args := []string{"--kubeconfig", resolveKubeconfig(spec, cfg.DefaultKubeconfig)}
		if spec.Namespace != "" {
			args = append(args, "-n", spec.Namespace)
		}
		args = append(args, "set", "image", target, image)
		return exec.CommandContext(ctx, "kubectl", args...), nil
	case "helm":
		if spec.ReleaseName == "" || spec.ChartPath == "" {
			return nil, fmt.Errorf("deploy.release_name 与 deploy.chart_path 为必填 (helm)")
		}
		args := []string{"--kubeconfig", resolveKubeconfig(spec, cfg.DefaultKubeconfig)}
		if spec.Namespace != "" {
			args = append(args, "-n", spec.Namespace)
		}
		args = append(args, "upgrade", "--install", spec.ReleaseName, spec.ChartPath)
		for k, v := range spec.HelmValues {
			args = append(args, "--set", fmt.Sprintf("%s=%s", k, v))
		}
		if spec.HelmSetImage {
			key := spec.HelmImageKey
			if key == "" {
				key = "image"
			}
			args = append(args, "--set", fmt.Sprintf("%s=%s", key, imageRef))
		}
		if spec.Wait {
			args = append(args, "--wait")
		}
		if spec.Timeout != "" {
			args = append(args, "--timeout", spec.Timeout)
		}
		return exec.CommandContext(ctx, "helm", args...), nil
	}
	return nil, fmt.Errorf("不支持的方式: %q", method)
}

// runLocalK3s performs a fully self-contained local rollout with no external
// registry: it optionally imports the freshly built image into the k3s
// containerd (skipped when K3sImportCmd is empty, e.g. OrbStack auto-shares
// docker images), applies the target manifest on every deploy so the Service
// and Deployment stay in sync with the file, then rolls the new image out via
// kubectl set-image.
func runLocalK3s(ctx context.Context, cfg KubeConfig, p store.Project, imageRef string, logw io.Writer) error {
	spec := p.Deploy
	if spec.Deployment == "" || spec.Container == "" {
		return fmt.Errorf("deploy.deployment 与 deploy.container 为必填 (local-k3s)")
	}

	// 0. precheck: the image must actually exist locally before we point the
	// Deployment at it. Without this, set-image "succeeds" while the new Pod
	// silently sits in ImagePullBackOff (the exact false-success we hit before).
	if !imageExistsLocally(ctx, cfg, imageRef) {
		msg := fmt.Sprintf("本地不存在镜像 %s，无法部署；请先 Build 该 tag，或在请求中指定已构建的 image_ref", imageRef)
		fmt.Fprintln(logw, "!! "+msg)
		return fmt.Errorf("%s", msg)
	}

	// 1. (optional) import the built image into k3s' containerd
	if strings.TrimSpace(spec.K3sImportCmd) != "" {
		// Re-check at execution time, not just on write: store.json can be edited
		// by hand, and this command is executed on the host.
		if err := store.ValidateImportCmd(spec.K3sImportCmd); err != nil {
			return err
		}
		fmt.Fprintln(logw, ">> 导入镜像到 k3s: "+spec.K3sImportCmd)
		if err := importImageToK3s(ctx, cfg, imageRef, spec.K3sImportCmd, logw); err != nil {
			return fmt.Errorf("导入镜像到 k3s 失败: %w", err)
		}
	} else {
		fmt.Fprintln(logw, ">> 跳过镜像导入（k3s_import_cmd 为空，假定镜像已对集群可见，如 OrbStack 自动共享）")
	}

	// 2. (re)apply the manifest on EVERY deploy so edits to it (Service type,
	// labels, resources, probes, …) are reconciled into the cluster, not just
	// on the very first deploy. kubectl apply is idempotent, so re-applying an
	// unchanged manifest is a no-op; skipping it (only applying when the
	// Deployment is absent) left already-created resources — notably the Service
	// — out of sync, which broke service-probe address derivation.
	if spec.ManifestPath != "" {
		fmt.Fprintln(logw, ">> apply 清单（确保 Service/Deployment 与清单同步）: "+spec.ManifestPath)
		args := []string{"--kubeconfig", resolveKubeconfig(spec, cfg.DefaultKubeconfig)}
		if spec.Namespace != "" {
			args = append(args, "-n", spec.Namespace)
		}
		args = append(args, "apply", "-f", spec.ManifestPath)
		if err := runKubectl(ctx, args, logw); err != nil {
			return fmt.Errorf("apply 清单失败: %w", err)
		}
	}

	// 3. roll the new image out
	fmt.Fprintln(logw, ">> kubectl set-image "+spec.Deployment+" "+spec.Container+"="+imageRef)
	setArgs := []string{"--kubeconfig", resolveKubeconfig(spec, cfg.DefaultKubeconfig)}
	if spec.Namespace != "" {
		setArgs = append(setArgs, "-n", spec.Namespace)
	}
	setArgs = append(setArgs, "set", "image",
		fmt.Sprintf("deployment/%s", spec.Deployment),
		fmt.Sprintf("%s=%s", spec.Container, imageRef))
	if err := runKubectl(ctx, setArgs, logw); err != nil {
		return fmt.Errorf("set-image 失败: %w", err)
	}

	// 4. wait for the rollout to actually become available. This turns the
	// previous "image updated => success" false-positive into a real check:
	// if the new Pods can't pull/start, rollout status fails and the run fails.
	if err := waitForRollout(ctx, cfg, spec, logw); err != nil {
		return fmt.Errorf("滚动更新未完成（新 Pod 未能就绪）: %w", err)
	}
	return nil
}

func runKubectl(ctx context.Context, args []string, logw io.Writer) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = logw
	cmd.Stderr = logw
	cmd.Env = os.Environ()
	return cmd.Run()
}

// sshImage resolves the image to deploy: an explicit SSHImage overrides the
// built imageRef; otherwise the locally built image (image_repo:tag) is used,
// which is exactly what the registry-free transfer mode saves and ships.
func sshImage(p store.Project, imageRef string) (string, error) {
	image := strings.TrimSpace(p.Deploy.SSHImage)
	if image == "" {
		image = strings.TrimSpace(imageRef)
	}
	if image == "" {
		return "", fmt.Errorf("ssh 部署缺少镜像：请设置 build.image_repo（构建会产出本地镜像），或在 deploy.ssh_image 显式填写")
	}
	return image, nil
}

// sshKeyArgs returns the SSH identity flag(s), or nil when no key is configured.
func sshKeyArgs(spec store.DeploySpec) []string {
	if k := strings.TrimSpace(spec.SSHKeyPath); k != "" {
		return []string{"-i", k}
	}
	return nil
}

func sshRemoteTarget(spec store.DeploySpec) string {
	return spec.SSHUser + "@" + spec.SSHHost
}

// sshRunScript builds the remote shell script: optionally pull/load the image,
// remove any prior container, then `docker run -d`. When transfer is true the
// image is already on the host (loaded from remoteTar) so no registry pull runs.
func sshRunScript(p store.Project, image, remoteTar string, transfer bool) string {
	var b strings.Builder
	if transfer {
		b.WriteString("docker load -i " + remoteTar + "\n")
	} else if p.Deploy.SSHPull {
		// image is validated shell-safe in validateSSH; single-quote anyway.
		b.WriteString("docker pull '" + image + "'\n")
	}
	// Default the container name to the project id so each project owns exactly
	// one container across redeploys: `docker rm -f` removes the prior container
	// before the new one starts, which avoids port conflicts when the same host
	// port is reused. An explicit SSHContainer always wins.
	c := strings.TrimSpace(p.Deploy.SSHContainer)
	if c == "" {
		c = defaultContainerName(p.ID)
	}
	// The host publish port is ALWAYS derived from SSHProbePort (default 8080)
	// and mapped to the container's 8080 (every generated Dockerfile EXPOSEs
	// 8080). SSHRunArgs is treated as EXTRA args only — any -p/--publish the
	// operator typed there is stripped so it can never override or duplicate
	// the probe-port mapping. This keeps the "探针端口" field authoritative:
	// changing it re-publishes on a new host port on every deploy (the cleanup
	// block below frees whichever port it resolves to). Previously a cloud
	// preset wrote `-p 8080:8080` into SSHRunArgs, which made SSHProbePort a
	// silent no-op and is why the port looked unchangeable.
	probePort := strings.TrimSpace(p.Deploy.SSHProbePort)
	if probePort == "" {
		probePort = "8080"
	}
	userArgs := stripPublishFlags(strings.TrimSpace(p.Deploy.SSHRunArgs))
	args := strings.TrimSpace("-p " + probePort + ":8080 --restart unless-stopped " + userArgs)
	// Free any published host ports up front so a redeploy never fails with
	// "address already in use". A left-over container from an earlier run
	// (possibly a different name — e.g. a manual `docker run` or a past build)
	// would otherwise hold the port and make the new `docker run` fail. The
	// name-based `rm -f` below only reaches this project's own container, so we
	// also hunt by port here. The `publish` filter is the clean path; the
	// port-column scan is a fallback for Docker builds where the filter misses
	// (it has historically returned incomplete results for some daemons).
	for _, port := range publishedHostPorts(args) {
		b.WriteString("for cid in $(docker ps -q --filter \"publish=" + port + "\"); do [ -n \"$cid\" ] && docker rm -f \"$cid\"; done\n")
		b.WriteString("for cid in $(docker ps --format '{{.ID}} {{.Ports}}' | awk -v p=" + port + " '$0 ~ \"[: ]\"p\"($|->|/)\" {print $1}'); do [ -n \"$cid\" ] && docker rm -f \"$cid\"; done\n")
	}
	// c (container name) and image are validated shell-safe by validateSSH /
	// defaultContainerName; single-quote them so a stray metacharacter in a
	// future code path can never reach the remote shell as an unquoted word.
	b.WriteString("docker rm -f '" + c + "' 2>/dev/null || true\n")
	b.WriteString("docker run -d --name '" + c + "' " + args + " '" + image + "'\n")
	return b.String()
}

// publishedHostPorts extracts the host-side ports from `-p/--publish` flags in
// the run args, so the deploy script can free them before starting a new
// container. It handles "8080", "8080:80" and "127.0.0.1:8080:80" forms
// (space-separated; the scan default `-p 8080:8080` is covered).
func publishedHostPorts(args string) []string {
	var ports []string
	f := strings.Fields(args)
	for i := 0; i < len(f); i++ {
		tok := f[i]
		switch {
		case tok == "-p" || tok == "--publish":
			if i+1 < len(f) {
				if p := hostPortOf(f[i+1]); p != "" {
					ports = append(ports, p)
				}
				i++ // skip the value token
			}
		case strings.HasPrefix(tok, "--publish="):
			if p := hostPortOf(strings.TrimPrefix(tok, "--publish=")); p != "" {
				ports = append(ports, p)
			}
		}
	}
	return ports
}

// hostPortOf parses a single -p value and returns the host port.
func hostPortOf(spec string) string {
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 1:
		return parts[0] // bare port, e.g. "8080"
	case 2:
		return parts[0] // host:container
	default:
		return parts[len(parts)-2] // ip:host:container
	}
}

// stripPublishFlags removes -p/--publish tokens (and their values) from
// user-supplied docker run args, so the platform owns the host port mapping
// and SSHRunArgs can only ever add extra flags (env, volumes, …) without
// conflicting with the probe-port-derived -p.
func stripPublishFlags(args string) string {
	f := strings.Fields(args)
	var out []string
	for i := 0; i < len(f); i++ {
		tok := f[i]
		switch {
		case tok == "-p" || tok == "--publish":
			if i+1 < len(f) {
				i++ // skip the value token
			}
		case strings.HasPrefix(tok, "--publish="):
			// drop entirely
		default:
			out = append(out, tok)
		}
	}
	return strings.Join(out, " ")
}

// defaultContainerName derives a stable, DNS-safe container name from a project
// id so each project maps to a single container name. Docker container names
// must match [a-zA-Z0-9][a-zA-Z0-9_.-]*; anything else collapses to a dash and
// a leading non-alphanumeric is prefixed with 'c'.
func defaultContainerName(id string) string {
	s := strings.TrimSpace(id)
	if s == "" {
		return "cicd-app"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if first := out[0]; !(first >= 'a' && first <= 'z') && !(first >= 'A' && first <= 'Z') && !(first >= '0' && first <= '9') {
		out = "c" + out
	}
	return out
}

// sanitizeImageName turns an image reference into a filesystem-safe token for
// the temporary tar filename shared between local save and remote load.
func sanitizeImageName(image string) string {
	repl := strings.NewReplacer("/", "_", ":", "_", "@", "_", ".", "_")
	s := repl.Replace(image)
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		return "img"
	}
	return b.String()
}

// sshBaseArgs builds the common `ssh` argument prefix (identity, port, host
// key and log-level options, remote target) shared by the registry-based
// bare-host deploy and the streaming image load. It is pure so the argument
// shape can be unit-tested. The remote command is appended by the caller.
func sshBaseArgs(spec store.DeploySpec) []string {
	port := strings.TrimSpace(spec.SSHPort)
	if port == "" {
		port = "22"
	}
	return append(sshKeyArgs(spec), "-p", port, "-o", "StrictHostKeyChecking=accept-new", "-o", "LogLevel=ERROR", sshRemoteTarget(spec))
}

// sshDeployParts builds the `ssh` invocation for a registry-based bare-host
// deploy. It is a pure function so the command construction can be unit-tested
// without a real SSH server. The image defaults to the built imageRef unless
// SSHImage is set.
func sshDeployParts(p store.Project, imageRef string) (string, []string, error) {
	image, err := sshImage(p, imageRef)
	if err != nil {
		return "", nil, err
	}
	script := sshRunScript(p, image, "", false)
	args := append(sshBaseArgs(p.Deploy), script)
	return "ssh", args, nil
}

// runSSH deploys to a bare host over SSH. When SSHTransfer is set the image is
// shipped registry-free (local docker save → scp → remote docker load);
// otherwise the remote pulls the image (optionally) and starts the container.
func runSSH(ctx context.Context, cfg KubeConfig, p store.Project, imageRef string, logw io.Writer) error {
	image, err := sshImage(p, imageRef)
	if err != nil {
		return err
	}
	// Fill connection fields from CICD_SSH_* env vars so they can live in .env.
	p.Deploy = store.ResolveSSHSpec(p.Deploy)
	// Fail fast on a full remote disk before touching the daemon (both the
	// streaming transfer and the registry-pull paths write image layers to the
	// remote Docker data-root).
	if err := sshCheckRemoteDisk(ctx, p.Deploy, localImageSizeBytes(ctx, cfg, image)); err != nil {
		return err
	}
	if p.Deploy.SSHTransfer {
		return runSSHTransfer(ctx, cfg, p, image, logw)
	}
	bin, args, err := sshDeployParts(p, imageRef)
	if err != nil {
		return err
	}
	fmt.Fprintln(logw, ">> ssh 部署（远端加载并启动容器）")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = logw
	cmd.Stderr = logw
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh 部署失败: %w", err)
	}
	return nil
}

// runSSHCommandString runs a single remote command over ssh and returns its
// stdout as a string. Stderr is discarded so diagnostic noise (e.g. the
// post-quantum key-exchange warning) never leaks into the deploy log.
func runSSHCommandString(ctx context.Context, spec store.DeploySpec, remoteCmd string) (string, error) {
	args := append(sshBaseArgs(spec), remoteCmd)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Env = os.Environ()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// mapUnameArch converts `uname -m` output into a docker/OCI platform string
// (e.g. "x86_64" -> "linux/amd64", "aarch64" -> "linux/arm64"). It is a pure
// function so the mapping can be unit-tested without an SSH server.
func mapUnameArch(m string) string {
	switch strings.TrimSpace(strings.ToLower(m)) {
	case "x86_64", "amd64":
		return "linux/amd64"
	case "aarch64", "arm64":
		return "linux/arm64"
	default:
		return ""
	}
}

// DetectRemoteArch reports the target host's docker platform by querying
// `uname -m` over SSH. It returns "" on any failure (so callers can fall back
// to their own default) — it never blocks a deploy on the probe itself. The
// spec is first normalised through ResolveSSHSpec so CICD_SSH_* env vars are
// honoured for host/user/key. This is how an ssh_transfer build learns the
// real host arch and ships a native image instead of relying on the runner's
// own architecture (which on an Apple-Silicon laptop would be arm64 and then
// fail under QEMU emulation on an amd64 cloud host).
func DetectRemoteArch(ctx context.Context, spec store.DeploySpec) string {
	spec = store.ResolveSSHSpec(spec)
	out, err := runSSHCommandString(ctx, spec, "uname -m 2>/dev/null")
	if err != nil {
		return ""
	}
	return mapUnameArch(out)
}

// localImageSizeBytes returns the uncompressed footprint of a locally built
// image (bytes). Used only to size the remote disk-space pre-check; a probe
// failure yields 0 and the check falls back to a flat floor.
func localImageSizeBytes(ctx context.Context, cfg KubeConfig, image string) int64 {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.Size}}", image)
	cmd.Env = os.Environ()
	if cfg.DockerHost != "" {
		cmd.Env = append(cmd.Env, "DOCKER_HOST="+cfg.DockerHost)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(buf.String()), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// sshCheckRemoteDisk fails fast with a clear, actionable message when the
// remote Docker data-root partition has too little free space for the image we
// are about to stream up. Without this guard, `docker load` on a full disk
// collapses deep inside the daemon with an opaque error (e.g. a shell-profile
// here-document "No space left on device"), and the daemon may thrash the CPU
// retrying. A probe failure is non-fatal — we never block a deploy on the
// pre-check itself.
func sshCheckRemoteDisk(ctx context.Context, spec store.DeploySpec, imageSizeBytes int64) error {
	root := "/var/lib/docker"
	if out, err := runSSHCommandString(ctx, spec, "docker info -f '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker"); err == nil {
		if r := strings.TrimSpace(out); r != "" {
			root = r
		}
	}
	freeKB, err := remotePartitionFreeKB(ctx, spec, root)
	if err != nil {
		return nil
	}
	return sshCheckDiskThreshold(root, freeKB, imageSizeBytes)
}

// sshCheckDiskThreshold is the pure threshold decision used by
// sshCheckRemoteDisk. It fails when the partition hosting the Docker data-root
// has too little free space for the image being deployed. Exposed for unit
// testing without an SSH server.
func sshCheckDiskThreshold(root string, freeKB, imageSizeBytes int64) error {
	freeBytes := freeKB * 1024
	// Require at least the image's uncompressed footprint (with margin) plus a
	// 512 MB daemon headroom; floor the requirement at 1 GB.
	need := int64(1) << 30
	if imageSizeBytes > 0 {
		if req := int64(float64(imageSizeBytes)*1.5) + 512*1024*1024; req > need {
			need = req
		}
	}
	if freeBytes < need {
		return fmt.Errorf("远端磁盘空间不足：Docker 数据目录(%s)仅剩 %.1f GB，本次部署至少需要 %.1f GB。请先到远端执行 `docker system prune -a -f` 清理未用镜像/容器，再重试",
			root, float64(freeBytes)/1024/1024/1024, float64(need)/1024/1024/1024)
	}
	return nil
}

// remotePartitionFreeKB reports the available space (in 1024-byte blocks) on
// the partition hosting path, queried on the remote host.
func remotePartitionFreeKB(ctx context.Context, spec store.DeploySpec, path string) (int64, error) {
	out, err := runSSHCommandString(ctx, spec, "df -P -k "+path+" 2>/dev/null | awk 'NR==2{print $4}'")
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("解析远端可用空间失败: %w", err)
	}
	return v, nil
}

// runSSHTransfer deploys without any container registry by streaming the
// locally built image straight into the remote docker daemon: `docker save`
// writes its tar to stdout, which is piped into `ssh host docker load` on the
// remote. Nothing is ever written to the remote host's disk, so a small tmpfs
// /tmp (common on cloud VMs) can no longer break the transfer with
// "scp: write remote ...: Failure". Ideal for pushing to a single bare VM from
// a laptop that has no registry.
func runSSHTransfer(ctx context.Context, cfg KubeConfig, p store.Project, image string, logw io.Writer) error {
	spec := p.Deploy

	// 0. fail-fast remote disk pre-check: a full disk must surface a clear
	// message, not an opaque daemon error plus CPU thrash.
	if err := sshCheckRemoteDisk(ctx, spec, localImageSizeBytes(ctx, cfg, image)); err != nil {
		return err
	}

	// 1. Stream the image into the remote daemon: docker save | ssh docker load.
	save := exec.CommandContext(ctx, "docker", "save", image)
	save.Env = os.Environ()
	if cfg.DockerHost != "" {
		save.Env = append(save.Env, "DOCKER_HOST="+cfg.DockerHost)
	}
	save.Stderr = logw

	loadArgs := append(sshBaseArgs(spec), "docker load")
	load := exec.CommandContext(ctx, "ssh", loadArgs...)
	load.Env = os.Environ()
	load.Stdout = logw
	load.Stderr = logw

	// Pipe the save stream into the remote load: take the read end of the
	// save's stdout and feed it to the load's stdin.
	pr, err := save.StdoutPipe()
	if err != nil {
		return fmt.Errorf("创建 docker save 管道失败: %w", err)
	}
	load.Stdin = pr

	fmt.Fprintln(logw, ">> 直推镜像到远端 docker load（流式，不落盘）")
	if err := save.Start(); err != nil {
		return fmt.Errorf("启动本地 docker save 失败: %w", err)
	}
	if err := load.Start(); err != nil {
		_ = save.Process.Kill()
		_ = save.Wait()
		return fmt.Errorf("启动远端 docker load 失败: %w", err)
	}
	// Wait for the consumer first (it drains stdin), then the producer. Surface a
	// local save failure preferentially — a broken upstream tar is the usual
	// root cause when the remote load complains about a malformed archive.
	loadErr := load.Wait()
	saveErr := save.Wait()
	if saveErr != nil {
		return fmt.Errorf("本地 docker save 失败: %w", saveErr)
	}
	if loadErr != nil {
		return fmt.Errorf("远端 docker load 失败: %w", loadErr)
	}

	// 2. ssh: (re)start the container. The image is already loaded, so the run
	// script needs no load/pull step.
	script := sshRunScript(p, image, "", false)
	sshArgs := append(sshBaseArgs(spec), script)
	fmt.Fprintln(logw, ">> ssh 远端启动容器")
	ssh := exec.CommandContext(ctx, "ssh", sshArgs...)
	ssh.Stdout = logw
	ssh.Stderr = logw
	ssh.Env = os.Environ()
	if err := ssh.Run(); err != nil {
		return fmt.Errorf("ssh 远端部署失败: %w", err)
	}
	return nil
}

// imageExistsLocally reports whether the image reference is present in the
// docker image store. Used by local-k3s to fail fast before pointing the
// Deployment at a non-existent image. It must honour DOCKER_HOST: otherwise a
// custom endpoint (OrbStack, remote daemon) is built against but probed on the
// default socket, and a perfectly good image looks missing.
func imageExistsLocally(ctx context.Context, cfg KubeConfig, imageRef string) bool {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", imageRef)
	cmd.Env = os.Environ()
	if cfg.DockerHost != "" {
		cmd.Env = append(cmd.Env, "DOCKER_HOST="+cfg.DockerHost)
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// waitForRollout blocks until the Deployment's new revision is fully rolled
// out (or the configured timeout elapses), so a deploy only "succeeds" when
// the new Pods are actually Ready.
func waitForRollout(ctx context.Context, cfg KubeConfig, spec store.DeploySpec, logw io.Writer) error {
	args := []string{"--kubeconfig", resolveKubeconfig(spec, cfg.DefaultKubeconfig)}
	if spec.Namespace != "" {
		args = append(args, "-n", spec.Namespace)
	}
	args = append(args, "rollout", "status", "deployment/"+spec.Deployment)
	if spec.Timeout != "" {
		args = append(args, "--timeout", spec.Timeout)
	}
	fmt.Fprintln(logw, ">> 等待滚动更新完成: kubectl "+strings.Join(args, " "))
	return runKubectl(ctx, args, logw)
}

func deploymentExists(ctx context.Context, cfg KubeConfig, spec store.DeploySpec) bool {
	args := []string{"--kubeconfig", resolveKubeconfig(spec, cfg.DefaultKubeconfig)}
	if spec.Namespace != "" {
		args = append(args, "-n", spec.Namespace)
	}
	args = append(args, "get", "deployment", spec.Deployment)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// importImageToK3s streams `docker save <imageRef>` into the k3s import command
// (default "k3s ctr images import -") via a pipe, so no intermediate tar file is
// written. importCmd is the raw shell-style command (e.g. "k3s ctr images
// import -" or "sudo k3s ctr images import -").
func importImageToK3s(ctx context.Context, cfg KubeConfig, imageRef, importCmd string, logw io.Writer) error {
	save := exec.CommandContext(ctx, "docker", "save", imageRef)
	save.Env = os.Environ()
	if cfg.DockerHost != "" {
		save.Env = append(save.Env, "DOCKER_HOST="+cfg.DockerHost)
	}
	save.Stderr = logw
	parts := strings.Fields(importCmd)
	if len(parts) == 0 {
		parts = []string{"k3s", "ctr", "images", "import", "-"}
	}
	imp := exec.CommandContext(ctx, parts[0], parts[1:]...)
	imp.Stdout = logw
	imp.Stderr = logw
	saveOut, err := save.StdoutPipe()
	if err != nil {
		return fmt.Errorf("docker save 管道创建失败: %w", err)
	}
	imp.Stdin = saveOut
	if err := save.Start(); err != nil {
		return fmt.Errorf("docker save 启动失败: %w", err)
	}
	if err := imp.Start(); err != nil {
		return fmt.Errorf("k3s 导入命令启动失败 (%s): %w", importCmd, err)
	}
	if err := save.Wait(); err != nil {
		return fmt.Errorf("docker save 失败: %w", err)
	}
	if err := imp.Wait(); err != nil {
		return fmt.Errorf("k3s 导入失败: %w", err)
	}
	return nil
}

func fail(res store.StageResult, msg string) (store.StageResult, error) {
	res.Status = store.StatusFailed
	res.Error = msg
	res.EndedAt = time.Now()
	return res, fmt.Errorf("%s", msg)
}
