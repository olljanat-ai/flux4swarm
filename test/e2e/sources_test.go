//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGitRepositorySemVer: spec.ref.semver selects the highest matching Git
// tag, and a new matching tag moves the source while a higher major does not.
func TestGitRepositorySemVer(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	ns := uniq("gsemver")
	svc := swarmServiceName(ns, "web")
	cleanupStack(t, ns)

	release := func(tag, image string, replicas int) {
		t.Helper()
		repo.setApp(ns, map[string]string{"deployment-web.yaml": deploymentYAML(app{NS: ns, Name: "web", Image: image, Replicas: replicas})})
		repo.commitPush("release " + tag)
		repo.git("tag", "-a", tag, "-m", tag)
		repo.git("push", "-q", "origin", tag)
	}
	release("v1.0.0", imageV1, 1)
	release("v1.1.0", imageV2, 1)
	release("v2.0.0", imageV1, 2)
	// main moves past every tag; semver must ignore it.
	repo.setApp(ns, map[string]string{"deployment-web.yaml": deploymentYAML(app{NS: ns, Name: "web", Image: imageV1, Replicas: 3})})
	repo.commitPush("unreleased")

	out := reconcile(t, repo, "--semver", ">=1.0.0 <2.0.0")
	if !strings.Contains(out, "v1.1.0@sha1:") {
		t.Fatalf("want revision v1.1.0@sha1:..., got:\n%s", out)
	}
	s := mustService(t, svc)
	if s.image() != imageV2 || s.replicas() != 1 {
		t.Fatalf("service %s: image %s replicas %d, want %s x1 (tag v1.1.0)", svc, s.image(), s.replicas(), imageV2)
	}

	release("v1.2.0", imageV1, 1)
	out = reconcile(t, repo, "--semver", ">=1.0.0 <2.0.0")
	if !strings.Contains(out, "v1.2.0@sha1:") {
		t.Fatalf("want revision v1.2.0@sha1:..., got:\n%s", out)
	}
	if s := mustService(t, svc); s.image() != imageV1 || s.replicas() != 1 {
		t.Fatalf("service %s: image %s replicas %d, want %s x1 (tag v1.2.0)", svc, s.image(), s.replicas(), imageV1)
	}
}

// ---------------------------------------------------------------------------
// OCI
// ---------------------------------------------------------------------------

// registry is the OCI registry started by scripts/up.sh.
type registry struct {
	host        string // 127.0.0.1:PORT, from the host
	clusterHost string // NAME:5000, from kind pods
}

func requireRegistry(t *testing.T) registry {
	t.Helper()
	r := registry{
		host:        "127.0.0.1:" + envOr("E2E_REGISTRY_PORT", "23781"),
		clusterHost: envOr("E2E_REGISTRY_NAME", "flux4swarm-e2e-registry") + ":5000",
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + r.host + "/v2/")
	if err != nil {
		t.Fatalf("OCI registry not reachable at %s (run scripts/up.sh): %v", r.host, err)
	}
	resp.Body.Close()
	return r
}

// pushArtifact packs files into a Flux artifact with the pinned flux CLI
// (`flux push artifact`) and pushes it as repo:tag.
func pushArtifact(t *testing.T, k *k8s, url, tag string, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	k.flux(t, "push", "artifact", url+":"+tag, "--path", dir,
		"--source", "https://example.invalid/e2e", "--revision", tag+"@sha1:0000000000000000000000000000000000000000",
		"--insecure-registry")
}

