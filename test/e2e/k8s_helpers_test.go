//go:build e2e

package e2e

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// k8s describes the kind cluster and git server started by scripts/k8s-up.sh.
// The defaults mirror scripts/env.sh; make passes the same variables.
type k8s struct {
	enabled        string // E2E_K8S: "0" off, "1" required, "" = use if up
	kubeconfig     string
	binDir         string
	gitRoot        string
	hostGitURL     string // http://127.0.0.1:PORT
	clusterGitURL  string // http://NAME:8080 (from pods)
	fluxComponents string
}

func k8sEnv() *k8s {
	wd, _ := os.Getwd()
	return &k8s{
		enabled:        os.Getenv("E2E_K8S"),
		kubeconfig:     envOr("E2E_KUBECONFIG", filepath.Join(stateDir(), "kubeconfig")),
		binDir:         envOr("E2E_BIN_DIR", filepath.Join(wd, ".bin")),
		gitRoot:        envOr("E2E_GIT_ROOT", filepath.Join(stateDir(), "git")),
		hostGitURL:     "http://127.0.0.1:" + envOr("E2E_GIT_PORT", "23780"),
		clusterGitURL:  "http://" + envOr("E2E_GIT_NAME", "flux4swarm-e2e-git") + ":8080",
		fluxComponents: envOr("FLUX_COMPONENTS", "source-controller,kustomize-controller"),
	}
}

