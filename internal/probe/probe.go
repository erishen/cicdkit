// Package probe performs a Postman-style HTTP availability check of a deployed
// service. A probe is configured per-project (ProbeSpec) and, when enabled,
// runs automatically after a successful deploy and on demand via the API.
//
// The target URL is resolved per deploy method. For ssh deploys the probe URL
// is derived first from the resolved connection — the host comes from CICD_SSH_HOST
// (.env) or deploy.ssh_host and the port from the probe port config — so the real
// host IP never appears in the project JSON. Only if no .env host is configured
// does it fall back to ProbeSpec.URLs[<method>] (per-target override) or
// ProbeSpec.URL (global manual URL). For cluster methods the URL is auto-derived
// from the Kubernetes Service backing the deployment (NodePort → the node's
// ExternalIP, else its InternalIP, else localhost; LoadBalancer → ingress IP). A
// ClusterIP-only Service cannot be reached from the host, in which case the probe
// is skipped with an explanatory note rather than failing the deploy.
//
// For a NodePort we try every candidate host (ExternalIP, InternalIP,
// localhost) in turn and report the first one that answers, because which
// address actually reaches the port depends on the cluster: local VM-backed
// clusters (OrbStack / Rancher Desktop / Lima) publish the NodePort on the
// VM's InternalIP (not localhost), whereas k3d/kind/native k3s publish it on
// localhost. Trying all of them makes the probe robust across setups.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/erishen/cicdkit/internal/config"
	"github.com/erishen/cicdkit/internal/store"
)

// MaxBody is the largest response body the probe retains for display. Larger
// bodies are truncated so a misbehaving service cannot blow up the stored run.
const MaxBody = 64 * 1024

var allowedMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "DELETE": true,
	"HEAD": true, "PATCH": true, "OPTIONS": true,
}

// Run executes the probe described by p.Probe. When the probe is disabled it
// returns a skipped result (no network activity). The ctx deadline / cancellation
// is honoured so a deploy cancel also cancels an in-flight probe. targetName,
// when non-empty, is also tried as a probe-URL key so each named DeployTarget
// can have its own probe address (e.g. probe.urls["腾讯云-prod"]).
func Run(ctx context.Context, p store.Project, cfg *config.Config, targetName string) store.ProbeResult {
	spec := p.Probe
	if !spec.Enabled {
		return store.ProbeResult{Status: "skip", Detail: "未启用服务探测（在项目设置中勾选「启用服务探测」）"}
	}
	method := strings.ToUpper(strings.TrimSpace(spec.Method))
	if method == "" {
		method = "GET"
	}
	if !allowedMethods[method] {
		return store.ProbeResult{Status: "err", Method: method, Error: "不支持的 HTTP 方法: " + spec.Method, Detail: "不支持的 HTTP 方法"}
	}

	urls, note := resolveURL(ctx, p, cfg, targetName)
	if len(urls) == 0 {
		return store.ProbeResult{Status: "skip", Method: method, URL: spec.URL, Detail: note}
	}

	timeout := 5 * time.Second
	if spec.Timeout != "" {
		if d, err := time.ParseDuration(spec.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}

	// Try each candidate URL in order. A definitive HTTP response (ok or fail)
	// stops the search; only a transport error (host unreachable) advances to
	// the next candidate. This makes NodePort auto-derivation work regardless
	// of whether the port is published on localhost or the node's InternalIP.
	var last store.ProbeResult
	for i, u := range urls {
		res := probeURL(ctx, method, u, spec, timeout)
		if res.Status == "ok" || res.Status == "fail" {
			return res
		}
		last = res
		if i < len(urls)-1 {
			continue
		}
	}
	return last
}

// probeURL performs a single HTTP check against url. It retries once (after a
// short backoff) on a transport error so a brief not-yet-listening window
// right after a rollout does not immediately fail the probe. A definitive HTTP
// response (any status) is returned without retry.
func probeURL(ctx context.Context, method, url string, spec store.ProbeSpec, timeout time.Duration) store.ProbeResult {
	const maxAttempts = 2
	var lastErr error
	var dur time.Duration
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Declare bodyReader as the io.Reader interface and only assign a
		// concrete *strings.Reader when there is a body. Assigning nil to a
		// typed variable (e.g. bodyReader = nil after strings.NewReader) would
		// yield a (*strings.Reader)(nil) — a non-nil interface holding a nil
		// pointer — which http.NewRequestWithContext then dereferences (v.Len())
		// and panics on. A nil interface is handled correctly by the stdlib.
		var bodyReader io.Reader
		if spec.Body != "" {
			bodyReader = strings.NewReader(spec.Body)
		}
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, method, url, bodyReader)
		if err != nil {
			cancel()
			return store.ProbeResult{Status: "err", Method: method, URL: url, Error: err.Error(), Detail: "构造请求失败"}
		}
		for k, v := range spec.Headers {
			req.Header.Set(k, v)
		}
		start := time.Now()
		resp, err := http.DefaultClient.Do(req)
		dur = time.Since(start)
		cancel()
		if err != nil {
			lastErr = err
			if attempt < maxAttempts && ctx.Err() == nil {
				time.Sleep(2 * time.Second)
				continue
			}
			return store.ProbeResult{Method: method, URL: url, DurationMs: dur.Milliseconds(), Status: "err", Error: err.Error(), Detail: "请求失败: " + err.Error()}
		}
		return parseResponse(method, url, spec, resp, dur)
	}
	if lastErr != nil {
		return store.ProbeResult{Method: method, URL: url, Status: "err", Error: lastErr.Error(), Detail: "请求失败: " + lastErr.Error()}
	}
	return store.ProbeResult{Method: method, URL: url, Status: "err", Detail: "请求失败"}
}

