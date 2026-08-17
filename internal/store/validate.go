package store

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MaskedSecret is the placeholder returned instead of a stored credential.
// Sending it back unchanged on update means "keep the existing secret".
const MaskedSecret = "********"

// allowedImportBins is the whitelist of binaries a project may invoke as its
// image-import command. Without this, deploy.k3s_import_cmd is an arbitrary
// command-execution primitive reachable by anyone who can create a project.
var allowedImportBins = map[string]bool{
	"k3s":      true,
	"k3d":      true,
	"ctr":      true,
	"crictl":   true,
	"nerdctl":  true,
	"microk8s": true,
}

func allowedImportList() string {
	names := make([]string, 0, len(allowedImportBins))
	for n := range allowedImportBins {
		names = append(names, n)
	}
	// stable order for readable error messages
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return strings.Join(names, ", ")
}

// ValidateImportCmd checks that an image-import command only invokes a known
// container-runtime binary (optionally via sudo). An empty command is valid and
// means "skip the import" (e.g. OrbStack shares docker images with the cluster).
func ValidateImportCmd(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Fields(raw)
	// The command is executed without a shell, so metacharacters would be passed
	// through as literal arguments. Reject them anyway: a config that looks like
	// it pipes but silently doesn't is worse than an explicit error.
	for _, p := range parts {
		if strings.ContainsAny(p, "|;&$`><\n") {
			return fmt.Errorf("镜像导入命令不支持 shell 元字符: %q", p)
		}
	}
	idx := 0
	if filepath.Base(parts[0]) == "sudo" {
		idx = 1
		for idx < len(parts) && strings.HasPrefix(parts[idx], "-") {
			idx++
		}
		if idx >= len(parts) {
			return fmt.Errorf("sudo 后缺少要执行的命令")
		}
	}
	if bin := filepath.Base(parts[idx]); !allowedImportBins[bin] {
		return fmt.Errorf("不允许的镜像导入命令 %q；仅允许 %s（可加 sudo 前缀）", bin, allowedImportList())
	}
	return nil
}

// pathWithinRoot reports an error when path escapes root. Both are resolved to
// absolute paths first so "..", symlink-free relative paths and trailing
// separators behave predictably.
func pathWithinRoot(root, path string) error {
	if root == "" || path == "" {
		return nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return fmt.Errorf("路径 %s 不在允许目录 %s 内", path, root)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("路径 %s 越出了允许目录 %s", path, root)
	}
	return nil
}

// Validate checks a project's configuration before it is stored or executed.
// workDir, when non-empty, confines every filesystem path the project can make
// the platform touch (build context, Dockerfile, manifest, chart, kubeconfig).
func (p Project) Validate(workDir string) error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("name 为必填")
	}
	if err := ValidateImportCmd(p.Deploy.K3sImportCmd); err != nil {
		return err
	}
	if !IsValidDeployMethod(p.Deploy.Method) {
		return fmt.Errorf("未知的部署方式 %q；可选 kubectl-apply | kubectl-set-image | helm | local-k3s | ssh", p.Deploy.Method)
	}
	if p.Deploy.Method == "ssh" {
		if err := validateSSH(p.Deploy); err != nil {
			return err
		}
	}
	for label, path := range map[string]string{
		"build.context":        p.Build.Context,
		"build.dockerfile":     p.Build.Dockerfile,
		"deploy.manifest_path": p.Deploy.ManifestPath,
		"deploy.chart_path":    p.Deploy.ChartPath,
	} {
		if err := pathWithinRoot(workDir, path); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	if err := validateProbe(p.Probe); err != nil {
		return err
	}
	// Validate named publish targets (multi-cloud). Each must have a unique,
	// non-empty name and a valid method; ssh targets go through the same
	// connection checks as the primary deploy.
	seen := map[string]bool{}
	for i, t := range p.Targets {
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("第 %d 个发布目标缺少名称", i+1)
		}
		if seen[t.Name] {
			return fmt.Errorf("发布目标名称重复: %s", t.Name)
		}
		seen[t.Name] = true
		if !IsValidDeployMethod(t.Method) {
			return fmt.Errorf("发布目标 %q 的部署方式非法: %q", t.Name, t.Method)
		}
		if t.Method == "ssh" {
			if err := validateSSH(t.DeploySpec); err != nil {
				return fmt.Errorf("发布目标 %q: %w", t.Name, err)
			}
		}
	}
	return nil
}

// ResolveDeploySpec returns the DeploySpec to use for a single run.
//
//   - If targetName is non-empty it must match a named DeployTarget; that
//     target's full DeploySpec is returned (the run is published to that target).
//   - Otherwise the project's primary Deploy is used, with an optional per-run
//     method override applied (the legacy "publish to a different method" flow).
//
// An empty targetName with an empty methodOverride returns the primary spec
// unchanged. A non-empty targetName that matches nothing is an error so the
// caller fails the run loudly instead of silently deploying the wrong target.
func (p Project) ResolveDeploySpec(targetName, methodOverride string) (DeploySpec, error) {
	if strings.TrimSpace(targetName) != "" {
		for _, t := range p.Targets {
			if t.Name == targetName {
				return t.DeploySpec, nil
			}
		}
		return DeploySpec{}, fmt.Errorf("未找到发布目标 %q", targetName)
	}
	spec := p.Deploy
	if strings.TrimSpace(methodOverride) != "" {
		spec.Method = methodOverride
	}
	return spec, nil
}

