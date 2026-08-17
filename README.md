# CI/CD Platform · Docker Build & k3s Release Management

A **zero external dependency** (Go standard library only) single-machine CI/CD management platform for:

- Defining projects (Git repo + Docker build + k3s release config)
- Building and pushing Docker images
- Releasing to a **k3s** cluster via `kubectl` / `helm`
- Triggering, viewing live logs, canceling runs, and auditing deployment history through a Web UI and REST API
- Receiving Git platform webhooks to auto-trigger pipelines

> The backend uses only the standard library (`net/http` + `encoding/json` + `os/exec` + `go:embed`). The **frontend is Vite + React** (a separate `cmd/server/web` project; `npm run build` produces `dist/`, which Go embeds into a single binary).

---

## Architecture

```
cmd/server/main.go          # Entry: embed web assets + start HTTP server
internal/config             # Config (JSON file + env var overrides)
internal/store              # Data models + file-based JSON storage (atomic write)
internal/build              # Docker build / push engine
internal/deploy             # kubectl / helm release engine
internal/pipeline           # Pipeline orchestration (concurrency limit + cancel + live logs)
internal/api                # REST API + static assets
cmd/server/web/            # Vite + React frontend (built dist/ embedded by Go)
```

Data is persisted to a `store.json` file (in-memory + atomic write to `.tmp` then `rename`), no database required.

---

## Requirements

| Tool | Purpose | Notes |
|------|---------|-------|
| Go 1.22+ | Compile backend | Standard library only, no network needed for deps |
| Node 20+ | Build frontend | `cmd/server/web` needs `npm install && npm run build` to produce `dist/` |
| `docker` | Build / push images | A reachable Docker daemon at runtime |
| `kubectl` | Release to k3s | Connect to cluster via kubeconfig |
| `helm` (optional) | Helm-style release | Only needed when deploy method is `helm` |
| k3s cluster | Release target | Provide a kubeconfig |

---

## Quick Start (Local)

```bash
# 1. Build frontend + compile backend (frontend needs Node 20+, produces dist/ and embeds it)
make build            # equivalent to: cd cmd/server/web && npm install && npm run build && go build

# 2. (Optional) Generate a sample config
make init            # writes ./data/config.json

# 3. Run (binds 127.0.0.1:8080 by default, no -addr needed)
./bin/server -config ./data/config.json
# Local dev works without a Token; for LAN/public access:
#   ./bin/server -addr 0.0.0.0:8080 and be sure to set API_TOKEN=<your-strong-token>
# Or use make run in the background (also binds localhost by default)

# 4. Open browser
open http://localhost:8080
```

> If you only changed backend Go code, a standalone `go build` works; but if `cmd/server/web/dist` does not exist, a placeholder page shows — run `make web` once to build the frontend first.

### Frontend Dev Mode (Hot Reload)

Separate frontend/backend dev: one terminal runs the Go backend, another runs the Vite dev server (HMR). Vite is configured to proxy `/api` to `:8080`, so there are no CORS issues:

```bash
make run          # Terminal A: Go backend :8080
make dev-web      # Terminal B: Vite :5173 (open http://localhost:5173)
```

Health check:

```bash
curl http://localhost:8080/api/health
# {"service":"cicdkit","status":"ok"}
```

---

## Configuration

Config precedence: **command-line `-config` file < environment variable overrides**.

| Config | Env Var | Default | Notes |
|--------|---------|---------|-------|
| `server.addr` | `PORT` / `SERVER_ADDR` | `127.0.0.1:8080` | Listen address (**binds localhost by default**; binding to all interfaces without `API_TOKEN` equals open command execution) |
| `server.api_token` | `API_TOKEN` | `""` | API auth token; empty means `/api` is fully open (local dev only). Set it when binding non-localhost |
| `server.auto_token` | `AUTO_TOKEN` | `false` | When enabled, auto-generates a one-time API Token at startup (no need to set `API_TOKEN` manually). The token is injected into the Web UI so the page auto-authenticates when opened locally; it is also printed to logs for other devices/scripts. Does not override an explicitly set `API_TOKEN`. Regenerated on every restart, not persisted |
| `server.webhook_secret` | `WEBHOOK_SECRET` | `""` | Webhook validation secret (no validation if empty) |
| `data_dir` | `DATA_DIR` | `./data` | Data directory |
| `store_file` | `STORE_FILE` | `./data/store.json` | Storage file |
| `runner.max_concurrent` | `MAX_CONCURRENT` | `2` | Concurrent pipelines |
| `runner.docker_host` | `DOCKER_HOST` | `""` | `DOCKER_HOST` passed to docker |
| `runner.default_kubeconfig` | `KUBECONFIG` | `""` | Default kubeconfig path |
| `runner.work_dir` | — | `./work` | Working directory |

