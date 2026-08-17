# 示例项目：hello-cicd

一个最小可运行的演示。默认走 **local-k3s（纯本地、无 registry）**：本机 `docker build` → 平台自动 `kubectl set-image` 滚动更新，**全程在 UI 里点 Build / Pipeline 即可，不需要手动敲 kubectl 或 make**。

```
examples/hello-cicd/
├── main.go              # 零依赖 Go HTTP 服务（读 PORT / APP_VERSION 环境变量）
├── go.mod               # 独立 module，不干扰主工程
├── Dockerfile           # 多阶段构建（golang:1.22 → alpine）
├── .dockerignore
├── k8s/deployment.yaml  # Deployment + Service（imagePullPolicy: IfNotPresent，占位 hello-cicd:local）
└── project.json         # 交给 cicdkit 的项目定义（snake_case，method=local-k3s）
scripts/
└── import-to-k3s.sh     # 备用逃生舱：手动把平台最新本地镜像导入 k3s containerd
```

## 前置条件

- 本机 `docker` 在跑（构建镜像）、`kubectl` 能连上 k3s（平台用 `kubectl --kubeconfig <...>` 操作集群）。
- `deploy.kubeconfig` 留空时读取服务启动时的 `KUBECONFIG` / 默认 `~/.kube/config`。**OrbStack 用户无需任何设置**——默认 context `orbstack` 就在 `~/.kube/config` 里。
- **关键：镜像对集群可见**。两种方式（平台按 `deploy.k3s_import_cmd` 决定）：
  - **OrbStack**：docker 构建的镜像会被 Kubernetes 自动共享，无需导入。示例 `k3s_import_cmd` 留空即可。
  - **纯 k3s（非 OrbStack）**：docker 与 k3s 的 containerd 不互通，需把 `k3s_import_cmd` 设为 `"k3s ctr images import -"`（或带 `sudo`），平台会在每次部署时 `docker save | k3s ctr import`。

---

## 快速上手（local-k3s，纯 UI 驱动）

### 1. 启动平台 + 导入示例

```bash
make dev              # 后端 :8080 + 前端 :5173
make example          # 把 project.json 导入运行中的服务
# 或手动 POST：
curl -X POST http://localhost:8080/api/projects -H 'Content-Type: application/json' \
  --data-binary @examples/hello-cicd/project.json
```

浏览器打开 http://localhost:8080 看到 `hello-cicd` 卡片。

### 2. 在 UI 里：Build → Pipeline

- 点卡片上的 **Build** → 平台在本机 `docker build` 出 `hello-cicd:<时间戳>` 镜像（不推送任何仓库）。
- 点 **Pipeline**（或 **Deploy**）→ 平台按 `local-k3s` 执行：
  1. 若 `k3s_import_cmd` 非空，先把镜像 `docker save | <导入命令>` 导入 k3s（OrbStack 留空则跳过）；
  2. 若目标 Deployment 不存在且配了 `manifest_path`，先 `kubectl apply -f <清单>` 建好资源；
  3. `kubectl set-image deployment/hello-cicd hello-cicd=hello-cicd:<时间戳> -n default --wait --timeout 120s` 滚动更新。

**也就是说首次部署也无需手动 apply、无需手动导入——UI 点一下就闭环。** 点开运行记录里的日志可实时看到这几步的输出。

### 3. 验证

```bash
kubectl -n default get pods -l app=hello-cicd
kubectl -n default port-forward svc/hello-cicd 8080:80
curl localhost:8080     # -> Hello from hello-cicd (version 1.0.0)
```

---

## 环境差异：OrbStack vs 纯 k3s

| 环境 | `k3s_import_cmd` | 镜像导入 | 说明 |
|------|------------------|----------|------|
| OrbStack（本机默认） | `""`（留空） | 不需要 | docker 镜像自动对 k8s 可见 |
| 纯 k3s（物理机/VM） | `"k3s ctr images import -"` | 平台自动做 | 需 `k3s` 在 PATH；若需 root 用 `"sudo k3s ctr images import -"` |

切换只需改 `project.json` 的 `deploy.k3s_import_cmd` 后重新导入（编辑项目 → 保存）。

---

## 方案 A（可选）：推真实仓库再发布

若要走「构建→推送→k3s 拉取」的标准云原生流程，把 `project.json` 改成：

```json
"build": {
  "image_repo": "docker.io/<你的用户名>/hello-cicd",
  "tag_strategy": "timestamp",
  "push": true,
  "registry": { "server": "https://registry-1.docker.io", "username": "<用户名>", "password": "<密码或 token>" },
  "build_args": { "APP_VERSION": "1.0.0" }
}
```