// validateSSH checks the bare-host SSH deploy configuration. Host and user are
// mandatory (either set on the project or supplied via CICD_SSH_* env vars, so
// operators can keep connection secrets in a .env file); the port must be
// numeric when set; and every value that ends up interpolated into a remote
// `docker` command is confined to shell-safe tokens (no |;&$`>< or newlines) so
// a project config cannot smuggle shell metacharacters onto the remote host.
func validateSSH(spec DeploySpec) error {
	// Effective values: project field wins, otherwise the matching env var.
	host := sshEnvOr(spec.SSHHost, "CICD_SSH_HOST", "")
	user := sshEnvOr(spec.SSHUser, "CICD_SSH_USER", "")
	port := sshEnvOr(spec.SSHPort, "CICD_SSH_PORT", "")
	container := sshEnvOr(spec.SSHContainer, "CICD_SSH_CONTAINER", "")
	keyPath := sshEnvOr(spec.SSHKeyPath, "CICD_SSH_KEY_PATH", "")
	runArgs := sshEnvOr(spec.SSHRunArgs, "CICD_SSH_RUN_ARGS", "")
	image := strings.TrimSpace(spec.SSHImage)

	if host == "" {
		return fmt.Errorf("ssh 部署缺少 deploy.ssh_host（或环境变量 CICD_SSH_HOST）")
	}
	if user == "" {
		return fmt.Errorf("ssh 部署缺少 deploy.ssh_user（或环境变量 CICD_SSH_USER）")
	}
	if port != "" {
		if _, err := strconv.Atoi(port); err != nil {
			return fmt.Errorf("deploy.ssh_port / CICD_SSH_PORT 非法（应为数字）: %q", port)
		}
	}
	// Every value interpolated into the remote docker command must be shell-safe.
	// We reject quotes, command separators, subshells, globs, redirections,
	// braces and backslash escapes — anything that could break out of the
	// remote shell. sshRunScript also single-quotes container/image at build
	// time (defense in depth), but the config must never carry these chars.
	const shellMetachars = "|;&$`><\"'(){}\n\\"
	for field, val := range map[string]string{
		"deploy.ssh_host / CICD_SSH_HOST":          host,
		"deploy.ssh_user / CICD_SSH_USER":          user,
		"deploy.ssh_container / CICD_SSH_CONTAINER": container,
		"deploy.ssh_key_path / CICD_SSH_KEY_PATH":    keyPath,
		"deploy.ssh_run_args / CICD_SSH_RUN_ARGS":    runArgs,
		"deploy.ssh_image":                          image,
	} {
		if strings.ContainsAny(val, shellMetachars) {
			return fmt.Errorf("%s 含非法字符（不允许 shell 元字符）", field)
		}
	}
	// Container name feeds `docker rm/run --name`; a leading dash would be
	// parsed as a flag, so require it to start with an alphanumerics char.
	if container != "" && strings.HasPrefix(container, "-") {
		return fmt.Errorf("deploy.ssh_container / CICD_SSH_CONTAINER 不能以 - 开头")
	}
	return nil
}

// sshEnvOr returns v when set, otherwise the value of envKey, otherwise def.
func sshEnvOr(v, envKey, def string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	if e := strings.TrimSpace(os.Getenv(envKey)); e != "" {
		return e
	}
	return def
}

// ResolveSSHSpec fills any empty SSH connection fields from CICD_SSH_* environment
// variables, so connection secrets can live in a .env file instead of the stored
// project / UI form. It is the single source of truth shared by the deploy stage
// and the service probe (which derives the bare-host probe URL from the resolved
// host + probe port). ResolveSSHSpec is a pure mapping of env vars onto the spec
// — no commands are executed, so it is safe to call from validation, deploy, and
// the probe alike.
func ResolveSSHSpec(spec DeploySpec) DeploySpec {
	spec.SSHHost = sshEnvOr(spec.SSHHost, "CICD_SSH_HOST", "")
	spec.SSHUser = sshEnvOr(spec.SSHUser, "CICD_SSH_USER", "")
	spec.SSHPort = sshEnvOr(spec.SSHPort, "CICD_SSH_PORT", "")
	spec.SSHKeyPath = sshEnvOr(spec.SSHKeyPath, "CICD_SSH_KEY_PATH", "")
	spec.SSHContainer = sshEnvOr(spec.SSHContainer, "CICD_SSH_CONTAINER", "")
	spec.SSHRunArgs = sshEnvOr(spec.SSHRunArgs, "CICD_SSH_RUN_ARGS", "")
	spec.SSHProbePort = sshEnvOr(spec.SSHProbePort, "CICD_SSH_PROBE_PORT", "")
	return spec
}

