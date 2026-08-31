# CI/CD 平台 · Docker 构建与 k3s 发布管理

一个**零外部依赖**（仅使用 Go 标准库）的单机 CI/CD 管理平台，用于：

- 定义项目（Git 仓库 + Docker 构建 + k3s 发布配置）
- 构建并推送 Docker 镜像
- 通过 `kubectl` / `helm` 向 **k3s** 集群发布
- 通过 Web UI 与 REST API 触发、查看实时日志、取消运行、审计部署历史
- 接收 Git 平台 Webhook 自动触发流水线

> 后端仅用标准库（`net/http` + `encoding/json` + `os/exec` + `go:embed`），**前端为 Vite + React**（独立 `cmd/server/web` 工程，`npm run build` 产出 `dist/` 由 Go 嵌入单二进制）。

---

## 架构

```
cmd/server/main.go          # 入口：embed web 资源 + 启动 HTTP 服务
internal/config             # 配置（JSON 文件 + 环境变量覆盖）
internal/store              # 数据模型 + 文件型 JSON 存储（原子写）
internal/build              # Docker 构建 / 推送引擎
internal/deploy             # kubectl / helm 发布引擎
internal/pipeline           # 流水线编排（并发限制 + 取消 + 实时日志）
internal/api                # REST API + 静态资源
cmd/server/web/            # Vite + React 前端工程（构建产物 dist/ 由 Go 嵌入）
```

数据以 `store.json` 文件持久化（内存 + 原子写 `.tmp` 再 `rename`），无需数据库。

---

## 环境要求

| 工具 | 用途 | 说明 |
|------|------|------|
| Go 1.22+ | 编译后端 | 仅标准库，无需联网拉依赖 |
| Node 20+ | 构建前端 | `cmd/server/web` 需 `npm install && npm run build` 产出 `dist/` |
| `docker` | 构建 / 推送镜像 | 运行时需要可访问的 Docker daemon |
| `kubectl` | 向 k3s 发布 | 通过 kubeconfig 对接集群 |
| `helm`（可选） | Helm 方式发布 | 仅当部署方式选 `helm` 时需要 |
| k3s 集群 | 发布目标 | 提供 kubeconfig 即可 |

---

## 快速开始（本地）

```bash
# 1. 构建前端 + 编译后端（前端需 Node 20+，产出 dist/ 并嵌入二进制）
make build            # 等价于：cd cmd/server/web && npm install && npm run build && go build

# 2. （可选）生成示例配置
make init            # 写入 ./data/config.json

# 3. 运行（默认已绑 127.0.0.1:8080，无需 -addr）
./bin/server -config ./data/config.json
# 本机开发无 Token 也能用；若要局域网/公网访问：
#   ./bin/server -addr 0.0.0.0:8080 并务必设 API_TOKEN=你的强令牌
# 或后台用 make run（同样默认绑本机）

# 4. 打开浏览器
open http://localhost:8080
```

> 若只改了后端 Go 代码，可单独 `go build`；但若 `cmd/server/web/dist` 不存在，
> 会显示占位页——此时先跑一次 `make web` 构建前端即可。

### 前端开发模式（热更新）

前后端分离开发：一个终端起 Go 后端，另一个终端起 Vite 开发服务器（HMR），
Vite 已配置把 `/api` 代理到 `:8080`，无跨域问题：

```bash
make run          # 终端 A：Go 后端 :8080
make dev-web      # 终端 B：Vite :5173（浏览器打开 http://localhost:5173）
```

健康检查：

```bash
curl http://localhost:8080/api/health
# {"service":"cicdkit","status":"ok"}
```

---

## 配置

配置来源优先级：**命令行 `-config` 文件 < 环境变量覆盖**。

