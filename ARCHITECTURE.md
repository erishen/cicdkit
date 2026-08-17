# CI/CD 发布管理平台 — 架构文档

> 本文档描述 `cicdkit` 的整体架构、模块职责、数据流、安全模型与运维要点。
> 面向需要理解、二次开发或排障的开发者。功能使用说明见 `README.zh.md` / `README.md`。

---

## 1. 定位与范围

`cicdkit` 是一个**单机 / 本地优先**的 CI/CD 发布管理平台，把「容器构建 → 发布到目标」这一闭环收敛到一个 Go 后端进程 + 一个 React 单页前端，无需外部数据库或消息队列。

它的核心目标是让「构建一个镜像并发布到本地 k3s / 远程裸机 / 集群」这一动作可以从一个 Web UI 里按按钮完成，而不用手敲 `docker build` / `kubectl` / `ssh` 一长串命令。平台本身**直接在本机执行 `docker`、`kubectl`、`ssh` 等外部命令**，因此它被设计成默认只监听本机回环地址，并强制 API Token 保护。

平台不解决「代码仓库 CI」（不替你跑单测、不打 Git tag），而是解决「我已经有个可构建的目录，怎么把它稳定地发到目标环境」。

---

## 2. 整体架构

```
┌──────────────────────────────────────────────────────────────────┐
│                          浏览器 (React SPA)                        │
│  App.jsx + components/*  ·  api.js(X-API-Token / AUTO_TOKEN 注入)  │
└───────────────┬──────────────────────────────────────────────────┘
                │  HTTP /api/*   (静态资源 /、/api/health 开放)
                ▼
┌──────────────────────────────────────────────────────────────────┐
│                     Go 后端 (cmd/server)                          │
│                                                                    │
│  api.Server ── 路由 / 鉴权(withAuth) / SPA 回退(+ token 注入)      │
│      │   handlers.go：projects / runs / deployments /            │
│      │              llm / kb / webhook / diagnose                │
│      ▼                                                            │
│  pipeline.Runner ── 并发信号量 · 取消 · 阶段编排                   │
│      ├─ build.Run        docker build / buildx(多架构) / push     │
│      ├─ deploy.Run       local-k3s | ssh | kubectl-* | helm      │
│      ├─ probe.Run        Postman 式 HTTP 服务探测                 │
│      ├─ scan.Scan*       本地目录 → 项目草稿                       │
│      └─ generate.*       缺失 Dockerfile/清单 脚手架              │
│                                                                    │
│  store  (jsonStore：项目/运行/部署 落盘 store.json)                │
│  kb     (知识库：采纳的诊断 → kb.json)                            │
│  llm    (可选 OpenAI 兼容 诊断，配置落 llm.json)                  │
│  config (配置加载 + 环境变量覆盖 + 本机绑定校验)                  │
└───────────────┬──────────────────────────────────────────────────┘
                │  exec.CommandContext → 复用宿主工具链
                ▼
   ┌─────────────────────────────────────────────────────────┐
   │  docker · kubectl · ssh · k3s ctr / crictl · helm        │
   └───────┬───────────────┬──────────────────┬──────────────┘
           ▼               ▼                  ▼
     本地 Docker     k3s containerd      远程裸机 Docker
     守护进程           (镜像导入)          (ssh docker load/run)
```

**关键设计取向**

- **零三方依赖**：后端仅用 Go 标准库（`go.mod` 只有 `module` 与 `go 1.22` 两行）。外部能力全部通过 `exec.CommandContext` 调用宿主机已安装的 `docker` / `kubectl` / `ssh` 等 CLI。这意味着没有供应链依赖风险、没有 cgo 编不过的麻烦，代价是「平台能力 ≈ 宿主机工具链能力」。
- **进程内全栈**：前端以 `embed.FS` 的形式编译进同一个 Go 二进制，生产运行无需单独的 Web 服务器。
- **文件即数据库**：状态用三个 JSON 文件持久化，无外部存储。适合单机，迁移=拷文件。
- **命令执行即能力边界**：所有「发布」最终都是拼一条命令交给宿主执行；因此安全模型（第 8 节）围绕「谁能触发命令 + 命令能是什么」展开。

---

