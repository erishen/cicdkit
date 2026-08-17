// Package llm provides a tiny OpenAI-compatible chat client used to turn build
// / deploy failure logs into a human-readable diagnosis. It is intentionally
// optional: the rest of the platform never imports it at call time unless the
// operator has configured (and enabled) an endpoint in the settings page.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the operator-supplied LLM connection settings. It is persisted to
// data/llm.json (gitignored) and editable from the UI settings page. The API
// key is sensitive: GET returns a masked view and the file is written 0600.
type Config struct {
	Enabled  bool   `json:"enabled"`
	BaseURL  string `json:"base_url"` // e.g. https://api.openai.com/v1
	APIKey   string `json:"api_key"`
	Model    string `json:"model"`
	System   string `json:"system,omitempty"` // optional custom system prompt
}

// defaultSystem focuses the model on actionable CI/CD failure analysis.
const defaultSystem = `你是一名资深的 CI/CD 与云原生运维专家。用户会贴出一次构建或部署失败的执行日志。
请分析日志，用简体中文给出：
1. 根本原因（一句话定位问题类别，如 Docker 构建失败 / 镜像拉取失败 / kubectl 连接或清单错误 / 平台架构不匹配 / 服务探测未通过 等）；
2. 关键证据（引用日志中最相关的 1-3 行，保留原始报错关键字）；
3. 可操作的修复步骤（按优先级列出，给出具体命令或配置改动建议）。
不要复述全部日志，聚焦可执行的结论。若日志不足以判断，明确说明还缺什么信息。`

// LLM_* environment variable names. They let operators keep the connection
// settings (endpoint / key / model / switch) in .env (or the real process
// environment) instead of the UI, while the UI still shows and can override
// them. The system prompt is intentionally NOT here: it is the built-in
// defaultSystem constant in this file and is the single source of truth.
const (
	envLLMEnabled = "CICD_LLM_ENABLED"
	envLLMBaseURL = "CICD_LLM_BASE_URL"
	envLLMAPIKey  = "CICD_LLM_API_KEY"
	envLLMModel   = "CICD_LLM_MODEL"
)

// Load reads the LLM config from path. A missing file is not an error: it
// returns a zero (disabled) config so the UI can offer first-time setup. In
// either case, after (optionally) reading the file, any empty field is filled
// from the CICD_LLM_* environment variables, so values configured in .env
// surface in the UI by default — including on a fresh install where no
// data/llm.json exists yet.
func Load(path string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			applyEnvFallback(c) // first run: fill from .env so the UI shows it
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return c, err
	}
	applyEnvFallback(c)
	return c, nil
}

// applyEnvFallback fills empty fields of c from CICD_LLM_* environment
// variables. A value already persisted in llm.json (or entered in the UI) wins;
// the environment only supplies defaults when a field is still empty. The
// system prompt is never read from the environment — it always comes from the
// built-in defaultSystem constant (or an explicit UI override).
func applyEnvFallback(c *Config) {
	if c.BaseURL == "" {
		c.BaseURL = strings.TrimSpace(os.Getenv(envLLMBaseURL))
	}
	if c.APIKey == "" {
		c.APIKey = strings.TrimSpace(os.Getenv(envLLMAPIKey))
	}
	if c.Model == "" {
		c.Model = strings.TrimSpace(os.Getenv(envLLMModel))
	}
	if !c.Enabled {
		if v := strings.TrimSpace(os.Getenv(envLLMEnabled)); v != "" {
			if strings.EqualFold(v, "true") || v == "1" {
				c.Enabled = true
			}
		}
	}
}

// Save writes the config as indented JSON with restrictive permissions.
func Save(path string, c *Config) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Masked returns a view safe to send to the browser: the API key value is
// never exposed, only whether one is set. default_system carries the built-in
// template so the settings UI can pre-fill the system-prompt field.
func (c Config) Masked() map[string]interface{} {
	return map[string]interface{}{
		"enabled":      c.Enabled,
		"base_url":     c.BaseURL,
		"model":        c.Model,
		"system":       c.System,
		"default_system": defaultSystem,
		"api_key_set":  c.APIKey != "",
	}
}

// Client calls an OpenAI-compatible /chat/completions endpoint.
type Client struct {
	cfg    Config
	client *http.Client
}

// NewClient builds a client with a generous timeout (model calls can be slow).
func NewClient(c Config) *Client {
	return &Client{cfg: c, client: &http.Client{Timeout: 90 * time.Second}}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Diagnose sends the failure logs to the configured chat model and returns the
// analysis text. It fails fast (4xx/5xx mapped to a plain error) when the
// config is incomplete or the endpoint is unreachable, so the UI can surface a
// clear message instead of a raw stack trace.
func (c *Client) Diagnose(ctx context.Context, logs string) (string, error) {
	system := c.cfg.System
	if strings.TrimSpace(system) == "" {
		system = defaultSystem
	}
	return c.chat(ctx, system, "以下是一次失败的 CI/CD 运行日志：\n\n"+logs)
}

// Test makes a minimal round-trip to confirm the endpoint, key, and model are
// reachable and valid. It is used by the settings page's "测试连接" button so
// operators learn about misconfiguration before they trigger a real failure.
func (c *Client) Test(ctx context.Context) (string, error) {
	return c.chat(ctx, "You are a helpful assistant. Reply concisely and in English.", "Please reply with exactly the word: ok")
}

// chat performs one /chat/completions call and returns the assistant's text.
func (c *Client) chat(ctx context.Context, system, user string) (string, error) {
	if c.cfg.BaseURL == "" || c.cfg.APIKey == "" || c.cfg.Model == "" {
		return "", fmt.Errorf("LLM 未正确配置（需 base_url / api_key / model）")
	}
	reqBody := chatRequest{
		Model: c.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("调用 LLM 接口失败: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取 LLM 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM 接口返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	var cr chatResponse
	if err := json.Unmarshal(rb, &cr); err != nil {
		return "", fmt.Errorf("解析 LLM 响应失败: %w", err)
	}
	if cr.Error != nil && cr.Error.Message != "" {
		return "", fmt.Errorf("LLM 错误: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("LLM 未返回内容")
	}
	return strings.TrimSpace(cr.Choices[0].Message.Content), nil
}
