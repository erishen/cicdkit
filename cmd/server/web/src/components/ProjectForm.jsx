import React, { useState, useEffect, useRef } from 'react'
import Modal from './Modal.jsx'
import API from '../api.js'

// CLOUD_PRESETS auto-fills typical SSH connection defaults when the operator
// picks a cloud vendor. They are convenience starting points only — every field
// stays editable afterwards (e.g. a Tencent Cloud CentOS image uses `root`,
// not `ubuntu`). The ssh deploy path itself is vendor-agnostic: it just needs an
// SSH-reachable host running Docker.
// CLOUD_PRESETS only pre-fill connection defaults (user/port) and the host
// publish port (probe_port). They intentionally do NOT touch ssh_run_args:
// the deploy path derives `-p <probe_port>:8080` from probe_port and treats
// run_args as extra flags only, so a preset can never lock the port.
const CLOUD_PRESETS = {
  tencent: { user: 'ubuntu', port: '22', probe_port: '8080' },
  alibaba: { user: 'root', port: '22', probe_port: '8080' },
  aws: { user: 'ec2-user', port: '22', probe_port: '8080' },
  huawei: { user: 'root', port: '22', probe_port: '8080' },
}

function parseKV(text) {
  const out = {}
  ;(text || '').split('\n').forEach((line) => {
    const i = line.indexOf('=')
    if (i > 0) {
      const k = line.slice(0, i).trim()
      if (k) out[k] = line.slice(i + 1).trim()
    }
  })
  return out
}