| 配置项 | 环境变量 | 默认值 | 说明 |
|--------|----------|--------|------|
| `server.addr` | `PORT` / `SERVER_ADDR` | `127.0.0.1:8080` | 监听地址（**默认仅绑本机**；绑到全网卡且未设 `API_TOKEN` 等于开放命令执行） |
| `server.api_token` | `API_TOKEN` | `""` | API 鉴权令牌；为空则 `/api` 完全开放（仅建议本机开发）。非本机绑定时务必设置 |
| `server.auto_token` | `AUTO_TOKEN` | `false` | 启用后启动时**自动生成一个一次性 API Token**（无需手动配置 `API_TOKEN`）。生成的 Token 会注入 Web UI，本机浏览器打开页面即自动鉴权；同时打印到日志供其他设备/脚本使用。已显式设置 `API_TOKEN` 时不覆盖。Token 每次重启重新生成，不落盘 |
| `server.webhook_secret` | `WEBHOOK_SECRET` | `""` | Webhook 校验密钥（为空则不校验） |
| `data_dir` | `DATA_DIR` | `./data` | 数据目录 |
| `store_file` | `STORE_FILE` | `./data/store.json` | 存储文件 |
| `runner.max_concurrent` | `MAX_CONCURRENT` | `2` | 并发流水线数 |
| `runner.docker_host` | `DOCKER_HOST` | `""` | 传给 docker 的 `DOCKER_HOST` |
| `runner.default_kubeconfig` | `KUBECONFIG` | `""` | 默认 kubeconfig 路径 |
| `runner.work_dir` | — | `./work` | 工作目录 |

示例见 [`config.example.json`](./config.example.json)。

---

## 项目模型

每个项目包含构建与发布两段定义：

### BuildSpec（Docker 构建）

| 字段 | 说明 |
|------|------|
| `context` | 构建上下文路径（如 `.`） |
| `dockerfile` | Dockerfile 文件名（如 `Dockerfile`） |
| `image_repo` | 镜像仓库（如 `registry.example.com/myapp`） |
| `tag_strategy` | `timestamp` / `git-sha` / `manual` |
| `push` | 构建后是否推送 |
| `build_args` | 构建参数 `KEY=VAL` |
| `target` / `platforms` | 多阶段目标 / 跨平台构建 |

### DeploySpec（发布）

`method` 五选一：

- **`local-k3s`**（推荐，纯本地无 registry）：可选把镜像导入 k3s containerd（由 `k3s_import_cmd` 控制，留空则跳过——OrbStack 等会自动共享 docker 镜像），若目标 Deployment 不存在且配了 `manifest_path` 则先 `kubectl apply`，再 `kubectl set image`。**UI 里 Build → Pipeline 一步到位**。
- **`kubectl-apply`**：`kubectl apply -f <manifest_path>`
- **`kubectl-set-image`**：`kubectl set image deployment/<deployment> <container>=<image_ref>`
- **`helm`**：`helm upgrade --install <release_name> <chart_path>`（可选 `--set <helm_image_key>=<image_ref>`）
- **`ssh`**（裸机 Docker，无 k8s）：通过 `ssh` 登录一台装了 Docker 的主机并 `docker run -d` 启动容器。镜像分发有两种方式：
  - **免仓库直传（推荐个人/单机场景）**：勾选 `ssh_transfer` 后，平台在本地 `docker save` 镜像 → `scp` 传到远端 → 远端 `docker load` → `docker run`。**无需任何镜像仓库、无需开启「构建后推送镜像」**，只需远端装了 Docker 且本机 ssh 能登上去。
  - **仓库 pull**：不勾选 `ssh_transfer` 时，远端先（可选）`docker pull` 再 run，需开启「构建后推送镜像」把镜像推到 ACR/TCR 等仓库，或镜像已在远端存在。
  - `ssh_image` 留空时复用构建出的 `image_ref`（即 `build.image_repo:tag`，本地构建即存在）。

