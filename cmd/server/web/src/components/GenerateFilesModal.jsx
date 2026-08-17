import React, { useState, useEffect } from 'react'
import Modal from './Modal.jsx'
import API from '../api.js'

// GenerateFilesModal previews the Dockerfile / k8s manifest that would be
// scaffolded for a project (because they are missing) and writes them into the
// project's build context on confirmation. After writing, the project config is
// updated to point at the new files so the project is immediately publishable.
export default function GenerateFilesModal({ projectId, onClose, onApplied }) {
  const [plan, setPlan] = useState(null)
  const [err, setErr] = useState(null)
  const [applying, setApplying] = useState(false)
  const [done, setDone] = useState(null)

  const load = async () => {
    try {
      const r = await API.generatePlan(projectId)
      setPlan(r)
      setErr(null)
    } catch (e) {
      setErr(e.message || '生成预览失败')
    }
  }

  useEffect(() => { load() }, [projectId])

  const apply = async () => {
    setApplying(true)
    setErr(null)
    try {
      const r = await API.generateApply(projectId)
      setDone(r)
      if (onApplied) onApplied()
    } catch (e) {
      setErr(e.message || '写入失败')
    } finally {
      setApplying(false)
    }
  }

  const FilePreview = ({ spec, title }) => (
    <div className="gen-file">
      <div className="gen-file-head">
        <strong>{title}</strong>
        <code className="gen-path">{spec.path}</code>
      </div>
      <div className="gen-reason">{spec.reason}</div>
      <pre className="gen-code">{spec.content}</pre>
    </div>
  )

  return (
    <Modal onClose={onClose}>
      <div className="modal-card modal-wide">
        <div className="modal-head">
          <h3>生成缺失文件 · {projectId}</h3>
          <button className="btn btn-ghost btn-sm" onClick={onClose}>✕</button>
        </div>

        <div className="modal-body">
          {err && !done && <div className="empty" style={{ color: 'var(--danger)' }}>{err}</div>}
          {!plan && !err && !done && <div className="empty">生成预览中…</div>}

          {done && (
            <div className="gen-done">
              <div className="validate-summary ok">✓ 已写入以下文件：</div>
              <ul>
                {done.dockerfile && <li><code>{done.dockerfile.path}</code></li>}
                {done.manifest && <li><code>{done.manifest.path}</code></li>}
              </ul>
              <div className="gen-note">项目配置已同步更新（dockerfile / manifest_path / deployment / container）。可直接发布。</div>
            </div>
          )}

          {plan && !done && (
            <>
              {!plan.needs_any && (
                <div className="empty">Dockerfile 与 k8s 清单均已存在，无需生成。</div>
              )}
              {plan.needs_any && (
                <>
                  {plan.dockerfile && <FilePreview spec={plan.dockerfile} title="Dockerfile" />}
                  {plan.manifest && <FilePreview spec={plan.manifest} title="k8s/deployment.yaml" />}
                  <div className="gen-config">
                    <div className="gen-config-title">写入后还将更新项目配置：</div>
                    <ul>
                      {Object.entries(plan.config_updates || {}).map(([k, v]) => (
                        <li key={k}><code>{k}</code> = <code>{v}</code></li>
                      ))}
                    </ul>
                  </div>
                </>
              )}
            </>
          )}
        </div>

        <div className="modal-foot">
          {done ? (
            <button className="btn btn-sm" onClick={onClose}>关闭</button>
          ) : (
            <>
              <button className="btn btn-sm" onClick={onClose}>取消</button>
              {plan && plan.needs_any && (
                <button className="btn btn-sm btn-primary" disabled={applying} onClick={apply}>
                  {applying ? '写入中…' : '写入文件'}
                </button>
              )}
            </>
          )}
        </div>
      </div>
    </Modal>
  )
}
