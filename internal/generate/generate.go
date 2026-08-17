// Package generate scaffolds the missing build/deploy files a project needs to
// be publishable: a Dockerfile (derived from the detected language) and a k8s
// Deployment+Service manifest (for cluster deploy methods such as local-k3s).
//
// It is strictly best-effort and intentionally conservative:
//   - files are only generated when they are absent (an existing Dockerfile or
//     manifest_path is never overwritten);
//   - every generated file is written inside the project's build.context, never
//     anywhere else;
//   - Plan returns a preview (no side effects) so the UI can show the content
//     before the user confirms; Apply writes to disk and updates the project
//     config (dockerfile / manifest_path / deployment / container / image_repo)
//     so the project is immediately publishable.
package generate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/erishen/cicdkit/internal/store"
)

// FileSpec is one file the generator proposes to write.
type FileSpec struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Reason  string `json:"reason"`
}

// Plan is the outcome of a dry-run (or applied) generation: the files to write
// and the project config fields that will be (or were) updated to point at them.
type Plan struct {
	Dockerfile    *FileSpec         `json:"dockerfile,omitempty"`
	Manifest      *FileSpec         `json:"manifest,omitempty"`
	ConfigUpdates map[string]string `json:"config_updates"`
	NeedsAny      bool              `json:"needs_any"`
}

// IsClusterMethod reports whether a deploy method needs a k8s manifest.
func IsClusterMethod(m string) bool {
	switch m {
	case "local-k3s", "kubectl-apply", "kubectl-set-image", "helm":
		return true
	}
	return false
}

// PlanProject computes what would be generated for p without touching disk.
func PlanProject(p store.Project, workDir string) (*Plan, error) {
	ctxDir := resolveDir(p.Build.Context, workDir)
	if ctxDir == "" {
		return nil, fmt.Errorf("build.context 不存在: %q（已尝试相对于工作目录与 %s）", p.Build.Context, workDir)
	}
	plan := &Plan{ConfigUpdates: map[string]string{}}
	lang := detectLang(ctxDir)

	// Dockerfile: only when the configured (or default) file is absent.
	if resolveFile(p.Build.Dockerfile, workDir) == "" {
		dfPath := filepath.Join(ctxDir, "Dockerfile")
		if p.Build.Dockerfile != "" {
			// The user pointed at a specific (missing) path inside the context:
			// honour that location rather than forcing the bare "Dockerfile".
			if d := filepath.Dir(p.Build.Dockerfile); isWithin(ctxDir, d) {
				dfPath = p.Build.Dockerfile
			}
		}
		plan.Dockerfile = &FileSpec{
			Path:    dfPath,
			Content: dockerfileFor(lang),
			Reason:  "未检测到 Dockerfile（" + langLabel(lang) + "），生成多阶段构建模板",
		}
		plan.ConfigUpdates["build.dockerfile"] = dfPath
	}

	// Manifest: only for cluster methods, and only when no manifest exists yet.
	if IsClusterMethod(p.Deploy.Method) {
		if resolveFile(p.Deploy.ManifestPath, workDir) == "" {
			name := k8sName(p.Name)
			manifestPath := filepath.Join(ctxDir, "k8s", "deployment.yaml")
			plan.Manifest = &FileSpec{
				Path:    manifestPath,
				Content: manifestFor(name, p, lang),
				Reason:  "部署方式为 " + p.Deploy.Method + "，未检测到 k8s 清单，生成 Deployment + Service",
			}
			plan.ConfigUpdates["deploy.manifest_path"] = manifestPath
			plan.ConfigUpdates["deploy.deployment"] = name
			plan.ConfigUpdates["deploy.container"] = name
			if strings.TrimSpace(p.Build.ImageRepo) == "" {
				plan.ConfigUpdates["build.image_repo"] = name
			}
		}
	}

	plan.NeedsAny = plan.Dockerfile != nil || plan.Manifest != nil
	return plan, nil
}

// projectStore is the minimal persistence surface Apply needs. It is satisfied
// by store.Store (the real implementation) and by test stubs.
type projectStore interface {
	UpdateProject(p store.Project) error
}