`ssh` 相关字段：`ssh_host`（必填，可来自 `CICD_SSH_HOST`）、`ssh_user`（必填，可来自 `CICD_SSH_USER`）、`ssh_port`（默认 22）、`ssh_key_path`（可选，留空用默认 ssh 配置）、`ssh_image`（可选，覆盖 `image_ref`）、`ssh_container`（可选，容器名）、`ssh_run_args`（可选，`docker run` 额外参数如 `-p 8080:8080 -e ENV=prod`）、`ssh_probe_port`（可选，服务探测端口，默认 8080）、`ssh_pull`（非直传模式下是否部署前 pull）、`ssh_transfer`（免仓库直传）。这些值禁止包含 shell 元字符（`|;&$`><` 等），以防远端命令注入。

**连接信息放 `.env`（推荐）**：`ssh_host` / `ssh_user` / `ssh_port` / `ssh_key_path` / `ssh_container` / `ssh_run_args` / `ssh_probe_port` 这几个字段若在项目 JSON 里留空，会在部署（及服务探测）时自动从同名环境变量补全（`CICD_SSH_HOST` / `CICD_SSH_USER` / `CICD_SSH_PORT` / `CICD_SSH_KEY_PATH` / `CICD_SSH_CONTAINER` / `CICD_SSH_RUN_ARGS` / `CICD_SSH_PROBE_PORT`）。把真实主机 IP、密钥路径、用户名等写进项目根目录的 `.env`（参考 `.env.example`），项目 JSON 里**完全不写真实主机信息**，换机器只需改 `.env`。服务启动时会自动读取 `.env` 与 `.env.local`（后者优先），真实环境变量始终优先于文件。校验也会认这些环境变量，所以留空的字段不会因「必填缺失」被拒绝。**ssh 部署的服务探测地址同样由此推导**（`http://<CICD_SSH_HOST>:<ssh_probe_port>`，默认 8080），因此 `probe.url` 留空也能自动探测，无需在 JSON 里硬编码主机 IP。

其它通用字段：`kubeconfig`、`namespace`、`wait`、`timeout`、`k3s_import_cmd` 等。

---

## 从目录导入（新建项目的快捷方式）

不想一项项手填？点 UI 顶部的「**从目录导入**」：选一个本地项目目录，平台自动识别并生成一份预填好的项目配置，你在表单里确认/补充后保存即可。

- **路径输入框**：粘贴服务器可访问的目录**绝对路径**（如 `/Users/you/code/myapp`）。平台在后端遍历该目录，构建上下文、Dockerfile、清单路径都设为绝对路径，可直接用于构建与 `kubectl`。
- **「选择目录…」按钮**：调用浏览器原生目录选择器（仅 Chrome/Edge 支持）。受浏览器安全限制，前端**读不到目录的绝对路径**，因此探测出的配置里「构建上下文 / 清单路径」只填了相对名，保存前需改成服务器实际可访问的路径（弹出的探测说明里会提示）。其他浏览器请直接用路径输入框。

探测会识别（尽力而为，识别不到的留空并在说明里提示）：

| 识别项 | 来源 |
|------|------|
| 语言 / 默认镜像名 | `go.mod` / `package.json` 等；否则用目录名 |
| `build.context` / `dockerfile` | 找到 `Dockerfile` 即设为对应绝对路径 |
| 端口提示 | `Dockerfile` 的 `EXPOSE` |
| 部署方式 / 清单 / Deployment / 容器 / 命名空间 | `*.yaml` 里的 `kind: Deployment` / `Service`（`type: NodePort|LoadBalancer|ClusterIP`） |
| 服务探测 | 检测到集群 `Service` 时自动启用 GET 探测，`url` 留空交由探针从 Service 推导；无 k8s 清单仅有 Dockerfile 时默认 `ssh` 免仓库直传 |

对应接口：`POST /api/projects/scan`，请求体二选一——`{"path":"<绝对目录>"}`（后端遍历磁盘）或 `{"root":"<目录名>","files":[{"path":"Dockerfile","content":"..."},...]}`（浏览器读到的文件内容）；返回 `{"project":<草稿>, "notes":[...]}`。该接口**只生成草稿、不落库**。

---

