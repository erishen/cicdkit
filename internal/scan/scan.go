// Package scan derives a best-effort cicdkit Project configuration from a
// local source directory (or, for browser-driven directory pickers, from an
// in-memory map of relative file paths → contents). It is intentionally
// heuristic and never fails hard: whatever it cannot determine is left empty
// and reported in the returned notes so the user can fill it in the form.
package scan

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// maxScanFiles caps how many files we read from a directory.
const maxScanFiles = 200

// maxFileBytes caps the size of any single file we read.
const maxFileBytes = 256 * 1024

// skipDirs are directory basenames we never descend into.
var skipDirs = map[string]bool{
	"node_modules": true, ".git": true, "vendor": true, "dist": true,
	"build": true, "target": true, ".next": true, ".turbo": true,
	".idea": true, ".cache": true,
}

// Draft is the outcome of a scan: a prefilled Project (not yet persisted) plus
// human-readable notes describing what was detected or assumed.
type Draft struct {
	Project *ProjectView `json:"project"`
	Notes   []string     `json:"notes"`
}

// ProjectView mirrors store.Project's JSON shape so the frontend can feed it
// straight into the existing create-project form. We avoid importing the store
// package to keep scan dependency-free and trivially testable.
type ProjectView struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Repository  string            `json:"repository"`
	Branch      string            `json:"branch"`
	Build       BuildView         `json:"build"`
	Deploy      DeployView        `json:"deploy"`
	Probe       ProbeView         `json:"probe"`
}

type BuildView struct {
	Context     string            `json:"context"`
	Dockerfile  string            `json:"dockerfile"`
	ImageRepo   string            `json:"image_repo"`
	TagStrategy string            `json:"tag_strategy"`
	Push        bool              `json:"push"`
	BuildArgs   map[string]string `json:"build_args,omitempty"`
}

type DeployView struct {
	Method        string `json:"method"`
	Kubeconfig    string `json:"kubeconfig"`
	Namespace     string `json:"namespace"`
	ManifestPath  string `json:"manifest_path"`
	Deployment    string `json:"deployment"`
	Container     string `json:"container"`
	K3sImportCmd  string `json:"k3s_import_cmd"`
	SSHHost       string `json:"ssh_host"`
	SSHUser       string `json:"ssh_user"`
	SSHPort       string `json:"ssh_port"`
	SSHKeyPath    string `json:"ssh_key_path"`
	SSHContainer  string `json:"ssh_container"`
	SSHRunArgs    string `json:"ssh_run_args"`
	SSHTransfer   bool   `json:"ssh_transfer"`
}

type ProbeView struct {
	Enabled        bool              `json:"enabled"`
	Method         string            `json:"method"`
	URL            string            `json:"url"`
	URLs           map[string]string `json:"urls,omitempty"`
	ExpectedStatus int               `json:"expected_status"`
	Timeout        string            `json:"timeout"`
}

// ScanPath walks root on disk and returns a draft. root must be an existing
// directory. Detected paths (build context, dockerfile, manifest) are stored as
// absolute paths so they resolve correctly from the server's working dir.
func ScanPath(root string) (*Draft, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("目录不存在: %s", root)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("不是目录: %s", root)
	}
	files, err := walkDir(root)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(strings.TrimRight(root, string(os.PathSeparator)))
	p, notes := detect(base, files)
	// In path mode the build context is the real, absolute directory and the
	// dockerfile / manifest paths are absolute too.
	p.Build.Context = root
	if p.Deploy.ManifestPath != "" {
		p.Deploy.ManifestPath = filepath.Join(root, p.Deploy.ManifestPath)
	}
	if p.Build.Dockerfile != "" && !filepath.IsAbs(p.Build.Dockerfile) {
		p.Build.Dockerfile = filepath.Join(root, p.Build.Dockerfile)
	}
	return &Draft{Project: p, Notes: notes}, nil
}

// ScanFiles derives a draft from an in-memory file map (relative path → content).
// A browser directory picker exposes only the folder name, never the real
// absolute path, so we must NOT guess a relative context (e.g. the bare folder
// name) — docker build <name> would then resolve against the server's working
// directory and fail with "path not found". We leave the context empty so the
// form can force the user to enter the server-visible absolute path.
func ScanFiles(root string, files map[string]string) *Draft {
	base := root
	if i := strings.LastIndexAny(root, "/\\"); i >= 0 {
		base = root[i+1:]
	}
	p, notes := detect(base, files)
	if !filepath.IsAbs(p.Build.Context) {
		p.Build.Context = ""
	}
	notes = append(notes, "⚠️ 浏览器目录选择器无法获取真实路径：构建上下文必须是服务器实际可访问的绝对路径（如 /Users/you/code/"+base+"），Dockerfile / 清单路径也需改为绝对路径，否则构建会报『path not found』。保存前请务必补全。")
	return &Draft{Project: p, Notes: notes}
}

