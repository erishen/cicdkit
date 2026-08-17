import { useEffect, useState, useCallback } from 'react'
import Modal from './Modal.jsx'
import API from '../api.js'

// KnowledgeModal lists adopted AI diagnoses so the operator can review what has
// been accumulated, search it, and remove stale entries.
export default function KnowledgeModal({ onClose, toast }) {
  const [loading, setLoading] = useState(true)
  const [query, setQuery] = useState('')
  const [entries, setEntries] = useState([])
  const [open, setOpen] = useState({})

  const load = useCallback((q) => {
    setLoading(true)
    API.kbList(q)
      .then((res) => setEntries(Array.isArray(res.entries) ? res.entries : []))
      .catch((e) => toast && toast('加载知识库失败: ' + e.message))
      .finally(() => setLoading(false))
  }, [toast])

  // Reload whenever the query changes (small dataset, no debounce needed).
  useEffect(() => { load(query) }, [load, query])

  async function removeItem(id) {
    if (!window.confirm('确定从知识库移除该条诊断？此操作不可撤销。')) return
    try {
      await API.kbRemove(id)
      toast && toast('已从知识库移除')
      load(query)
    } catch (e) {
      toast && toast('移除失败: ' + e.message)
    }
  }

  return (
    <Modal onClose={onClose}>
      <div className="modal-card modal-wide" onClick={(e) => e.stopPropagation()}>
        <div className="modal-head">
          <h3>诊断知识库</h3>
          <button className="btn btn-ghost btn-sm" onClick={onClose}>✕</button>
        </div>
        <div className="modal-body">
          <input
            className="kb-search"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="搜索项目 / 阶段 / 错误关键字 / 诊断内容…"
          />
          {loading ? (
            <div className="muted" style={{ marginTop: 12 }}>加载中…</div>
          ) : entries.length === 0 ? (
            <div className="muted" style={{ marginTop: 12 }}>
              {query ? '没有匹配的知识库条目。' : '知识库为空。在「AI 故障诊断」中采纳一条结论后，会沉淀到这里，供同类失败复用。'}
            </div>
          ) : (
            <div className="kb-list">
              {entries.map((e, i) => {
                const key = e.id || i
                return (
                  <div className="kb-item" key={key}>
                    <button className="kb-item-head" onClick={() => setOpen((o) => ({ ...o, [key]: !o[key] }))}>
                      <span className="kb-item-title">{e.project_name || '未知项目'} · {e.stage || '—'}</span>
                      <span className="kb-item-count">采纳 {e.adopted_count || 0} 次 ▾</span>
                    </button>
                    {e.error_excerpt && <div className="kb-item-err">{e.error_excerpt}</div>}
                    {open[key] && <pre className="diagnose-result">{e.diagnosis}</pre>}
                    <div className="kb-item-foot">
                      <span className="muted">{(e.adopted_at || '').replace('T', ' ').slice(0, 19)}</span>
                      <button className="btn btn-ghost btn-sm btn-danger-text" onClick={() => removeItem(e.id)}>移除</button>
                    </div>
                  </div>
                )
              })}
            </div>
          )}
        </div>
        <div className="modal-foot">
          <button className="btn btn-ghost" onClick={onClose}>关闭</button>
        </div>
      </div>
    </Modal>
  )
}