See [`config.example.json`](./config.example.json) for an example.

---

## Project Model

Each project contains two definitions: build and release.

### BuildSpec (Docker build)

| Field | Notes |
|-------|-------|
| `context` | Build context path (e.g. `.`) |
| `dockerfile` | Dockerfile name (e.g. `Dockerfile`) |
| `image_repo` | Image repository (e.g. `registry.example.com/myapp`) |
| `tag_strategy` | `timestamp` / `git-sha` / `manual` |
| `push` | Whether to push after build |
| `build_args` | Build args `KEY=VAL` |
| `target` / `platforms` | Multi-stage target / cross-platform build |

### DeploySpec (Release)

`method` (choose one of five):

- **`local-k3s`** (recommended, pure local, no registry): optionally import the image into k3s containerd (controlled by `k3s_import_cmd`; empty skips — OrbStack etc. auto-share docker images), and if the target Deployment does not exist and `manifest_path` is set, first `kubectl apply`, then `kubectl set image`. **The UI does Build → Pipeline in one step**.
- **`kubectl-apply`**: `kubectl apply -f <manifest_path>`
- **`kubectl-set-image`**: `kubectl set image deployment/<deployment> <container>=<image_ref>`
- **`helm`**: `helm upgrade --install <release_name> <chart_path>` (optional `--set <helm_image_key>=<image_ref>`)
- **`ssh`** (bare-metal Docker, no k8s): SSH into a host with Docker installed and `docker run -d` to start the container. Image distribution has two modes:
  - **Registry-free direct transfer (recommended for personal/single-machine)**: with `ssh_transfer` checked, the platform locally `docker save` → `scp` to remote → remote `docker load` → `docker run`. **No image registry needed, no "push after build" needed** — just SSH access from the host.
  - **Registry pull**: without `ssh_transfer`, the remote first (optionally) `docker pull` then runs; requires "push after build" to push to ACR/TCR etc., or the image already exists remotely.
  - `ssh_image` empty reuses the built `image_ref` (i.e. `build.image_repo:tag`, which exists locally after build).

