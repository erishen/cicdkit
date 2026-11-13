// Package a2a 实现 cicdkit 对 lume 集群的 A2A 接入：
//
//   - A2A 标准面：GET /.well-known/agent-card.json（公开发现）+ POST /a2a
//     （JSON-RPC：message/send / tasks/get / tasks/cancel），任务落盘可追溯；
//   - peer 签名面：GET /peer/health + POST /peer/invoke + GET /peer/tools，
//     HMAC-SHA256 认证，协议与 lume-core/lib/peer.lume 字节级一致。
//
// 设计原则与 lume-a2a / tsm-hub 同源：
//   * 凭据缺失 = 整面关闭（fail-closed），绝不放行；
//   * 不认识的 capability 不猜、不兜底转发 → 回 rejected / input-required；
//   * 签名串、scope 拼接、密钥派生公式**一字不改** —— 任何一侧改动都会让
//     全舰队同时 401，表现为"密钥配错"，排查成本极高。
package a2a

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode 是实际生效的签名凭据方案。优先级 derived > shared > off。
type Mode string

const (
	ModeOff     Mode = "off"     // 未配任何凭据，peer 面关闭
	ModeShared  Mode = "shared"  // PEER_SHARED 单密钥（无法防冒充）
	ModeDerived Mode = "derived" // PEER_ROOT + 按节点派生（防冒充）
)

// Peer 持有签名凭据与节点注册表。
type Peer struct {
	Name     string // 本节点名（出站签名用）
	shared   string // CICDKIT_PEER_SHARED
	root     string // CICDKIT_PEER_ROOT
	registry string // registry 文件路径
	skewSecs int    // 时间窗（默认 120）

	mu       sync.RWMutex
	known    map[string]bool
	regMTime time.Time
	regErr   error
}

type registryFile struct {
	Peers []string `json:"peers"`
}

// New 创建 Peer。skewSecs <= 0 时用默认 120。
func New(name, shared, root, registry string, skewSecs int) *Peer {
	if skewSecs <= 0 {
		skewSecs = 120
	}
	p := &Peer{
		Name:     name,
		shared:   shared,
		root:     root,
		registry: registry,
		skewSecs: skewSecs,
		known:    map[string]bool{},
	}
	p.reload()
	return p
}

// Mode 返回当前生效的凭据方案。
func (p *Peer) Mode() Mode {
	if p.root != "" {
		return ModeDerived
	}
	if p.shared != "" {
		return ModeShared
	}
	return ModeOff
}

// Enabled 报告 peer 签名面是否开启。
func (p *Peer) Enabled() bool { return p.Mode() != ModeOff }

// reload 在注册表文件 mtime 变化时重读。
func (p *Peer) reload() {
	if p.registry == "" {
		return
	}
	fi, err := os.Stat(p.registry)
	if err != nil {
		p.mu.Lock()
		p.regErr = err
		p.mu.Unlock()
		return
	}
	p.mu.RLock()
	same := p.regMTime.Equal(fi.ModTime())
	p.mu.RUnlock()
	if same {
		return
	}
	raw, err := os.ReadFile(p.registry)
	if err != nil {
		p.mu.Lock()
		p.regErr = err
		p.mu.Unlock()
		return
	}
	var rf registryFile
	if err := json.Unmarshal(raw, &rf); err != nil {
		p.mu.Lock()
		p.regErr = err
		p.mu.Unlock()
		return
	}
	known := make(map[string]bool, len(rf.Peers))
	for _, n := range rf.Peers {
		if n = strings.TrimSpace(n); n != "" {
			known[n] = true
		}
	}
	p.mu.Lock()
	p.known = known
	p.regMTime = fi.ModTime()
	p.regErr = nil
	p.mu.Unlock()
}

// Known 报告某节点是否在注册表里。
func (p *Peer) Known(node string) bool {
	p.reload()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.known[node]
}

// registryWarning 返回注册表加载问题。
func (p *Peer) registryWarning() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.regErr != nil {
		return "peer registry unreadable: " + p.regErr.Error()
	}
	return ""
}

// deriveKey: peer_key(node) = hmac_sha256(PEER_ROOT, "peer:" + node)
func (p *Peer) deriveKey(node string) string {
	m := hmac.New(sha256.New, []byte(p.root))
	m.Write([]byte("peer:" + node))
	return hex.EncodeToString(m.Sum(nil))
}

// SigningString 与 peer.lume 一致。
func SigningString(node, ts, nonce, scope string) string {
	return node + "\n" + ts + "\n" + nonce + "\n" + scope
}

func scopeGET(path string) string         { return "GET " + path }
func scopePOST(path, body string) string  { return "POST " + path + "\n" + body }

type verifyResult struct {
	OK   bool
	Err  string
	Node string
}

// Verify 校验一个带签名 query（node/ts/nonce/sig）的请求。
func (p *Peer) Verify(r *http.Request, method, scope string) verifyResult {
	mode := p.Mode()
	if mode == ModeOff {
		return verifyResult{Err: "peer disabled: CICDKIT_PEER_SHARED / CICDKIT_PEER_ROOT not set"}
	}
	q := r.URL.Query()
	node := q.Get("node")
	ts := q.Get("ts")
	nonce := q.Get("nonce")
	sig := q.Get("sig")
	if node == "" || ts == "" || nonce == "" || sig == "" {
		return verifyResult{Err: "missing auth params (node/ts/nonce/sig)"}
	}
	tsI, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || tsI <= 0 {
		return verifyResult{Err: "bad ts"}
	}
	drift := time.Now().Unix() - tsI
	if drift < 0 {
		drift = -drift
	}
	if int(drift) > p.skewSecs {
		return verifyResult{Err: fmt.Sprintf("ts outside window (%ds)", drift)}
	}
	var key string
	switch mode {
	case ModeDerived:
		if !p.Known(node) {
			return verifyResult{Err: "unknown peer '" + node + "' (not in registry)"}
		}
		key = p.deriveKey(node)
	default:
		key = p.shared
	}
	expect := signWith(key, node, ts, nonce, scope)
	if !constEq(expect, sig) {
		return verifyResult{Err: "bad signature for peer '" + node + "'"}
	}
	return verifyResult{OK: true, Node: node}
}

func signWith(key, node, ts, nonce, scope string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(SigningString(node, ts, nonce, scope)))
	return hex.EncodeToString(m.Sum(nil))
}

func constEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return hmac.Equal([]byte(a), []byte(b))
}

// WriteRegistry 在首次部署时生成注册表文件（幂等）。
func WriteRegistry(path string, peers []string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	rf := registryFile{Peers: peers}
	raw, err := json.MarshalIndent(rf, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

var errInvalidRegistryFormat = errors.New("peer registry must be JSON with a \"peers\" array")