export default function ProjectForm({ project, notes, onSaved, onCancel }) {
  const isEdit = !!(project && project.id)
  const [form, setForm] = useState(() => {
    const base = {
      name: '', description: '', repository: '', branch: '',
      context: '.', dockerfile: 'Dockerfile', image_repo: '', tag_strategy: 'timestamp', push: false, build_args: '',
      method: 'kubectl-apply', kubeconfig: '', namespace: '', manifest_path: '', deployment: '', container: '',
      chart_path: '', release_name: '', helm_image_key: '', helm_set_image: false, k3s_import_cmd: '', wait: false, timeout: '',
      ssh_host: '', ssh_user: '', ssh_port: '', ssh_key_path: '', ssh_image: '', ssh_container: '', ssh_run_args: '', ssh_probe_port: '', ssh_pull: false, ssh_transfer: false, cloud_provider: '',
      probe_enabled: false, probe_method: 'GET', probe_url: '', probe_urls: '', probe_headers: '', probe_body: '', probe_expected_status: '', probe_body_contains: '', probe_timeout: '',
    }
    if (!project) return base
    const b = project.build || {}
    const d = project.deploy || {}
    const pr = project.probe || {}
    // A scanned draft carries build.context == "" (browser cannot expose the real
    // path); honor it so the field shows blank and the user is forced to fill the
    // absolute path. Only a plain "new project" (no project) defaults to ".".
    const ctx = b.context != null ? b.context : ''
    return {
      ...base,
      name: project.name, description: project.description || '', repository: project.repository || '', branch: project.branch || '',
      context: ctx, dockerfile: b.dockerfile || 'Dockerfile', image_repo: b.image_repo || '',
      tag_strategy: b.tag_strategy || 'timestamp', push: !!b.push,
      build_args: b.build_args ? Object.entries(b.build_args).map(([k, v]) => k + '=' + v).join('\n') : '',
      method: d.method || 'kubectl-apply', kubeconfig: d.kubeconfig || '', namespace: d.namespace || '',
      manifest_path: d.manifest_path || '', deployment: d.deployment || '', container: d.container || '',
      chart_path: d.chart_path || '', release_name: d.release_name || '',
      helm_image_key: d.helm_image_key || '', helm_set_image: !!d.helm_set_image,
      k3s_import_cmd: d.k3s_import_cmd || '',
      ssh_host: d.ssh_host || '', ssh_user: d.ssh_user || '', ssh_port: d.ssh_port || '',
      ssh_key_path: d.ssh_key_path || '', ssh_image: d.ssh_image || '',
      ssh_container: d.ssh_container || '', ssh_run_args: d.ssh_run_args || '', ssh_probe_port: d.ssh_probe_port || '', ssh_pull: !!d.ssh_pull, ssh_transfer: !!d.ssh_transfer, cloud_provider: '',
      wait: !!d.wait, timeout: d.timeout || '',
      probe_enabled: !!pr.enabled, probe_method: pr.method || 'GET', probe_url: pr.url || '',
      probe_urls: pr.urls ? Object.entries(pr.urls).map(([k, v]) => k + '=' + v).join('\n') : '',
      probe_headers: pr.headers ? Object.entries(pr.headers).map(([k, v]) => k + '=' + v).join('\n') : '',
      probe_body: pr.body || '', probe_expected_status: pr.expected_status ? String(pr.expected_status) : '',
      probe_body_contains: pr.body_contains || '', probe_timeout: pr.timeout || '',
    }
  })
  // Named publish targets (multi-cloud). Each mirrors the deploy fields so a
  // project can be rolled out to several hosts/clouds. Empty means single target.
  const [targets, setTargets] = useState(() => {
    if (!project || !project.targets) return []
    return project.targets.map((t) => ({
      name: t.name || '',
      method: t.method || 'ssh',
      ssh_host: t.ssh_host || '', ssh_user: t.ssh_user || '', ssh_port: t.ssh_port || '',
      ssh_key_path: t.ssh_key_path || '', ssh_image: t.ssh_image || '', ssh_container: t.ssh_container || '',
      ssh_run_args: t.ssh_run_args || '', ssh_probe_port: t.ssh_probe_port || '',
      ssh_pull: !!t.ssh_pull, ssh_transfer: !!t.ssh_transfer,
      kubeconfig: t.kubeconfig || '', namespace: t.namespace || '', manifest_path: t.manifest_path || '',
      deployment: t.deployment || '', container: t.container || '',
      chart_path: t.chart_path || '', release_name: t.release_name || '',
      helm_image_key: t.helm_image_key || '', helm_set_image: !!t.helm_set_image,
      k3s_import_cmd: t.k3s_import_cmd || '', cloud_provider: t.cloud_provider || 'custom',
    }))
  })
  const [err, setErr] = useState('')
  // Lazily opened "manage deploy targets" modal — keeps the main form short.
  const [showTargets, setShowTargets] = useState(false)

  // True when this is a scanned draft whose context still needs the absolute path.
  const ctxMissing = !!project && !form.context.trim()

  const set = (k) => (e) => {
    const t = e.target
    const v = t.type === 'checkbox' ? t.checked : t.value
    setForm((f) => ({ ...f, [k]: v }))
  }

  // Per-target editors operate on the `targets` array (indexed).
  const updateTarget = (i, k) => (e) => {
    const v = e.target.type === 'checkbox' ? e.target.checked : e.target.value
    setTargets((ts) => ts.map((t, j) => (j === i ? { ...t, [k]: v } : t)))
  }
  const addTarget = () => setTargets((ts) => [...ts, {
    name: '', method: 'ssh', cloud_provider: 'custom',
    ssh_host: '', ssh_user: '', ssh_port: '', ssh_key_path: '', ssh_image: '',
    ssh_container: '', ssh_run_args: '', ssh_probe_port: '', ssh_pull: false, ssh_transfer: false,
    kubeconfig: '', namespace: '', manifest_path: '', deployment: '', container: '',
    chart_path: '', release_name: '', helm_image_key: '', helm_set_image: false, k3s_import_cmd: '',
  }])
  const removeTarget = (i) => setTargets((ts) => ts.filter((_, j) => j !== i))
  // 打开项目时若尚无发布目标，直接把 .env 里配置的腾讯云（CICD_SSH_*）写成一个
  // 「腾讯云」目标，无需任何「导入」按钮——用户开「管理发布目标」就能看到它并已预填，
  // 可继续改。值来自后端 GET /api/config/ssh-defaults（即 .env），保证与配置同步。
  const didInitTargets = useRef(false)
  useEffect(() => {
    if (didInitTargets.current) return
    didInitTargets.current = true
    if (targets.length > 0) return
    API.sshDefaults().then((d) => {
      if (!d || !d.host) return
      setTargets((ts) => [...ts, {
        name: '腾讯云', method: 'ssh', cloud_provider: 'tencent',
        ssh_host: d.host || '', ssh_user: d.user || '', ssh_port: d.port || '',
        ssh_key_path: d.key_path || '',
        ssh_image: '', ssh_container: '', ssh_run_args: '', ssh_probe_port: '',
        ssh_pull: false, ssh_transfer: false,
        kubeconfig: '', namespace: '', manifest_path: '', deployment: '', container: '',
        chart_path: '', release_name: '', helm_image_key: '', helm_set_image: false, k3s_import_cmd: '',
      }])
    }).catch(() => {})
  }, [])

  const submit = async (e) => {
    e.preventDefault()
    setErr('')
    if (!form.context.trim()) {
      setErr('请填写构建上下文路径（用浏览器目录选择器时无法自动获取真实路径，需填服务器可访问的绝对路径，如 /Users/you/code/hello-node）')
      return
    }
    const payload = {
      name: form.name.trim(),
      description: form.description.trim(),
      repository: form.repository.trim(),
      branch: form.branch.trim(),
      build: {
        context: form.context.trim() || '.',
        dockerfile: form.dockerfile.trim() || 'Dockerfile',
        image_repo: form.image_repo.trim(),
        tag_strategy: form.tag_strategy,
        push: form.push,
        build_args: parseKV(form.build_args),
      },
      deploy: {
        method: form.method,
        kubeconfig: form.kubeconfig.trim(),
        namespace: form.namespace.trim(),
        manifest_path: form.manifest_path.trim(),
        deployment: form.deployment.trim(),
        container: form.container.trim(),
        chart_path: form.chart_path.trim(),
        release_name: form.release_name.trim(),
        helm_image_key: form.helm_image_key.trim(),
        helm_set_image: form.helm_set_image,
        k3s_import_cmd: form.k3s_import_cmd.trim(),
        ssh_host: form.ssh_host.trim(),
        ssh_user: form.ssh_user.trim(),
        ssh_port: form.ssh_port.trim(),
        ssh_key_path: form.ssh_key_path.trim(),
        ssh_image: form.ssh_image.trim(),
        ssh_container: form.ssh_container.trim(),
        ssh_run_args: form.ssh_run_args.trim(),
        ssh_probe_port: form.ssh_probe_port.trim(),
        ssh_pull: form.ssh_pull,
        ssh_transfer: form.ssh_transfer,
        wait: form.wait,
        timeout: form.timeout.trim(),
      },
      probe: {
        enabled: form.probe_enabled,
        method: form.probe_method,
        url: form.probe_url.trim(),
        urls: parseKV(form.probe_urls),
        headers: parseKV(form.probe_headers),
        body: form.probe_body,
        expected_status: form.probe_expected_status ? parseInt(form.probe_expected_status, 10) : 0,
        body_contains: form.probe_body_contains.trim(),
        timeout: form.probe_timeout.trim(),
      },
      targets: targets
        .filter((t) => t.name.trim())
        .map((t) => ({
          name: t.name.trim(),
          method: t.method,
          kubeconfig: t.kubeconfig.trim(),
          namespace: t.namespace.trim(),
          manifest_path: t.manifest_path.trim(),
          deployment: t.deployment.trim(),
          container: t.container.trim(),
          chart_path: t.chart_path.trim(),
          release_name: t.release_name.trim(),
          helm_image_key: t.helm_image_key.trim(),
          helm_set_image: t.helm_set_image,
          k3s_import_cmd: t.k3s_import_cmd.trim(),
          ssh_host: t.ssh_host.trim(),
          ssh_user: t.ssh_user.trim(),
          ssh_port: t.ssh_port.trim(),
          ssh_key_path: t.ssh_key_path.trim(),
          ssh_image: t.ssh_image.trim(),
          ssh_container: t.ssh_container.trim(),
          ssh_run_args: t.ssh_run_args.trim(),
          ssh_probe_port: t.ssh_probe_port.trim(),
          ssh_pull: t.ssh_pull,
          ssh_transfer: t.ssh_transfer,
        })),
    }
    if (!payload.name) { setErr('请填写名称'); return }
    try {
      if (isEdit) await API.updateProject(project.id, payload)
      else await API.createProject(payload)
      onSaved()
    } catch (e) { setErr('保存失败: ' + e.message) }
  }

  return (
    <React.Fragment>
      <Modal onClose={onCancel}>
      <div className="modal-card form-wide">
        <div className="modal-head">
          <h3>{isEdit ? '编辑项目' : '新建项目'}</h3>
          <button type="button" className="btn btn-ghost btn-sm" onClick={onCancel}>✕</button>
        </div>
        <div className="modal-body">
          {notes && notes.length > 0 && (
            <div className="scan-notes">
              <strong>目录自动探测说明：</strong>
              <ul>
                {notes.map((n, i) => (<li key={i}>{n}</li>))}
              </ul>
            </div>
          )}
          <form onSubmit={submit}>
            <fieldset>
              <legend>基本信息</legend>
              <div className="form-row">
                <label><span className="lbl-text">名称<span className="req">*</span></span><input className="input" value={form.name} onChange={set('name')} required /></label>
                <label><span className="lbl-text">分支</span><input className="input" placeholder="main" value={form.branch} onChange={set('branch')} /></label>
              </div>
              <label>描述 <input className="input" value={form.description} onChange={set('description')} /></label>
              <label>仓库 <input className="input" placeholder="https://github.com/you/app" value={form.repository} onChange={set('repository')} /></label>
            </fieldset>

            <fieldset>
              <legend>构建 (Docker)</legend>
              <label>上下文路径
                <input className="input" placeholder={ctxMissing ? '请填写绝对路径，如 /Users/you/code/hello-node' : '.'}
                  value={form.context} onChange={set('context')}
                  style={ctxMissing ? { borderColor: 'var(--warning, #b45309)' } : undefined} />
              </label>
              {ctxMissing && (
                <p className="muted" style={{ fontSize: 12, color: 'var(--warning, #b45309)', marginTop: -4 }}>
                  ⚠️ 浏览器目录选择器无法获取真实路径，必须填写服务器实际可访问的绝对路径，否则构建会报「path not found」。
                </p>
              )}
              <div className="form-grid">
                <label>Dockerfile <input className="input" placeholder="Dockerfile" value={form.dockerfile} onChange={set('dockerfile')} /></label>
                <label>标签策略
                  <select className="input" value={form.tag_strategy} onChange={set('tag_strategy')}>
                    <option value="timestamp">timestamp</option>
                    <option value="git-sha">git-sha</option>
                    <option value="manual">manual</option>
                  </select>
                </label>
              </div>
              <label>镜像仓库 <input className="input" placeholder="registry.example.com/myapp" value={form.image_repo} onChange={set('image_repo')} /></label>
              <label className="checkbox"><input type="checkbox" checked={form.push} onChange={set('push')} /> 构建后推送镜像</label>
              <label>构建参数 (KEY=VAL 每行一个)
                <textarea className="input" rows="3" placeholder={'BUILD_ENV=prod\nVERSION=1.0'} value={form.build_args} onChange={set('build_args')} />
              </label>
            </fieldset>

            <fieldset>
              <legend>发布 (Deploy)</legend>
              <label>方式
                <select className="input" value={form.method} onChange={set('method')}>
                  <option value="kubectl-apply">kubectl-apply</option>
                  <option value="kubectl-set-image">kubectl-set-image</option>
                  <option value="helm">helm</option>
                  <option value="local-k3s">local-k3s（纯本地：自动建 Deployment + set-image）</option>
                  <option value="ssh">ssh（裸机 Docker，无 k8s）</option>
                </select>
              </label>
              {form.method !== 'ssh' && (
                <div className="form-grid">
                  <label>kubeconfig 路径 <input className="input" placeholder="留空用默认" value={form.kubeconfig} onChange={set('kubeconfig')} /></label>
                  <label>命名空间 <input className="input" placeholder="default" value={form.namespace} onChange={set('namespace')} /></label>
                  <label>清单路径 <input className="input" placeholder="deploy/prod.yaml" value={form.manifest_path} onChange={set('manifest_path')} /></label>
                  <label>Deployment 名 <input className="input" placeholder="myapp" value={form.deployment} onChange={set('deployment')} /></label>
                  <label>容器名 <input className="input" placeholder="myapp" value={form.container} onChange={set('container')} /></label>
                </div>
              )}
              {form.method === 'local-k3s' && (
                <React.Fragment>
                  <p className="muted" style={{ fontSize: 12, lineHeight: 1.5 }}>
                    local-k3s：发布/Deploy 时自动①若 Deployment 不存在则先 apply 清单路径②kubectl set-image 到本地镜像。
                    若集群已能直接看到 docker 构建的镜像（如 OrbStack 自动共享），留空即可；纯 k3s 需填导入命令。
                  </p>
                  <label>k3s 镜像导入命令（留空=跳过）
                    <input className="input" placeholder="k3s ctr images import -" value={form.k3s_import_cmd} onChange={set('k3s_import_cmd')} />
                  </label>
                </React.Fragment>
              )}
              {form.method === 'helm' && (
                <div className="form-grid">
                  <label>Chart 路径 <input className="input" placeholder="charts/myapp" value={form.chart_path} onChange={set('chart_path')} /></label>
                  <label>Release 名 <input className="input" placeholder="myapp-prod" value={form.release_name} onChange={set('release_name')} /></label>
                  <label>镜像 values 键 <input className="input" placeholder="image.repository" value={form.helm_image_key} onChange={set('helm_image_key')} /></label>
                  <label className="checkbox"><input type="checkbox" checked={form.helm_set_image} onChange={set('helm_set_image')} /> 用 --set 注入镜像</label>
                </div>
              )}
              {form.method === 'ssh' && (
                <React.Fragment>
                  <p className="muted" style={{ fontSize: 12, lineHeight: 1.5 }}>
                    ssh 部署目标是一台装了 Docker 的裸机（无 k8s）。两种分发镜像方式：
                    ① 勾选「免仓库直传」——本地 docker save → scp 到远端 → docker load，<b>无需任何镜像仓库</b>（推荐个人场景）；
                    ② 不勾选则需开启「构建后推送镜像」推到 ACR/TCR 等仓库，远端自动 docker pull。SSH 登录建议用密钥。
                  </p>
                  <div className="subgroup">
                    <div className="subgroup-title">连接</div>
                    <label>云厂商（选后自动预填典型参数，可改）
                      <select
                        className="input"
                        value={form.cloud_provider || 'custom'}
                        onChange={(e) => {
                          const v = e.target.value
                          set('cloud_provider')(e)
                          const preset = CLOUD_PRESETS[v]
                          if (preset) {
                            setForm((f) => ({
                              ...f,
                              ssh_port: preset.port || f.ssh_port,
                              ssh_user: preset.user || f.ssh_user,
                              ssh_probe_port: preset.probe_port || f.ssh_probe_port,
                            }))
                          }
                        }}
                      >
                        <option value="custom">自定义</option>
                        <option value="tencent">腾讯云</option>
                        <option value="alibaba">阿里云</option>
                        <option value="aws">AWS</option>
                        <option value="huawei">华为云</option>
                      </select>
                    </label>
                    <label className="checkbox"><input type="checkbox" checked={form.ssh_transfer} onChange={set('ssh_transfer')} /> 免仓库直传（本地 save → scp → 远端 load）</label>
                    <div className="form-grid">
                      <label>主机 <input className="input" placeholder="192.168.1.10 或 example.com" value={form.ssh_host} onChange={set('ssh_host')} /></label>
                      <label>用户 <input className="input" placeholder="root" value={form.ssh_user} onChange={set('ssh_user')} /></label>
                      <label>端口 <input className="input" placeholder="22" value={form.ssh_port} onChange={set('ssh_port')} /></label>
                      <label>探针端口（宿主发布端口，容器固定 8080）<input className="input" placeholder="8080" value={form.ssh_probe_port} onChange={set('ssh_probe_port')} /></label>
                    </div>
                    <label>私钥路径（留空用默认 ssh 配置）<input className="input" placeholder="~/.ssh/id_rsa" value={form.ssh_key_path} onChange={set('ssh_key_path')} /></label>
                  </div>
                  <div className="subgroup">
                    <div className="subgroup-title">镜像与运行</div>
                    <div className="form-grid">
                      <label>镜像（留空=构建出的 image_ref）<input className="input" placeholder="hello-cicd（本地标签，直传用）" value={form.ssh_image} onChange={set('ssh_image')} /></label>
                      <label>容器名（留空=不命名）<input className="input" placeholder="hello-cicd" value={form.ssh_container} onChange={set('ssh_container')} /></label>
                    </div>
                    <label>docker run 额外参数（不含 -p，宿主端口由上方探针端口决定）
                      <input className="input" placeholder="-e ENV=prod -v /data:/data" value={form.ssh_run_args} onChange={set('ssh_run_args')} />
                    </label>
                  </div>
                  <div className="subgroup">
                    <div className="subgroup-title">选项</div>
                    <label className="checkbox"><input type="checkbox" checked={form.ssh_pull} onChange={set('ssh_pull')} /> 部署前先 docker pull（仅非直传模式有效）</label>
                  </div>
                </React.Fragment>
              )}
              <label className="checkbox"><input type="checkbox" checked={form.wait} onChange={set('wait')} /> 等待就绪</label>
              <label>超时 <input className="input" placeholder="120s" value={form.timeout} onChange={set('timeout')} /></label>
            </fieldset>

            <fieldset>
              <legend>发布目标（可选）</legend>
              <p className="muted" style={{ fontSize: 12, lineHeight: 1.5 }}>
                上方「发布」区即默认单目标配置。需要把同一份代码发到多台主机/云时（如「腾讯云-prod」「AWS-staging」），点下方按钮管理命名发布目标，每个目标自带完整发布配置，保存后在卡片「发布 ▾」下拉里选对应目标即可。
              </p>
              <div className="targets-bar">
                <span className="muted" style={{ fontSize: 13 }}>
                  {targets.length > 0
                    ? `已配置 ${targets.length} 个发布目标：${targets.map((t) => `${t.name?.trim() || '未命名'}(${t.ssh_probe_port?.trim() || '8080'})`).join('、')}`
                    : '未配置额外目标（仅用主配置）'}
                </span>
                <button type="button" className="btn btn-ghost btn-sm" onClick={() => setShowTargets(true)}>管理发布目标</button>
              </div>
            </fieldset>

            <fieldset>
              <legend>服务探测 (Probe · 可选)</legend>
              <p className="muted" style={{ fontSize: 12, lineHeight: 1.5 }}>
                部署成功后自动探测服务是否可用（也可在卡片点「检查服务」手动探测，类 Postman）。
                留空 URL 时尝试从 k8s Service 推导（NodePort/LoadBalancer）。
              </p>
              {form.method === 'ssh' && (
                <p className="muted" style={{ fontSize: 12, lineHeight: 1.5, color: 'var(--warning, #b45309)' }}>
                  ssh 部署不会从 Service 推导地址，请在下方「按部署方式覆盖探针 URL」填 ssh=http://&lt;主机公网IP&gt;:8080（或用上方全局 URL）。
                </p>
              )}
              <label className="checkbox"><input type="checkbox" checked={form.probe_enabled} onChange={set('probe_enabled')} /> 启用服务探测</label>
              <div className="form-grid">
                <label>方法
                  <select className="input" value={form.probe_method} onChange={set('probe_method')}>
                    <option value="GET">GET</option>
                    <option value="POST">POST</option>
                    <option value="PUT">PUT</option>
                    <option value="DELETE">DELETE</option>
                    <option value="HEAD">HEAD</option>
                    <option value="PATCH">PATCH</option>
                  </select>
                </label>
                <label>期望状态码（默认 200）<input className="input" placeholder="200" value={form.probe_expected_status} onChange={set('probe_expected_status')} /></label>
                <label>超时（默认 5s）<input className="input" placeholder="5s" value={form.probe_timeout} onChange={set('probe_timeout')} /></label>
              </div>
              <label>URL（留空=自动推导）<input className="input" placeholder="http://localhost:8080/healthz" value={form.probe_url} onChange={set('probe_url')} /></label>
              <label>按部署方式覆盖探针 URL（可选，KEY=VAL 每行一个；如 ssh=http://1.2.3.4:8080）
                <textarea className="input" rows="2" placeholder={'ssh=http://1.2.3.4:8080\nlocal-k3s=http://localhost:30196'} value={form.probe_urls} onChange={set('probe_urls')} />
              </label>
              <label>请求头 (KEY=VAL 每行一个)
                <textarea className="input" rows="2" placeholder={'Authorization=Bearer xxx'} value={form.probe_headers} onChange={set('probe_headers')} />
              </label>
              <label>请求体（POST/PUT 用）
                <textarea className="input" rows="2" placeholder={'{"key":"value"}'} value={form.probe_body} onChange={set('probe_body')} />
              </label>
              <label>响应须包含文本（可选）<input className="input" placeholder="ok" value={form.probe_body_contains} onChange={set('probe_body_contains')} /></label>
            </fieldset>

            {err && <p className="muted" style={{ color: 'var(--danger)' }}>{err}</p>}
            <div className="form-actions">
              <button type="button" className="btn btn-ghost" onClick={onCancel}>取消</button>
              <button type="submit" className="btn btn-primary">保存</button>
            </div>
          </form>
        </div>
      </div>
    </Modal>
      {showTargets && (
        <Modal onClose={() => setShowTargets(false)}>
          <div className="modal-card">
            <div className="modal-head">
              <h3>管理发布目标（多云）</h3>
              <button type="button" className="btn btn-ghost btn-sm" onClick={() => setShowTargets(false)}>✕</button>
            </div>
            <div className="modal-body">
              <p className="muted" style={{ fontSize: 12, lineHeight: 1.5 }}>
                每个命名发布目标自带完整发布配置，保存后在卡片「发布 ▾」下拉里选对应目标即可。
              </p>
              {targets.map((t, i) => (
                <div className="target-card" key={i}>
                  <div className="target-head">
                    <strong>目标 {i + 1}</strong>
                    <button type="button" className="btn btn-ghost btn-sm" onClick={() => removeTarget(i)}>✕ 删除</button>
                  </div>
                  <label>名称 <input className="input" placeholder="如 腾讯云-prod" value={t.name} onChange={updateTarget(i, 'name')} /></label>
                  <label>方式
                    <select className="input" value={t.method} onChange={updateTarget(i, 'method')}>
                      <option value="kubectl-apply">kubectl-apply</option>
                      <option value="kubectl-set-image">kubectl-set-image</option>
                      <option value="helm">helm</option>
                      <option value="local-k3s">local-k3s</option>
                      <option value="ssh">ssh（裸机 Docker）</option>
                    </select>
                  </label>
                  {t.method === 'ssh' && (
                    <React.Fragment>
                      <label>云厂商（选后自动预填典型参数）
                        <select
                          className="input"
                          value={t.cloud_provider || 'custom'}
                          onChange={(e) => {
                            const v = e.target.value
                            updateTarget(i, 'cloud_provider')(e)
                            const preset = CLOUD_PRESETS[v]
                            if (preset) {
                              setTargets((ts) => ts.map((x, j) => j === i
                                ? { ...x, ssh_port: preset.port || x.ssh_port, ssh_user: preset.user || x.ssh_user, ssh_probe_port: preset.probe_port || x.ssh_probe_port }
                                : x))
                            }
                          }}
                        >
                          <option value="custom">自定义</option>
                          <option value="tencent">腾讯云</option>
                          <option value="alibaba">阿里云</option>
                          <option value="aws">AWS</option>
                          <option value="huawei">华为云</option>
                        </select>
                      </label>
                      <label className="checkbox"><input type="checkbox" checked={t.ssh_transfer} onChange={updateTarget(i, 'ssh_transfer')} /> 免仓库直传（本地 save → scp → 远端 load）</label>
                      <label>主机 <input className="input" placeholder="1.2.3.4 或 host" value={t.ssh_host} onChange={updateTarget(i, 'ssh_host')} /></label>
                      <label>用户 <input className="input" placeholder="root" value={t.ssh_user} onChange={updateTarget(i, 'ssh_user')} /></label>
                      <label>端口 <input className="input" placeholder="22" value={t.ssh_port} onChange={updateTarget(i, 'ssh_port')} /></label>
                      <label>私钥路径 <input className="input" placeholder="~/.ssh/id_rsa" value={t.ssh_key_path} onChange={updateTarget(i, 'ssh_key_path')} /></label>
                      <label>镜像（留空=构建出的 image_ref）<input className="input" placeholder="hello-cicd" value={t.ssh_image} onChange={updateTarget(i, 'ssh_image')} /></label>
                      <label>容器名（留空=不命名）<input className="input" placeholder="hello-cicd" value={t.ssh_container} onChange={updateTarget(i, 'ssh_container')} /></label>
                      <label>docker run 额外参数（不含 -p，宿主端口由探针端口决定）<input className="input" placeholder="-e ENV=prod -v /data:/data" value={t.ssh_run_args} onChange={updateTarget(i, 'ssh_run_args')} /></label>
                      <label>探针端口（宿主发布端口，容器固定 8080）<input className="input" placeholder="8080" value={t.ssh_probe_port} onChange={updateTarget(i, 'ssh_probe_port')} /></label>
                      <label className="checkbox"><input type="checkbox" checked={t.ssh_pull} onChange={updateTarget(i, 'ssh_pull')} /> 部署前 docker pull</label>
                    </React.Fragment>
                  )}
                  {t.method !== 'ssh' && (
                    <React.Fragment>
                      <label>kubeconfig 路径 <input className="input" placeholder="留空用默认" value={t.kubeconfig} onChange={updateTarget(i, 'kubeconfig')} /></label>
                      <label>命名空间 <input className="input" placeholder="default" value={t.namespace} onChange={updateTarget(i, 'namespace')} /></label>
                      <label>清单路径 <input className="input" placeholder="deploy/prod.yaml" value={t.manifest_path} onChange={updateTarget(i, 'manifest_path')} /></label>
                      <label>Deployment 名 <input className="input" placeholder="myapp" value={t.deployment} onChange={updateTarget(i, 'deployment')} /></label>
                      <label>容器名 <input className="input" placeholder="myapp" value={t.container} onChange={updateTarget(i, 'container')} /></label>
                      {t.method === 'helm' && (
                        <React.Fragment>
                          <label>Chart 路径 <input className="input" placeholder="charts/myapp" value={t.chart_path} onChange={updateTarget(i, 'chart_path')} /></label>
                          <label>Release 名 <input className="input" placeholder="myapp-prod" value={t.release_name} onChange={updateTarget(i, 'release_name')} /></label>
                          <label>镜像 values 键 <input className="input" placeholder="image.repository" value={t.helm_image_key} onChange={updateTarget(i, 'helm_image_key')} /></label>
                          <label className="checkbox"><input type="checkbox" checked={t.helm_set_image} onChange={updateTarget(i, 'helm_set_image')} /> 用 --set 注入镜像</label>
                        </React.Fragment>
                      )}
                      {t.method === 'local-k3s' && (
                        <label>k3s 导入命令 <input className="input" placeholder="k3s ctr images import -" value={t.k3s_import_cmd} onChange={updateTarget(i, 'k3s_import_cmd')} /></label>
                      )}
                    </React.Fragment>
                  )}
                </div>
              ))}
              <button type="button" className="btn btn-ghost btn-sm" onClick={addTarget}>＋ 添加发布目标</button>
              <div className="form-actions">
                <button type="button" className="btn btn-primary" onClick={() => setShowTargets(false)}>完成</button>
              </div>
            </div>
          </div>
        </Modal>
      )}
    </React.Fragment>
  )
}