// Apply writes the planned files to disk and updates + persists the project so
// it points at them. p must be a fresh copy from the store (it is mutated and
// saved back via st.UpdateProject).
func Apply(p store.Project, workDir string, st projectStore) (*Plan, error) {
	plan, err := PlanProject(p, workDir)
	if err != nil {
		return nil, err
	}
	if !plan.NeedsAny {
		return plan, nil
	}
	if plan.Dockerfile != nil {
		if err := writeFile(plan.Dockerfile.Path, plan.Dockerfile.Content); err != nil {
			return nil, err
		}
		p.Build.Dockerfile = plan.Dockerfile.Path
	}
	if plan.Manifest != nil {
		if err := writeFile(plan.Manifest.Path, plan.Manifest.Content); err != nil {
			return nil, err
		}
		p.Deploy.ManifestPath = plan.Manifest.Path
		p.Deploy.Deployment = plan.ConfigUpdates["deploy.deployment"]
		p.Deploy.Container = plan.ConfigUpdates["deploy.container"]
	}
	if strings.TrimSpace(p.Build.ImageRepo) == "" && plan.ConfigUpdates["build.image_repo"] != "" {
		p.Build.ImageRepo = plan.ConfigUpdates["build.image_repo"]
	}
	p.UpdatedAt = time.Now()
	if err := st.UpdateProject(p); err != nil {
		return nil, fmt.Errorf("更新项目配置失败: %w", err)
	}
	return plan, nil
}

// ---- resolution helpers (mirror check.resolveContext but file-aware) ----

func resolveDir(p, workDir string) string {
	if abs, err := filepath.Abs(p); err == nil {
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			return abs
		}
	}
	if workDir != "" {
		joined := filepath.Join(workDir, p)
		if abs, err := filepath.Abs(joined); err == nil {
			if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
				return abs
			}
		}
	}
	return ""
}

func resolveFile(p, workDir string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}
	if workDir != "" {
		joined := filepath.Join(workDir, p)
		if abs, err := filepath.Abs(joined); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	return ""
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建目录失败 %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入文件失败 %s: %w", path, err)
	}
	return nil
}

func isWithin(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}

// ---- language detection ----

func detectLang(dir string) string {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	switch {
	case has("go.mod"):
		return "go"
	case has("package.json"):
		return "node"
	case has("requirements.txt"), has("pyproject.toml"), has("Pipfile"), has("setup.py"):
		return "python"
	case has("pom.xml"):
		return "java-maven"
	case has("build.gradle"), has("build.gradle.kts"):
		return "java-gradle"
	case has("Cargo.toml"):
		return "rust"
	default:
		return "unknown"
	}
}

func langLabel(lang string) string {
	switch lang {
	case "go":
		return "Go"
	case "node":
		return "Node.js"
	case "python":
		return "Python"
	case "java-maven":
		return "Java/Maven"
	case "java-gradle":
		return "Java/Gradle"
	case "rust":
		return "Rust"
	default:
		return "未识别语言"
	}
}

// k8sName sanitises a free-form project name into a valid Kubernetes object
// name: lowercase [a-z0-9-], must start with a letter.
var k8sNameRe = regexp.MustCompile(`[^a-z0-9-]+`)

func k8sName(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	s = k8sNameRe.ReplaceAllString(s, "")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "app"
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "app-" + s
	}
	return s
}

// ---- templates ----

