import React, { useState, useEffect, useCallback, useRef } from 'react'
import API from './api.js'
import ProjectForm from './components/ProjectForm.jsx'
import ScanModal from './components/ScanModal.jsx'
import LogModal from './components/LogModal.jsx'
import ValidateModal from './components/ValidateModal.jsx'
import ProbeModal from './components/ProbeModal.jsx'
import GenerateFilesModal from './components/GenerateFilesModal.jsx'
import SettingsModal from './components/SettingsModal.jsx'
import DiagnoseModal from './components/DiagnoseModal.jsx'
import KnowledgeModal from './components/KnowledgeModal.jsx'
import Modal from './components/Modal.jsx'

const STATUS_CLASS = {
  queued: 'badge-queue', running: 'badge-run', success: 'badge-ok', failed: 'badge-err', canceled: 'badge-err',
}
const STATUS_LABEL = {
  queued: '排队中', running: '运行中', success: '成功', failed: '失败', canceled: '已取消',
}
const ACTION_LABEL = { build: '构建', pipeline: '发布', deploy: '部署' }
const PROBE_LABEL = { ok: '通过', skip: '跳过', fail: '未达标', err: '出错' }
const METHOD_LABEL = {
  'local-k3s': '本地 k3s', ssh: 'SSH 远程主机',
  'kubectl-apply': 'kubectl apply', 'kubectl-set-image': 'kubectl set-image', helm: 'Helm',
}
// UI 构建戳：每次前端改动后人工升一位，用于一眼确认浏览器看到的是否为最新版。
// 后端戳走 /api/version，由后端 BuildStamp 提供。
const UI_BUILD = '2026-08-16 12:50'

// 哨兵是否在视口内（含顶部/底部越界判断），用于首屏/视口未填满时持续追加。
function inViewport(el) {
  const r = el.getBoundingClientRect()
  return r.top < window.innerHeight && r.bottom > 0
}

// 无限滚动哨兵：在列表尾部放一个 ref 元素，进入视口（含 rootMargin 预取区）即自动
// 触发 loadMore 追加下一页；首屏会主动链式填满直到 hasMore=false（用户无需滚到底）。
// loadingRef 保证同列表并发只加载一次。
function useInfiniteSentinel(loadMore, hasMore, onBusy) {
  const ref = useRef(null)
  const loadingRef = useRef(false)
  const loadRef = useRef(loadMore); loadRef.current = loadMore
  const hasMoreRef = useRef(hasMore); hasMoreRef.current = hasMore
  const busyRef = useRef(onBusy); busyRef.current = onBusy

  const tryLoad = useCallback(async () => {
    if (loadingRef.current) return
    loadingRef.current = true
    if (busyRef.current) busyRef.current(true)
    try {
      // 链式：只要后端还有更多页就持续追加，不再依赖 sentinel 是否在视口。
      // 单次 fetch 失败不应中断后续页加载（内层 try/catch 包裹 await）。
      while (true) {
        let n = 0
        try { n = await loadRef.current() } catch (_) { break }
        if (n <= 0 || !hasMoreRef.current) break
      }
    } finally {
      loadingRef.current = false
      if (busyRef.current) busyRef.current(false)
    }
  }, [])

  useEffect(() => {
    const el = ref.current
    if (!el || !hasMore) return
    const obs = new IntersectionObserver((entries) => {
      // 用户滚到哨兵可见时再次触发，允许他们在加载中途快速滚到底触发 prefetch。
      if (entries[0].isIntersecting) tryLoad()
    }, { rootMargin: '240px' })
    obs.observe(el)
    // 首次创建 observer 时主动触发一次填充：保证无论用户是否滚动都能一次性加载完。
    tryLoad()
    return () => obs.disconnect()
  }, [hasMore, tryLoad])

  return ref
}

