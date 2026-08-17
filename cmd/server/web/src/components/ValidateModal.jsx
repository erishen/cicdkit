import React, { useState, useEffect } from 'react'
import Modal from './Modal.jsx'
import API from '../api.js'

const CHECK_DOT = {
  ok: 'var(--ok)',
  warn: 'var(--running)',
  error: 'var(--danger)',
}
const CHECK_LABEL = {
  ok: '通过',
  warn: '警告',
  error: '失败',
}

// ValidateModal runs a dry-run validation of a project (config sanity, docker
// daemon and kubeconfig reachability) without building or deploying. It shows
// one row per check with a colored status dot.
export default function ValidateModal({ projectId, onClose }) {
  const [result, setResult] = useState(null)
  const [err, setErr] = useState(null)

  const load = async () => {
    try {
      const r = await API.validate(projectId)
      setResult(r)
      setErr(null)
    } catch (e) {
      setErr(e.message || '校验请求失败')
    }
  }

  useEffect(() => { load() }, [projectId])

  return (
    <Modal onClose={onClose}>
      <div className="modal-card">
        <div className="modal-head">
          <h3>配置校验 / 试跑 · {projectId}</h3>
          <button className="btn btn-ghost btn-sm" onClick={onClose}>✕</button>
        </div>
        {err && <div className="empty" style={{ color: 'var(--danger)' }}>{err}</div>}
        {!result && !err && <div className="empty">校验中…</div>}
        {result && (
          <>
            <div className={'validate-summary ' + (result.ok ? 'ok' : 'err')}>
              {result.ok ? '✓ 全部检查通过，可以放心构建/部署' : '✗ 存在问题，请先修复下方标红项'}
            </div>
            <ul className="checks">
              {(result.checks || []).map((c, i) => (
                <li className="check" key={i}>
                  <span className="dot" style={{ background: CHECK_DOT[c.status] }} />
                  <span className="check-name">{c.name}</span>
                  <span className={'badge ' + (c.status === 'ok' ? 'badge-ok' : c.status === 'warn' ? 'badge-run' : 'badge-err')}>
                    {CHECK_LABEL[c.status]}
                  </span>
                  <span className="check-detail">{c.detail}</span>
                </li>
              ))}
            </ul>
            <div className="modal-foot">
              <button className="btn btn-sm" onClick={load}>重新校验</button>
            </div>
          </>
        )}
      </div>
    </Modal>
  )
}
