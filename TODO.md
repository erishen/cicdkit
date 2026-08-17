# TODO：部署到云机 + 配域名 + Webhook 触发

> 状态：规划中（未实施）
> 背景：当前 cicdkit 是**本地优先**平台——构建源是本地目录、触发靠 UI 手动点。要变成"push 即构建"的 CI，
> 需要解决两件事：① 让平台跑在公网可达的云机上并配域名；② 加 GitHub webhook 端点让 push 能自动触发 pipeline。
> 参考：`ARCHITECTURE.md`（部署方式对比 / 安全模型章节）。当前 8 个示例全部 `local-k3s`。

---

## 一、部署平台到云机 + 配域名

### 1.1 前置准备
- [ ] 确认云机已安装 `docker`；若继续 `local-k3s` 闭环，确认已装 `k3s` / `k3d`
- [ ] 云机**安全组**只放行 `443`（HTTPS）与 `22`（SSH，仅密钥登录）
- [ ] 准备域名（如 `cicd.<你的域名>`）并把 A 记录解析到 `<你的云机公网IP>`

### 1.2 构建与运行
- [ ] 在云机 `docker build -t cicdkit:latest .`（根目录已有多阶段 Dockerfile）
- [ ] 运行容器：挂载 `docker.sock`（构建/部署需要）、挂载 `.env`、映射端口
- [ ] **关闭 `AUTO_TOKEN` 便利模式**（公网不能自动注入 token）
- [ ] 确认监听地址经 nginx 反代，**不要直曝后端端口**（当前默认本机绑定需调整）

### 1.3 反向代理 + TLS
- [ ] 安装 `nginx`
- [ ] 配置反代到后端 `:8080`，强制 HTTPS（HTTP 301 跳转）
- [ ] Let's Encrypt 证书（`certbot` 或 `acme.sh`，配置自动续期）

### 1.4 验证
- [ ] `curl https://<你的域名>/api/health` 可达
- [ ] 浏览器登录 + API Token 鉴权正常、知识库/LLM 诊断可用

---

## 二、Webhook 触发

### 2.1 后端新增端点
- [ ] 新增 `POST /api/webhook/github`
- [ ] **HMAC 校验** `X-Hub-Signature-256`（密钥存 `.env`，如 `GITHUB_WEBHOOK_SECRET`，防伪造）
- [ ] 解析 payload：`repository` / `ref`(branch) / `head_commit` / `pusher`
- [ ] 按 `repo + branch` 匹配项目并启动 run（复用现有 `pipeline` 模块）

### 2.2 构建阶段支持 git clone
- [ ] 当前 `build` 只 build 本地目录（`build_path`）；需新增 `git clone <repository>` 到临时目录
- [ ] 支持按 `ref`/`branch` checkout
- [ ] 可选：浅克隆 + 依赖缓存，加速重复构建

### 2.3 在 GitHub 配置 webhook
- [ ] 仓库 `Settings → Webhooks`：Payload URL = `https://<你的域名>/api/webhook/github`
- [ ] Content-Type = `application/json`，Secret 与 `.env` 一致
- [ ] 触发事件：先 `push`（PR / tag 按需再加）
- [ ] 可选：webhook 路径加 **GitHub IP 白名单**

### 2.4 验证
- [ ] `git push` 后平台自动起 pipeline（Build → Deploy → Probe）
- [ ] 失败时在 UI / 日志可见，LLM 诊断可用

---

## 三、安全加固清单（**必做，非可选**）

> cicdkit 在云机上能直接 exec `docker` / `kubectl` / `ssh`，暴露公网攻击面大于普通 Web 服务。

- [ ] 全站 HTTPS，HTTP 跳转；证书自动续期
- [ ] 防火墙仅 `443` + `SSH(密钥)`；关掉其余端口
- [ ] 保留 **API Token 鉴权**；禁用 `AUTO_TOKEN`
- [ ] webhook **HMAC 校验** + 可选 IP 白名单
- [ ] `docker.sock` 仅在容器内挂载，不外溢到宿主机其他进程
- [ ] 密钥走 `.env`，绝不提交（`.gitignore` 已覆盖 `.env` / `*.local.json` / `data/*.json`）
- [ ] 可选：容器运行用户非 root、`fail2ban`、定期系统更新

---

## 四、待定决策（需用户拍板）

| 决策点 | 选项 |
|---|---|
| 域名与证书 | 自有域名开子域 + Let's Encrypt / 自带证书 / 暂用 IP + 自签 |
| k3s 位置 | 同云机 `local-k3s` 闭环 / 部署到其他 K8s 集群 / 先只跑平台本身 |
| GitHub 仓改名 | 是否同步把仓改名 `erishen/cicdkit`（示例 `project.json` 的 `repository` 已指向 `cicdkit`） |
| 触发方式 | 仅 `push` / 加 `PR` / 加 `tag` |
| 执行方式 | 我生成全套部署产物（Dockerfile/nginx/TLS/服务/DNS 指引）你在本机跑 / 你给我云机 SSH 访问 |

---

## 五、最小可行路径（建议起步）

1. 域名 A 记录 → 云机 IP
2. 云机 `docker build` + 运行 cicdkit（关 AUTO_TOKEN、挂 docker.sock、`.env`）
3. nginx + Let's Encrypt 反代 `:8080`
4. 先手动验证平台在公网可用
5. 再加 `POST /api/webhook/github`（HMAC + git clone）+ GitHub webhook 配置
6. 最终 `git push` 自动触发

> 注：通过 webhook 让平台从"本地一键发布器"升级为"push 即构建"的 CI，最关键的两条是
> ① 加一个接收端点并校验签名；② 让 build 阶段支持 `git clone` 而非只 build 本地路径；
> ③ 解决"GitHub 如何打到本机"的网络可达性（域名 + 公网 IP）。