// ResolveRegistrySpec fills empty registry fields from CICD_REGISTRY_* env vars
// so operators can keep registry credentials in a .env file instead of the
// project JSON (which is persisted to disk). At build time this restores the
// real password even when the on-disk copy was masked (when CICD_REGISTRY_PASSWORD
// is configured the store writes a placeholder to disk rather than the secret).
func ResolveRegistrySpec(reg RegistryAuth) RegistryAuth {
	reg.Server = sshEnvOr(reg.Server, "CICD_REGISTRY_SERVER", "")
	reg.Username = sshEnvOr(reg.Username, "CICD_REGISTRY_USERNAME", "")
	reg.Password = sshEnvOr(reg.Password, "CICD_REGISTRY_PASSWORD", "")
	return reg
}

// allowedProbeMethods bounds the HTTP verbs a probe may send, avoiding
// unexpected side effects from a misconfigured probe.
var allowedProbeMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true,
	"HEAD": true, "PATCH": true, "OPTIONS": true,
}

// validateProbe sanity-checks a probe config only when it is enabled. An empty
// (disabled) probe is always valid.
func validateProbe(spec ProbeSpec) error {
	if !spec.Enabled {
		return nil
	}
	method := strings.ToUpper(strings.TrimSpace(spec.Method))
	if method != "" && !allowedProbeMethods[method] {
		return fmt.Errorf("probe 不支持的 HTTP 方法 %q；可选 GET/POST/PUT/DELETE/HEAD/PATCH/OPTIONS", spec.Method)
	}
	if spec.URL != "" {
		if u, err := parseProbeURL(spec.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("probe.url %q 不是合法的 http(s) 地址", spec.URL)
		}
	}
	for k, v := range spec.URLs {
		if u, err := parseProbeURL(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("probe.urls[%q]=%q 不是合法的 http(s) 地址", k, v)
		}
	}
	if spec.ExpectedStatus != 0 && (spec.ExpectedStatus < 100 || spec.ExpectedStatus > 599) {
		return fmt.Errorf("probe.expected_status %d 不在 100-599 范围内", spec.ExpectedStatus)
	}
	if spec.Timeout != "" {
		if _, err := time.ParseDuration(spec.Timeout); err != nil {
			return fmt.Errorf("probe.timeout %q 非法（例: 5s, 500ms）", spec.Timeout)
		}
	}
	return nil
}

// parseProbeURL is a thin wrapper so the validation above can reuse a single
// code path for URL parsing.
func parseProbeURL(raw string) (*url.URL, error) {
	return url.Parse(strings.TrimSpace(raw))
}

// Redacted returns a copy safe to send over the API: stored credentials and
// local filesystem paths are replaced with MaskedSecret so a GET never leaks a
// registry password nor discloses the operator's home directory / username via
// absolute key or kubeconfig paths.
func (p Project) Redacted() Project {
	if p.Build.Registry.Password != "" {
		p.Build.Registry.Password = MaskedSecret
	}
	if p.Deploy.SSHKeyPath != "" {
		p.Deploy.SSHKeyPath = MaskedSecret
	}
	if p.Deploy.Kubeconfig != "" {
		p.Deploy.Kubeconfig = MaskedSecret
	}
	return p
}

// MergeSecrets carries forward secrets the caller did not intend to change: an
// empty or masked incoming password / path keeps the previously stored one, so
// a GET-edit-PUT round trip through the UI cannot silently erase credentials
// or the local paths it cannot see.
func (p Project) MergeSecrets(old Project) Project {
	if p.Build.Registry.Password == "" || p.Build.Registry.Password == MaskedSecret {
		p.Build.Registry.Password = old.Build.Registry.Password
	}
	if p.Deploy.SSHKeyPath == "" || p.Deploy.SSHKeyPath == MaskedSecret {
		p.Deploy.SSHKeyPath = old.Deploy.SSHKeyPath
	}
	if p.Deploy.Kubeconfig == "" || p.Deploy.Kubeconfig == MaskedSecret {
		p.Deploy.Kubeconfig = old.Deploy.Kubeconfig
	}
	return p
}

// knownDeployMethods is the canonical set of supported deploy methods. Keep it in
// sync with the switch in Validate and the UI's publish-target menu.
var knownDeployMethods = []string{
	"kubectl-apply", "kubectl-set-image", "helm", "local-k3s", "ssh",
}

// IsValidDeployMethod reports whether m is a supported deploy method (empty is
// allowed and means "default/inherit"). Used both by Validate and by the API to
// vet a per-run method override before it reaches deploy.Run.
func IsValidDeployMethod(m string) bool {
	if m == "" {
		return true
	}
	for _, k := range knownDeployMethods {
		if k == m {
			return true
		}
	}
	return false
}
