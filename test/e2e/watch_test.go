//go:build e2e

package e2e

import (
	"bytes"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// startImportWatch runs `flux4swarm import --watch` (Swarm stays the source
// of truth, Git follows) in the background until the test ends.
func startImportWatch(t *testing.T, repo *gitRepo) *syncBuffer {
	t.Helper()
	bin := requireController(t)
	args := []string{"import", "--watch", "--interval", "5s", "--debounce", "1s",
		"--cluster", clusterName, "--git-url", repo.remoteURL(), "--branch", branch, "--path", clusterPath,
		"--bootstrap", "--secret-store", "fake", "--secret-store-provider", "fake"}
	cmd := exec.Command(bin, args...)
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("flux4swarm import --watch output:\n%s", out.String())
		}
	})
	return out
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitInGit pulls until file satisfies cond ("" when it does not exist).
func waitInGit(t *testing.T, repo *gitRepo, file, what string, cond func(string) bool) {
	t.Helper()
	eventually(t, 2*time.Minute, file+" "+what, func() (bool, string) {
		repo.pull()
		s, _ := repo.read(file)
		return cond(s), s
	})
}

// Kubernetes-only additions of the platform team: a root kustomization.yaml
// that patches the exported Deployment, and a PodDisruptionBudget (Swarm has
// none) in platform/. In a real cluster the root also lists flux-system;
// the test's own Flux sync ignores that directory (see k8s.sync).
func platformFiles(ns string) map[string]string {
	return map[string]string{
		clusterPath + "/kustomization.yaml": `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - apps
  - platform
patches:
  - target:
      kind: Deployment
      name: web
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: web
        annotations:
          platform.example.com/tier: gold
`,
		clusterPath + "/platform/kustomization.yaml": kustomizationYAML([]string{"pdb-web.yaml"}),
		clusterPath + "/platform/pdb-web.yaml": `apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: web
  namespace: ` + ns + `
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app: web
`,
	}
}

// TestImportWatch: with `import --watch` the dev team keeps using Swarm
// directly and Git follows; the platform team's Kubernetes-only files
// outside apps/ survive every export, and Flux2 applies both together.
func TestImportWatch(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	ns := uniq("watch")
	cleanupStack(t, ns)
	svc := createStackService(t, ns, "web", "--replicas", "1")
	waitServiceRunning(t, svc, 1)
	out := startImportWatch(t, repo)

	deploy := importedDeployment(ns, "web")
	replicas := func(n string) *regexp.Regexp { return regexp.MustCompile(`replicas:\s*` + n + `\b`) }
	waitInGit(t, repo, deploy, "exported", func(s string) bool {
		return strings.Contains(s, "image: "+imageV1) && replicas("1").MatchString(s)
	})
	assertNoOwnLabels(t, mustService(t, svc))

	// The platform team adds Kubernetes-only work and edits a bootstrap file.
	fluxKs := repo.mustRead(fluxSysPath+"/kustomization.yaml") + "# edited by the platform team\n"
	platform := platformFiles(ns)
	platform[fluxSysPath+"/kustomization.yaml"] = fluxKs
	for f, c := range platform {
		repo.write(f, c)
	}
	repo.commitPush("platform: PDB and tier annotation for web")

	// The dev team deploys a new version straight to Swarm.
	docker(t, "service", "update", "--detach", "--image", imageV2, "--replicas", "2", svc)
	waitInGit(t, repo, deploy, "updated from Swarm", func(s string) bool {
		return strings.Contains(s, "image: "+imageV2) && replicas("2").MatchString(s)
	})
	assertFiles(t, repo, platform)

	// Nothing changes in Swarm: the periodic resyncs (every 5s) and the
	// task events of the rolling update must not add commits.
	waitServiceRunning(t, svc, 2)
	time.Sleep(3 * time.Second)
	repo.pull()
	head := repo.head()
	time.Sleep(15 * time.Second)
	repo.pull()
	if got := repo.head(); got != head {
		t.Errorf("import --watch committed without a Swarm change: %s -> %s\n%s", head, got, repo.git("log", "--stat", "-3"))
	}

	t.Run("flux2", func(t *testing.T) {
		k := requireK8s(t)
		k.validateApps(t, repo)
		s := k.sync(t, repo, fluxSyncOpts{Wait: false})
		s.reconcile(t)
		d := k.mustDeployment(t, ns, "web")
		if d.image() != imageV2 || d.replicas() != 2 {
			t.Errorf("k8s: deployment %s/web image=%q replicas=%d, want %s/2", ns, d.image(), d.replicas(), imageV2)
		}
		if got := k.kubectl(t, "get", "deployment", "web", "-n", ns, "-o", `jsonpath={.metadata.annotations.platform\.example\.com/tier}`); got != "gold" {
			t.Errorf("k8s: platform patch not applied, tier annotation = %q", got)
		}
		if !k.exists(t, ns, "poddisruptionbudget", "web") {
			t.Errorf("k8s: platform PodDisruptionBudget %s/web missing", ns)
		}
	})

	// The service is removed from Swarm: its manifests go, the platform
	// files stay (the patch has a target selector, so no match is fine).
	docker(t, "service", "rm", svc)
	waitInGit(t, repo, deploy, "removed", func(s string) bool { return s == "" })
	assertFiles(t, repo, platform)
	if strings.Contains(out.String(), "level=ERROR") {
		t.Errorf("import --watch logged errors:\n%s", out.String())
	}
}

// assertNoOwnLabels checks that watch mode never writes to Swarm.
func assertNoOwnLabels(t *testing.T, s serviceInfo) {
	t.Helper()
	if v, ok := s.Spec.Labels[managedByKey]; ok {
		t.Errorf("import --watch labelled the service %s=%s; Swarm must stay untouched", managedByKey, v)
	}
}

func assertFiles(t *testing.T, repo *gitRepo, files map[string]string) {
	t.Helper()
	for f, want := range files {
		if got, _ := repo.read(f); got != want {
			t.Errorf("%s was changed by import --watch:\n got: %q\nwant: %q", f, got, want)
		}
	}
}