## 3. 技术栈

| 层 | 技术 | 说明 |
|---|---|---|
| 后端语言 | Go 1.22 | 仅标准库 |
| 前端 | React 18 + Vite 5 | 无 UI 框架以外的依赖（手写 CSS） |
| 静态资源 | `//go:embed all:web/dist` | 构建时把 `dist` 编入二进制 |
| 持久化 | JSON 文件（原子 rename） | `data/store.json` / `llm.json` / `kb.json` |
| 外部工具 | docker / kubectl / ssh / k3s ctr / helm | 宿主机提供，平台不内置 |
| 鉴权 | API Token（Bearer / X-API-Token） + 可选 AUTO_TOKEN | 见第 8 节 |

---

## 4. 目录结构

```
cicdkit/
├── cmd/server/
│   ├── main.go              # 入口：加载配置→建 store→建 runner→嵌前端→启动
│   └── web/                 # React 前端源码（npm run build → dist）
│       ├── src/
│       │   ├── App.jsx       # 主应用：列表、无限滚动、UI_BUILD 戳
│       │   ├── api.js        # 前端 API 客户端 + 鉴权（tokenPrompting / autoToken）
│       │   └── components/   # ProjectForm / ScanModal / LogModal / ValidateModal /
│       │                      #   ProbeModal / GenerateFilesModal / SettingsModal /
│       │                      #   DiagnoseModal / KnowledgeModal / Modal
│       └── dist/            # 构建产物（被 Go 嵌入）
├── internal/
│   ├── api/                 # HTTP 层：server.go(路由/鉴权/SPA) + handlers.go
│   ├── config/              # 配置模型 + 环境变量覆盖 + 本机绑定/路径校验
│   ├── store/               # 数据模型(models.go) + 文件存储(store.go) + 校验/脱敏(validate.go)
│   ├── pipeline/            # Runner：并发/取消/阶段编排/部署目标解析
│   ├── build/               # docker build / buildx 多架构 / push / login
│   ├── deploy/              # 5 种部署方式实现
│   ├── scan/                # 本地目录启发式扫描 → 项目草稿
│   ├── generate/            # 缺失 Dockerfile / k8s 清单 脚手架
│   ├── probe/               # Postman 式 HTTP 服务探测
│   ├── check/               # dry-run 校验（配置/daemon/kubeconfig/上下文/探测）
│   ├── kb/                  # 知识库：采纳的诊断条目
│   └── llm/                 # 可选 OpenAI 兼容诊断客户端
├── examples/                # 8 个可发布示例（见第 11 节）
├── scripts/                 # kill-ports.sh / import-to-k3s.sh / seed-example.sh
├── Makefile                 # 构建/运行/测试/镜像目标
├── Dockerfile               # 多阶段构建平台自身
├── .dockerignore           # 排除 .env / data/ 进入镜像构建上下文
├── .gitignore               # 忽略 .env / data/*.json / 含真实 IP 的 project-tencent-cloud.json
└── config.example.json / .env.example
```

---

## 5. 后端模块设计

### 5.1 `config` — 配置与绑定安全
- `Config` 含 `Server` / `DataDir` / `StoreFile` / `Runner` 四块。
- `Load` 先读 JSON 文件，再用**真实环境变量覆盖**（`.env` 通过 `LoadDotEnv()` 在 `main` 最早阶段注入）。
- `ServerConfig.IsLoopback()` 判断监听地址是否仅本机；`main.go` 据此**强制**：非本机绑定必须设 `API_TOKEN`，否则直接 `log.Fatalf` 拒绝启动——因为本平台会在宿主执行命令，暴露给公网等于开放命令执行。
- `FSRoots` / `FSPathAllowed` 限制服务器侧目录浏览器的可访问范围，默认仅当前工作目录，避免枚举家目录。