`k8s/deployment.yaml` 的 `image` 改成对应仓库地址（`imagePullPolicy: IfNotPresent` 或 `Always`）。此模式把 `deploy.method` 改回 `kubectl-set-image` 或 `local-k3s` 均可（local-k3s 的导入步骤因 `k3s_import_cmd` 为空会自动跳过），直接点 Pipeline。

---

## 四种部署方式（无需改代码）

`deploy.method` 支持：

- **`local-k3s`**（示例默认）— 纯本地闭环：可选导入镜像 + 自动建 Deployment（若缺失）+ `set-image`。UI 一步到位。
- **`kubectl-apply`** — 用 `manifest_path` 指向一份清单做声明式发布（镜像 tag 静态，不随构建自动更新）。
  ```json
  "deploy": { "method": "kubectl-apply", "manifest_path": "examples/hello-cicd/k8s/deployment.yaml", "namespace": "default", "wait": true, "timeout": "120s" }
  ```
- **`kubectl-set-image`** — 把刚构建的镜像更新到已存在的 Deployment（不含自动建资源）。
- **`helm`** — 填 `release_name` + `chart_path`，并设 `helm_set_image: true` 让平台把新镜像写进 `image`（或 `helm_image_key` 自定义 key）。

---

## 关于镜像 tag

`tag_strategy` 默认 `timestamp`（每次构建打 `20060102-150405` 时间戳 tag，平台自动生成，UI 直接点）。`manual` 需触发时显式传 tag；`git-sha` 在 build context 内执行 `git rev-parse --short HEAD`（是 git 仓库才生效，否则回退 timestamp）。

## 手动逃生舱（一般无需）

`scripts/import-to-k3s.sh` 是独立的手动导入脚本：读取「最近一次成功构建」的 tag，把对应镜像 `docker save | k3s ctr import` 进 k3s。`make k3s-import` 等价调用它。仅在你想绕开平台的自动导入、或调试镜像可见性时使用。

---

## hello-node（Node.js 版，等价示例）

`examples/hello-node/` 与 `hello-cicd` **功能与流程完全等价**，只是换成 Node.js，用来验证平台对解释型语言的闭环同样跑得通。两个示例可同时导入同一台平台、各自独立发布到本地 k3s 或腾讯云。

```
examples/hello-node/
├── server.js            # 零依赖 Node.js HTTP 服务（读 PORT / APP_VERSION 环境变量）
├── package.json         # 无外部依赖，无需 npm install
├── Dockerfile           # 单阶段 node:24-alpine（当前 Active LTS，支持至 2028-04），非 root（node 用户）运行
├── .dockerignore
├── k8s/deployment.yaml  # Deployment + Service（imagePullPolicy: IfNotPresent，占位 hello-node:local）
└── project.json         # 交给 cicdkit 的项目定义（snake_case，method=local-k3s）
```

与 Go 版的唯一实质差异在镜像构建阶段：

- **没有编译步骤**。`Dockerfile` 直接从 `node:24-alpine`（当前 Active LTS，安全支持到 2028-04）起，只 `COPY package.json server.js`；`APP_VERSION` 同样通过 `build_args` → `ARG` → `ENV` 注入。若要加依赖，在 `COPY` 前补 `COPY package*.json ./ && RUN npm install --omit=dev` 即可。
- **以非 root 运行**：`node:alpine` 镜像自带 `node` 用户，`Dockerfile` 末尾 `USER node`，比 Go 版更省一步（Go 版要 `adduser`）。

其余完全一致：`local-k3s`（纯本地无 registry）/ `ssh` 腾讯云直传、`kubectl apply -f` 每次幂等、探针按部署方式选 URL、NodePort Service 带 `app: hello-node` 标签。导入与触发命令把路径换成 node 版即可：

```bash
curl -X POST http://localhost:8080/api/projects -H 'Content-Type: application/json' \
  --data-binary @examples/hello-node/project.json

# 腾讯云裸机版（ssh 免仓库直传；JSON 不含任何真实主机信息，连接方式全部来自项目根 .env 的 CICD_SSH_*）
curl -X POST http://localhost:8080/api/projects -H 'Content-Type: application/json' \
  --data-binary @examples/hello-node/project-tencent-cloud.json
```

验证（本地 k3s）：

```bash
kubectl -n default get pods -l app=hello-node
kubectl -n default port-forward svc/hello-node 8080:80
curl localhost:8080     # -> Hello from hello-node (version 1.0.0)
```

---

## hello-java（Java/Maven 版，专用于演示「生成缺失文件」）

`examples/hello-java/` 与 `hello-cicd` 流程等价，但**刻意不含 Dockerfile 与 k8s 清单**——用来验证平台的「生成缺失文件」功能：导入后点一下，由平台按 `java-maven` 检测自动生成多阶段 Dockerfile 与 Deployment+Service。