// ociSyncYAML is an OCIRepository with a semver ref plus a Kustomization
// that applies the artifact root. The same objects work in Flux2.
func ociSyncYAML(name, url, semver string) string {
	return fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: %[1]s
  namespace: flux-system
spec:
  interval: 1m
  url: %[2]s
  insecure: true
  ref:
    semver: %[3]q
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %[1]s
  namespace: flux-system
spec:
  interval: 1m
  path: ./
  prune: true
  wait: true
  timeout: 3m
  sourceRef:
    kind: OCIRepository
    name: %[1]s
`, name, url, semver)
}

// TestOCIRepositorySemVer pushes Flux artifacts with `flux push artifact` to
// a local registry and reconciles an OCIRepository with spec.ref.semver
// into Swarm; with the K8s side up, stock Flux2 applies the very same
// objects in kind and must pick the same version.
func TestOCIRepositorySemVer(t *testing.T) {
	requireController(t)
	reg := requireRegistry(t)
	k := k8sEnv()
	if _, err := os.Stat(k.tool("flux")); err != nil {
		t.Skip("flux CLI not found (installed by scripts/k8s-up.sh); needed for `flux push artifact`")
	}
	ns := uniq("oci")
	svc := swarmServiceName(ns, "web")
	cleanupStack(t, ns)
	repoPath := "e2e/" + ns
	url := "oci://" + reg.host + "/" + repoPath

	artifact := func(image string, replicas int) map[string]string {
		return map[string]string{
			"namespace.yaml":      namespaceYAML(ns),
			"deployment-web.yaml": deploymentYAML(app{NS: ns, Name: "web", Image: image, Replicas: replicas}),
		}
	}
	pushArtifact(t, k, url, "1.0.0", artifact(imageV1, 1))
	pushArtifact(t, k, url, "1.1.0", artifact(imageV2, 1))
	pushArtifact(t, k, url, "2.0.0", artifact(imageV1, 2))
	pushArtifact(t, k, url, "latest", artifact(imageV1, 3))

	const semver = ">=1.0.0 <2.0.0"
	cfg := filepath.Join(t.TempDir(), "oci-sync.yaml")
	if err := os.WriteFile(cfg, []byte(ociSyncYAML("oci", url, semver)), 0o644); err != nil {
		t.Fatal(err)
	}
	reconcileOCI := func(wantTag string) {
		t.Helper()
		out := flux4swarm(t, "reconcile", "--config", cfg, "--cache-dir", t.TempDir())
		if !strings.Contains(out, "OCIRepository/flux-system/oci") || !strings.Contains(out, wantTag+"@sha256:") {
			t.Fatalf("want OCIRepository at %s@sha256:..., got:\n%s", wantTag, out)
		}
	}
	reconcileOCI("1.1.0")
	if s := mustService(t, svc); s.image() != imageV2 || s.replicas() != 1 {
		t.Fatalf("service %s: image %s replicas %d, want %s x1 (1.1.0)", svc, s.image(), s.replicas(), imageV2)
	}

	// The CLI shortcut resolves the same version (dry run: the services
	// stay owned by the Kustomization above).
	out := flux4swarm(t, "reconcile", "--url", url, "--insecure", "--semver", semver, "--dry-run", "--cache-dir", t.TempDir())
	if !strings.Contains(out, "1.1.0@sha256:") {
		t.Fatalf("reconcile --url oci://: want 1.1.0, got:\n%s", out)
	}

	if k.enabled != "0" {
		k = requireK8s(t)
		name := uniq("ocisync")
		k.applyYAML(t, ociSyncYAML(name, "oci://"+reg.clusterHost+"/"+repoPath, semver))
		t.Cleanup(func() {
			_, _ = k.try(t, "kubectl", "-n", "flux-system", "delete", "kustomization", name, "--wait", "--timeout", "3m")
			_, _ = k.try(t, "kubectl", "-n", "flux-system", "delete", "ocirepository", name, "--wait", "--timeout", "1m")
		})
		k.waitDeploymentAvailable(t, ns, "web", imageV2, 1)
		var src struct {
			Status struct{ Artifact struct{ Revision string } }
		}
		k.getJSON(t, "flux-system", "ocirepository", name, &src)
		if !strings.HasPrefix(src.Status.Artifact.Revision, "1.1.0@sha256:") {
			t.Fatalf("Flux2 OCIRepository revision %q, want 1.1.0@sha256:...", src.Status.Artifact.Revision)
		}
	}

	// A new matching version moves the source.
	pushArtifact(t, k, url, "1.2.0", artifact(imageV1, 1))
	reconcileOCI("1.2.0")
	if s := mustService(t, svc); s.image() != imageV1 || s.replicas() != 1 {
		t.Fatalf("service %s: image %s replicas %d, want %s x1 (1.2.0)", svc, s.image(), s.replicas(), imageV1)
	}
}