func (k *k8s) gitServerUp() bool {
	if k.enabled == "0" {
		return false
	}
	if fi, err := os.Stat(k.gitRoot); err != nil || !fi.IsDir() {
		return false
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(k.hostGitURL + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// requireK8s returns the cluster, or skips the test when the K8s side is off
// (E2E_K8S=0) or not up. With E2E_K8S=1 (what `make e2e` sets) a missing
// cluster is a failure, not a skip.
func requireK8s(t *testing.T) *k8s {
	t.Helper()
	k := k8sEnv()
	if k.enabled == "0" {
		t.Skip("E2E_K8S=0: skipping Kubernetes/Flux2 test")
	}
	notUp := func(why string) {
		t.Helper()
		if k.enabled == "1" {
			t.Fatalf("E2E_K8S=1 but %s (run scripts/k8s-up.sh)", why)
		}
		t.Skipf("kind cluster not up (%s); run `make e2e-k8s-up` or set E2E_K8S=0", why)
	}
	if _, err := os.Stat(k.kubeconfig); err != nil {
		notUp("no kubeconfig at " + k.kubeconfig)
	}
	if out, err := k.try(t, "kubectl", "get", "crd", "kustomizations.kustomize.toolkit.fluxcd.io"); err != nil {
		notUp("Flux CRDs not reachable: " + out)
	}
	if !k.gitServerUp() {
		notUp("git server not reachable at " + k.hostGitURL)
	}
	return k
}

// tool resolves a CLI: the pinned copy in E2E_BIN_DIR, else PATH.
func (k *k8s) tool(name string) string {
	p := filepath.Join(k.binDir, name)
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

func (k *k8s) try(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	return runEnv(t, 5*time.Minute, "", []string{"KUBECONFIG=" + k.kubeconfig}, k.tool(name), args...)
}

func (k *k8s) must(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := k.try(t, name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

func (k *k8s) kubectl(t *testing.T, args ...string) string {
	t.Helper()
	return k.must(t, "kubectl", args...)
}
func (k *k8s) flux(t *testing.T, args ...string) string {
	t.Helper()
	return k.must(t, "flux", args...)
}

// applyYAML applies manifests from stdin.
func (k *k8s) applyYAML(t *testing.T, yaml string) {
	t.Helper()
	cmd := exec.Command(k.tool("kubectl"), "apply", "-f", "-")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+k.kubeconfig)
	cmd.Stdin = strings.NewReader(yaml)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kubectl apply: %v\n%s\n---\n%s", err, out, yaml)
	}
}

// getJSON returns an object as JSON, and whether it exists.
func (k *k8s) getJSON(t *testing.T, ns, kind, name string, into any) bool {
	t.Helper()
	args := []string{"get", kind, name, "-o", "json"}
	if ns != "" {
		args = append(args, "-n", ns)
	}
	out, err := k.try(t, "kubectl", args...)
	if err != nil {
		if strings.Contains(out, "NotFound") || strings.Contains(out, "not found") {
			return false
		}
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	if into != nil {
		if err := json.Unmarshal([]byte(out), into); err != nil {
			t.Fatalf("decode %s/%s: %v", kind, name, err)
		}
	}
	return true
}

func (k *k8s) exists(t *testing.T, ns, kind, name string) bool {
	t.Helper()
	return k.getJSON(t, ns, kind, name, nil)
}

// fluxSync is a test-owned GitRepository + Kustomization in flux-system that
// deploys path ./clusters/e2e of a test repo with stock Flux2. The repo's own
// flux-system directory is excluded with spec.ignore, so the tests never
// replace the cluster's Flux installation (it is validated separately).
type fluxSync struct {
	k    *k8s
	repo *gitRepo
	Name string
}

type fluxSyncOpts struct {
	// Wait makes the Kustomization health-check everything it applied
	// (spec.wait). Off for imported trees whose images may be pinned to
	// digests that are not preloaded into kind.
	Wait bool
}

func (k *k8s) sync(t *testing.T, repo *gitRepo, opts fluxSyncOpts) *fluxSync {
	t.Helper()
	if repo.ClusterURL == "" {
		t.Fatal("repo is not served by the git server")
	}
	s := &fluxSync{k: k, repo: repo, Name: uniq("sync")}
	k.applyYAML(t, `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: `+s.Name+`
  namespace: flux-system
spec:
  interval: 1m
  url: `+repo.ClusterURL+`
  ref:
    branch: `+branch+`
  ignore: |
    /`+fluxSysPath+`/
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: `+s.Name+`
  namespace: flux-system
spec:
  interval: 1m
  path: ./`+clusterPath+`
  prune: true
  wait: `+boolStr(opts.Wait)+`
  timeout: 3m
  sourceRef:
    kind: GitRepository
    name: `+s.Name+`
`)
	t.Cleanup(func() {
		// prune: true -> deleting the Kustomization garbage-collects what it applied.
		_, _ = k.try(t, "kubectl", "-n", "flux-system", "delete", "kustomization", s.Name, "--wait", "--timeout", "3m")
		_, _ = k.try(t, "kubectl", "-n", "flux-system", "delete", "gitrepository", s.Name, "--wait", "--timeout", "1m")
	})
	return s
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// reconcile makes Flux fetch the repo and apply it, and fails unless the
// Kustomization is Ready at the repo's current HEAD.
func (s *fluxSync) reconcile(t *testing.T) {
	t.Helper()
	want := s.repo.head()
	out, err := s.k.try(t, "flux", "reconcile", "kustomization", s.Name, "-n", "flux-system",
		"--with-source", "--timeout", "4m")
	var ks struct {
		Status struct {
			LastAppliedRevision string
			Conditions          []struct{ Type, Status, Reason, Message string }
		}
	}
	s.k.getJSON(t, "flux-system", "kustomization", s.Name, &ks)
	ready := false
	for _, c := range ks.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			ready = true
		}
	}
	if err != nil || !ready || !strings.HasSuffix(ks.Status.LastAppliedRevision, want) {
		t.Fatalf("Flux Kustomization %s not Ready at %s (lastApplied=%s, conditions=%+v): %v\n%s\n%s",
			s.Name, want, ks.Status.LastAppliedRevision, ks.Status.Conditions, err, out,
			s.k.flux(t, "get", "all", "-n", "flux-system"))
	}
}

// ---------------------------------------------------------------------------
// object helpers
// ---------------------------------------------------------------------------

type deployment struct {
	Spec struct {
		Replicas *int
		Template struct {
			Spec struct {
				Containers []struct{ Image string }
				Volumes    []json.RawMessage
			}
		}
	}
	Status struct {
		AvailableReplicas  int
		UpdatedReplicas    int
		Replicas           int
		ObservedGeneration int64
	}
	Metadata struct{ Generation int64 }
}

func (d deployment) image() string {
	if len(d.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	return d.Spec.Template.Spec.Containers[0].Image
}

func (d deployment) replicas() int {
	if d.Spec.Replicas == nil {
		return 1
	}
	return *d.Spec.Replicas
}

func (k *k8s) mustDeployment(t *testing.T, ns, name string) deployment {
	t.Helper()
	var d deployment
	if !k.getJSON(t, ns, "deployment", name, &d) {
		t.Fatalf("deployment %s/%s not found; objects:\n%s", ns, name, k.kubectl(t, "get", "all", "-n", ns))
	}
	return d
}

// waitDeploymentAvailable waits until the rollout of image with replicas is done.
func (k *k8s) waitDeploymentAvailable(t *testing.T, ns, name, image string, replicas int) {
	t.Helper()
	eventually(t, 3*time.Minute, "deployment "+ns+"/"+name+" available", func() (bool, string) {
		var d deployment
		if !k.getJSON(t, ns, "deployment", name, &d) {
			return false, "not found"
		}
		ok := d.image() == image && d.replicas() == replicas &&
			d.Status.ObservedGeneration >= d.Metadata.Generation &&
			d.Status.UpdatedReplicas == replicas && d.Status.AvailableReplicas == replicas && d.Status.Replicas == replicas
		return ok, strings.TrimSpace(k.kubectl(t, "get", "deploy,pods", "-n", ns, "-o", "wide"))
	})
}

// secretValue returns data[key] of a Kubernetes Secret, decoded.
func (k *k8s) secretValue(t *testing.T, ns, name, key string) (string, bool) {
	t.Helper()
	var s struct{ Data map[string]string }
	if !k.getJSON(t, ns, "secret", name, &s) {
		return "", false
	}
	v, ok := s.Data[key]
	if !ok {
		return "", false
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		t.Fatalf("decode secret %s/%s[%s]: %v", ns, name, key, err)
	}
	return string(b), true
}

// validateBootstrap checks clusters/e2e/flux-system the way Flux would use
// it: it must build with kustomize, contain the flux-system GitRepository and
// Kustomization (path ./clusters/e2e), pass a server-side dry-run against the
// stock Flux CRDs in kind, and `flux build kustomization` (offline, the same
// generator kustomize-controller uses) must build the whole cluster path.
func (k *k8s) validateBootstrap(t *testing.T, repo *gitRepo) {
	t.Helper()
	dir := filepath.Join(repo.Work, fluxSysPath)
	built := k.kubectl(t, "kustomize", dir)
	for _, want := range []string{"kind: GitRepository", "kind: Kustomization", "name: flux-system", "path: ./" + clusterPath} {
		if !strings.Contains(built, want) {
			t.Errorf("kubectl kustomize %s: output lacks %q", fluxSysPath, want)
		}
	}
	k.kubectl(t, "apply", "--dry-run=server", "-k", dir)
	k.flux(t, "build", "kustomization", "flux-system", "--dry-run",
		"--path", filepath.Join(repo.Work, clusterPath),
		"--kustomization-file", filepath.Join(dir, "gotk-sync.yaml"))
}

// validateApps runs `kubectl kustomize` (offline) on every directory below
// apps/ that has a kustomization.yaml.
func (k *k8s) validateApps(t *testing.T, repo *gitRepo) {
	t.Helper()
	root := filepath.Join(repo.Work, appsPath)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "kustomization.yaml" {
			return err
		}
		k.kubectl(t, "kustomize", filepath.Dir(p))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", appsPath, err)
	}
}
