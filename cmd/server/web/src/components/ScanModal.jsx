import React, { useEffect, useState, useCallback } from 'react'
import Modal from './Modal.jsx'
import API from '../api.js'

// 服务端目录浏览用的路径工具。服务器（dev 模式下与浏览器同机）在自己的文件系统上
// 列目录，前端跟着点选；选出的路径天然是服务器可访问的绝对路径，构建不会再报
// "path not found"。浏览器原生选择器拿不到真实路径，故此处完全改用服务端浏览。
function splitPath(p) {
  return p.split(/[/\\]/)
}
function parentOf(p) {
  const s = p.replace(/[/\\]+$/, '')
  const i = Math.max(s.lastIndexOf('/'), s.lastIndexOf('\\'))
  if (i <= 0) return null
  return s.slice(0, i) || '/'
}
function buildCrumbs(p) {
  const parts = splitPath(p)
  const crumbs = []
  let acc = ''
  parts.forEach((seg, i) => {
    if (i === 0) {
      acc = seg === '' ? '/' : seg
      crumbs.push({ label: acc === '/' ? '/' : acc, path: acc })
    } else {
      acc = acc === '/' ? '/' + seg : acc + '/' + seg
      crumbs.push({ label: seg, path: acc })
    }
  })
  if (crumbs.length === 0) crumbs.push({ label: '/', path: '/' })
  return crumbs
}

export default function ScanModal({ onScanned, onCancel }) {
  const [roots, setRoots] = useState([])
  const [current, setCurrent] = useState('')
  const [entries, setEntries] = useState([])
  const [loading, setLoading] = useState(false)
  const [scanning, setScanning] = useState(false)
  const [err, setErr] = useState('')

  const fetchList = useCallback(async (dir) => {
    if (!dir) return
    setLoading(true)
    setErr('')
    try {
      const data = await API.fsList(dir)
      setEntries(data.entries || [])
      setCurrent(data.dir || dir)
    } catch (e) {
      setErr('读取目录失败: ' + e.message)
      setEntries([])
    } finally {
      setLoading(false)
    }
  }, [])

  // 初始化：拉取允许访问的根目录，从第一个根开始浏览。
  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const data = await API.fsRoots()
        const rs = data.roots || []
        if (cancelled) return
        setRoots(rs)
        if (rs.length > 0) await fetchList(rs[0])
      } catch (e) {
        if (!cancelled) setErr('获取根目录失败: ' + e.message)
      }
    })()
    return () => { cancelled = true }
  }, [fetchList])

  const navigateTo = (dir) => { setCurrent(dir); fetchList(dir) }
  const enterDir = (name) => {
    const next = current === '/' ? '/' + name : current.replace(/[/\\]+$/, '') + '/' + name
    navigateTo(next)
  }
  const goUp = () => {
    const p = parentOf(current)
    if (p != null) navigateTo(p)
  }

  const doScan = async () => {
    const p = current.trim()
    if (!p) { setErr('请先用上方浏览器选择一个目录'); return }
    setScanning(true)
    setErr('')
    try {
      const draft = await API.scanProject({ path: p })
      onScanned(draft)
    } catch (e) {
      setErr('扫描失败: ' + e.message)
      setScanning(false)
    }
  }

  const crumbs = current ? buildCrumbs(current) : []
  const atRoot = parentOf(current) == null

  return (
    <Modal onClose={onCancel}>
      <div className="modal-card" style={{ maxWidth: 640 }}>
        <div className="modal-head">
          <h3>从目录导入</h3>
          <button type="button" className="btn btn-ghost btn-sm" onClick={onCancel}>✕</button>
        </div>
        <div className="modal-body">
          <p className="muted" style={{ fontSize: 12, lineHeight: 1.6 }}>
            由<b>服务器</b>在其文件系统上列目录（dev 模式下即你本机），点选后自动识别
            Dockerfile、语言、镜像名、端口与 k8s 清单，并生成预填好的项目配置。所选路径服务器
            一定可访问，构建不会再报「path not found」。
          </p>

          {/* 面包屑导航 */}
          <div className="fs-crumbs" style={{
            display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 4,
            padding: '8px 10px', border: '1px solid var(--border, #ddd)',
            borderRadius: 6, background: 'var(--surface-2, #fafafa)', marginBottom: 8,
          }}>
            <button type="button" className="btn btn-sm" disabled={atRoot} onClick={goUp} title="上一级">↑</button>
            {crumbs.map((c, i) => (
              <span key={i} style={{ display: 'inline-flex', alignItems: 'center' }}>
                {i > 0 && <span className="muted" style={{ margin: '0 2px' }}>/</span>}
                <button type="button" className="btn btn-link btn-sm" style={{ padding: '2px 4px' }}
                  onClick={() => navigateTo(c.path)}>{c.label}</button>
              </span>
            ))}
          </div>

          {/* 目录/文件列表 */}
          <div className="fs-list" style={{
            maxHeight: 260, overflowY: 'auto', border: '1px solid var(--border, #ddd)',
            borderRadius: 6, padding: 4,
          }}>
            {loading && <p className="muted" style={{ padding: 8 }}>读取中…</p>}
            {!loading && entries.length === 0 && (
              <p className="muted" style={{ padding: 8 }}>此目录为空或无可见条目</p>
            )}
            {!loading && entries.map((e) => (
              <div key={e.name} className="fs-item"
                onClick={e.is_dir ? () => enterDir(e.name) : undefined}
                style={{
                  display: 'flex', alignItems: 'center', gap: 8, padding: '6px 8px',
                  borderRadius: 4, cursor: e.is_dir ? 'pointer' : 'default',
                  color: e.is_dir ? 'var(--text, #111)' : 'var(--muted, #888)',
                }}
                onMouseEnter={(ev) => { if (e.is_dir) ev.currentTarget.style.background = 'var(--surface-2, #f0f0f0)' }}
                onMouseLeave={(ev) => { ev.currentTarget.style.background = 'transparent' }}>
                <span style={{ width: 18, textAlign: 'center' }}>{e.is_dir ? '📁' : '📄'}</span>
                <span style={{ flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{e.name}</span>
                {!e.is_dir && <span className="muted" style={{ fontSize: 11 }}>{fmtSize(e.size)}</span>}
                {e.is_dir && <span className="muted" style={{ fontSize: 11 }}>目录</span>}
              </div>
            ))}
          </div>

          {/* 手动粘贴路径 + 操作 */}
          <label style={{ display: 'block', marginTop: 10 }}>
            目录路径（服务器可访问的绝对路径，可手动粘贴后点「刷新」）
            <input className="input" placeholder="如 /Users/you/code/myapp" value={current}
              onChange={(e) => setCurrent(e.target.value)} />
          </label>
          <div className="row" style={{ marginTop: 10, display: 'flex', gap: 10, flexWrap: 'wrap', alignItems: 'center' }}>
            <button type="button" className="btn" onClick={() => fetchList(current.trim())} disabled={loading || !current}>
              刷新目录
            </button>
            <button type="button" className="btn btn-primary" onClick={doScan} disabled={scanning || !current}>
              {scanning ? '扫描中…' : '用此目录创建项目'}
            </button>
          </div>

          {err && <p className="muted" style={{ color: 'var(--danger)', marginTop: 10 }}>{err}</p>}
        </div>
      </div>
    </Modal>
  )
}

function fmtSize(n) {
  if (n < 1024) return n + ' B'
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB'
  return (n / 1024 / 1024).toFixed(1) + ' MB'
}
