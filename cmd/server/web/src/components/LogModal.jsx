import React, { useState, useEffect, useRef } from 'react'
import Modal from './Modal.jsx'
import API from '../api.js'

const STATUS_CLASS = {
  queued: 'badge-queue', running: 'badge-run', success: 'badge-ok', failed: 'badge-err', canceled: 'badge-err',
}
const STATUS_LABEL = {
  queued: '排队中', running: '运行中', success: '成功', failed: '失败', canceled: '已取消',
}
const DOT = {
  success: 'var(--ok)', failed: 'var(--danger)', running: 'var(--running)', canceled: 'var(--danger)', queued: 'var(--queued)',
}

// Probe status (from the backend service-availability check) mapped to the same
// presentation vocabulary as pipeline stages, so 服务探测 renders as a step.
const PROBE_VIEW = {
  ok:   { label: '通过',   cls: 'badge-ok',   dot: 'var(--ok)' },
  fail: { label: '未通过', cls: 'badge-err',  dot: 'var(--danger)' },
  err:  { label: '错误',   cls: 'badge-err',  dot: 'var(--danger)' },
  skip: { label: '跳过',   cls: 'badge-queue', dot: 'var(--queued)' },
}

export default function LogModal({ runId, onClose, onRunDone }) {
  const [run, setRun] = useState(null)
  const [fullscreen, setFullscreen] = useState(false)
  const logRef = useRef(null)
  const timer = useRef(null)

  const load = async () => {
    try {
      const r = await API.getRun(runId)
      setRun(r)
      if (logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight
      const live = r.status === 'running' || r.status === 'queued'
      if (!live) {
        if (timer.current) { clearInterval(timer.current); timer.current = null }
        // Run reached a terminal state (success/failed/canceled): refresh the
        // project list so the card's "最近部署" (last_deploy) reflects this run.
        if (onRunDone) onRunDone()
      }
    } catch (e) { /* ignore transient */ }
  }

  useEffect(() => {
    load()
    timer.current = setInterval(load, 1500)
    return () => { if (timer.current) clearInterval(timer.current) }
  }, [runId])

  // Esc 退出全屏
  useEffect(() => {
    if (!fullscreen) return
    const onKey = (e) => { if (e.key === 'Escape') setFullscreen(false) }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [fullscreen])

  const cancel = async () => {
    try { await API.cancelRun(runId); load() } catch (e) { /* ignore */ }
  }

  const live = run && (run.status === 'running' || run.status === 'queued')

  return (
    <Modal onClose={onClose} className={fullscreen ? 'modal-fullscreen-backdrop' : ''}>
      <div className={'modal-card modal-wide' + (fullscreen ? ' modal-fullscreen' : '')}>
        <div className="modal-head">
          <h3>运行日志 · {runId}</h3>
          <div>
            {run && <span className={'badge ' + STATUS_CLASS[run.status]}>{STATUS_LABEL[run.status]}</span>}
            {live && (
              <button className="btn btn-warn btn-sm" onClick={cancel} style={{ marginLeft: 8 }}>取消运行</button>
            )}
            <button
              className="btn btn-ghost btn-sm"
              onClick={() => setFullscreen((v) => !v)}
              style={{ marginLeft: 8 }}
              title={fullscreen ? '退出全屏 (Esc)' : '全屏查看日志'}
            >
              {fullscreen ? '⤡ 退出全屏' : '⤢ 全屏'}
            </button>
            <button className="btn btn-ghost btn-sm" onClick={onClose} style={{ marginLeft: 8 }}>✕</button>
          </div>
        </div>
        {run && (
          <div className="stages">
            {(run.stages || []).map((s, i) => (
              <div className="stage" key={i}>
                <span className="dot" style={{ background: DOT[s.status] }} />
                <span>{s.name}</span>
                <span className={'badge ' + STATUS_CLASS[s.status]}>{STATUS_LABEL[s.status]}</span>
              </div>
            ))}
            {run.probe && (() => {
              const v = PROBE_VIEW[run.probe.status] || { label: run.probe.status, cls: 'badge-queue', dot: 'var(--queued)' }
              const detail = run.probe.detail || (run.probe.status_code ? ('HTTP ' + run.probe.status_code + '，' + run.probe.duration_ms + 'ms') : '')
              return (
                <div className="stage" key="probe">
                  <span className="dot" style={{ background: v.dot }} />
                  <span>服务探测</span>
                  <span className={'badge ' + v.cls}>{v.label}</span>
                  {detail && <span className="stage-detail">{detail}</span>}
                </div>
              )
            })()}
          </div>
        )}
        <pre className="log" ref={logRef}>{run ? (run.log || '(无输出)') : '加载中…'}</pre>
      </div>
    </Modal>
  )
}
