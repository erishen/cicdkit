package store

import "time"

// RunStatus is the lifecycle state of a pipeline run or a single stage.
type RunStatus string

const (
	StatusQueued   RunStatus = "queued"
	StatusRunning  RunStatus = "running"
	StatusSuccess  RunStatus = "success"
	StatusFailed   RunStatus = "failed"
	StatusCanceled RunStatus = "canceled"
)

// RegistryAuth holds credentials used for `docker login` before a push.
type RegistryAuth struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password,omitempty"`
}

// BuildSpec describes how to build (and optionally push) a container image.
type BuildSpec struct {
	Context     string            `json:"context"`
	Dockerfile  string            `json:"dockerfile"`
	ImageRepo   string            `json:"image_repo"`
	TagStrategy string            `json:"tag_strategy"` // timestamp | git-sha | manual
	Push        bool              `json:"push"`
	Registry    RegistryAuth      `json:"registry,omitempty"`
	BuildArgs   map[string]string `json:"build_args,omitempty"`
	Target      string            `json:"target,omitempty"`
	Platforms   []string          `json:"platforms,omitempty"`
}

// DeploySpec describes how to roll the image out. Method is one of:
// kubectl-apply | kubectl-set-image | helm | local-k3s | ssh.
// For local-k3s, K3sImportCmd optionally imports the built image into the k3s
// containerd (leave empty when the cluster already sees docker images, e.g.
// OrbStack which auto-shares them). For ssh, the target is a bare host running
// Docker (no Kubernetes); see the SSH* fields below.
type DeploySpec struct {
	Method        string            `json:"method"`
	Kubeconfig    string            `json:"kubeconfig,omitempty"`
	Namespace     string            `json:"namespace,omitempty"`
	ManifestPath  string            `json:"manifest_path,omitempty"`
	Deployment    string            `json:"deployment,omitempty"`
	Container     string            `json:"container,omitempty"`
	ChartPath     string            `json:"chart_path,omitempty"`
	ReleaseName   string            `json:"release_name,omitempty"`
	HelmValues    map[string]string `json:"helm_values,omitempty"`
	HelmSetImage  bool              `json:"helm_set_image,omitempty"`
	HelmImageKey  string            `json:"helm_image_key,omitempty"`
	K3sImportCmd  string            `json:"k3s_import_cmd,omitempty"`
	Wait          bool              `json:"wait,omitempty"`
	Timeout       string            `json:"timeout,omitempty"`

	// SSH deploy targets a bare host running Docker (no Kubernetes). Two ways
	// to get the image onto the host:
	//   - SSHTransfer=true (registry-free, recommended for solo): the locally
	//     built image is `docker save`d, scp'd to the host, then `docker load`ed
	//     — no registry or build.push needed; SSHImage overrides the built
	//     imageRef when set.
	//   - otherwise the host `docker pull`s the image (set SSHPull=true and
	//     enable build.push so it lands in a registry first, e.g. ACR/TCR).
	// These values are confined to shell-safe tokens in Validate, because they
	// are interpolated into a remote `docker` command.
	SSHHost      string `json:"ssh_host,omitempty"`
	SSHUser      string `json:"ssh_user,omitempty"`
	SSHPort      string `json:"ssh_port,omitempty"` // default "22"
	SSHKeyPath   string `json:"ssh_key_path,omitempty"`
	SSHImage     string `json:"ssh_image,omitempty"`
	SSHContainer string `json:"ssh_container,omitempty"`
	SSHRunArgs   string `json:"ssh_run_args,omitempty"`
	// SSHProbePort is the host port the deployed container listens on, used by
	// the service probe to derive its target URL for ssh deploys (host comes
	// from CICD_SSH_HOST / deploy.ssh_host, port from here / CICD_SSH_PROBE_PORT,
	// default 8080). Keeping it here means the real host IP never has to be
	// hardcoded in the project JSON — operators store it only in .env.
	SSHProbePort string `json:"ssh_probe_port,omitempty"`
	SSHPull      bool   `json:"ssh_pull,omitempty"`
	SSHTransfer  bool   `json:"ssh_transfer,omitempty"`
	// SSHBuildPlatforms overrides Build.Platforms when this run deploys via
	// ssh_transfer to a host of a different architecture (e.g. building on an
	// Apple-Silicon Mac for an amd64 bare host). Without it the arm64 image is
	// transferred and the remote runs it under QEMU emulation — it works, but
	// slowly, and emits a "image platform does not match host" warning. Leave
	// empty to inherit Build.Platforms (native arch for local-k3s). May also be
	// set globally via CICD_SSH_BUILD_PLATFORMS (comma-separated, see
	// ResolveSSHSpec / the pipeline's ssh_transfer default). With two or more
	// entries a MULTI-ARCH image is built (docker buildx --platform a,b
	// --output type=oci) so the remote's `docker run` picks its own arch
	// automatically — no manual single-arch guess needed.
	SSHBuildPlatforms []string `json:"ssh_build_platforms,omitempty"`
	// TransferArtifact is the local path of a build-produced artifact (a
	// multi-arch OCI tar) to ship during ssh_transfer instead of `docker save`.
	// It is populated by the build stage and consumed by the deploy stage; it is
	// json:"-" so it never leaves/enters the persisted store.
	TransferArtifact string `json:"-"`
}

