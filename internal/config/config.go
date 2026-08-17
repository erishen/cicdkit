package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// Config is the top-level configuration for the CI/CD platform server.
type Config struct {
	Server    ServerConfig `json:"server"`
	DataDir   string       `json:"data_dir"`
	StoreFile string       `json:"store_file"`
	Runner    RunnerConfig `json:"runner"`
}

// ServerConfig controls the HTTP server and its authentication.
//
// APIToken guards every /api/* endpoint (except /api/health and the webhook,
// which has its own secret). It is empty by default, which is only safe because
// Addr defaults to loopback: this platform executes docker/kubectl commands on
// the host, so an unauthenticated listener on a routable address is equivalent
// to remote code execution. Set APIToken whenever Addr is not loopback.
type ServerConfig struct {
	Addr          string   `json:"addr"`
	WebhookSecret string   `json:"webhook_secret"`
	APIToken      string   `json:"api_token"`
	// AutoToken, when true, makes the server generate a one-time random API
	// Token at startup (instead of requiring the operator to set API_TOKEN).
	// The generated token is injected into the served web UI so the local
	// browser authenticates transparently; it is printed to the log for
	// out-of-band access. It is a no-op when APIToken is already set. The token
	// is fixed for the process lifetime (regenerated on each restart).
	//
	// It defaults to true so a solo local user is protected without manual
	// setup — but it is ONLY safe on a loopback bind, because the token is
	// written into the served page. main.go refuses to start on a non-loopback
	// address when the only token is an auto-generated one; use an explicit
	// API_TOKEN for any wider bind.
	AutoToken bool `json:"auto_token"`
	FSRoots   []string `json:"fs_roots"`
}

// EffectiveFSRoots returns the allowed roots for the server-side directory
// browser. If FSRoots is empty it defaults to the server's current working
// directory only — NOT the user's entire home directory — so the directory
// picker cannot be used to enumerate the operator's home tree (which would
// disclose file layout, and combined with an open API is an info-leak). Set
// fs_roots explicitly to grant access to the project directories you build.
func (s ServerConfig) EffectiveFSRoots() []string {
	if len(s.FSRoots) > 0 {
		out := make([]string, 0, len(s.FSRoots))
		for _, r := range s.FSRoots {
			if a, err := filepath.Abs(r); err == nil {
				out = append(out, a)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		return []string{cwd}
	}
	return []string{}
}

// FSPathAllowed reports whether absPath equals one of the roots or lies inside
// one of them. The separator check prevents /home/foo from matching /home/foobar.
func (s ServerConfig) FSPathAllowed(absPath string) bool {
	for _, root := range s.EffectiveFSRoots() {
		if absPath == root {
			return true
		}
		if strings.HasPrefix(absPath, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// IsLoopback reports whether Addr binds only to the local machine.
func (s ServerConfig) IsLoopback() bool {
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		// Forms like ":8080" have an empty host, which means "all interfaces".
		host = strings.TrimSuffix(s.Addr, ":")
		if i := strings.LastIndex(s.Addr, ":"); i >= 0 {
			host = s.Addr[:i]
		}
	}
	if host == "" {
		return false // ":8080" listens on every interface
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// RunnerConfig controls concurrency and the toolchain environment used by
// build/deploy engines.
type RunnerConfig struct {
	MaxConcurrent     int    `json:"max_concurrent"`
	DockerHost        string `json:"docker_host"`
	DefaultKubeconfig string `json:"default_kubeconfig"`
	WorkDir           string `json:"work_dir"`
}

// Default returns a configuration with sensible built-in defaults.
func Default() *Config {
	return &Config{
	// Loopback by default: this server runs docker/kubectl on the host, so
	// exposing it without a token would hand out command execution.
	// AutoToken is on so a solo local user is protected without manual setup;
	// a non-loopback bind still requires an explicit API_TOKEN (see main.go).
	Server:    ServerConfig{Addr: "127.0.0.1:8080", AutoToken: true},
		DataDir:   "./data",
		StoreFile: "./data/store.json",
		Runner:    RunnerConfig{MaxConcurrent: 2},
	}
}

// Load reads the JSON config file at path (if it exists) and overlays
// environment variables. A missing file is not an error: defaults are used.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return cfg, nil
			}
			return nil, err
		}
		if err := json.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("解析配置文件失败: %w", err)
		}
	}
	applyEnv(cfg)
	if cfg.DataDir == "" {
		cfg.DataDir = "./data"
	}
	if cfg.StoreFile == "" {
		cfg.StoreFile = filepath.Join(cfg.DataDir, "store.json")
	}
	if cfg.Runner.MaxConcurrent <= 0 {
		cfg.Runner.MaxConcurrent = 2
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("PORT"); v != "" {
		cfg.Server.Addr = ":" + v
	}
	if v := os.Getenv("SERVER_ADDR"); v != "" {
		cfg.Server.Addr = v
	}
	if v := os.Getenv("DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("STORE_FILE"); v != "" {
		cfg.StoreFile = v
	}
	if v := os.Getenv("KUBECONFIG"); v != "" {
		cfg.Runner.DefaultKubeconfig = v
	}
	if v := os.Getenv("DOCKER_HOST"); v != "" {
		cfg.Runner.DockerHost = v
	}
	if v := os.Getenv("WEBHOOK_SECRET"); v != "" {
		cfg.Server.WebhookSecret = v
	}
	if v := os.Getenv("API_TOKEN"); v != "" {
		cfg.Server.APIToken = v
	}
	if v := os.Getenv("AUTO_TOKEN"); v != "" {
		cfg.Server.AutoToken = isTrue(v)
	}
	if v := os.Getenv("MAX_CONCURRENT"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.Runner.MaxConcurrent = n
		}
	}
	if v := os.Getenv("CICD_FS_ROOTS"); v != "" {
		cfg.Server.FSRoots = splitList(v)
	}
}

// splitList splits a comma-separated env value into non-empty, trimmed parts.
func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isTrue interprets a loosely-formatted boolean env value (1/true/yes/on,
// case-insensitive). Anything else is treated as false.
func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y", "t":
		return true
	default:
		return false
	}
}

// GenerateToken returns a cryptographically random hex token suitable for use
// as an API Token. 32 random bytes produce a 64-character hex string.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Save writes the configuration as indented JSON (used by `server -init`).
func (c *Config) Save(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