`ssh` related fields: `ssh_host` (required, can come from `CICD_SSH_HOST`), `ssh_user` (required, can come from `CICD_SSH_USER`), `ssh_port` (default 22), `ssh_key_path` (optional, empty uses default ssh config), `ssh_image` (optional, overrides `image_ref`), `ssh_container` (optional, container name), `ssh_run_args` (optional, extra `docker run` args like `-p 8080:8080 -e ENV=prod`), `ssh_probe_port` (optional, service probe port, default 8080), `ssh_pull` (whether to pull before deploy in non-transfer mode), `ssh_transfer` (registry-free direct transfer). These values must not contain shell metacharacters (`|;&$`><` etc.) to prevent remote command injection.

**Put connection info in `.env` (recommended)**: `ssh_host` / `ssh_user` / `ssh_port` / `ssh_key_path` / `ssh_container` / `ssh_run_args` / `ssh_probe_port`, if left empty in the project JSON, are auto-filled at deploy (and probe) time from the same-named environment variables (`CICD_SSH_HOST` / `CICD_SSH_USER` / `CICD_SSH_PORT` / `CICD_SSH_KEY_PATH` / `CICD_SSH_CONTAINER` / `CICD_SSH_RUN_ARGS` / `CICD_SSH_PROBE_PORT`). Write the real host IP, key path, username, etc. into the project-root `.env` (see `.env.example`); the project JSON contains **no real host info**, so switching machines only requires editing `.env`. The service reads `.env` and `.env.local` (latter takes precedence) at startup; real env vars always take precedence over the file. Validation also honors these env vars, so empty fields won't be rejected for "missing required". **The SSH deploy's service probe URL is derived the same way** (`http://<CICD_SSH_HOST>:<ssh_probe_port>`, default 8080), so `probe.url` can be left empty and auto-probed without hardcoding the host IP in JSON.

Other common fields: `kubeconfig`, `namespace`, `wait`, `timeout`, `k3s_import_cmd`, etc.

---

## Import from Directory (Quick Way to Create a Project)

Don't want to fill in every field? Click "**Import from Directory**" at the top of the UI: pick a local project directory, and the platform auto-detects and generates a pre-filled project config; confirm/supplement in the form and save.

- **Path input**: paste an absolute directory path the server can access (e.g. `/Users/you/code/myapp`). The platform traverses the directory on the backend; build context, Dockerfile, and manifest paths are set to absolute paths, ready for build and `kubectl`.
- **"Select Directory…" button**: invokes the browser's native directory picker (Chrome/Edge only). Due to browser security limits, the frontend **cannot read the directory's absolute path**, so the detected config's "build context / manifest path" only contains the relative name; change it to the server-actually-accessible path before saving (the popup detection note will remind you). Other browsers should use the path input directly.

Detection identifies (best-effort; unrecognized fields are left empty with a note):

| Item | Source |
|------|--------|
| Language / default image name | `go.mod` / `package.json` etc.; otherwise the directory name |
| `build.context` / `dockerfile` | set to the absolute path when a `Dockerfile` is found |
| Port hint | `EXPOSE` in `Dockerfile` |
| Deploy method / manifest / Deployment / container / namespace | `kind: Deployment` / `Service` in `*.yaml` (`type: NodePort|LoadBalancer|ClusterIP`) |
| Service probe | auto-enables GET probe when a cluster `Service` is detected, `url` left empty so the probe derives from Service; when only a Dockerfile exists (no k8s manifest), defaults to `ssh` registry-free transfer |

Corresponding endpoint: `POST /api/projects/scan`, request body is one of — `{"path":"<absolute dir>"}` (backend traverses disk) or `{"root":"<dir name>","files":[{"path":"Dockerfile","content":"..."},...]}` (file contents read by browser); returns `{"project":<draft>, "notes":[...]}`. This endpoint **only generates a draft, does not persist**.

---

## REST API

| Method | Path | Notes |
|--------|------|-------|
| GET | `/api/health` | Health check |
| GET | `/api/projects` | Project list |
| POST | `/api/projects` | Create project |
| POST | `/api/projects/scan` | Detect draft from directory (see "Import from Directory" above) |
| GET / PUT / DELETE | `/api/projects/{id}` | Project detail / update / delete |
| POST | `/api/projects/{id}/build` | Build only (includes push) |
| POST | `/api/projects/{id}/pipeline` | Build + push + release |
| POST | `/api/projects/{id}/deploy` | Release only (needs `image_ref` or `tag`) |
| GET / POST | `/api/projects/{id}/validate` | Config validation / dry run (checks config validity, Docker daemon, Kubeconfig reachability, service availability; **no build, no deploy**) |
| GET / POST | `/api/projects/{id}/probe` | Service availability probe (like Postman: sends HTTP request, returns status code / latency / response headers / body) |
| GET | `/api/runs?project_id=` | Run list |
| GET / POST | `/api/runs/{id}` / `/api/runs/{id}/cancel` | Run detail / cancel |
| GET | `/api/deployments?project_id=` | Deployment history |
| POST | `/api/webhook/{id}?secret=` | Webhook-triggered pipeline |

Example request bodies:

```bash
# Manually trigger pipeline
curl -X POST http://localhost:8080/api/projects/<id>/pipeline \
  -H 'Content-Type: application/json' -d '{"tag":"v1.2.3"}'

# Release an existing image only
curl -X POST http://localhost:8080/api/projects/<id>/deploy \
  -H 'Content-Type: application/json' \
  -d '{"image_ref":"registry.example.com/myapp:v1.2.3"}'

# Override deploy method for a single run (release same project to different targets, e.g. local k3s vs ssh bare-metal)
# Omit or leave method empty to use the stored deploy.method
curl -X POST http://localhost:8080/api/projects/<id>/pipeline \
  -H 'Content-Type: application/json' -d '{"method":"ssh"}'
```

---

## Webhook (Git Platform Auto-Trigger)

Configure a Webhook on a Git platform (GitHub / GitLab / Gitea etc.) pointing to:

```
POST /api/webhook/<project_id>?secret=<WEBHOOK_SECRET>
# or header X-Webhook-Secret: <WEBHOOK_SECRET>
```

On receipt it auto-triggers a `pipeline` (build + push + release).
No validation when `WEBHOOK_SECRET` is not set.

---

## Docker Deployment (incl. k3s Release)

The image runtime has `docker` CLI and `kubectl` built in. **Building images requires access to a Docker daemon**; mounting the host Docker socket for reuse is recommended (avoid docker-in-docker):

```bash
docker build -t cicdkit:latest .
docker run -d --name cicdkit -p 8080:8080 \
  -e API_TOKEN=your-strong-token \
  -v $(pwd)/data:/app/data \
  -v $HOME/.kube/config:/app/kubeconfig:ro \
  -v /var/run/docker.sock:/var/run/docker.sock \
  cicdkit:latest \
  -addr :8080 -config /app/data/config.json
```

> Inside the container `-addr :8080` together with `API_TOKEN` is a safe combo: the port mapping is controlled by the host, and callers without the token cannot invoke any `/api` endpoint (including creating projects and triggering deploys, the latter executes docker/kubectl on the host).

- Mount `/var/run/docker.sock` so the container's `docker` reuses the host daemon
- Mount kubeconfig (`-v $HOME/.kube/config:/app/kubeconfig:ro`), and point to that path in the project DeploySpec or `KUBECONFIG` env var
- Mount the data directory to a volume to persist projects and run records

### Connecting to k3s

k3s generates kubeconfig at `/etc/rancher/k3s/k3s.yaml` by default. Copy it to the management machine, change the `server` field's `127.0.0.1` to the k3s node's reachable address, then:

```bash
export KUBECONFIG=/path/to/k3s.yaml
# or specify that path directly in project DeploySpec.kubeconfig
```

Then configure `local-k3s` / `kubectl-set-image` / `kubectl-apply` for the target project in the Web UI for one-click release (`local-k3s` is the pure-local no-registry closed loop, most convenient).

---

## Development

```bash
make vet       # backend static checks
make test      # unit tests
make run       # Go backend only (:8080)
make dev-web   # Vite frontend dev server only (:5173, proxies /api to :8080)
make web       # build frontend only (npm install && npm run build -> dist/)
make build     # full frontend + backend build
make image     # build image (frontend build + embed done inside the image)
```

### Enable Git Hooks (Secret Leak Prevention)

The repo ships `hooks/pre-commit` (gitleaks secret scan), but `core.hooksPath` lives in local `.git/config` and **is not committed**, so run this once per fresh clone:

```bash
./scripts/setup-hooks.sh
```

It sets `git config core.hooksPath hooks` and tells you whether gitleaks is installed. Once `gitleaks` is on your machine, every `git commit` scans staged files and blocks the commit if a secret is found (see `.gitleaks.toml` for allow-list rules). Emergency bypass: `git commit --no-verify`.

---

## Service Probe (Probe · Optional)

The platform has a built-in "Postman-like" service availability probe. Each project can enable `probe` in settings; after a successful deploy it **auto-probes** whether the service is available, and the result (status code / latency / response headers / body) is written to **deployment history**; you can also click "Check Service" on the card for a **manual probe**. The probe **does not build or deploy**, it only sends one HTTP request.

`probe` config fields (`project.probe`):

| Field | Notes |
|-------|-------|
| `enabled` | Whether to enable |
| `method` | HTTP method, default `GET` (GET/POST/PUT/DELETE/HEAD/PATCH/OPTIONS) |
| `url` | Target address (global). **Empty** tries to derive from the k8s Service (`NodePort` → try node `ExternalIP`, then `InternalIP`, then `localhost`, first reachable wins; `LoadBalancer` → ingress IP; `ClusterIP` not directly reachable from host so skip with note). Local VM-style clusters (OrbStack / Rancher Desktop / Lima) publish NodePort on the node `InternalIP` (VM IP) not `localhost`, so derivation tries it first. **`ssh` deploy does not derive from Service** — either fill this field, or use the `urls` below keyed by deploy method |
| `urls` | **Override probe address by deploy method** (optional), `{"<deploy method>": "<address>"}` form, e.g. `{"ssh": "http://1.2.3.4:8080"}`. Resolution precedence: `urls[current method]` > `url` > cluster auto-derive. Lets **one project** released to different targets (local k3s auto-derive + bare-metal ssh explicit address) both auto-probe |
| `headers` | Request headers (`KEY=VALUE` form) |
| `body` | Request body (POST/PUT) |
| `expected_status` | Expected status code, default `200` |
| `body_contains` | String the response body must contain (optional, validates health response) |
| `timeout` | Timeout, default `5s` |

Probe result `status`: `ok` (met) / `fail` (status code or content mismatch) / `skip` (disabled or address undrivable) / `err` (request failed).

> Auto-probe only serves as **availability evidence** written to deployment history; it does **not** fail the deployment itself (even if the service isn't ready yet, the deploy already succeeded).

---

## Smart Diagnosis & Knowledge Base (LLM · Optional)

On build / deploy failure, click "AI Diagnose" on the run record to let a model analyze the failure log. This capability is **off by default and fully optional**; not configuring it does not affect other features.

### Configure LLM

Two ways; UI takes precedence over `.env`:

- **UI (recommended)**: top bar "⚙ Settings" fill `Base URL` / `API Key` / `Model` and check "Enable". The `API Key` is only written to local `data/llm.json` (perms 0600, gitignored), GET endpoints never return it in plaintext; empty falls back to the stored value when testing connection / diagnosing. **You can click "Test Connection"** to validate reachability with a minimal request, avoiding discovering a misconfiguration only at real failure.
- **`.env`**: use `CICD_LLM_ENABLED` / `CICD_LLM_BASE_URL` / `CICD_LLM_API_KEY` / `CICD_LLM_MODEL` as fallback (UI auto-shows these values when fields are absent). The system prompt **does not** need an env var — it is a code-built-in constant (Chinese CI/CD failure-analysis template), viewable / editable / clearable in the UI.
- System prompt: built-in template enabled when empty; can also be customized/overridden in settings.

### Knowledge Base Accumulation

- After diagnosis you can "✓ Adopt & add to knowledge base": the conclusion is persisted to `data/kb.json`, dedup-merged by "project + stage + normalized error", repeated adoptions accumulate counts.
- Top bar "📚 Knowledge Base" views all adopted entries, supports **keyword search** and **per-entry removal**.
- **Reuse**: on diagnosing a similar failure again, if it hits a historical entry the conclusion is reused directly (no model call), the popup is labeled "📚 Reused from knowledge base"; meanwhile "historical similar diagnoses" are shown for reference. Each adoption strengthens that knowledge base entry.
- Repeated "AI Diagnose" on the same failed run returns a cached result (`⚡ Cached result this time`), no model call.

### Generate Missing Dockerfile / k8s Manifest

If a project to be released lacks a `Dockerfile` or `k8s/deployment.yaml`, click "Generate Missing Files" on the card or new-project form: the platform first detects by language (Go / Node / Python …) and previews the multi-stage `Dockerfile` and `Deployment`/`Service` manifest to be written, then writes them to the build context directory after confirmation. Templates align with `examples/hello-cicd` — the `local-k3s`-generated manifest can be `kubectl apply`'d directly.

---

## Security Notes

> ⚠️ This platform executes `docker` / `kubectl` on the **host**, so "can call the API" ≈ "can run commands on the host".
> Each item below is to prevent this capability from being obtained by unauthorized parties.

- **API auth (must read)**: all `/api/*` endpoints (except `/api/health` and `/api/webhook/*`) require `API_TOKEN`:
  request header `X-API-Token: <token>` or `Authorization: Bearer <token>`. When `API_TOKEN` is not configured
  `/api` is fully open — **local dev only**. Once you bind `server.addr` to non-localhost (e.g. `0.0.0.0`),
  be sure to also set `API_TOKEN`, otherwise it equals open remote command execution (RCE).
- **One-click no-config auth (`auto_token`)**: if manually setting `API_TOKEN` is a hassle, set `AUTO_TOKEN=1` (or
  `server.auto_token: true`). At startup a one-time strong token is generated with `crypto/rand` and injected into the Web UI —
  **the page auto-authenticates when opened locally, no manual input**; the token is also printed to logs for other devices/scripts.
  The token is regenerated on every restart and not persisted; if `API_TOKEN` is explicitly configured it is not overridden. Note: the auto token is delivered to the browser along with the page, so it suits "localhost/trusted LAN" use; if you bind the address to public internet, a fixed strong `API_TOKEN` is still recommended over relying on the auto token.
- **Binds localhost by default**: `server.addr` defaults to `127.0.0.1:8080`. For LAN/public access use
  `./bin/server -addr 0.0.0.0:8080` and set `API_TOKEN=strong-token`. At startup it detects "non-localhost + no token"
  and warns in logs, but does not block (compatible with existing container environments).
- **Image import command allowlist (RCE prevention)**: `deploy.k3s_import_cmd` can only invoke container runtime binaries
  (`k3s` / `k3d` / `ctr` / `crictl` / `nerdctl` / `microk8s`, with optional `sudo` prefix),
  rejecting shell metacharacters and arbitrary commands. Empty means skip import. Out-of-bounds commands are `400`-rejected at project create/update.
- **Password masking**: after `build.registry.password` is stored, any `GET` endpoint response replaces it with `********`;
  UI write-back keeps the old value if the password is empty or still the mask, so editing back and forth won't wipe the password.
- `webhook_secret` is recommended in production to avoid unauthorized triggers.
- Do not commit `config.json` containing image registry passwords to the version control (already in `.gitignore`).
- When mounting the host Docker socket, the container effectively has host Docker control; use only in trusted environments.

---

## Example Projects

The repo ships multiple minimal runnable examples, all covering the full **build → local k3s release** closed loop (a zero-dependency HTTP service + Dockerfile + k8s manifest + platform project definition, default `local-k3s` pure-local no-registry). Each example validates the platform's build / release / probe loop for its language; multiple can be imported into the same platform and released independently to local k3s.

| Example | Language | Build | In-container Port | Notes |
|---------|----------|-------|-------------------|-------|
| `hello-cicd` | Go 1.22 | Multi-stage (golang → alpine) | 8080 | Zero-dep `net/http`, baseline example |
| `hello-node` | Node.js 24 (Active LTS) | Single-stage (node:alpine) | 8080 | Validates interpreted-language loop |
| `hello-java` | Java 21 (Maven/Gradle) | Multi-stage (maven / eclipse-temurin) | 8080 | Deliberately no Dockerfile, demos "Generate Missing Files" |
| `hello-rust` | Rust 1.85 | Multi-stage (rust:slim compile → debian run) | 8080 | Zero-dep `std::net` |
| `hello-ruby` | Ruby 3.3 | Single-stage (ruby:alpine) | 8080 | Zero-dep `socket` |
| `hello-php` | PHP 8.3 | Single-stage (php:cli-alpine built-in server) | 8080 | `php -S` built-in server, no Apache |
| `hello-dotnet` | .NET 8 | Multi-stage (SDK compile → aspnet run) | 8080 | Kestrel / ASP.NET Core minimal API |
| `hello-python` | Python 3.12 | Single-stage (python:slim) | 8080 | Zero-dep `http.server` |

All examples lock full patch versions in their `Dockerfile` (e.g. `ruby:3.3.8-alpine`) for reproducible builds; the run stage uniformly starts as a non-root user (`debian` family uses `nobody` / `useradd`, `alpine` family uses `adduser`). Measured image sizes (decompressed, `docker images`) are approx: ruby 75 MB, rust 76–98 MB, php 96–105 MB, python 132–157 MB, dotnet 218–250 MB (largest, includes the ASP.NET Core framework).

```bash
make dev         # start platform (backend :8080 + frontend :5173)
make example     # import the hello-cicd sample project into the running service
# open http://localhost:8080 to see the example card
# click Build → Pipeline on the card; the platform auto-creates Deployment + set-image, no CLI needed
```

Full usage (OrbStack auto-shared images / pure-k3s import differences, placeholder replacement, triggering Pipeline, importing other-language examples, pushing to a real registry via Plan A, generating missing files, etc.) is in [`examples/README.md`](./examples/README.md).