### 5.2 `store` — 数据模型与持久化
- **模型**（`models.go`）：`Project`（含 `BuildSpec` / `DeploySpec` / `ProbeSpec` / `Targets`）、`Run`（含 `Stages []StageResult`、`Diagnosis`、`Probe`）、`Deployment`、`DeployTarget`、`RunDiagnosis`、`ProbeResult`。
- **存储**（`store.go`）：`jsonStore` 全量内存 + 延迟合并落盘（200ms 窗口，避免构建过程中每阶段一次写整文件）；写入走**临时文件 + 原子 rename**，文件权限 `0600`（含密钥）。`MaxRuns=200` 自动裁剪历史，但**永不淘汰未结束的 run**。
- **向后兼容**：`backfillLastDeploy` / `backfillRunProbe` 在加载时把部署历史回填到项目卡片，并把「探测失败但曾被记为成功」的旧 run 升级为失败。
- **脱敏**（`validate.go`）：`Redacted()` 把 registry 密码、SSH key 路径、kubeconfig 路径替换为 `********` 再经 API 外发；`MergeSecrets()` 保证 UI 的「读取→编辑→保存」往返不会因看不到密钥而误清空。

### 5.3 `api` — HTTP 层与鉴权
- `withAuth` 中间件：静态资源与 `/api/health`、`/api/version` 开放；`/api/webhook/*` 在配置了 `WebhookSecret` 时由自身校验；其余 `/api/*` 走 API Token（Bearer 或 `X-API-Token`，常量时间比较）。
- `serveApp` 提供 SPA：真实文件直接服务，未知路由回退 `index.html`；当启用 `AUTO_TOKEN` 时把一次性 token 注入页面 `<script>`，本机浏览器自动鉴权。
- 路由覆盖：项目增删改查/扫描/校验/探测/生成、运行触发/取消/诊断/采纳、部署历史、LLM 配置与测试、知识库、Webhook。

### 5.4 `pipeline` — 编排核心
- `Runner` 持有**并发信号量**（`MaxConcurrent`，默认 2），`execute` 先抢信号量再跑阶段。
- `launch` 在返回前就先把 run 注册进 `active`（`sync.Map`），使「触发后立即取消」也能找到 run。
- 阶段顺序：`build → (push) → deploy → probe`。**探测失败/不可达会把整次 run 升级为失败**——容器起来了 ≠ 服务可用。
- 部署目标解析：`method` 覆盖（单次运行临时换方式）与命名 `DeployTarget`（多目标如「腾讯云-prod」）二选一；命名目标取用其完整 `DeploySpec`。
- `ssh` + 未配置 registry + 未显式 `ssh_pull` 时自动默认 `ssh_transfer`（免仓库直传）；`ssh_transfer` 到异架构宿主时自动探测远端 `uname -m` 并构建对应架构镜像，避免 QEMU 模拟拖慢。

### 5.5 `build` — 镜像构建
- 单架构走经典 `docker build`；多架构（`Platforms` 含多项）走 `docker buildx build --platform … --output type=oci` 并复用专属 `cicd-multiarch` 容器化 builder。
- 平台解析优先级：`SSHBuildPlatforms` → `Build.Platforms` →（ssh_transfer 单目标时）默认 `linux/amd64`。
- `docker login` 仅在配置了 registry 用户名时执行，凭据优先从 `CICD_REGISTRY_*` 环境变量还原。

### 5.6 `deploy` — 五种部署方式
| 方式 | 机制 | 关键安全/正确性处理 |
|---|---|---|
| `local-k3s` | 可选 `docker save \| k3s ctr import` → `kubectl apply` 清单 → `kubectl set-image` → `rollout status` 等待 | 导入前**预检镜像本地存在**（否则 set-image 假成功、Pod 陷 ImagePullBackOff）；每次部署都 re-apply 清单保证 Service 同步 |
| `ssh`（transfer） | `docker save \| ssh docker load` 流式直传 + 远端 `docker run` | 远端磁盘预检；`-p` 端口由 `SSHProbePort` 独占、剥离用户多余 `-p`；容器名稳定单例 |
| `ssh`（pull） | 远端 `docker pull` + `docker run` | 需配合 `build.push` 把镜像推进 registry |
| `kubectl-apply` | `kubectl apply -f` | 支持 `--wait` / `--timeout` |
| `kubectl-set-image` | `kubectl set-image` | 设 `Wait` 时等待 rollout 真正就绪 |
| `helm` | `helm upgrade --install` | 支持 `--set image=…` 注入镜像 |

