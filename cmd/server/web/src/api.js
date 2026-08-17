// 与后端 REST API 通信的封装。所有路径相对根 "/"，由 Go 服务与（开发时）Vite 代理统一提供。
//
// 当服务端配置了 API Token（API_TOKEN 环境变量）时，请求需要带上该 Token。
// 这里的做法是：首次收到 401 就提示输入，存入 localStorage，后续自动附带。
const TOKEN_KEY = 'cicd_api_token'

// 归一化 token：去掉首尾空白与可选的引号。.env 里 API_TOKEN 常写成
// "xxx"（带双引号），后端 LoadDotEnv 会自动去引号，但用户从 .env 复制时
// 常把引号一起粘进浏览器弹框，导致发到后端的 X-API-Token 带引号、与后端
// 期望的不带引号 token 长度不一致而恒为 401。这里读写两侧都归一化，避免该坑。
function normalizeToken(t) {
  if (!t) return ''
  const s = String(t).trim()
  return s.replace(/^["']|["']$/g, '')
}
function readToken() {
  try { return normalizeToken(localStorage.getItem(TOKEN_KEY) || '') } catch { return '' }
}
function writeToken(t) {
  try { localStorage.setItem(TOKEN_KEY, normalizeToken(t)) } catch { /* 隐私模式下忽略 */ }
}
export function clearToken() {
  try { localStorage.removeItem(TOKEN_KEY) } catch { /* 忽略 */ }
}

function withAuth(headers) {
  const h = { ...(headers || {}) }
  const t = readToken()
  if (t) h['X-API-Token'] = t
  return h
}

// AUTO_TOKEN: the server can inject a one-time token into the page as
// window.__CICD_API_TOKEN__ (only when it auto-generated one). Persist it so
// withAuth picks it up automatically — the local browser authenticates with no
// manual input. An explicitly configured API_TOKEN is never injected, so this
// stays a no-op in that case. The 401 prompt below remains the fallback.
if (window.__CICD_API_TOKEN__) {
  writeToken(window.__CICD_API_TOKEN__)
}

// request 统一处理鉴权、错误状态与 JSON 解析。
// 注意：之前的 GET 封装不检查 response.ok，出错时会把 {error:...} 当成正常数据
// 返回，导致列表静默变空；这里统一抛错。
// 避免多个并发请求同时 401 时连环弹框：同一时刻只允许一个 prompt 在途。
let tokenPrompting = false

async function request(url, opts = {}, retried = false) {
  const r = await fetch(url, { ...opts, headers: withAuth(opts.headers) })
  if (r.status === 401) {
    // 已重试过、或已有 prompt 在途（其他并发请求先触发了），不再弹框，直接失败，
    // 否则首页 5 个并发请求会连环弹「请输入 Token」框，体验极差。
    if (retried || tokenPrompting) {
      throw new Error('API Token 无效或缺失')
    }
    tokenPrompting = true
    try {
      // 预填当前已存的 token，方便用户核对/直接确认；取消或留空则清空失效缓存，
      // 避免「脏 token 残留 localStorage → 每次请求都 401 却不知为何」的死循环。
      const t = window.prompt('服务端已启用 API Token，请输入 Token：', readToken())
      if (!t) {
        clearToken()
        throw new Error('已取消 Token 输入，本地缓存已清空')
      }
      writeToken(t)
      return request(url, opts, true)
    } finally {
      tokenPrompting = false
    }
  }
  if (!r.ok) {
    let msg = r.statusText
    try { msg = (await r.json()).error || msg } catch { /* 非 JSON 响应 */ }
    throw new Error(msg)
  }
  return r.json()
}

const jsonBody = (b) => ({
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(b),
})

// pageQuery builds a query string from pagination options:
//   { project_id, limit, offset } -> "?project_id=..&limit=..&offset=.."
// Omitted/empty keys are left out entirely.
const pageQuery = (opts) => {
  const o = opts || {}
  const p = new URLSearchParams()
  if (o.project_id) p.set('project_id', o.project_id)
  if (o.q) p.set('q', o.q)
  if (o.limit) p.set('limit', String(o.limit))
  // 用 != null 而非 truthy 判断，否则 offset=0（首屏）会被错误跳过，
  // 导致请求 URL 缺 offset 参数，后端虽会用默认 0 兜底、但行为不显式、调试不友好。
  if (o.offset != null) p.set('offset', String(o.offset))
  const s = p.toString()
  return s ? '?' + s : ''
}

const API = {
  health: () => request('/api/health'),
  version: () => request('/api/version'),
  // 鉴权探针：单独、轻量地验证 token 是否有效。App 启动时先调它，通过后再并发加载
  // 各列表，避免首页多个请求同时 401 而连环弹「请输入 Token」框。
  authProbe: () => request('/api/projects?limit=1'),
  listProjects: (opts) => request('/api/projects' + pageQuery(opts)),
  createProject: (p) => request('/api/projects', { method: 'POST', ...jsonBody(p) }),
  updateProject: (id, p) => request('/api/projects/' + id, { method: 'PUT', ...jsonBody(p) }),
  // 服务端目录浏览：列出允许范围内的根目录，以及某目录下的直接条目。
  fsRoots: () => request('/api/fs/roots'),
  fsList: (dir) => request('/api/fs/list?dir=' + encodeURIComponent(dir)),
  // 从目录探测：传 {path}（服务器扫描本地目录，路径来自服务端目录浏览，保证服务器可访问）。
  scanProject: (body) => request('/api/projects/scan', { method: 'POST', ...jsonBody(body) }),
  deleteProject: (id) => request('/api/projects/' + id, { method: 'DELETE' }),
  trigger: (id, action, body) =>
    request('/api/projects/' + id + '/' + action, { method: 'POST', ...jsonBody(body || {}) }),
  listRuns: (opts) => request('/api/runs' + pageQuery(opts)),
  getRun: (id) => request('/api/runs/' + id),
  cancelRun: (id) => request('/api/runs/' + id + '/cancel', { method: 'POST', ...jsonBody({}) }),
  // 一键清空（带确认在前端）：DELETE 会清空全局历史，不可撤销。
  clearRuns: () => request('/api/runs', { method: 'DELETE' }),
  clearDeployments: () => request('/api/deployments', { method: 'DELETE' }),
  validate: (id) => request('/api/projects/' + id + '/validate', { method: 'POST', ...jsonBody({}) }),
  probe: (id) => request('/api/projects/' + id + '/probe', { method: 'POST', ...jsonBody({}) }),
  // 生成缺失文件：GET 预览（不落盘），POST 写入 build.context 并更新项目配置。
  generatePlan: (id) => request('/api/projects/' + id + '/generate', { method: 'GET' }),
  generateApply: (id) => request('/api/projects/' + id + '/generate', { method: 'POST', ...jsonBody({}) }),
  listDeployments: (opts) => request('/api/deployments' + pageQuery(opts)),
  // ---- 可选 LLM 智能诊断 ----
  // 读取（掩码，不含明文 key）/ 保存 LLM 配置。
  llmConfig: () => request('/api/config/llm'),
  saveLlmConfig: (c) => request('/api/config/llm', { method: 'PUT', ...jsonBody(c) }),
  // 用当前填写（或已保存）的配置做一次最小连通性测试，返回模型回包。
  testLlm: (c) => request('/api/config/llm/test', { method: 'POST', ...jsonBody(c) }),
  // 读取全局 SSH 连接默认值（来自 .env 的 CICD_SSH_*），供「一键导入腾讯云目标」使用。
  sshDefaults: () => request('/api/config/ssh-defaults'),
  // 对一次失败的运行 / 部署发起 AI 诊断，返回模型分析文本。
  diagnoseRun: (id) => request('/api/runs/' + id + '/diagnose', { method: 'POST', ...jsonBody({}) }),
  diagnoseDeployment: (id) => request('/api/deployments/' + id + '/diagnose', { method: 'POST', ...jsonBody({}) }),
  // 采纳 / 不采纳一次诊断；采纳会把结论写入知识库。
  adoptDiagnosis: (id, adopt) => request('/api/runs/' + id + '/diagnosis/adopt', { method: 'POST', ...jsonBody({ adopt }) }),
  // 知识库：列出所有已采纳的诊断；可选 q 关键字过滤。
  kbList: (q) => request('/api/kb' + (q ? '?q=' + encodeURIComponent(q) : '')),
  kbRemove: (id) => request('/api/kb/' + encodeURIComponent(id), { method: 'DELETE' }),
}

export default API