export default function App() {
  const [projects, setProjects] = useState([])
  const [filter, setFilter] = useState('')
  const [health, setHealth] = useState('连接中…')
  const [healthCls, setHealthCls] = useState('badge-unknown')
  // 后端构建戳：footer 展示，用于确认浏览器连的是否为最新二进制（修复分页重叠后尤为关键）。
  const [backendStamp, setBackendStamp] = useState('')
  const [activeTab, setActiveTab] = useState('runs')
  const [runs, setRuns] = useState([])
  const [deployments, setDeployments] = useState([])
  // 分页：每页条数固定；用 ref 记录已加载偏移与总数，避免闭包拿到旧值。
  const PAGE = 20
  const runsOff = useRef(0)
  const runsTotal = useRef(0)
  const deploysOff = useRef(0)
  const deploysTotal = useRef(0)
  const projectsOff = useRef(0)
  const projectsTotal = useRef(0)
  const [runsHasMore, setRunsHasMore] = useState(false)
  const [deploysHasMore, setDeploysHasMore] = useState(false)
  const [projectsHasMore, setProjectsHasMore] = useState(false)
  // 无限滚动「加载中」状态：仅用于哨兵处的 spinner/文案展示。
  const [runsLoadingMore, setRunsLoadingMore] = useState(false)
  const [deploysLoadingMore, setDeploysLoadingMore] = useState(false)
  const [projLoadingMore, setProjLoadingMore] = useState(false)
  // filter 的实时值镜像，供 loadProjects 在闭包内读到最新筛选词（服务端过滤）。
  const filterRef = useRef('')
  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState(null)
  const [logRun, setLogRun] = useState(null)
  const [validatePid, setValidatePid] = useState(null)
  const [probePid, setProbePid] = useState(null)
  const [genPid, setGenPid] = useState(null)
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [diagRunId, setDiagRunId] = useState(null)
  const [kbOpen, setKbOpen] = useState(false)
  const [scanOpen, setScanOpen] = useState(false)
  const [scanDraft, setScanDraft] = useState(null)
  const [toast, setToast] = useState(null)
  const [pubMenu, setPubMenu] = useState(null)
  const [moreMenu, setMoreMenu] = useState(null)
  const toastTimer = useRef(null)

  // 弹窗背景滚动锁已下沉到 components/Modal.jsx：每个弹窗自管理，新增弹窗不会漏锁。

  const showToast = useCallback((msg, kind) => {
    setToast({ msg, kind: kind || '' })
    if (toastTimer.current) clearTimeout(toastTimer.current)
    toastTimer.current = setTimeout(() => setToast(null), 2600)
  }, [])

  const loadProjects = useCallback(async (reset = true) => {
    try {
      if (reset) projectsOff.current = 0
      const d = await API.listProjects({ limit: PAGE, offset: projectsOff.current, q: filterRef.current })
      const items = d.projects || []
      setProjects((prev) => (reset ? items : [...prev, ...items]))
      projectsTotal.current = d.total || 0
      projectsOff.current += items.length
      setProjectsHasMore(projectsOff.current < projectsTotal.current)
      return items.length
    } catch (e) { showToast('加载项目失败: ' + e.message, 'err'); return 0 }
  }, [showToast])

  // loadRuns(reset=true) 重新从第一页加载；reset=false 时在已加载基础上追加下一页。
  const loadRuns = useCallback(async (reset = true) => {
    try {
      if (reset) runsOff.current = 0
      const d = await API.listRuns({ limit: PAGE, offset: runsOff.current })
      const items = d.runs || []
      setRuns((prev) => (reset ? items : [...prev, ...items]))
      runsTotal.current = d.total || 0
      runsOff.current += items.length
      setRunsHasMore(runsOff.current < runsTotal.current)
      return items.length
    } catch (e) { return 0 }
  }, [])
  // loadDeployments 同上。
  const loadDeployments = useCallback(async (reset = true) => {
    try {
      if (reset) deploysOff.current = 0
      const d = await API.listDeployments({ limit: PAGE, offset: deploysOff.current })
      const items = d.deployments || []
      setDeployments((prev) => (reset ? items : [...prev, ...items]))
      deploysTotal.current = d.total || 0
      deploysOff.current += items.length
      setDeploysHasMore(deploysOff.current < deploysTotal.current)
      return items.length
    } catch (e) { return 0 }
  }, [])

  // 无限滚动哨兵：滚动到列表底部（含 240px 预取区）时自动追加下一页。
  const projectsSentinelRef = useInfiniteSentinel(() => loadProjects(false), projectsHasMore, setProjLoadingMore)
  const runsSentinelRef = useInfiniteSentinel(() => loadRuns(false), runsHasMore, setRunsLoadingMore)
  const deploysSentinelRef = useInfiniteSentinel(() => loadDeployments(false), deploysHasMore, setDeploysLoadingMore)

  // clearRuns / clearDeployments：一键清空全局历史（不可撤销），前端先确认。
  const clearRuns = useCallback(async () => {
    if (!confirm('确认清空全部运行记录？此操作不可撤销。')) return
    try {
      await API.clearRuns()
      runsOff.current = 0
      await loadRuns()
      showToast('运行记录已清空', 'ok')
    } catch (e) { showToast('清空失败: ' + e.message, 'err') }
  }, [loadRuns])

  const clearDeployments = useCallback(async () => {
    if (!confirm('确认清空全部部署历史？此操作不可撤销。')) return
    try {
      await API.clearDeployments()
      deploysOff.current = 0
      await loadDeployments()
      showToast('部署历史已清空', 'ok')
    } catch (e) { showToast('清空失败: ' + e.message, 'err') }
  }, [loadDeployments])

  const checkHealth = useCallback(async () => {
    try { await API.health(); setHealth('已连接'); setHealthCls('badge-ok') }
    catch { setHealth('离线'); setHealthCls('badge-err') }
  }, [])

  useEffect(() => {
    let alive = true
    const boot = async () => {
      try {
        // 单独先做一次鉴权探针：token 缺失/错误时只在这里弹一次框；
        // 通过后再并发加载三个列表（此时 localStorage 已有有效 token，不再连环弹）。
        await API.authProbe()
        if (!alive) return
        await Promise.all([loadProjects(), loadRuns(), loadDeployments()])
      } catch {
        // 未提供/无效 token：列表保持空，等用户刷新重输；不静默重试。
      }
      if (!alive) return
      checkHealth()
      API.version().then((d) => setBackendStamp(d.build || '')).catch(() => {})
    }
    boot()
    const t = setInterval(checkHealth, 20000)
    return () => { alive = false; clearInterval(t) }
  }, [checkHealth, loadProjects, loadRuns, loadDeployments])

  const triggerAction = async (id, action, body) => {
    try {
      const run = await API.trigger(id, action, body || {})
      showToast(ACTION_LABEL[action] + ' 已触发: ' + run.id, 'ok')
      setLogRun(run.id)
      loadRuns(); loadDeployments()
    } catch (e) { showToast('触发失败: ' + e.message, 'err') }
  }

  // 按命名目标发布：优先复用最近一次成功构建的镜像（deploy，不重编）；若该目标
  // 尚未构建过任何镜像，则回退为完整流水线（pipeline，先构建再发布）。这样同架构
  // 的多个目标（如腾讯云→阿里云）共用一份本地镜像，无需重复编译。
  const deployToTarget = async (id, target) => {
    setPubMenu(null)
    try {
      const run = await API.trigger(id, 'deploy', { target })
      showToast('已触发（复用/必要时自动构建）: ' + run.id, 'ok')
      setLogRun(run.id); loadRuns(); loadDeployments()
      return
    } catch (e) {
      if (!/成功构建/.test(e.message)) { showToast('触发失败: ' + e.message, 'err'); return }
    }
    try {
      const run = await API.trigger(id, 'pipeline', { target })
      showToast('已触发（先构建再发布）: ' + run.id, 'ok')
      setLogRun(run.id); loadRuns(); loadDeployments()
    } catch (e2) { showToast('触发失败: ' + e2.message, 'err') }
  }

  const deleteProject = async (id) => {
    if (!confirm('确认删除该项目？此操作不可撤销。')) return
    try { await API.deleteProject(id); showToast('已删除', 'ok'); loadProjects() }
    catch (e) { showToast('删除失败: ' + e.message, 'err') }
  }

  const openForm = (project) => { setEditing(project || null); setFormOpen(true) }
  const onFormSaved = () => { setFormOpen(false); setEditing(null); setScanDraft(null); loadProjects() }
  const onScanned = (draft) => {
    setScanOpen(false)
    setScanDraft(draft)
    setEditing(draft.project)
    setFormOpen(true)
    if (draft.notes && draft.notes.length) {
      showToast('已根据目录生成草稿，请确认/补充后保存', '')
    }
  }

  // 筛选已下沉到服务端（handleListProjects 的 q 参数），这里直接渲染已加载页。
  const filtered = projects

  return (
    <React.Fragment>
      <header className="topbar">
        <div className="brand">
          <span className="logo">⚙</span>
          <div>
            <h1>CI/CD 平台</h1>
            <p className="muted">Docker 构建 · K8s / 裸机发布管理</p>
          </div>
        </div>
        <div className="topbar-right">
          <span className={'badge ' + healthCls}>{health}</span>
          <button className="btn" onClick={() => setScanOpen(true)}>从目录导入</button>
          <button className="btn btn-ghost" onClick={() => setSettingsOpen(true)} title="LLM 智能诊断配置">⚙ 设置</button>
          <button className="btn btn-ghost" onClick={() => setKbOpen(true)} title="诊断知识库">📚 知识库</button>
          <button className="btn btn-primary" onClick={() => openForm(null)}>+ 新建项目</button>
        </div>
      </header>

      <main className="layout">
        <section className="col col-projects">
          <div className="section-head">
            <h2>项目 <span className="count">共 {projectsTotal.current}</span></h2>
            <input className="input input-sm" placeholder="筛选…" value={filter}
              onChange={(e) => { const v = e.target.value; filterRef.current = v; setFilter(v); loadProjects(true) }} />
          </div>
          <div className="cards">
            {filtered.length === 0 && <div className="empty">暂无项目，点击「+ 新建项目」开始。</div>}
            {filtered.map((p, i) => (
              <div className="card" key={p.id}>
                <h3><span className="seq">{i + 1}</span>{p.name}</h3>
                <div className="meta">{p.repository ? p.repository : '无仓库'} · {p.branch || '—'}</div>
                <div className="meta">镜像: {p.build?.image_repo
                  ? p.build.image_repo
                  : <span className="muted">未配置镜像</span>}</div>
                <div className="tags">
                  {p.deploy?.method && <span className="tag">{p.deploy.method}</span>}
                  {p.build?.push && <span className="tag">push</span>}
                </div>
                {p.last_deploy && (
                  <div className="last-deploy">
                    <span className="muted">最近部署</span>
                    <span className="tag tag-accent">{METHOD_LABEL[p.last_deploy.method] || p.last_deploy.method}</span>
                    {p.last_deploy.target && <span className="tag tag-accent" style={{ background: 'var(--accent-2, #0ea5e9)', color: '#fff' }}>{p.last_deploy.target}</span>}
                    <span>{new Date(p.last_deploy.at).toLocaleString('zh-CN')}</span>
                    {p.last_deploy.probe_status === 'ok' && <span className="badge badge-ok">探针通过</span>}
                    {(p.last_deploy.probe_status === 'fail' || p.last_deploy.probe_status === 'err') && <span className="badge badge-err">探针失败</span>}
                    {p.last_deploy.probe_status === 'skip' && <span className="badge badge-queue">探针跳过</span>}
                  </div>
                )}
                <div className="row">
                  <button className="btn btn-sm" onClick={() => triggerAction(p.id, 'build')}>Build</button>
                  <div style={{ position: 'relative', display: 'inline-block' }}>
                    <button className="btn btn-sm btn-primary" onClick={() => setPubMenu(pubMenu === p.id ? null : p.id)}>发布 ▾</button>
                    {pubMenu === p.id && (
                      <div style={{ position: 'absolute', zIndex: 30, top: '100%', left: 0, marginTop: 4, background: 'var(--surface, #fff)', border: '1px solid #d8dde3', borderRadius: 6, boxShadow: '0 6px 18px rgba(0,0,0,.14)', minWidth: 188, overflow: 'hidden' }}>
                        {/* 单一有序列表：主方法排在最前（带「主」标记），其后按声明顺序列出命名目标，
                            不再用「命名发布目标」分隔标题，使发布目标整体按工作流顺序呈现。 */}
                        {p.deploy?.method && (
                          <button className="split-item" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', width: '100%', textAlign: 'left', padding: '8px 12px', border: 0, background: 'transparent', cursor: 'pointer', fontSize: 13 }} onClick={() => { setPubMenu(null); triggerAction(p.id, 'pipeline', { method: p.deploy.method }) }}>
                            <span>发布到 {METHOD_LABEL[p.deploy.method] || p.deploy.method}</span>
                            <span style={{ fontSize: 10, color: 'var(--accent, #2f6fed)', border: '1px solid currentColor', borderRadius: 4, padding: '0 4px', lineHeight: '14px' }}>主</span>
                          </button>
                        )}
                        {p.targets && p.targets.map((t) => (
                          <button key={t.name} className="split-item" style={{ display: 'block', width: '100%', textAlign: 'left', padding: '8px 12px', border: 0, borderTop: '1px solid #eef1f4', background: 'transparent', cursor: 'pointer', fontSize: 13 }} onClick={() => deployToTarget(p.id, t.name)}>发布到 {t.name}</button>
                        ))}
                      </div>
                    )}
                  </div>
                  <button className="btn btn-sm btn-ghost" onClick={() => openForm(p)}>编辑</button>
                  <button className="btn btn-sm btn-ghost" onClick={() => deleteProject(p.id)}>删除</button>
                  <div style={{ position: 'relative', display: 'inline-block' }}>
                    <button className="btn btn-sm btn-ghost" onClick={() => setMoreMenu(moreMenu === p.id ? null : p.id)}>更多 ▾</button>
                    {moreMenu === p.id && (
                      <div style={{ position: 'absolute', zIndex: 30, top: '100%', left: 0, marginTop: 4, background: 'var(--surface, #fff)', border: '1px solid #d8dde3', borderRadius: 6, boxShadow: '0 6px 18px rgba(0,0,0,.14)', minWidth: 148, overflow: 'hidden' }}>
                        <button className="split-item" style={{ display: 'block', width: '100%', textAlign: 'left', padding: '8px 12px', border: 0, background: 'transparent', cursor: 'pointer', fontSize: 13 }} onClick={() => { setMoreMenu(null); setProbePid(p.id) }}>检查服务</button>
                        <button className="split-item" style={{ display: 'block', width: '100%', textAlign: 'left', padding: '8px 12px', border: 0, borderTop: '1px solid #eef1f4', background: 'transparent', cursor: 'pointer', fontSize: 13 }} onClick={() => { setMoreMenu(null); setValidatePid(p.id) }}>校验</button>
                        <button className="split-item" style={{ display: 'block', width: '100%', textAlign: 'left', padding: '8px 12px', border: 0, borderTop: '1px solid #eef1f4', background: 'transparent', cursor: 'pointer', fontSize: 13 }} onClick={() => { setMoreMenu(null); setGenPid(p.id) }}>生成缺失文件</button>
                      </div>
                    )}
                  </div>
                </div>
              </div>
            ))}
            {projectsHasMore && (
              <div className="load-more" ref={projectsSentinelRef}>
                {projLoadingMore
                  ? (<><span className="spinner" /> 加载中…</>)
                  : (<>
                      <button className="btn btn-ghost btn-sm" onClick={() => loadProjects(false)}>加载更多</button>
                      <span className="muted">已显示 {projects.length} / {projectsTotal.current} · 或向下滚动自动加载</span>
                    </>)}
              </div>
            )}
          </div>
        </section>

        <section className="col col-detail">
          <div className="tabs">
            <button className={'tab ' + (activeTab === 'runs' ? 'active' : '')}
              onClick={() => setActiveTab('runs')}>运行记录</button>
            <button className={'tab ' + (activeTab === 'deploys' ? 'active' : '')}
              onClick={() => setActiveTab('deploys')}>部署历史</button>
          </div>

          <div className={'tab-panel ' + (activeTab === 'runs' ? '' : 'hidden')}>
            <div className="section-head">
              <h2>运行记录 <span className="count">共 {runsTotal.current}</span></h2>
              <div className="section-actions">
                <button className="btn btn-ghost btn-sm" onClick={() => loadRuns()}>刷新</button>
                <button className="btn btn-danger btn-sm" onClick={clearRuns}>清空</button>
              </div>
            </div>
            <div className="runs">
              {runs.length === 0 && <div className="empty">暂无运行记录。</div>}
              {runs.map((r, i) => (
                <div className="run" key={r.id} onClick={() => setLogRun(r.id)}>
                  <div className="run-head">
                    <span className="seq seq-sm">{i + 1}</span>
                    <strong>{r.project_name || r.project_id}</strong>
                    <span className="run-actions">
                      {r.status === 'failed' && (
                        <button className="btn btn-sm btn-accent" onClick={(e) => { e.stopPropagation(); setDiagRunId(r.id) }}>AI 诊断</button>
                      )}
                      {r.diagnosis && (r.diagnosis.adopted
                        ? <span className="kb-badge ok">✓已采纳</span>
                        : <span className="kb-badge muted">已诊断</span>)}
                      <span className={'badge ' + STATUS_CLASS[r.status]}>{STATUS_LABEL[r.status]}</span>
                    </span>
                  </div>
                  <div className="run-sub">{r.id} · {r.image_ref || ''} · {r.trigger || ''} · {r.created_at
                    ? new Date(r.created_at).toLocaleString('zh-CN') : ''}</div>
                </div>
              ))}
            </div>
            {runsHasMore && (
              <div className="load-more" ref={runsSentinelRef}>
                {runsLoadingMore
                  ? (<><span className="spinner" /> 加载中…</>)
                  : (<>
                      <button className="btn btn-ghost btn-sm" onClick={() => loadRuns(false)}>加载更多</button>
                      <span className="muted">已显示 {runs.length} / {runsTotal.current} · 或向下滚动自动加载</span>
                    </>)}
              </div>
            )}
          </div>

          <div className={'tab-panel ' + (activeTab === 'deploys' ? '' : 'hidden')}>
            <div className="section-head">
              <h2>部署历史 <span className="count">共 {deploysTotal.current}</span></h2>
              <div className="section-actions">
                <button className="btn btn-ghost btn-sm" onClick={() => loadDeployments()}>刷新</button>
                <button className="btn btn-danger btn-sm" onClick={clearDeployments}>清空</button>
              </div>
            </div>
            <div className="deployments">
              {deployments.length === 0 && <div className="empty">暂无部署记录。</div>}
              {deployments.map((d, i) => (
                <div className="dep" key={d.id}>
                  <div><span className="seq seq-sm">{i + 1}</span><strong>{d.project_name || d.project_id}</strong> · <span className="badge badge-ok">{d.status}</span></div>
                  <div className="dep-sub">{d.image_ref || ''}</div>
                  <div className="dep-sub">{d.method || ''} · {d.namespace || '—'} · {d.created_at
                    ? new Date(d.created_at).toLocaleString('zh-CN') : ''}</div>
                  {d.probe && d.probe.status && (
                    <div className="dep-probe">
                      <span className={'badge ' + (d.probe.status === 'ok' ? 'badge-ok' : d.probe.status === 'skip' ? 'badge-run' : 'badge-err')}>
                        探测 {PROBE_LABEL[d.probe.status] || d.probe.status}
                      </span>
                      <span className="dep-sub" style={{ marginLeft: 6 }}>
                        {d.probe.method || 'GET'} {d.probe.url || ''}
                        {d.probe.status_code ? ' · ' + d.probe.status_code : ''}
                        {typeof d.probe.duration_ms === 'number' ? ' · ' + d.probe.duration_ms + 'ms' : ''}
                      </span>
                      {d.probe.body ? (
                        <pre className="dep-probe-body">{d.probe.body.slice(0, 400)}{d.probe.body.length > 400 ? '…' : ''}</pre>
                      ) : null}
                    </div>
                  )}
                </div>
              ))}
            </div>
            {deploysHasMore && (
              <div className="load-more" ref={deploysSentinelRef}>
                {deploysLoadingMore
                  ? (<><span className="spinner" /> 加载中…</>)
                  : (<>
                      <button className="btn btn-ghost btn-sm" onClick={() => loadDeployments(false)}>加载更多</button>
                      <span className="muted">已显示 {deployments.length} / {deploysTotal.current} · 或向下滚动自动加载</span>
                    </>)}
              </div>
            )}
          </div>
        </section>
      </main>

      <footer className="app-footer">
        <span>CI/CD 平台</span>
        <span className="muted">前端 {UI_BUILD} · 后端 {backendStamp || '…'}</span>
      </footer>

      {formOpen && (
        <ProjectForm project={editing} notes={scanDraft ? scanDraft.notes : null} onSaved={onFormSaved}
          onCancel={() => { setFormOpen(false); setEditing(null); setScanDraft(null) }} />
      )}
      {scanOpen && <ScanModal onScanned={onScanned} onCancel={() => setScanOpen(false)} />}
      {logRun && <LogModal runId={logRun} onClose={() => setLogRun(null)} onRunDone={() => loadProjects()} />}
      {validatePid && <ValidateModal projectId={validatePid} onClose={() => setValidatePid(null)} />}
      {probePid && <ProbeModal projectId={probePid} onClose={() => setProbePid(null)} />}
      {genPid && <GenerateFilesModal projectId={genPid} onClose={() => setGenPid(null)} onApplied={() => { loadProjects(); }} />}
      {settingsOpen && <SettingsModal onClose={() => setSettingsOpen(false)} toast={showToast} />}
      {diagRunId && <DiagnoseModal runId={diagRunId} onClose={() => setDiagRunId(null)} toast={showToast} />}
      {kbOpen && <KnowledgeModal onClose={() => setKbOpen(false)} toast={showToast} />}

      {pubMenu !== null && <div style={{ position: 'fixed', inset: 0, zIndex: 5 }} onClick={() => setPubMenu(null)} />}
      {moreMenu !== null && <div style={{ position: 'fixed', inset: 0, zIndex: 5 }} onClick={() => setMoreMenu(null)} />}
      {toast && <div className={'toast ' + (toast.kind ? toast.kind : '')}>{toast.msg}</div>}
    </React.Fragment>
  )
}