> 命名 `DeployTarget` 让单个代码库可以从 UI 一键发到多个云/主机，每个目标自带完整 `DeploySpec`。

### 5.7 `scan` / `generate` — 导入与脚手架
- `scan` 对本地目录做**启发式扫描**：识别 Dockerfile、语言（go/node/python/java/rust…）、k8s 清单，自动抽取 deployment/container/service 类型，并在检测到 `NodePort`/`LoadBalancer` 时**自动开启服务探测**。输出是「预填草稿」，不落盘，交给前端创建表单。
- 浏览器目录选择器拿不到真实绝对路径，`scan.ScanFiles` 会**故意留空构建上下文**并提示用户补全，避免把目录名喂给 `docker build` 报 `path not found`。
- `generate` 在「缺失 Dockerfile 或清单」时生成多阶段模板（按语言），`Plan` 只预览、`Apply` 才写盘并更新项目配置使其立即可发布；**已有文件绝不覆盖**。

### 5.8 `probe` — 服务探测
- Postman 式 HTTP 检查（方法/URL/headers/body/期望状态码/响应包含），部署后自动跑、也可按需触发。
- 目标 URL 解析优先级：ssh 由解析后的连接（`.env` 的 `CICD_SSH_*` + 探针端口，真实 IP 不进项目 JSON）→ 按目标名的 `probe.urls[target]` → 全局 `probe.url` → 集群 Service 自动推导（`NodePort` 依次试 ExternalIP/InternalIP/localhost；`LoadBalancer` 用 ingress IP；`ClusterIP` 不可直连则 skip 并说明）。
- 单次请求在传输错误时重试一次（应对滚动后短暂未监听）。

### 5.9 `check` — dry-run 校验
不构建不发布，只验证：配置合法、docker daemon 可达（尊重 `DOCKER_HOST`）、集群可达（cluster 类方式）、构建上下文存在、服务探测可用。UI 的「校验」按钮即调用它。

### 5.10 `kb` + `llm` — AI 诊断与知识库
- `llm` 是可选 OpenAI 兼容 `/chat/completions` 客户端（配置落 `llm.json`，API Key 永不经 API 明文回传）。诊断前会把日志里的 **IPv4/IPv6 / user@host / 本地路径**正则替换为占位符再外发。
- 失败时「诊断」→ 得到分析 → 可「采纳」写入 `kb`；后续同类错误先匹配知识库复用结论，**省一次 LLM 调用**。知识库按采纳次数排序，相似度匹配取前 3。

---

## 6. 一次 Pipeline 的生命周期

```
UI 点击「发布」
  → POST /api/projects/:id/pipeline  {tag?, method?, target?}
  → runner.TriggerPipeline  → launch 先把 Run 写库并注册 active
  → go execute():
       抢并发信号量 → 解析部署目标/方式 → 处理 ssh_transfer 架构
       build.Run(docker build)            [失败则 run=failed 返回]
       (build.push? → build.Push)
       deploy.Run(按 method 拼命令执行)    [失败则 run=failed 返回]
       probe.Enabled? → probe.Run(HTTP 探测)，fail/err → probeFailed
       SaveDeployment(写部署历史 + 回填项目 LastDeploy)
       run = 成功（除非 probeFailed）
  → 前端轮询 /api/runs/:id 拿阶段日志与状态
  → 失败可「AI 诊断」→ 采纳进 kb
```

取消：`POST /api/runs/:id/cancel` → `runner.Cancel` 调 `runCtl.cancel()` → `ctx` 取消，在途的 `docker`/`kubectl`/`ssh` 进程被杀死。

---

## 7. 前端

- **技术**：React 18 + Vite 5，手写 CSS，无组件库。构建产物 `web/dist` 被 Go `embed` 进后端二进制。
- **鉴权流**（`api.js`）：请求携带 `X-API-Token`（来自 `localStorage`）；401 时弹窗要 token，带 `tokenPrompting` 锁避免并发请求连环弹框；`AUTO_TOKEN` 模式下后端把一次性 token 注入页面全局变量，浏览器静默鉴权。
- **列表**：无限滚动哨兵 + 链式首屏填满，分页走后端 `limit`/`offset`；`UI_BUILD` 戳与后端 `BuildStamp` 同显示在 footer，用于一眼确认浏览器连的是否最新构建。
- **模态组件**：项目表单、目录扫描、运行日志、校验、探测、文件生成、设置（LLM）、AI 诊断、知识库。