## REST API

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/health` | 健康检查 |
| GET | `/api/projects` | 项目列表 |
| POST | `/api/projects` | 新建项目 |
| POST | `/api/projects/scan` | 从目录探测草稿（见下「从目录导入」） |
| GET / PUT / DELETE | `/api/projects/{id}` | 项目详情 / 更新 / 删除 |
| POST | `/api/projects/{id}/build` | 仅构建（含推送） |
| POST | `/api/projects/{id}/pipeline` | 构建 + 推送 + 发布 |
| POST | `/api/projects/{id}/deploy` | 仅发布（需 `image_ref` 或 `tag`） |
| GET / POST | `/api/projects/{id}/validate` | 配置校验 / 试跑（检查配置合法性、Docker daemon、Kubeconfig 可达性、服务可用性，**不构建不部署**） |
| GET / POST | `/api/projects/{id}/probe` | 服务可用性探测（类似 Postman：发 HTTP 请求，返回状态码 / 耗时 / 响应头 / 响应体） |
| GET | `/api/runs?project_id=` | 运行列表 |
| GET / POST | `/api/runs/{id}` / `/api/runs/{id}/cancel` | 运行详情 / 取消 |
| GET | `/api/deployments?project_id=` | 部署历史 |
| POST | `/api/webhook/{id}?secret=` | Webhook 触发流水线 |

触发请求体示例：

```bash
# 手动触发流水线
curl -X POST http://localhost:8080/api/projects/<id>/pipeline \
  -H 'Content-Type: application/json' -d '{"tag":"v1.2.3"}'

# 仅发布已有镜像
curl -X POST http://localhost:8080/api/projects/<id>/deploy \
  -H 'Content-Type: application/json' \
  -d '{"image_ref":"registry.example.com/myapp:v1.2.3"}'

# 单次运行覆盖部署方式（同项目发布到不同目标，如本地 k3s vs ssh 裸机）
# 留空或不传 method 则沿用项目存储的 deploy.method
curl -X POST http://localhost:8080/api/projects/<id>/pipeline \
  -H 'Content-Type: application/json' -d '{"method":"ssh"}'
```

---

## Webhook（Git 平台自动触发）

在 Git 平台（GitHub / GitLab / Gitea 等）配置 Webhook，指向：

```
POST /api/webhook/<project_id>?secret=<WEBHOOK_SECRET>
# 或请求头 X-Webhook-Secret: <WEBHOOK_SECRET>
```

收到请求后会自动触发一次 `pipeline`（构建 + 推送 + 发布）。
未设置 `WEBHOOK_SECRET` 时不校验。

---

## Docker 部署（含 k3s 发布）

镜像运行时内置 `docker` CLI 与 `kubectl`。**构建镜像需访问 Docker daemon**，推荐通过挂载宿主 Docker socket 复用（避免 docker-in-docker）：

```bash
docker build -t cicdkit:latest .
docker run -d --name cicdkit -p 8080:8080 \
  -e API_TOKEN=你的强令牌 \
  -v $(pwd)/data:/app/data \
  -v $HOME/.kube/config:/app/kubeconfig:ro \
  -v /var/run/docker.sock:/var/run/docker.sock \
  cicdkit:latest \
  -addr :8080 -config /app/data/config.json
