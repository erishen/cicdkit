import { useEffect, useState } from 'react'
import Modal from './Modal.jsx'
import API from '../api.js'

// DiagnoseModal runs the AI failure analysis for a run (or a deployment that
// resolves to its run) and shows the model's plain-text report. The button
// that opens it lives on failed run / deployment rows. Operators can adopt the
// diagnosis into the knowledge base, or mark it not useful.
export default function DiagnoseModal({ runId, deploymentId, onClose, toast }) {
  const [state, setState] = useState('loading') // loading | done | error
  const [analysis, setAnalysis] = useState('')
  const [error, setError] = useState('')
  const [cached, setCached] = useState(false)
  const [cachedFromKb, setCachedFromKb] = useState(false)
  const [adopted, setAdopted] = useState(false)
  const [rejected, setRejected] = useState(false)
  const [similar, setSimilar] = useState([])
  const [acting, setActing] = useState(false)
  const [showSimilar, setShowSimilar] = useState(false)

  useEffect(() => {
    let alive = true
    const call = runId ? API.diagnoseRun(runId) : API.diagnoseDeployment(deploymentId)
    call
      .then((res) => {
        if (!alive) return
        setAnalysis(res.analysis || '')
        setCached(!!res.cached)
        setCachedFromKb(!!res.cached_from_kb)
        setAdopted(!!res.adopted)
        setRejected(!!res.rejected)
        setSimilar(Array.isArray(res.similar) ? res.similar : [])
        setState('done')
      })
      .catch((e) => {
        if (!alive) return
        setError(e.message)
        setState('error')
      })
    return () => { alive = false }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  async function adopt(value) {
    if (!runId) return
    setActing(true)
    try {
      const res = await API.adoptDiagnosis(runId, value)
      setAdopted(!!res.adopted)
      setRejected(!!res.rejected)
      toast && toast(value ? '已采纳并加入知识库' : '已标记为不采纳')
    } catch (e) {
      toast && toast('操作失败: ' + e.message)
    } finally {
      setActing(false)
    }
  }

  return (
    <Modal onClose={onClose}>
      <div className="modal-card modal-wide" onClick={(e) => e.stopPropagation()}>
        <div className="modal-head">
          <h3>AI 故障诊断{cached ? (cachedFromKb ? '（复用知识库）' : '（缓存）') : ''}</h3>
          <button className="btn btn-ghost btn-sm" onClick={onClose}>✕</button>
        </div>
        <div className="modal-body">
          {state === 'loading' && (
            <div className="muted diagnose-loading">
              <span className="spinner" /> 正在分析失败日志，请稍候…
            </div>
          )}
          {state === 'error' && (
            <div className="alert-err">
              <strong>诊断失败：</strong>
              <div style={{ marginTop: 6, whiteSpace: 'pre-wrap' }}>{error}</div>
              <div className="muted" style={{ marginTop: 8 }}>
                请检查「设置 · LLM」中的 Base URL / API Key / Model 是否已正确填写并启用；
                或确认该运行确实处于失败状态。
              </div>
            </div>
          )}
          {state === 'done' && (
            <>
              {similar.length > 0 && (
                <div className="kb-similar">
                  <button className="btn btn-ghost btn-sm" onClick={() => setShowSimilar((v) => !v)}>
                    历史同类诊断（知识库 {similar.length} 条）{showSimilar ? ' ▲' : ' ▼'}
                  </button>
                  {showSimilar && (
                    <div className="kb-similar-list">
                      {similar.map((e, i) => (
                        <div className="kb-similar-item" key={e.id || i}>
                          <div className="kb-similar-meta">
                            {e.project_name || '未知项目'} · {e.stage || '—'} · 采纳 {e.adopted_count || 0} 次
                          </div>
                          {e.error_excerpt && <div className="kb-similar-err">{e.error_excerpt}</div>}
                          <pre className="diagnose-result">{e.diagnosis}</pre>
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              )}
              {cachedFromKb && (
                <div className="muted" style={{ marginBottom: 8 }}>
                  📚 本次直接复用知识库中已采纳的同类结论，未调用模型。若仍不准确可重新「采纳」以强化该条目。
                </div>
              )}
              {cached && !cachedFromKb && (
                <div className="muted" style={{ marginBottom: 8 }}>⚡ 本次为缓存结果，未重新调用模型。</div>
              )}
              <pre className="diagnose-result">{analysis}</pre>
              {runId ? (
                <div className="diag-actions">
                  {adopted ? (
                    <span className="kb-badge ok">✓ 已采纳到知识库</span>
                  ) : rejected ? (
                    <>
                      <span className="kb-badge muted">已标记为不采纳</span>
                      <button className="btn btn-primary btn-sm" disabled={acting} onClick={() => adopt(true)}>重新采纳</button>
                    </>
                  ) : (
                    <>
                      <button className="btn btn-primary btn-sm" disabled={acting} onClick={() => adopt(true)}>✓ 采纳并加入知识库</button>
                      <button className="btn btn-ghost btn-sm" disabled={acting} onClick={() => adopt(false)}>✗ 不采纳</button>
                    </>
                  )}
                </div>
              ) : (
                <div className="muted" style={{ marginTop: 8 }}>该记录缺少运行关联，无法采纳到知识库。</div>
              )}
            </>
          )}
        </div>
        <div className="modal-foot">
          <button className="btn btn-ghost" onClick={onClose}>关闭</button>
        </div>
      </div>
    </Modal>
  )
}