```
examples/hello-java/
├── pom.xml                                    # <finalName>app</finalName>，对齐生成模板的 /app/app.jar
├── src/main/java/com/example/HelloApp.java     # 零依赖 JDK HttpServer，读 PORT/APP_VERSION
├── .dockerignore
└── project.json                               # 指向尚不存在的 Dockerfile / k8s 路径（触发生成）
```

导入并触发生成：

```bash
# 1. 启动平台（后端 :8080 + 前端 :5173）
make dev
# 2. 导入示例（此时还没有 Dockerfile / k8s 清单）
curl -X POST http://localhost:8080/api/projects -H 'Content-Type: application/json' \
  --data-binary @examples/hello-java/project.json
# 3. 在卡片上点「生成缺失文件」→ 预览 java-maven 多阶段 Dockerfile + Deployment/Service → 确认
```

确认后 `examples/hello-java/Dockerfile`（maven:3.9-eclipse-temurin-21 → eclipse-temurin:21-jre，`java -jar /app/app.jar`）与 `examples/hello-java/k8s/deployment.yaml` 即被写入；`project.json` 里原本指向的缺失路径现在指向真实文件，项目立即可走 Build → Pipeline 闭环。

> 想顺手验证 Gradle 分支：把 `pom.xml` 换成 `build.gradle`（`plugins { id 'java' }`，jar 任务产出 `build/libs/*.jar`），「生成缺失文件」会改为生成 `gradle:8-jdk21` 多阶段模板。

> ⚠️ **`java -jar` 要求 jar 带 `Main-Class` 清单项**。普通 Maven 工程要在 `pom.xml` 的 `maven-jar-plugin` 里声明 `<mainClass>com.example.HelloApp</mainClass>`（本示例已配），否则镜像能跑起来却 `CrashLoopBackOff: no main manifest attribute, in /app/app.jar`。Spring Boot 工程靠 `spring-boot-maven-plugin` 的 repackage 自动获得可执行 jar，无需额外配置。

单元用例见 `internal/generate/generate_test.go` 的 `TestPlanAndApplyJava`（覆盖 maven / gradle 两条检测路径）。

---

## 新增多语言示例：hello-rust / hello-ruby / hello-php / hello-dotnet / hello-python

这五个示例与 `hello-node`（以及 `hello-cicd` / `hello-java`）**流程完全等价**——每个都是一个能被 cicdkit Build → Pipeline 的最小 HTTP 服务，仅语言不同，用来验证平台对多种语言的构建 / 发布 / 探针闭环都跑得通。可同时导入同一台平台、各自独立发布到本地 k3s。

| 示例 | 语言 | 构建方式 | 容器内端口 | 备注 |
|------|------|----------|------------|------|
| hello-rust | Rust 1.85 | 多阶段（rust:slim 编译 → debian 运行） | 8080 | 零依赖，仅标准库 `std::net` |
| hello-ruby | Ruby 3.3 | 单阶段（ruby:alpine） | 8080 | 零依赖，仅标准库 `socket` |
| hello-php | PHP 8.3 | 单阶段（php:*-apache） | 80 | Apache 自动服务 `index.php` |
| hello-dotnet | .NET 8 | 多阶段（SDK 编译 → runtime 运行） | 8080 | 零依赖 `HttpListener`，无 ASP.NET Core |
| hello-python | Python 3.12 | 单阶段（python:slim） | 8080 | 零依赖，仅标准库 `http.server` |

> 注意 PHP 容器内 Apache 监听 80，故其 `k8s/deployment.yaml` 的 `targetPort` 与 `ssh_run_args` 都是 `8080:80`（其它语言为 `8080:8080`）。

导入（任选其一，其余路径同理）：

```bash
curl -X POST http://localhost:8080/api/projects -H 'Content-Type: application/json' \
  --data-binary @examples/hello-rust/project.json
# 其余：hello-ruby / hello-php / hello-dotnet / hello-python
```

本地直跑（不依赖 Docker / k8s）：

```bash
# Rust（需先 cargo build）
cd examples/hello-rust && cargo run
# Ruby（PORT=9090 ruby ... 可改端口）
ruby examples/hello-ruby/server.rb
# PHP（本机 php 内置服务器；或丢进 Apache 的 DocumentRoot）
php -S 0.0.0.0:8080 examples/hello-php/index.php
# .NET
cd examples/hello-dotnet && dotnet run
# Python（PORT=9090 python3 ... 可改端口）
python3 examples/hello-python/server.py
```

验证（本地 k3s，以 hello-rust 为例，其余替换名字）：

```bash
kubectl -n default get pods -l app=hello-rust
kubectl -n default port-forward svc/hello-rust 8080:80
curl localhost:8080     # -> Hello from hello-rust (version 1.0.0)
```

