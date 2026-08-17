package generate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erishen/cicdkit/internal/store"
)

func TestK8sName(t *testing.T) {
	cases := map[string]string{
		"hello-cicd":       "hello-cicd",
		"Hello CICD":       "hello-cicd",
		"My_App":           "my-app",
		"123app":           "app-123app",
		"  ":               "app",
		"foo/bar:baz":      "foobarbaz",
	}
	for in, want := range cases {
		if got := k8sName(in); got != want {
			t.Errorf("k8sName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectLang(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"go.mod":         "go",
		"package.json":    "node",
		"requirements.txt": "python",
		"pom.xml":        "java-maven",
		"build.gradle":   "java-gradle",
		"Cargo.toml":     "rust",
	}
	for name, want := range cases {
		sub := filepath.Join(dir, want)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		mkIn := func(d, n string) {
			if err := os.WriteFile(filepath.Join(d, n), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		mkIn(sub, name)
		if got := detectLang(sub); got != want {
			t.Errorf("detectLang(%s) = %q, want %q", name, got, want)
		}
	}
	if got := detectLang(dir); got != "unknown" {
		t.Errorf("detectLang(empty) = %q, want unknown", got)
	}
}

func TestDockerfileForNonEmpty(t *testing.T) {
	for _, lang := range []string{"go", "node", "python", "java-maven", "java-gradle", "rust", "unknown"} {
		if c := dockerfileFor(lang); c == "" {
			t.Errorf("dockerfileFor(%q) returned empty", lang)
		}
	}
}

// TestPlanAndApply writes a Go project to a temp dir, runs Plan then Apply, and
// asserts the files land in the build context and the config is updated.
func TestPlanAndApply(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := store.Project{
		ID:   "demo",
		Name: "Demo App",
		Build: store.BuildSpec{
			Context:    dir,
			Dockerfile: "",
			ImageRepo:  "",
		},
		Deploy: store.DeploySpec{Method: "local-k3s", Namespace: "demo-ns"},
	}

	plan, err := PlanProject(p, "")
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if !plan.NeedsAny {
		t.Fatal("expected NeedsAny = true")
	}
	if plan.Dockerfile == nil || plan.Manifest == nil {
		t.Fatal("expected both dockerfile and manifest to be planned")
	}
	if plan.ConfigUpdates["deploy.deployment"] != "demo-app" {
		t.Errorf("deployment = %q, want demo-app", plan.ConfigUpdates["deploy.deployment"])
	}
	if plan.ConfigUpdates["build.image_repo"] != "demo-app" {
		t.Errorf("image_repo = %q, want demo-app", plan.ConfigUpdates["build.image_repo"])
	}

	// Apply needs a store; use a no-op满足接口的 stub via a real jsonStore is
	// heavy, so we just verify Apply writes files when given a stub store.
	stub := &stubStore{}
	if _, err := Apply(p, "", stub); err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		t.Errorf("Dockerfile not written: %v", err)
	}
	manifestPath := filepath.Join(dir, "k8s", "deployment.yaml")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Errorf("manifest not written: %v", err)
	}
	content, _ := os.ReadFile(manifestPath)
	if !strings.Contains(string(content), "name: demo-app") || !strings.Contains(string(content), "namespace: demo-ns") {
		t.Errorf("manifest missing expected fields:\n%s", content)
	}
}

// TestPlanAndApplyJava exercises the generate flow for a Java project, covering
// both the Maven (pom.xml) and Gradle (build.gradle) detection branches. The
// generated Dockerfile must match the Java toolchain (multi-stage build jar ->
// JRE) and the manifest must carry the sanitised deployment name + namespace.
func TestPlanAndApplyJava(t *testing.T) {
	cases := []struct {
		name       string
		marker     string
		markerBody string
		dockerWant []string // substrings expected in the generated Dockerfile
	}{
		{
			name:       "maven",
			marker:     "pom.xml",
			markerBody: "<project></project>",
			dockerWant: []string{"maven:3.9-eclipse-temurin-21", "target/*.jar", "MaxRAMPercentage=75.0", "/app/app.jar\""},
		},
		{
			name:       "gradle",
			marker:     "build.gradle",
			markerBody: "plugins { id 'java' }",
			dockerWant: []string{"gradle:8-jdk21", "build/libs/*.jar", "MaxRAMPercentage=75.0", "/app/app.jar\""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.marker), []byte(tc.markerBody), 0o644); err != nil {
				t.Fatal(err)
			}
			p := store.Project{
				ID:   "java-" + tc.name,
				Name: "Java Demo App",
				Build: store.BuildSpec{
					Context:    dir,
					Dockerfile: "",
					ImageRepo:  "",
				},
				Deploy: store.DeploySpec{Method: "local-k3s", Namespace: "java-ns"},
			}

			plan, err := PlanProject(p, "")
			if err != nil {
				t.Fatalf("Plan error: %v", err)
			}
			if !plan.NeedsAny {
				t.Fatal("expected NeedsAny = true")
			}
			if plan.Dockerfile == nil {
				t.Fatal("expected a Dockerfile to be planned")
			}
			if plan.Manifest == nil {
				t.Fatal("expected a manifest to be planned for local-k3s")
			}
			for _, want := range tc.dockerWant {
				if !strings.Contains(plan.Dockerfile.Content, want) {
					t.Errorf("generated Dockerfile missing %q:\n%s", want, plan.Dockerfile.Content)
				}
			}
			if got := plan.ConfigUpdates["deploy.deployment"]; got != "java-demo-app" {
				t.Errorf("deployment = %q, want java-demo-app", got)
			}
			if got := plan.ConfigUpdates["build.image_repo"]; got != "java-demo-app" {
				t.Errorf("image_repo = %q, want java-demo-app", got)
			}

			if _, err := Apply(p, "", &stubStore{}); err != nil {
				t.Fatalf("Apply error: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
				t.Errorf("Dockerfile not written: %v", err)
			}
			manifestPath := filepath.Join(dir, "k8s", "deployment.yaml")
			if _, err := os.Stat(manifestPath); err != nil {
				t.Errorf("manifest not written: %v", err)
			}
			content, _ := os.ReadFile(manifestPath)
			if !strings.Contains(string(content), "name: java-demo-app") || !strings.Contains(string(content), "namespace: java-ns") {
				t.Errorf("manifest missing expected fields:\n%s", content)
			}
			// Java 系默认内存应抬高到 limit 256Mi / request 64Mi（JVM 自身开销大）。
			if !strings.Contains(string(content), "memory: 256Mi") || !strings.Contains(string(content), "memory: 64Mi") {
				t.Errorf("java manifest should use memory 256Mi/64Mi, got:\n%s", content)
			}
		})
	}
}

// stubStore satisfies generate.projectStore minimally for Apply's UpdateProject call.
type stubStore struct{}

func (s *stubStore) UpdateProject(p store.Project) error { return nil }
