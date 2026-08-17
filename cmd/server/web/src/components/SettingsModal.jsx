import { useEffect, useState } from 'react'
import Modal from './Modal.jsx'
import API from '../api.js'

// SettingsModal hosts the optional LLM configuration used by the failure
// diagnosis feature. The API key is write-only from the UI's perspective: GET
// returns only api_key_set, so we keep whatever the user already typed and only
// send it when they change the field.
export default function SettingsModal({ onClose, toast }) {
  const [enabled, setEnabled] = useState(false)
  const [baseUrl, setBaseUrl] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [model, setModel] = useState('')
  const [system, setSystem] = useState('')
  const [keySet, setKeySet] = useState(false)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)
  const [testMsg, setTestMsg] = useState(null) // { ok: bool, text: string }

  // 任意字段被改动后，旧的测试结果已不可信，清掉避免误导。
  const clearTest = () => setTestMsg(null)

  useEffect(() => {
    API.llmConfig()
      .then((c) => {
        setEnabled(!!c.enabled)
        setBaseUrl(c.base_url || '')
        setModel(c.model || '')
        setSystem(c.system || c.default_system || '')
        setKeySet(!!c.api_key_set)
      })
      .catch((e) => toast && toast('读取配置失败: ' + e.message))
      .finally(() => setLoading(false))
  }, [toast])

  async function testConnection() {
    setTesting(true)
    setTestMsg(null)
    try {
      // Send what the user has typed so an unsaved config can be verified too;
      // an empty api_key means "use the already-stored one" on the server.
      const body = { base_url: baseUrl, model, system }
      if (apiKey.trim() !== '') body.api_key = apiKey
      const res = await API.testLlm(body)
      setTestMsg({ ok: true, text: '连接成功' + (res.reply ? '：模型回包「' + res.reply + '」' : '') })
    } catch (e) {
      setTestMsg({ ok: false, text: e.message })
    } finally {
      setTesting(false)
    }
  }

  async function save() {
    setSaving(true)
    try {
      // Only send api_key if the user typed a new one; otherwise leave it empty
      // so the backend keeps the stored value.
      const body = { enabled, base_url: baseUrl, model, system }
      if (apiKey.trim() !== '') body.api_key = apiKey
      const saved = await API.saveLlmConfig(body)
      setKeySet(!!saved.api_key_set)
      setApiKey('')
      toast && toast('LLM 配置已保存' + (saved.enabled ? '（已启用）' : '（未启用）'))
      onClose()
    } catch (e) {
      toast && toast('保存失败: ' + e.message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal onClose={onClose}>
      <div className="modal-card modal-wide" onClick={(e) => e.stopPropagation()}>
        <div className="modal-head">
          <h3>设置 · LLM 智能诊断</h3>
          <button className="btn btn-ghost btn-sm" onClick={onClose}>✕</button>
        </div>
        <div className="modal-body">
          {loading ? (
            <div className="muted">加载中…</div>
          ) : (
            <>
              <p className="muted" style={{ marginTop: 0 }}>
                可选功能：构建 / 部署失败时，把失败阶段日志发给兼容 OpenAI 的对话接口，
                返回中文根因分析与修复建议。配置保存在服务端 <code>data/llm.json</code>（不入库、权限 0600）。
                不配置则运行记录里的「AI 诊断」按钮会提示先设置。
              </p>
              <label className="chk-row">
                <input type="checkbox" checked={enabled} onChange={(e) => { setEnabled(e.target.checked); clearTest() }} />
                <span>启用 LLM 诊断</span>
              </label>
              <label className="fld">
                <span>Base URL（OpenAI 兼容，以 /v1 结尾）</span>
                <input
                  value={baseUrl}
                  onChange={(e) => { setBaseUrl(e.target.value); clearTest() }}
                  placeholder="https://api.openai.com/v1"
                />
              </label>
              <label className="fld">
                <span>API Key{keySet ? '（已设置，留空保持不变）' : ''}</span>
                <input
                  type="password"
                  value={apiKey}
                  onChange={(e) => { setApiKey(e.target.value); clearTest() }}
                  placeholder={keySet ? '•••••••• 不更改请留空' : 'sk-...'}
                />
              </label>
              <label className="fld">
                <span>Model</span>
                <input
                  value={model}
                  onChange={(e) => { setModel(e.target.value); clearTest() }}
                  placeholder="gpt-4o-mini / deepseek-chat / ..."
                />
              </label>
              <label className="fld">
                <span>系统提示词（已内置默认模板，可直接修改或清空用内置）</span>
                <textarea
                  value={system}
                  onChange={(e) => { setSystem(e.target.value); clearTest() }}
                  rows={14}
                  style={{ minHeight: '240px', resize: 'vertical', width: '100%', lineHeight: 1.6 }}
                  placeholder="清空则使用内置的 CI/CD 故障分析提示词"
                />
              </label>
              {testMsg && (
                <div
                  className={testMsg.ok ? 'alert-ok' : 'alert-err'}
                  role="status"
                  style={{ marginTop: 14, textAlign: 'center' }}
                >
                  {testMsg.ok ? '✓ ' : '✗ '}{testMsg.text}
                </div>
              )}
            </>
          )}
        </div>
        <div className="modal-foot">
          <button className="btn btn-ghost" onClick={onClose} disabled={saving || testing}>取消</button>
          <button className="btn btn-ghost" onClick={testConnection} disabled={loading || saving || testing || !baseUrl || !model}>
            {testing ? '测试中…' : '测试连接'}
          </button>
          <button className="btn btn-primary" onClick={save} disabled={saving || loading}>
            {saving ? '保存中…' : '保存'}
          </button>
        </div>
      </div>
    </Modal>
  )
}
