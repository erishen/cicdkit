package scan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanPathDetectsNodeExample(t *testing.T) {
	dir := t.TempDir()
	dockerfile := `FROM node:24-alpine
WORKDIR /app
COPY package.json server.js ./
EXPOSE 8080
USER node
CMD ["node", "server.js"]
`
	manifest := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: hello-node
spec:
  template:
    spec:
      containers:
        - name: hello-node
          image: hello-node:local
          ports:
            - containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: hello-node
spec:
  type: NodePort
  ports:
    - port: 80
      targetPort: 80
      nodePort: 30080
`
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "k8s"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "k8s", "deployment.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"hello-node","engines":{"node":">=18"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := ScanPath(dir)
	if err != nil {
		t.Fatalf("ScanPath error: %v", err)
	}
	p := d.Project
	if p.Name != "hello-node" {
		t.Fatalf("name = %q, want hello-node", p.Name)
	}
	if p.Build.Context != dir {
		t.Fatalf("context = %q, want %q", p.Build.Context, dir)
	}
	if p.Build.Dockerfile != filepath.Join(dir, "Dockerfile") {
		t.Fatalf("dockerfile = %q", p.Build.Dockerfile)
	}
	if p.Deploy.Method != "local-k3s" {
		t.Fatalf("method = %q, want local-k3s", p.Deploy.Method)
	}
	if p.Deploy.Deployment != "hello-node" {
		t.Fatalf("deployment = %q, want hello-node", p.Deploy.Deployment)
	}
	if p.Deploy.Container != "hello-node" {
		t.Fatalf("container = %q, want hello-node", p.Deploy.Container)
	}
	if p.Probe.Enabled != true {
		t.Fatalf("probe should be enabled for NodePort Service")
	}
	if p.Probe.URL != "" {
		t.Fatalf("probe URL should be empty (auto-derived), got %q", p.Probe.URL)
	}
}

func TestScanPathMissingDir(t *testing.T) {
	if _, err := ScanPath("/nonexistent/path/xyz"); err == nil {
		t.Fatalf("expected error for missing dir")
	}
}

func TestScanFilesDockerfileOnlyDefaultsSSH(t *testing.T) {
	files := map[string]string{
		"Dockerfile": "FROM golang:1.22\nEXPOSE 8080\n",
	}
	d := ScanFiles("mygoapp", files)
	p := d.Project
	if p.Deploy.Method != "ssh" {
		t.Fatalf("method = %q, want ssh (no k8s)", p.Deploy.Method)
	}
	if !p.Deploy.SSHTransfer {
		t.Fatalf("ssh_transfer should default true")
	}
	if len(d.Notes) == 0 {
		t.Fatalf("expected notes")
	}
}

// A browser directory picker only exposes the folder name, never the real path.
// ScanFiles must NOT fill the build context with that bare name (which would make
// `docker build mygoapp` resolve against the server cwd and fail with "path not
// found"). It must leave the context empty so the form forces an absolute path.
func TestScanFilesLeavesContextEmpty(t *testing.T) {
	files := map[string]string{
		"Dockerfile": "FROM node:24-alpine\nEXPOSE 8080\n",
	}
	d := ScanFiles("hello-node", files)
	if d.Project.Build.Context != "" {
		t.Fatalf("ScanFiles context = %q, want empty (browser cannot expose real path)", d.Project.Build.Context)
	}
	// And the note must warn the user to fill the absolute path.
	if !strings.Contains(d.Notes[len(d.Notes)-1], "绝对路径") {
		t.Fatalf("note should warn about filling the absolute path, got %q", d.Notes[len(d.Notes)-1])
	}
}