// walkDir collects whitelisted files under root into a relativePath→content map.
func walkDir(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			// Bound recursion depth to avoid runaway scans.
			if depth(root, path) > 6 {
				return filepath.SkipDir
			}
			return nil
		}
		if !interesting(d.Name()) {
			return nil
		}
		if len(out) >= maxScanFiles {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil // skip unreadable files
		}
		if len(b) > maxFileBytes {
			b = b[:maxFileBytes]
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func depth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return 99
	}
	if rel == "." {
		return 0
	}
	return strings.Count(rel, string(os.PathSeparator)) + 1
}

func interesting(name string) bool {
	lower := strings.ToLower(name)
	if lower == "dockerfile" || strings.HasPrefix(lower, "dockerfile.") {
		return true
	}
	if strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml") {
		return true
	}
	switch lower {
	case "package.json", "go.mod", "requirements.txt", "pyproject.toml",
		"pipfile", "cargo.toml", "pom.xml", "build.gradle",
		"build.gradle.kts", "composer.json":
		return true
	}
	return false
}

// detect runs the heuristic over the relativePath→content map.
func detect(base string, files map[string]string) (*ProjectView, []string) {
	notes := []string{}
	p := &ProjectView{
		Name:   base,
		Build:  BuildView{Context: ".", Dockerfile: "Dockerfile", TagStrategy: "timestamp"},
		Deploy: DeployView{Method: "local-k3s"},
	}
	rels := make([]string, 0, len(files))
	for k := range files {
		rels = append(rels, k)
	}
	sort.Strings(rels)

	// Dockerfile.
	dfRel, dfContent := findDockerfile(files, rels)
	if dfRel != "" {
		p.Build.Dockerfile = dfRel
		p.Build.Context = dirOf(dfRel)
		if port := firstExpose(dfContent); port != "" {
			notes = append(notes, "Dockerfile 检测到 EXPOSE "+port+"，建议把该端口用于服务探测（k8s Service 端口优先）。")
		}
		if img := dockerFromImageStage(dfContent); img != "" {
			notes = append(notes, "Dockerfile 基础镜像: "+img)
		}
	} else {
		notes = append(notes, "未发现 Dockerfile，请手动填写构建配置，或先将 Dockerfile 放入该目录。")
	}

	// Language / image repo default.
	if name, ok := manifestAppName(files, rels); ok && name != "" {
		p.Build.ImageRepo = name
		// The project name follows the manifest app name (e.g. package.json
		// "name" / go.mod module leaf) when present; otherwise it stays the
		// directory basename (set at init).
		p.Name = name
	} else {
		p.Build.ImageRepo = base
	}

	// k8s manifests.
	manifestRels := filterManifests(rels)
	if len(manifestRels) > 0 {
		var sb strings.Builder
		for _, r := range manifestRels {
			sb.WriteString(files[r])
			sb.WriteString("\n---\n")
		}
		info := extractK8s(sb.String())
		if info.hasK8s {
			p.Deploy.Method = "local-k3s"
			p.Deploy.ManifestPath = manifestRels[0]
			if info.deployment != "" {
				p.Deploy.Deployment = info.deployment
			}
			if info.container != "" {
				p.Deploy.Container = info.container
			}
			if info.namespace != "" {
				p.Deploy.Namespace = info.namespace
			}
			if info.image != "" {
				if repo := stripTag(info.image); repo != "" {
					p.Build.ImageRepo = repo
				}
			}
			notes = append(notes, "检测到 k8s 清单（"+
				strings.Join(nonEmpty(info.deployment, info.container, info.svcType), ", ")+
				"），部署方式设为 local-k3s。")
			// Probe auto-derivation works for cluster Services.
			if info.svcType == "NodePort" || info.svcType == "LoadBalancer" || info.svcType == "ClusterIP" {
				p.Probe.Enabled = true
				p.Probe.Method = "GET"
				p.Probe.ExpectedStatus = 200
				p.Probe.Timeout = "5s"
				// Empty URL → probe derives from the Service automatically.
				switch info.svcType {
				case "NodePort":
					notes = append(notes, "Service 为 NodePort，探针 URL 留空将自动从节点地址推导（本机集群走 InternalIP/localhost）。")
				case "LoadBalancer":
					notes = append(notes, "Service 为 LoadBalancer，探针 URL 留空将自动从 ingress IP 推导。")
				default:
					notes = append(notes, "Service 为 ClusterIP，宿主机不可直连；若探针需可达，请在表单手动填写 URL。")
				}
			}
		}
	} else if dfRel != "" {
		// No k8s but we have a Dockerfile: bare-docker (ssh) is the natural fit.
		p.Deploy.Method = "ssh"
		p.Deploy.SSHTransfer = true
		p.Deploy.SSHContainer = base
		p.Deploy.SSHRunArgs = "-p 8080:8080 --restart unless-stopped"
		notes = append(notes, "未发现 k8s 清单，仅有 Dockerfile：默认部署方式设为 ssh（裸机 Docker 免仓库直传）。请在表单填写目标主机（或放 .env 的 CICD_SSH_*），并确认 ssh_run_args 的端口映射。")
	}

	return p, notes
}