// ProbeSpec describes a service-availability probe (Postman-style HTTP check)
// run after a successful deploy (and on demand). When Enabled is false the
// probe is skipped entirely. The probe target URL is resolved with this
// precedence: URLs[<deploy method>] (per-target override) → URL (global manual
// URL) → auto-derived from the deployed Kubernetes Service (NodePort/LB). This
// lets a single project probe both a local cluster service (auto-derived) and a
// bare ssh host (explicit URL) depending on which target it is published to.
type ProbeSpec struct {
	Enabled        bool              `json:"enabled"`
	Method         string            `json:"method"` // GET/POST/...; defaults to GET
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers,omitempty"`
	Body           string            `json:"body,omitempty"`
	ExpectedStatus int               `json:"expected_status,omitempty"` // default 200
	BodyContains   string            `json:"body_contains,omitempty"`
	Timeout        string            `json:"timeout,omitempty"` // e.g. "5s"

	// URLs overrides the probe target per deploy method, e.g.
	// {"ssh": "http://1.2.3.4:8080"}. It takes precedence over URL for that
	// method, so a project published to both a cluster and a bare host can
	// probe each with its own address.
	URLs map[string]string `json:"urls,omitempty"`
}

// DeployTarget is a named, self-contained publish destination. A project may
// declare several (e.g. "腾讯云-prod" and "AWS-staging") so a single codebase can
// be rolled out to multiple clouds / hosts from the UI without editing the
// project. Each target carries its own full DeploySpec (method + method-specific
// connection details). When a run is published to a named target, that target's
// DeploySpec is used verbatim; the project's top-level Deploy remains the default
// used when no target name is supplied (i.e. the legacy single-target / method
// override flow).
type DeployTarget struct {
	Name string `json:"name"`
	DeploySpec
}