```

> 容器内部 `-addr :8080` 配合 `API_TOKEN` 是安全组合：端口映射由宿主控制，
> 未持令牌者无法调用任何 `/api` 接口（含创建项目、触发部署，后者会在宿主机执行 docker/kubectl）。

- 挂载 `/var/run/docker.sock` 让容器内的 `docker` 复用宿主 daemon
- 挂载 kubeconfig（`-v $HOME/.kube/config:/app/kubeconfig:ro`），并在项目 DeploySpec 或 `KUBECONFIG` 环境变量中指向该路径
- 数据目录挂载到卷，保证项目与运行记录持久化

### 对接 k3s

k3s 默认生成 kubeconfig 于 `/etc/rancher/k3s/k3s.yaml`。将其复制到管理机后，
把 `server` 字段里的 `127.0.0.1` 改为 k3s 节点可达地址，然后：

```bash
export KUBECONFIG=/path/to/k3s.yaml
# 或直接在项目 DeploySpec.kubeconfig 指定该文件路径
```

随后在 Web UI 中为目标项目配置 `local-k3s` / `kubectl-set-image` / `kubectl-apply` 即可一键发布（`local-k3s` 为纯本地无 registry 闭环，最省事）。

---

## 开发

```bash
make vet       # 后端静态检查
make test      # 单元测试
make run       # 仅起 Go 后端（:8080）
make dev-web   # 仅起 Vite 前端开发服务器（:5173，代理 /api 到 :8080）
make web       # 仅构建前端（npm install && npm run build -> dist/）
make build     # 前端 + 后端完整构建
make image     # 构建镜像（镜像内已完成前端构建与嵌入）
```

### 启用 Git Hooks（密钥防泄漏）

仓库内置 `hooks/pre-commit`（gitleaks 密钥扫描），但 `core.hooksPath` 写在本地 `.git/config`、**不随仓库提交**，所以每个新 clone 都需先跑一次：

```bash
./scripts/setup-hooks.sh
```

脚本会设好 `git config core.hooksPath hooks` 并提示是否安装 gitleaks。本机装了 `gitleaks` 后，每次 `git commit` 会自动扫描暂存文件，发现密钥即阻止提交（详见 `.gitleaks.toml` 放行规则）。紧急绕过：`git commit --no-verify`。

---

## 服务探测 (Probe · 可选)

平台内置「类 Postman」的服务可用性探测。每个项目可在设置中开启 `probe`，部署成功后会**自动探测**服务是否可用，结果（状态码 / 耗时 / 响应头 / 响应体）写入**部署历史**；也可在卡片点「检查服务」**手动探测**。探测**不构建、不部署**，只发一个 HTTP 请求。

`probe` 配置字段（`project.probe`）：

| 字段 | 说明 |
|------|------|
| `enabled` | 是否启用 |
| `method` | HTTP 方法，默认 `GET`（GET/POST/PUT/DELETE/HEAD/PATCH/OPTIONS） |
| `url` | 目标地址（全局）。**留空**时会尝试从 k8s Service 推导（`NodePort` → 依次尝试节点 `ExternalIP`、节点 `InternalIP`、最后 `localhost`，取第一个能连通的；`LoadBalancer` → ingress IP；`ClusterIP` 宿主机不可直连则跳过并说明）。本地 VM 类集群（OrbStack / Rancher Desktop / Lima）的 NodePort 发布在节点 `InternalIP`（VM IP）而非 `localhost`，故推导会优先试它。**`ssh` 部署不会从 Service 推导**——此时要么填本字段，要么用下方 `urls` 按部署方式指定 |
| `urls` | **按部署方式覆盖探针地址**（可选），`{"<部署方式>": "<地址>"}` 形式，如 `{"ssh": "http://1.2.3.4:8080"}`。解析优先级：`urls[当前部署方式]` > `url` > 集群自动推导。让**同一个项目**发布到不同目标（本地 k3s 自动推导 + 裸机 ssh 显式地址）都能自动探活 |
| `headers` | 请求头（`KEY=VALUE` 形式） |
| `body` | 请求体（POST/PUT 用） |
| `expected_status` | 期望状态码，默认 `200` |
| `body_contains` | 响应体须包含的字符串（可选，用于校验健康响应） |
| `timeout` | 超时，默认 `5s` |

探测结果 `status`：`ok`（达标）/ `fail`（状态码或内容不符）/ `skip`（未启用或地址无法推导）/ `err`（请求失败）。

> 自动探测只作为**可用性证据**写入部署历史，**不会**让部署本身失败（即便服务还没就绪，部署已成功）。

---

## 智能诊断与知识库（LLM · 可选）

构建 / 部署失败时，可点运行记录上的「AI 诊断」让大模型分析失败日志。该能力**默认关闭、完全可选**，不配置也不影响其余功能。

### 配置 LLM

两种方式，UI 优先级高于 `.env`：

- **UI（推荐）**：顶栏「⚙ 设置」填写 `Base URL` / `API Key` / `Model` 并勾选「启用」。`API Key` 仅写入本地 `data/llm.json`（权限 0600，已 gitignore），GET 接口永不回明文；留空则在测试连接 / 诊断时回退使用已存值。**可点「测试连接」** 用最小请求验证连通，避免等到真实失败才发现配错。
- **`.env`**：用 `CICD_LLM_ENABLED` / `CICD_LLM_BASE_URL` / `CICD_LLM_API_KEY` / `CICD_LLM_MODEL` 兜底（字段缺省时 UI 自动展示这些值）。系统提示词**不需要**环境变量——它由代码内置常量提供（中文 CI/CD 故障分析模板），UI 可查看 / 修改 / 清空。
- 系统提示词：内置模板留空即启用；也可在设置里自定义覆盖。

### 知识库积累

- 诊断完成后可「✓ 采纳并加入知识库」：结论沉淀到 `data/kb.json`，按「项目 + 阶段 + 归一化错误」去重合并，重复采纳累加次数。
- 顶栏「📚 知识库」可查看全部已采纳条目，支持**关键字搜索**与**逐条移除**。
- **复用**：再次诊断同类失败时，若命中历史条目则直接复用其结论（不再调用模型），弹窗标注「📚 复用知识库」；同时展示「历史同类诊断」供参考。每次采纳都会强化该知识库条目。
- 同一失败运行重复点「AI 诊断」返回缓存结果（`⚡ 本次为缓存结果`），不再调模型。

### 生成缺失的 Dockerfile / k8s 清单

准备发布的项目若缺少 `Dockerfile` 或 `k8s/deployment.yaml`，可在卡片或新建表单点「生成缺失文件」：平台先按语言检测（Go / Node / Python …）预览将要写入的多阶段 `Dockerfile` 与 `Deployment`/`Service` 清单，确认后再写入构建上下文目录。模板对齐 `examples/hello-cicd`——`local-k3s` 生成的清单可直接 `kubectl apply`。

---

## 安全提示

> ⚠️ 本平台会在**宿主机**执行 `docker` / `kubectl`，因此「能调 API」≈「能在宿主机跑命令」。
> 下面每一项都是为了防止这个能力被未授权者拿到。

- **API 鉴权（必看）**：所有 `/api/*` 接口（除 `/api/health` 与 `/api/webhook/*`）都需 `API_TOKEN`：
  请求头 `X-API-Token: <令牌>` 或 `Authorization: Bearer <令牌>`。未配置 `API_TOKEN` 时
  `/api` 完全开放——**仅限本机开发**。一旦把 `server.addr` 绑到非本机（如 `0.0.0.0`），
  务必同时设置 `API_TOKEN`，否则等价于开放远程命令执行（RCE）。
- **一键免配鉴权（`auto_token`）**：若嫌手动设 `API_TOKEN` 麻烦，可设 `AUTO_TOKEN=1`（或
  `server.auto_token: true`）。启动时会用 `crypto/rand` 生成一个一次性强令牌，注入 Web UI——
  **本机浏览器打开页面即自动鉴权，无需手动输入**；Token 同时打印到日志，供其他设备/脚本使用。
  该 Token 每次重启重新生成、不落盘；若已显式配置 `API_TOKEN` 则不覆盖。注意：自动 Token 随
  页面一起下发给浏览器，因此适合「本机/受信局域网」使用；若把地址绑到公网，仍建议用固定的
  强 `API_TOKEN` 而非依赖自动 Token。
- **默认仅绑本机**：`server.addr` 默认 `127.0.0.1:8080`。需要局域网/公网访问时用
  `./bin/server -addr 0.0.0.0:8080` 并设 `API_TOKEN=强令牌`。启动时会检测「非本机 + 无 Token」
  并在日志告警，但不阻断（兼容既有容器环境）。
- **镜像导入命令白名单（防 RCE）**：`deploy.k3s_import_cmd` 只能调用容器运行时二进制
  （`k3s` / `k3d` / `ctr` / `crictl` / `nerdctl` / `microk8s`，可加 `sudo` 前缀），
  拒绝 shell 元字符与任意命令。留空表示跳过导入。越界的命令在创建/更新项目时会被 `400` 拒绝。
- **密码脱敏**：`build.registry.password` 存入后，任何 `GET` 接口返回都替换为 `********`；
  UI 回写时若密码为空或仍是掩码则沿用旧值，不会因来回编辑把密码抹掉。
- `webhook_secret` 建议在生产环境必填，避免未授权触发。
- 不要将含镜像仓库密码的 `config.json` 提交到版本库（已列入 `.gitignore`）。
- 通过挂载宿主 Docker socket 时，容器实质拥有宿主 Docker 控制权，请仅在受信环境使用。

---

## 示例项目

仓库内置多个最小可运行示例，全部覆盖 **build → 本地发布到 k3s** 的完整闭环（一个零依赖 HTTP 服务 + Dockerfile + k8s 清单 + 平台项目定义，默认 `local-k3s` 纯本地无 registry）。每个示例都用来验证平台对对应语言的构建 / 发布 / 探针闭环，可同时导入同一台平台、各自独立发布到本地 k3s。

| 示例 | 语言 | 构建方式 | 容器内端口 | 备注 |
|------|------|----------|------------|------|
| `hello-cicd` | Go 1.22 | 多阶段（golang → alpine） | 8080 | 零依赖 `net/http`，基准示例 |
| `hello-node` | Node.js 24 (Active LTS) | 单阶段（node:alpine） | 8080 | 验证解释型语言闭环 |
| `hello-java` | Java 21 (Maven/Gradle) | 多阶段（maven / eclipse-temurin） | 8080 | 刻意无 Dockerfile，演示「生成缺失文件」 |
| `hello-rust` | Rust 1.85 | 多阶段（rust:slim 编译 → debian 运行） | 8080 | 零依赖 `std::net` |
| `hello-ruby` | Ruby 3.3 | 单阶段（ruby:alpine） | 8080 | 零依赖 `socket` |
| `hello-php` | PHP 8.3 | 单阶段（php:cli-alpine 内置服务器） | 8080 | `php -S` 内置服务器，无 Apache |
| `hello-dotnet` | .NET 8 | 多阶段（SDK 编译 → aspnet 运行） | 8080 | Kestrel / ASP.NET Core 最小 API |
| `hello-python` | Python 3.12 | 单阶段（python:slim） | 8080 | 零依赖 `http.server` |

所有示例的 `Dockerfile` 均锁定完整补丁版本（如 `ruby:3.3.8-alpine`），保证构建可复现；运行阶段统一以非 root 用户启动（`debian` 系用 `nobody` / `useradd`，`alpine` 系用 `adduser`）。各示例镜像体积（解压后，`docker images` 实测）约：ruby 75 MB、rust 76–98 MB、php 96–105 MB、python 132–157 MB、dotnet 218–250 MB（含 ASP.NET Core 框架，最大）。

```bash
make dev         # 启动平台（后端 :8080 + 前端 :5173）
make example     # 把 hello-cicd 示例项目导入运行中的服务
# 浏览器打开 http://localhost:8080 即可看到示例卡片
# 在卡片上点 Build → Pipeline，平台自动建 Deployment + set-image，全程无需敲命令行
```

完整使用说明（含 OrbStack 自动共享镜像 / 纯 k3s 导入差异、占位值替换、触发 Pipeline、其它语言示例导入、方案 A 推真实仓库、生成缺失文件等）见 [`examples/README.md`](./examples/README.md)。

## 相关文章
- [cicdkit 工程实践：一个用纯 Go 标准库打造的本地 CI/CD 单二进制平台](https://erishen.cn/cicdkit/)