// parseResponse turns an HTTP response into a ProbeResult, applying the
// expected-status / body-contains matching rules.
func parseResponse(method, url string, spec store.ProbeSpec, resp *http.Response, dur time.Duration) store.ProbeResult {
	defer resp.Body.Close()
	res := store.ProbeResult{Method: method, URL: url, DurationMs: dur.Milliseconds(), StatusCode: resp.StatusCode}
	res.Headers = map[string]string{}
	for k, vals := range resp.Header {
		if len(vals) > 0 {
			res.Headers[k] = vals[0]
		}
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	res.Body = string(raw)

	expected := spec.ExpectedStatus
	if expected == 0 {
		expected = 200
	}
	codeOK := resp.StatusCode == expected
	if spec.BodyContains != "" {
		res.Matched = strings.Contains(res.Body, spec.BodyContains)
	} else {
		res.Matched = codeOK
	}

	if res.Matched {
		res.Status = "ok"
		res.Detail = fmt.Sprintf("HTTP %d，%dms", resp.StatusCode, res.DurationMs)
		if spec.BodyContains != "" {
			res.Detail += "，响应包含 " + strconv.Quote(spec.BodyContains)
		}
	} else {
		res.Status = "fail"
		if spec.BodyContains != "" {
			res.Detail = fmt.Sprintf("HTTP %d，%dms，响应未包含 %s", resp.StatusCode, res.DurationMs, strconv.Quote(spec.BodyContains))
		} else {
			res.Detail = fmt.Sprintf("HTTP %d，期望 %d，%dms", resp.StatusCode, expected, res.DurationMs)
		}
	}
	return res
}

// resolveURL returns candidate probe targets. Resolution precedence:
//  1. for ssh deploys, auto-derivation from the resolved connection
//     (CICD_SSH_HOST env / deploy.ssh_host + probe port) — the real host IP
//     lives only in .env, never in the project JSON;
//  2. a per-method override URLs[<deploy method>] (fallback for ssh if no
//     .env host is configured, or for other methods);
//  3. a global manual URL;
//  4. auto-derivation from the deployed Kubernetes Service (cluster methods),
//     which may yield several candidate hosts (see deriveFromService).
//
// When no target can be resolved the returned slice is empty and note explains
// why (the caller turns that into a "skip" rather than a hard failure).
func resolveURL(ctx context.Context, p store.Project, cfg *config.Config, targetName string) ([]string, string) {
	method := p.Deploy.Method
	if method == "ssh" {
		// ssh 部署没有集群 Service，但连接信息（含主机）来自 .env 的
		// CICD_SSH_*，所以探针 URL 优先由解析后的 ssh host + 探针端口推导，
		// 真实主机 IP 不进项目 JSON。仅当 .env 也未配置时才回退到 JSON。
		if urls, _ := deriveFromSSH(p); len(urls) > 0 {
			return urls, ""
		}
	}
	// A per-target probe URL (keyed by the named DeployTarget's name) wins over
	// the per-method URL, so multiple ssh targets (e.g. 腾讯云 vs AWS) can each
	// probe their own address rather than sharing probe.urls["ssh"].
	if tn := strings.TrimSpace(targetName); tn != "" {
		if u := strings.TrimSpace(p.Probe.URLs[tn]); u != "" {
			return []string{u}, ""
		}
	}
	if u := strings.TrimSpace(p.Probe.URLs[method]); u != "" {
		return []string{u}, ""
	}
	if u := strings.TrimSpace(p.Probe.URL); u != "" {
		return []string{u}, ""
	}
	if method == "ssh" {
		return nil, "ssh 部署未配置 ssh_host（也没设置 CICD_SSH_HOST 环境变量），无法推导探测地址；请在 .env 配置 CICD_SSH_HOST，或在 probe.urls.ssh 中手动指定地址"
	}
	if !needsCluster(method) {
		return nil, "未配置 probe.url 且部署方式不涉及集群，无法自动推导服务地址（请手动填写 URL，或在 probe.urls 中按部署方式指定）"
	}
	return deriveFromService(ctx, p, cfg)
}

// deriveFromSSH builds the bare-host probe URL for an ssh deploy from the
// resolved connection (host via CICD_SSH_HOST env or deploy.ssh_host) plus the
// probe port (deploy.ssh_probe_port / CICD_SSH_PROBE_PORT, default 8080). This
// keeps the real host IP out of the project JSON: operators store it only in the
// .env file, and the probe derives its target at run time.
func deriveFromSSH(p store.Project) ([]string, string) {
	spec := store.ResolveSSHSpec(p.Deploy)
	host := strings.TrimSpace(spec.SSHHost)
	if host == "" {
		return nil, "ssh 部署未配置 ssh_host（也没设置 CICD_SSH_HOST 环境变量），无法推导探测地址；请在 .env 配置 CICD_SSH_HOST，或在 probe.urls.ssh 中手动指定地址"
	}
	port := strings.TrimSpace(spec.SSHProbePort)
	if port == "" {
		port = "8080"
	}
	return []string{fmt.Sprintf("http://%s:%s", host, port)}, ""
}

// deriveFromService inspects the Kubernetes Service matching app=<deployment>
// and constructs reachable candidate URLs. NodePort services are reached on the
// node's ExternalIP (remote clusters), else its InternalIP (local VM-backed
// clusters such as OrbStack / Rancher Desktop / Lima, where the NodePort is
// published on the VM IP rather than localhost), else localhost (k3d/kind and
// native k3s). LoadBalancer services use their ingress IP; ClusterIP-only
// services cannot be reached from the host, so we skip with a note.
func deriveFromService(ctx context.Context, p store.Project, cfg *config.Config) ([]string, string) {
	kc := resolveKubeconfig(p.Deploy.Kubeconfig, cfg.Runner.DefaultKubeconfig)
	ns := p.Deploy.Namespace
	if ns == "" {
		ns = "default"
	}
	args := []string{}
	if kc != "" {
		args = append(args, "--kubeconfig", kc)
	}
	args = append(args, "get", "svc", "-n", ns, "-l", "app="+p.Deploy.Deployment, "-o", "json")

	out, err := runKubectl(ctx, args...)
	if err != nil {
		return nil, "自动推导失败（kubectl get svc 出错），请在项目 probe 中手动配置 URL"
	}

	var svc struct {
		Items []struct {
			Spec struct {
				Type  string `json:"type"`
				Ports []struct {
					Port     int `json:"port"`
					NodePort int `json:"nodePort"`
				} `json:"ports"`
			} `json:"spec"`
			Status struct {
				LoadBalancer struct {
					Ingress []struct {
						IP string `json:"ip"`
					} `json:"ingress"`
				} `json:"loadBalancer"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &svc); err != nil || len(svc.Items) == 0 {
		return nil, "未找到匹配 app=" + p.Deploy.Deployment + " 的 Service，请在项目 probe 中手动配置 URL"
	}
	item := svc.Items[0]
	if len(item.Spec.Ports) == 0 {
		return nil, "匹配到的 Service 无端口，无法推导探测地址"
	}
	port := item.Spec.Ports[0].Port
	var hosts []string
	switch {
	case item.Spec.Type == "NodePort" && item.Spec.Ports[0].NodePort != 0:
		// Try every address the NodePort might be published on, in priority
		// order. nodeAddresses returns the ExternalIP (remote clusters) and the
		// InternalIP (local VM clusters); localhost covers k3d/kind/native k3s.
		ext, internal := nodeAddresses(ctx, kc)
		if ext != "" {
			hosts = append(hosts, ext)
		}
		if internal != "" {
			hosts = append(hosts, internal)
		}
		hosts = append(hosts, "localhost")
		port = item.Spec.Ports[0].NodePort
	case item.Spec.Type == "LoadBalancer" && len(item.Status.LoadBalancer.Ingress) > 0 && item.Status.LoadBalancer.Ingress[0].IP != "":
		hosts = append(hosts, item.Status.LoadBalancer.Ingress[0].IP)
	default:
		return nil, "Service 为 " + item.Spec.Type + "（宿主机不可直连），请在项目 probe 中手动配置 URL"
	}

	seen := map[string]bool{}
	var urls []string
	for _, h := range hosts {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		urls = append(urls, fmt.Sprintf("http://%s:%d", h, port))
	}
	if len(urls) == 0 {
		return nil, "未能从 Service 推导出可达地址，请在项目 probe 中手动配置 URL"
	}
	return urls, ""
}

func runKubectl(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// nodeAddresses returns the first ExternalIP and first InternalIP among the
// cluster's nodes. ExternalIP is preferred for remote clusters; InternalIP is
// the address a local VM-backed cluster (OrbStack / Rancher Desktop / Lima)
// publishes its NodePort on. Either may be "" when the node reports none.
func nodeAddresses(ctx context.Context, kc string) (external, internal string) {
	args := []string{}
	if kc != "" {
		args = append(args, "--kubeconfig", kc)
	}
	args = append(args, "get", "nodes", "-o", "json")
	out, err := runKubectl(ctx, args...)
	if err != nil {
		return "", ""
	}
	return firstNodeExternalIP(out), firstNodeInternalIP(out)
}

// firstNodeExternalIP parses `kubectl get nodes -o json` and returns the first
// reported ExternalIP address, skipping the sentinel "<none>". It is a pure
// function so the derivation can be unit-tested without a live cluster.
func firstNodeExternalIP(out string) string {
	var nodes struct {
		Items []struct {
			Status struct {
				Addresses []struct {
					Type  string `json:"type"`
					Addr  string `json:"address"`
				} `json:"addresses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &nodes); err != nil {
		return ""
	}
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type == "ExternalIP" && a.Addr != "" && a.Addr != "<none>" {
				return a.Addr
			}
		}
	}
	return ""
}

// firstNodeInternalIP is the InternalIP counterpart of firstNodeExternalIP,
// used as the NodePort target for local VM-backed clusters.
func firstNodeInternalIP(out string) string {
	var nodes struct {
		Items []struct {
			Status struct {
				Addresses []struct {
					Type  string `json:"type"`
					Addr  string `json:"address"`
				} `json:"addresses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &nodes); err != nil {
		return ""
	}
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type == "InternalIP" && a.Addr != "" && a.Addr != "<none>" {
				return a.Addr
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

func resolveKubeconfig(projectKc, defaultKc string) string {
	if strings.TrimSpace(projectKc) != "" {
		return projectKc
	}
	return defaultKc
}