// Project is a managed application with build + deploy definitions.
type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Repository  string    `json:"repository"`
	Branch      string    `json:"branch"`
	Build       BuildSpec `json:"build"`
	Deploy      DeploySpec `json:"deploy"`
	// Targets are optional named publish destinations on top of the primary
	// Deploy. Empty means "single target" — the legacy behaviour where the one
	// Deploy config is the only destination.
	Targets     []DeployTarget `json:"targets,omitempty"`
	Probe       ProbeSpec `json:"probe,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// LastDeploy captures the most recent successful rollout so the project
	// card can show where it was actually published last — separate from
	// Deploy.Method, which is only the project's configured default target. A
	// per-run override (e.g. publishing to ssh/腾讯云 once) still surfaces here.
	LastDeploy *LastDeployInfo `json:"last_deploy,omitempty"`
}

// LastDeployInfo is a compact snapshot of the latest successful deployment,
// surfaced on the project card. It mirrors the relevant fields of Deployment.
type LastDeployInfo struct {
	Method      string    `json:"method"` // local-k3s | ssh | kubectl-* | helm
	Target      string    `json:"target,omitempty"` // named DeployTarget, if published to one
	ImageRef    string    `json:"image_ref,omitempty"`
	At          time.Time `json:"at"`
	ProbeStatus string    `json:"probe_status,omitempty"` // ok | err | skip
}

// StageResult is the outcome of one pipeline stage (build / push / deploy).
type StageResult struct {
	Name      string    `json:"name"`
	Status    RunStatus `json:"status"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	Log       string    `json:"log"`
	Error     string    `json:"error,omitempty"`
	// ArtifactPath is the local path of a build-produced file artifact (e.g. a
	// multi-arch OCI tar created by `docker buildx build --output type=oci`),
	// used by a later stage (ssh_transfer) in place of `docker save`. Empty for
	// single-arch builds that load straight into the local daemon.
	ArtifactPath string `json:"artifact_path,omitempty"`
}

// Run is a single execution of build/push/deploy for a project.
type Run struct {
	ID          string        `json:"id"`
	ProjectID   string        `json:"project_id"`
	ProjectName string        `json:"project_name"`
	Trigger     string        `json:"trigger"` // manual | webhook
	ImageTag    string        `json:"image_tag"`
	ImageRef    string        `json:"image_ref"`
	Stages      []StageResult `json:"stages"`
	Status      RunStatus      `json:"status"`
	CreatedAt   time.Time      `json:"created_at"`
	StartedAt   time.Time      `json:"started_at,omitempty"`
	EndedAt     time.Time      `json:"ended_at,omitempty"`
	Log         string        `json:"log"`
	// Diagnosis caches the last AI failure analysis for this run so re-opening
	// it does not re-call the model, and records whether the operator adopted it
	// into the knowledge base (Adopted) or marked it not useful (Rejected).
	Diagnosis *RunDiagnosis `json:"diagnosis,omitempty"`
	// Probe is the outcome of the post-deploy service-availability check, if one
	// ran. It mirrors Deployment.Probe so the UI can show 服务探测 as a run
	// step (build / deploy / 服务探测) without digging into deployment history.
	Probe *ProbeResult `json:"probe,omitempty"`
}

// RunDiagnosis is a cached AI analysis attached to a Run.
type RunDiagnosis struct {
	Text      string `json:"text"`
	Adopted   bool   `json:"adopted,omitempty"`
	Rejected  bool   `json:"rejected,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	AdoptedAt string `json:"adopted_at,omitempty"`
	FromCache bool   `json:"from_cache,omitempty"`
}

// ProbeResult is the outcome of a single service-availability probe. Status is
// one of: ok | fail | skip | err. It is embedded in both Run (deploy stage) and
// Deployment so the availability of every rollout is visible in the UI.
type ProbeResult struct {
	Status     string            `json:"status"` // ok | fail | skip | err
	URL        string            `json:"url"`
	Method     string            `json:"method"`
	StatusCode int               `json:"status_code"`
	DurationMs int64             `json:"duration_ms"`
	Matched    bool              `json:"matched"`
	Error      string            `json:"error,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	Detail     string            `json:"detail,omitempty"`
}

// Deployment is a historical record of a rollout.
type Deployment struct {
	ID          string       `json:"id"`
	ProjectID   string       `json:"project_id"`
	ProjectName string       `json:"project_name"`
	ImageRef    string       `json:"image_ref"`
	Cluster     string       `json:"cluster"`
	Namespace   string       `json:"namespace"`
	Method      string       `json:"method"`
	Target      string       `json:"target,omitempty"` // named DeployTarget, if published to one
	Status      string       `json:"status"`
	Probe       ProbeResult   `json:"probe,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	// RunID links this rollout back to the originating pipeline Run so the UI
	// can jump from a deployment-history row to the full stage logs (and from
	// there to an AI diagnosis). Empty for legacy records written before this
	// field existed.
	RunID string `json:"run_id,omitempty"`
}