func nonEmpty(vals ...string) []string {
	out := []string{}
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func dirOf(rel string) string {
	if i := strings.LastIndexAny(rel, "/\\"); i >= 0 {
		return rel[:i]
	}
	return "."
}

func findDockerfile(files map[string]string, rels []string) (string, string) {
	for _, r := range rels {
		base := strings.ToLower(filepath.Base(r))
		if base == "dockerfile" || strings.HasPrefix(base, "dockerfile.") {
			return r, files[r]
		}
	}
	return "", ""
}

func filterManifests(rels []string) []string {
	out := []string{}
	for _, r := range rels {
		lower := strings.ToLower(r)
		if strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml") {
			out = append(out, r)
		}
	}
	return out
}

func manifestAppName(files map[string]string, rels []string) (string, bool) {
	for _, r := range rels {
		lower := strings.ToLower(r)
		switch {
		case lower == "package.json":
			if n := jsonField(files[r], "name"); n != "" {
				return n, true
			}
		case lower == "go.mod":
			if m := reFind(`(?m)^\s*module\s+(\S+)`, files[r]); m != "" {
				if i := strings.LastIndex(m, "/"); i >= 0 {
					return m[i+1:], true
				}
				return m, true
			}
		}
	}
	return "", false
}

func jsonField(s, key string) string {
	// Minimal, dependency-free extraction: "key" : "value" (string) or "key": "value".
	re := regexp.MustCompile("(?m)\"" + regexp.QuoteMeta(key) + "\"\\s*:\\s*\"([^\"]*)\"")
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

func firstExpose(dockerfile string) string {
	return reFind(`(?m)^\s*EXPOSE\s+(\d+)`, dockerfile)
}

func dockerFromImageStage(dockerfile string) string {
	// First FROM line (skip ARG-driven multi-stage lookups for simplicity).
	return reFind(`(?m)^\s*FROM\s+(\S+)`, dockerfile)
}

func stripTag(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[:i]
	}
	return image
}

func reFind(re, s string) string {
	reCompiled := regexp.MustCompile(re)
	if m := reCompiled.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// k8sInfo holds the fields we pull out of the manifests.
type k8sInfo struct {
	hasK8s    bool
	deployment string
	container  string
	namespace  string
	svcType    string
	image      string
}

// extractK8s scans concatenated YAML (docs separated by ---) for the fields the
// platform needs. It uses tolerant, indentation-aware regexes rather than a full
// YAML parser: the manifests this tool generates and the common cases it meets
// are simple enough that this avoids a heavy dependency while never panicking.
func extractK8s(yaml string) k8sInfo {
	info := k8sInfo{}
	docs := strings.Split(yaml, "---")
	for _, doc := range docs {
		kind := reFind(`(?m)^\s*kind:\s*(\S+)`, doc)
		switch kind {
		case "Deployment":
			info.hasK8s = true
			if v := reFind(`(?m)^[ \t]{2}name:\s*(\S+)`, doc); v != "" {
				info.deployment = v
			}
			if v := reFind(`(?m)^[ \t]*-[ \t]*name:\s*(\S+)`, doc); v != "" {
				info.container = v
			}
			if v := reFind(`(?m)^\s*image:\s*(\S+)`, doc); v != "" {
				info.image = v
			}
		case "Service":
			info.hasK8s = true
			if v := reFind(`(?m)^\s*type:\s*(\S+)`, doc); v != "" {
				info.svcType = v
			}
			if v := reFind(`(?m)^[ \t]{2}namespace:\s*(\S+)`, doc); v != "" {
				info.namespace = v
			}
		}
	}
	return info
}
