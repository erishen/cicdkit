import React, { useState, useEffect } from 'react'
import Modal from './Modal.jsx'
import API from '../api.js'

const STATUS_DOT = {
  ok: 'var(--ok)',
  skip: 'var(--running)',
  fail: 'var(--danger)',
  err: 'var(--danger)',
}
const STATUS_LABEL = {
  ok: '通过',
  skip: '跳过',
  fail: '未达标',
  err: '出错',
}
const STATUS_CLASS = {
  ok: 'badge-ok',
  skip: 'badge-run',
  fail: 'badge-err',
  err: 'badge-err',
}

// ProbeModal runs the service-availability probe on demand and shows a
// Postman-style request/response: status code (colored), latency, response
// headers and body. It does not build or deploy anything.
export default function ProbeModal({ projectId, onClose }) {
  const [result, setResult] = useState(null)
  const [err, setErr] = useState(null)

  const load = async () => {
    setErr(null)
    try {
      const r = await API.probe(projectId)
      setResult(r)
    } catch (e) {
      setErr(e.message || '探测请求失败')
    }
  }

  useEffect(() => { load() }, [projectId])

  return (
    <Modal onClose={onClose}>
      <div className="modal-card">
        <div className="modal-head">
          <h3>服务探测 · {projectId}</h3>
          <button className="btn btn-ghost btn-sm" onClick={onClose}>✕</button>
        </div>
        {err && <div className="empty" style={{ color: 'var(--danger)' }}>{err}</div>}
        {!result && !err && <div className="empty">探测中…</div>}
        {result && (
          <div className="probe">
            <div className={'validate-summary ' + (result.status === 'ok' ? 'ok' : result.status === 'skip' ? 'warn' : 'err')}>
              <span className="dot" style={{ background: STATUS_DOT[result.status] }} />
              {STATUS_LABEL[result.status] || result.status} · {result.detail}
            </div>

            <div className="probe-req">
              <span className="probe-method">{result.method || 'GET'}</span>
              <span className="probe-url">{result.url || '（自动推导失败）'}</span>
            </div>

            <div className="probe-meta">
              {result.status_code ? <span>状态码 <strong>{result.status_code}</strong></span> : null}
              {typeof result.duration_ms === 'number' ? <span>耗时 <strong>{result.duration_ms}ms</strong></span> : null}
              {typeof result.matched === 'boolean' ? <span>内容匹配 <strong>{result.matched ? '是' : '否'}</strong></span> : null}
            </div>

            {result.error && (
              <div className="probe-error">{result.error}</div>
            )}

            {result.headers && Object.keys(result.headers).length > 0 && (
              <div className="probe-section">
                <div className="probe-section-title">响应头</div>
                <table className="probe-headers">
                  <tbody>
                    {Object.entries(result.headers).map(([k, v]) => (
                      <tr key={k}><td className="h-k">{k}</td><td className="h-v">{v}</td></tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

            {result.body ? (
              <div className="probe-section">
                <div className="probe-section-title">响应体</div>
                <pre className="probe-body">{result.body}</pre>
              </div>
            ) : null}

            <div className="modal-foot">
              <button className="btn btn-sm" onClick={load}>重新探测</button>
            </div>
          </div>
        )}
      </div>
    </Modal>
  )
}