func dockerfileFor(lang string) string {
	switch lang {
	case "go":
		return `# 多阶段构建：golang 编译 → alpine 运行
FROM golang:1.22-alpine AS build
WORKDIR /src
ARG APP_VERSION=dev
ENV APP_VERSION=$APP_VERSION
COPY go.mod go.sum* ./
RUN go mod download 2>/dev/null || true
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/app .

FROM alpine:3.20
WORKDIR /app
RUN adduser -D -u 10001 appuser
COPY --from=build --chown=appuser:appuser /out/app /app/app
USER appuser
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app/app"]
`
	case "node":
		return `# Node.js：安装依赖并运行（按需调整 CMD / 端口）
FROM node:24-alpine
WORKDIR /app
ARG APP_VERSION=dev
ENV APP_VERSION=$APP_VERSION
COPY package*.json ./
RUN npm install --omit=dev
COPY . .
ENV PORT=8080
EXPOSE 8080
USER node
CMD ["npm", "start"]
`
	case "python":
		return `# Python：pip 安装依赖后运行（按需修改 CMD 入口，如 uvicorn/gunicorn）
FROM python:3.12-slim
WORKDIR /app
ARG APP_VERSION=dev
ENV APP_VERSION=$APP_VERSION PYTHONUNBUFFERED=1
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
ENV PORT=8080
EXPOSE 8080
CMD ["python", "app.py"]
`
	case "java-maven":
		return `# Java (Maven)：多阶段构建 jar → 运行（按需调整 jar 名与 ENTRYPOINT）
FROM maven:3.9-eclipse-temurin-21 AS build
WORKDIR /src
COPY pom.xml ./
RUN mvn -q dependency:go-offline || true
COPY src ./src
RUN mvn -q -DskipTests package
FROM eclipse-temurin:21-jre
WORKDIR /app
COPY --from=build /src/target/*.jar /app/app.jar
ENV PORT=8080
EXPOSE 8080
# MaxRAMPercentage 让 JVM 按容器内存上限（如 128Mi） sizing 堆，避免默认按宿主机内存分配导致被 OOMKill
ENTRYPOINT ["java", "-XX:MaxRAMPercentage=75.0", "-XX:InitialRAMPercentage=50.0", "-jar", "/app/app.jar"]
`
	case "java-gradle":
		return `# Java (Gradle)：多阶段构建 jar → 运行（按需调整 jar 名与 ENTRYPOINT）
FROM gradle:8-jdk21 AS build
WORKDIR /src
COPY . .
RUN gradle build --no-daemon -x test
FROM eclipse-temurin:21-jre
WORKDIR /app
COPY --from=build /src/build/libs/*.jar /app/app.jar
ENV PORT=8080
EXPOSE 8080
# MaxRAMPercentage 让 JVM 按容器内存上限（如 128Mi） sizing 堆，避免默认按宿主机内存分配导致被 OOMKill
ENTRYPOINT ["java", "-XX:MaxRAMPercentage=75.0", "-XX:InitialRAMPercentage=50.0", "-jar", "/app/app.jar"]
`
	case "rust":
		return `# Rust：多阶段编译 → 精简运行（按需修改二进制名）
FROM rust:1-alpine AS build
WORKDIR /src
COPY . .
RUN cargo build --release
FROM alpine:3.20
WORKDIR /app
COPY --from=build /src/target/release/app /app/app
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app/app"]
`
	default:
		return `# TODO: 未识别到项目语言（go.mod / package.json / requirements.txt / pom.xml / build.gradle / Cargo.toml 均缺失）。
# 请按你的技术栈补全下方构建步骤；以下为占位模板。
FROM alpine:3.20
WORKDIR /app
COPY . .
ENV PORT=8080
EXPOSE 8080
# CMD ["your-binary-or-command"]
`
	}
}

func manifestFor(name string, p store.Project, lang string) string {
	ns := ""
	if n := strings.TrimSpace(p.Deploy.Namespace); n != "" {
		ns = "  namespace: " + n + "\n"
	}
	image := strings.TrimSpace(p.Build.ImageRepo)
	if image == "" {
		image = name
	}
	image += ":local"
	// Java 系（JVM 自身开销大）抬高默认内存：limit 256Mi / request 64Mi；其余 128Mi / 32Mi。
	memLimit, memReq := "128Mi", "32Mi"
	if lang == "java-maven" || lang == "java-gradle" {
		memLimit, memReq = "256Mi", "64Mi"
	}
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
%s  name: %s
  labels:
    app: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
        - name: %s
          # 占位镜像：首次 apply 前需先 docker build 并导入 k3s（如 scripts/import-to-k3s.sh）。
          # 平台执行 Pipeline / Deploy 时会用 kubectl set image 覆盖为最新构建的本地镜像 tag。
          image: %s
          # 纯本地模式必须 IfNotPresent：k3s 直接用本地已导入镜像，绝不外网 pull
          imagePullPolicy: IfNotPresent
          ports:
            - containerPort: 8080
          env:
            - name: PORT
              value: "8080"
          resources:
            requests:
              cpu: 50m
              memory: %s
            limits:
              cpu: 200m
              memory: %s
---
apiVersion: v1
kind: Service
metadata:
%s  name: %s
  # 必须带 app=<部署名> 标签：平台服务探测按 Service 标签推导可达地址
  labels:
    app: %s
spec:
  # NodePort：本地 k3s 单节点下节点即 localhost，探针可经 localhost:<nodePort> 直达
  type: NodePort
  selector:
    app: %s
  ports:
    - port: 80
      targetPort: 8080
`, ns, name, name, name, name, name, image, memReq, memLimit, ns, name, name, name)
}