---

## 8. 安全模型（重点）

平台在本机执行命令，安全模型围绕「谁能触发命令」与「命令能是什么」：

1. **本机绑定 + Token**：非 loopback 绑定**必须**设 `API_TOKEN`，否则拒绝启动；`AUTO_TOKEN` 生成的一次性 token 会写进前端页面，**仅在本机安全**，外网绑定会拒绝。
2. **镜像导入命令白名单**（`validate.go`）：`k3s_import_cmd` 只允许 `k3s/k3d/ctr/crictl/nerdctl/microk8s`（可加 `sudo` 前缀），拒绝 `|;&$`\`><` 等 shell 元字符——否则它是任意命令执行原语。
3. **路径越界防护**：项目可触碰的文件路径（build.context / Dockerfile / manifest / chart / kubeconfig）必须落在 `Runner.WorkDir` 内（`pathWithinRoot`），防目录穿越。
4. **SSH 字段 shell 元字符防护**：所有会拼进远端 `docker` 命令的 SSH 字段（host/user/container/key/run_args/image）禁止 shell 元字符；远端脚本里 container/image 再单引号兜底（纵深防御）。
5. **密钥脱敏**：API 响应 `Redacted()` 把 registry 密码 / SSH key 路径 / kubeconfig 替换成 `********`；`store.json` 写权限 `0600`；`.gitignore` 已忽略 `.env`、`data/*.json`、含真实云主机 IP 的 `project-tencent-cloud.json`。
6. **LLM 外发日志脱敏**：发往第三方模型的日志先替换 IPv4/IPv6 / `user@host` / `/Users/ /home/ /root/` 路径为占位符，避免泄露内部基建。
7. **fs_roots 限制**：服务器侧目录浏览器仅能在配置的根目录内枚举，默认仅工作目录。

> 注意：沙箱环境无 `.git`，无法核查 git **历史**是否曾提交过敏感文件。若 `.env` / `data/store.json` 曾在被忽略前就被 commit，请用 `git log --all --oneline -- .env data/store.json examples/*/project-tencent-cloud.json` 排查并酌情 `git filter-repo` 清洗 + 轮换密钥。

---

## 9. 数据持久化

| 文件 | 内容 | 权限 | 是否 git 忽略 |
|---|---|---|---|
| `data/store.json` | 项目 / 运行 / 部署历史（含 `ssh_key_path` 路径、registry 密码占位） | 0600 | 是 |
| `data/llm.json` | LLM 配置（含真实 API Key） | 0600 | 是 |
| `data/kb.json` | 采纳的 AI 诊断条目 | 0600 | 是（随 `data/*.json`） |

合并写入用「临时文件 + `os.Rename` 原子替换」，200ms 合并窗口避免高频写。`CICD_REGISTRY_PASSWORD` 配置时，落盘用占位符、运行时再从环境变量还原真实密码，使磁盘上永不留明文密钥。

---

## 10. 构建与运行

- **Makefile**：
  - `make build`：先 `npm run build` 出 `dist`，再 `CGO_ENABLED=1 go build -ldflags=-linkmode=external`（规避 macOS 15 的 `LC_UUID`/dyld 问题）编译嵌入前端二进制的 `bin/server`。
  - `make build-linux` / `build-linux-arm64`：`CGO_ENABLED=0 GOOS=linux` 交叉编译静态单文件，可直接拷到同架构 Linux 运行。
  - `make run` / `dev`：本地运行（`dev` 用 `/api/health` 门控先后端再前端，消除竞态）。
  - `make test`：`go test -race`（并发日志写与取消必须被测出）。
  - `make image` / `k3s-import` / `example`：构建平台镜像 / 导入本地镜像到 k3s / 导入示例项目。
- **Dockerfile（平台自身）**：多阶段（node 构建前端 → golang 编译 → alpine 运行时含 `docker-cli` + `kubectl`）。运行时**挂载宿主 `/var/run/docker.sock`** 复用宿主 daemon，`/app/data` 挂卷持久化。
- **部署平台自身到 k3s**：本质也是「构建镜像 → 导入 → 发布」，与示例项目一致。

---

## 11. 示例项目

`examples/` 含 8 个可发布示例，全部基于 `local-k3s`：

| 示例 | 语言 | 最终镜像基础 | 非 root 做法 | 实测体积* |
|---|---|---|---|---|
| hello-cicd | Go | 静态二进制（极小基础） | — | ~12.6 MB |
| hello-node | Node.js | `node:24-alpine` | `USER node` | ~162 MB |
| hello-java | Java | `eclipse-temurin:21-jre` | JRE 运行 | 207–375 MB |
| hello-rust | Rust | `debian:bookworm-slim` + 二进制 | `USER nobody` | ~76–98 MB |
| hello-ruby | Ruby | `ruby:3.3.8-alpine` | `adduser -D -u 1000 appuser` | ~70–76 MB |
| hello-php | PHP | `php:8.3.8-cli-alpine` | `adduser -D -u 1000 appuser` | ~96–105 MB |
| hello-dotnet | .NET | `aspnet:8.0-bookworm-slim` | `useradd -M -u 1000 appuser` | ~218–250 MB |
| hello-python | Python | `python:3.12.7-slim-bookworm` | `useradd -m -u 1000 appuser` | ~132–157 MB |

\* 实测体积来自宿主 `docker images`，因基础镜像/依赖层/二进制符号略有浮动。

**非 root 要点（踩坑固化）**：Alpine 系（`ruby`/`php` 的 `*-alpine`）用 `adduser`；debian/ubuntu 系（`dotnet`/`python`/`rust` 的 `*-slim` 基于 debian）用 `useradd`；`rust` 的 `debian:slim` 自带 `nobody`。**不要**假设官方 runtime 镜像自带某用户名（如 python 的 `USER python` 不存在），一律显式创建用户。

**瘦身经验**：`hello-php` 从 `php:*-apache` 完整版（400MB+）换 `cli-alpine`（~37MB 下载）小约 10 倍；`rust`/`dotnet` 用多阶段构建把 SDK 巨像挡在最终镜像外。

---

## 12. 扩展性

- **存储**：`store.Store` 是接口，当前 `jsonStore` 满足单机；要上规模可在同接口后换 BoltDB / SQLite，API 与 runner 无需改动。
- **部署方式**：`deploy.Run` 的 `switch spec.Method` 与 `store.IsValidDeployMethod` 是两处需同步的清单点；新增方式加一个分支即可。
- **LLM 后端**：`llm.Client` 仅依赖 OpenAI 兼容 `/chat/completions`，换模型/网关改 `BaseURL` 即可（UI 与推理逻辑与具体模型解耦）。
- **示例**：复制 `examples/hello-cicd` 调整语言与 `project.json` 即可新增演示。

---

## 13. 已知约束与运维注意

- **单节点、依赖宿主工具链**：平台不在容器里自带 docker/kubectl，需宿主机已安装；平台自身若以容器运行则必须挂 `/var/run/docker.sock`。
- **镜像必须导入 k3s**：`local-k3s` 手动路径下需 `docker save | k3s ctr images import`（即 `project.json` 的 `k3s_import_cmd`）；OrbStack 等会自动共享镜像，此时该命令留空即可。漏导入是「set-image 成功但 Pod 卡 ImagePullBackOff」的头号根因。
- **macOS 15 链接坑**：本地 `go build`/`go run`/`go test` 需 `CGO_ENABLED=1 -linkmode=external`，否则二进制因缺 `LC_UUID` 一绑端口即 abort；Linux 交叉编译无此问题（Makefile 已处理）。
- **构建上下文必须是服务器可访问的绝对路径**：浏览器目录选择器拿不到真实路径，创建项目时需手动补全 `build.context`。
- **网络/代理**：国内环境 `npm install` 可能需 `--registry https://registry.npmmirror.com`；Go 纯标准库无此问题。

---

_本文档基于源码静态分析生成，覆盖 `cmd/`、`internal/` 全部模块与 `examples/`、`Makefile`、`Dockerfile`。如需补充某模块的时序图或接口字段表，可单独提出。_
