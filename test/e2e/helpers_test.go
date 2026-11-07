//go:build e2e

// Package e2e holds end-to-end tests that run the flux4swarm binary against a
// real single-node Docker Swarm (scripts/up.sh) and check that the very same
// Git repo is also deployable by stock Flux2 on a kind cluster
// (scripts/k8s-up.sh).
//
// Every assumption about flux4swarm (CLI, repo layout, Swarm naming and
// labels, import output) lives in the "flux4swarm contract" section of this
// file, so it can be adjusted in one place. README.md lists the same
// assumptions.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	clusterName = "e2e"
	clusterPath = "clusters/" + clusterName
	appsPath    = clusterPath + "/apps"
	fluxSysPath = clusterPath + "/flux-system"
	branch      = "main"

	// Label put on every Swarm object the tests create directly, so leftovers
	// can be cleaned (scripts/down.sh) without touching anything else.
	e2eLabel = "io.flux4swarm.e2e=true"

	imageV1 = "nginx:alpine"
	imageV2 = "nginx:stable-alpine"

	// Where the test Deployments mount their ConfigMap / Secret.
	configMountPath = "/etc/e2e-config"
	secretMountPath = "/etc/e2e-secret"
)

// ===========================================================================
// flux4swarm contract (the only place that knows the CLI and the mapping)
// ===========================================================================

const (
	// Labels flux4swarm puts on Swarm objects it manages.
	managedByKey = "app.kubernetes.io/managed-by"
	managedByVal = "flux4swarm"
	stackKey     = "com.docker.stack.namespace"
	// Logical name of a versioned (hashed) config/secret.
	nameLabel = "flux4swarm.io/name"
)

// swarmServiceName maps a workload in a namespace to its Swarm service name:
// namespace = stack, so web in demo -> demo_web; default -> web.
func swarmServiceName(ns, name string) string {
	if ns == "default" {
		return name
	}
	return ns + "_" + name
}

// versionedNameRE matches the Swarm config/secret created for one key of a
// ConfigMap/Secret: NS_OBJECT_KEY-HASH10 (no NS_ prefix in default).
func versionedNameRE(ns, obj, key string) *regexp.Regexp {
	prefix := ns + "_"
	if ns == "default" {
		prefix = ""
	}
	return regexp.MustCompile("^" + regexp.QuoteMeta(prefix+obj+"_"+key) + "-[A-Za-z0-9]{10}$")
}

// Files written by `flux4swarm import` (clusters/NAME/apps/NS/...).
func appDir(ns string) string                { return appsPath + "/" + ns }
func importedDeployment(ns, n string) string { return appDir(ns) + "/deployment-" + n + ".yaml" }
func importedPVC(ns, n string) string        { return appDir(ns) + "/persistentvolumeclaim-" + n + ".yaml" }
func importedHTTPRoute(ns, r string) string  { return appDir(ns) + "/httproute-" + r + ".yaml" }
func importedGateway() string                { return appsPath + "/traefik/gateway.yaml" }

// flux4swarmBin returns the controller binary path: $FLUX4SWARM_BIN, or
// <repo>/bin/flux4swarm by default.
func flux4swarmBin() string {
	if p := os.Getenv("FLUX4SWARM_BIN"); p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	abs, _ := filepath.Abs(filepath.Join("..", "..", "bin", "flux4swarm"))
	return abs
}

// requireController skips the test when the controller binary is not built.
func requireController(t *testing.T) string {
	t.Helper()
	bin := flux4swarmBin()
	fi, err := os.Stat(bin)
	if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		t.Skipf("flux4swarm binary not found at %s (run `make build` or set FLUX4SWARM_BIN); skipping controller test", bin)
	}
	return bin
}

// flux4swarm runs the controller with args and fails the test on error.
func flux4swarm(t *testing.T, args ...string) string {
	t.Helper()
	bin := requireController(t)
	out, err := run(t, 5*time.Minute, "", bin, args...)
	if err != nil {
		t.Fatalf("flux4swarm %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// importToGit exports the current Swarm state into the Git repo (Swarm ->
// Git). The binary clones, commits and pushes. Like reconcile, it uses the
// smart-HTTP URL when the git server is up, else file://. The URL ends up in
// flux-system/gotk-sync.yaml, which is why an http(s) URL matters for Flux.
func importToGit(t *testing.T, repo *gitRepo, extra ...string) string {
	t.Helper()
	args := []string{"import",
		"--cluster", clusterName,
		"--git-url", repo.remoteURL(), "--branch", branch,
		"--path", clusterPath,
	}
	return flux4swarm(t, append(args, extra...)...)
}

// importFlags is what the import tests pass: adopt running services and use
// the ESO fake provider for secrets.
var importFlags = []string{"--adopt", "--bootstrap", "--secret-store", "fake", "--secret-store-provider", "fake"}

// reconcile applies the Git repo to Swarm once (Git -> Swarm). It uses the
// HTTP URL when the git server is up (same URL scheme a real deployment would
// use) and file:// otherwise. Exits non-zero unless the result is Ready.
func reconcile(t *testing.T, repo *gitRepo, extra ...string) string {
	t.Helper()
	args := []string{"reconcile",
		"--url", repo.remoteURL(), "--branch", branch,
		"--path", "./" + clusterPath,
		"--wait", "--timeout", "3m",
	}
	return flux4swarm(t, append(args, extra...)...)
}

// ===========================================================================
// manifests (standard Kubernetes / Flux / ESO objects)
// ===========================================================================

// app describes the test workload: one Deployment, optionally mounting one
// key of a ConfigMap and of a Secret.
type app struct {
	NS, Name, Image string
	Replicas        int
	ConfigMap, Key  string // mounted at configMountPath/Key
	Secret          string // mounted at secretMountPath/Key (same Key)
}

func namespaceYAML(ns string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
`, ns)
}

func deploymentYAML(a app) string {
	var vols, mounts strings.Builder
	if a.ConfigMap != "" {
		fmt.Fprintf(&mounts, "            - name: config\n              mountPath: %s\n              readOnly: true\n", configMountPath)
		fmt.Fprintf(&vols, "        - name: config\n          configMap:\n            name: %s\n", a.ConfigMap)
	}
	if a.Secret != "" {
		fmt.Fprintf(&mounts, "            - name: secret\n              mountPath: %s\n              readOnly: true\n", secretMountPath)
		fmt.Fprintf(&vols, "        - name: secret\n          secret:\n            secretName: %s\n", a.Secret)
	}
	m, v := mounts.String(), vols.String()
	if m != "" {
		m = "          volumeMounts:\n" + m
		v = "      volumes:\n" + v
	}
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    app: %[1]s
    io.flux4swarm.e2e: "true"
spec:
  replicas: %[4]d
  selector:
    matchLabels:
      app: %[1]s
  template:
    metadata:
      labels:
        app: %[1]s
    spec:
      containers:
        - name: %[1]s
          image: %[3]s
          env:
            - name: E2E
              value: "1"
%[5]s%[6]s`, a.Name, a.NS, a.Image, a.Replicas, m, v)
}

func configMapYAML(ns, name, key, value string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  %s: %q
`, name, ns, key, value)
}

// secretStoreYAML is an ESO SecretStore with the fake provider (works on both
// Swarm and Kubernetes).
func secretStoreYAML(ns, key, value string) string {
	return fmt.Sprintf(`apiVersion: external-secrets.io/v1
kind: SecretStore
metadata:
  name: fake
  namespace: %s
spec:
  provider:
    fake:
      data:
        - key: %s
          value: %q
`, ns, key, value)
}

// externalSecretYAML creates Secret `name` with data[key] = store[remoteKey].
func externalSecretYAML(ns, name, key, remoteKey string) string {
	return fmt.Sprintf(`apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  refreshInterval: 1m
  secretStoreRef:
    kind: SecretStore
    name: fake
  target:
    name: %[1]s
  data:
    - secretKey: %[3]s
      remoteRef:
        key: %[4]s
`, name, ns, key, remoteKey)
}

func kustomizationYAML(resources []string) string {
	var b strings.Builder
	b.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n")
	if len(resources) == 0 {
		b.WriteString("resources: []\n")
		return b.String()
	}
	b.WriteString("resources:\n")
	for _, r := range resources {
		b.WriteString("  - " + r + "\n")
	}
	return b.String()
}

// ===========================================================================
// process helpers
// ===========================================================================

func run(t *testing.T, timeout time.Duration, dir, name string, args ...string) (string, error) {
	t.Helper()
	return runEnv(t, timeout, dir, nil, name, args...)
}

func runEnv(t *testing.T, timeout time.Duration, dir string, env []string, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return strings.TrimSpace(buf.String()), err
}

// runIn runs a command with stdin and fails on error.
func runIn(t *testing.T, stdin, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		ok, msg := cond()
		if ok {
			return
		}
		last = msg
		time.Sleep(time.Second)
	}
	t.Fatalf("timed out after %s waiting for %s; last: %s", timeout, what, last)
}

// uniq returns a short, per-run unique name that is valid as a Kubernetes
// namespace/object name and as a Swarm stack/service name.
func uniq(prefix string) string {
	return fmt.Sprintf("e2e-%s-%d", prefix, time.Now().UnixNano()%1_000_000)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// stateDir mirrors E2E_STATE_DIR in scripts/env.sh.
func stateDir() string {
	return envOr("E2E_STATE_DIR", filepath.Join(envOr("TMPDIR", "/tmp"), "flux4swarm-e2e"))
}

// ===========================================================================
// swarm helpers
// ===========================================================================

// docker runs the docker CLI against $DOCKER_HOST and fails on error.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := run(t, 2*time.Minute, "", "docker", args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// dockerTry runs the docker CLI and ignores failures (for cleanup).
func dockerTry(t *testing.T, args ...string) {
	t.Helper()
	_, _ = run(t, 2*time.Minute, "", "docker", args...)
}

type serviceInfo struct {
	Version   struct{ Index uint64 }
	UpdatedAt string
	Spec      struct {
		Name         string
		Labels       map[string]string
		TaskTemplate struct {
			ContainerSpec struct {
				Image  string
				Env    []string
				Mounts []struct {
					Type, Source, Target string
					ReadOnly             bool
				}
			}
		}
		Mode struct {
			Replicated *struct{ Replicas *uint64 }
		}
	}
}

func (s serviceInfo) image() string {
	// Swarm may pin the tag to a digest: nginx:alpine@sha256:...
	img, _, _ := strings.Cut(s.Spec.TaskTemplate.ContainerSpec.Image, "@")
	return img
}

func (s serviceInfo) replicas() uint64 {
	if s.Spec.Mode.Replicated == nil || s.Spec.Mode.Replicated.Replicas == nil {
		return 0
	}
	return *s.Spec.Mode.Replicated.Replicas
}

// inspectService returns the service and whether it exists.
func inspectService(t *testing.T, name string) (serviceInfo, bool) {
	t.Helper()
	out, err := run(t, time.Minute, "", "docker", "service", "inspect", name)
	if err != nil {
		return serviceInfo{}, false
	}
	var s []serviceInfo
	if err := json.Unmarshal([]byte(out), &s); err != nil || len(s) != 1 {
		t.Fatalf("decode service inspect %s: %v\n%s", name, err, out)
	}
	return s[0], true
}

func mustService(t *testing.T, name string) serviceInfo {
	t.Helper()
	s, ok := inspectService(t, name)
	if !ok {
		t.Fatalf("service %s does not exist; services:\n%s", name, docker(t, "service", "ls"))
	}
	return s
}

// waitServiceRunning waits until the service has `want` running tasks.
func waitServiceRunning(t *testing.T, name string, want int) {
	t.Helper()
	eventually(t, 2*time.Minute, "service "+name+" running", func() (bool, string) {
		out, _ := run(t, time.Minute, "", "docker", "service", "ps", name,
			"--filter", "desired-state=running", "--format", "{{.CurrentState}}")
		n := 0
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "Running") {
				n++
			}
		}
		return n == want, out
	})
}

// taskTemplate returns the service's Spec.TaskTemplate as JSON.
func taskTemplate(t *testing.T, name string) string {
	t.Helper()
	return docker(t, "service", "inspect", name, "--format", "{{json .Spec.TaskTemplate}}")
}

// runningTasks returns the IDs of the service's tasks that should be running.
func runningTasks(t *testing.T, name string) string {
	t.Helper()
	return strings.Join(strings.Fields(docker(t, "service", "ps", "-q", "--filter", "desired-state=running", name)), ",")
}

// waitServiceGone waits until the service no longer exists.
func waitServiceGone(t *testing.T, name string) {
	t.Helper()
	eventually(t, time.Minute, "service "+name+" removed", func() (bool, string) {
		_, exists := inspectService(t, name)
		return !exists, "still exists"
	})
}

// objectsByLabel lists config/secret/service names carrying a label filter
// (key or key=value).
func objectsByLabel(t *testing.T, kind, label string) []string {
	t.Helper()
	out := docker(t, kind, "ls", "--filter", "label="+label, "--format", "{{.Name}}")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// versionedObjects returns the Swarm configs/secrets flux4swarm created for
// one key of a ConfigMap/Secret (labelled flux4swarm.io/name, named
// NS_OBJECT_KEY-HASH10).
func versionedObjects(t *testing.T, kind, ns, obj, key string) []string {
	t.Helper()
	re := versionedNameRE(ns, obj, key)
	var got []string
	for _, n := range objectsByLabel(t, kind, nameLabel) {
		if re.MatchString(n) {
			got = append(got, n)
		}
	}
	return got
}

// serviceFile reads a file from a running task container of a service.
func serviceFile(t *testing.T, service, path string) string {
	t.Helper()
	var out string
	eventually(t, time.Minute, "a running container of "+service, func() (bool, string) {
		ids := docker(t, "ps", "-q", "--filter", "label=com.docker.swarm.service.name="+service)
		if ids == "" {
			return false, "no container"
		}
		var err error
		out, err = run(t, time.Minute, "", "docker", "exec", strings.Fields(ids)[0], "cat", path)
		return err == nil, out
	})
	return out
}

// cleanupServices removes services at test end.
func cleanupServices(t *testing.T, names ...string) {
	t.Cleanup(func() {
		for _, n := range names {
			dockerTry(t, "service", "rm", n)
		}
	})
}

// cleanupStack removes everything of a stack (namespace) at test end:
// services, then configs/secrets (which may be named NS_*-HASH), then
// volumes. Swarm refuses to remove configs/secrets still in use, so services
// go first.
func cleanupStack(t *testing.T, ns string) {
	t.Cleanup(func() {
		for _, s := range objectsByLabel(t, "service", stackKey+"="+ns) {
			dockerTry(t, "service", "rm", s)
		}
		eventually(t, time.Minute, "stack "+ns+" containers gone", func() (bool, string) {
			out, _ := run(t, time.Minute, "", "docker", "ps", "-aq", "--filter", "label="+stackKey+"="+ns)
			return out == "", out
		})
		for _, kind := range []string{"config", "secret"} {
			out, _ := run(t, time.Minute, "", "docker", kind, "ls", "--format", "{{.Name}}")
			for _, n := range strings.Split(out, "\n") {
				if strings.HasPrefix(n, ns+"_") {
					dockerTry(t, kind, "rm", n)
				}
			}
		}
		out, _ := run(t, time.Minute, "", "docker", "volume", "ls", "-q", "--filter", "name="+ns+"_")
		for _, v := range strings.Fields(out) {
			dockerTry(t, "volume", "rm", v)
		}
	})
}

// ===========================================================================
// git helpers
// ===========================================================================

// gitRepo is a bare repo (the GitOps source) plus a working clone used by the
// tests to commit desired state.
//
//   - URL      file:// URL of the bare repo (pushes, flux4swarm import)
//   - HTTPURL  smart-HTTP URL from the host ("" without git server)
//   - ClusterURL the same repo as seen from pods in kind ("" without it)
type gitRepo struct {
	URL, HTTPURL, ClusterURL string
	Work                     string
	t                        *testing.T
}

// remoteURL is the URL flux4swarm is given: smart HTTP when served, else file://.
func (r *gitRepo) remoteURL() string {
	if r.HTTPURL != "" {
		return r.HTTPURL
	}
	return r.URL
}

func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	return gitIn(r.t, r.Work, args...)
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{"-c", "user.name=e2e", "-c", "user.email=e2e@example.invalid",
		"-c", "init.defaultBranch=" + branch, "-c", "commit.gpgsign=false",
		"-c", "push.negotiate=false"}
	out, err := run(t, time.Minute, dir, "git", append(base, args...)...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// newGitRepo creates a bare repo with an initial commit on main. When the
// git server is up (K8s side enabled), the repo lives in E2E_GIT_ROOT so it is
// also served over HTTP to the host and to the kind cluster.
func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	root := t.TempDir()
	name := strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())) + fmt.Sprintf("-%d.git", time.Now().UnixNano()%1_000_000)
	bareDir := root
	k := k8sEnv()
	served := k.gitServerUp()
	if served {
		bareDir = k.gitRoot
	}
	bare := filepath.Join(bareDir, name)
	gitIn(t, root, "init", "-q", "--bare", "-b", branch, bare)
	if served {
		t.Cleanup(func() { _ = os.RemoveAll(bare) })
	}
	r := &gitRepo{URL: "file://" + bare, Work: filepath.Join(root, "work"), t: t}
	if served {
		r.HTTPURL = k.hostGitURL + "/" + name
		r.ClusterURL = k.clusterGitURL + "/" + name
	}
	gitIn(t, root, "clone", "-q", r.URL, r.Work)
	r.write("README.md", "flux4swarm e2e fixture\n")
	r.commitPush("initial commit")
	return r
}

func (r *gitRepo) write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.Work, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *gitRepo) commitPush(msg string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	r.git("push", "-q", "origin", "HEAD:"+branch)
}

// head returns the commit SHA of main in the working clone.
func (r *gitRepo) head() string {
	r.t.Helper()
	return r.git("rev-parse", "HEAD")
}

// pull refreshes the working clone (e.g. after flux4swarm import pushed).
func (r *gitRepo) pull() {
	r.t.Helper()
	r.git("pull", "-q", "--rebase", "origin", branch)
}

func (r *gitRepo) read(rel string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(r.Work, rel))
	return string(b), err == nil
}

func (r *gitRepo) mustRead(rel string) string {
	r.t.Helper()
	s, ok := r.read(rel)
	if !ok {
		r.t.Fatalf("missing %s; repo tree:\n%s", rel, r.git("ls-files"))
	}
	return s
}

// glob lists repo files matching a pattern relative to the work tree.
func (r *gitRepo) glob(pattern string) []string {
	r.t.Helper()
	m, err := filepath.Glob(filepath.Join(r.Work, pattern))
	if err != nil {
		r.t.Fatal(err)
	}
	for i := range m {
		m[i], _ = filepath.Rel(r.Work, m[i])
	}
	return m
}

// setApp replaces clusters/e2e/apps/NS with files (name -> content) plus
// namespace.yaml and a kustomization.yaml listing them. A nil map removes the
// namespace directory. apps/kustomization.yaml is regenerated to list the
// namespace directories, so the path always exists (also when it is empty).
func (r *gitRepo) setApp(ns string, files map[string]string) {
	r.t.Helper()
	dir := filepath.Join(r.Work, appDir(ns))
	if err := os.RemoveAll(dir); err != nil {
		r.t.Fatal(err)
	}
	if files != nil {
		res := []string{"namespace.yaml"}
		r.write(appDir(ns)+"/namespace.yaml", namespaceYAML(ns))
		for name, content := range files {
			r.write(appDir(ns)+"/"+name, content)
			res = append(res, name)
		}
		sort.Strings(res[1:])
		r.write(appDir(ns)+"/kustomization.yaml", kustomizationYAML(res))
	}
	entries, _ := os.ReadDir(filepath.Join(r.Work, appsPath))
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	r.write(appsPath+"/kustomization.yaml", kustomizationYAML(dirs))
}

// writeBootstrap writes clusters/e2e/flux-system exactly as `flux bootstrap`
// would (pinned flux CLI): gotk-components.yaml, gotk-sync.yaml pointing at
// the in-cluster URL of this repo, and kustomization.yaml.
func (r *gitRepo) writeBootstrap(k *k8s) {
	r.t.Helper()
	comps := k.flux(r.t, "install", "--export", "--components", k.fluxComponents)
	src := k.flux(r.t, "create", "source", "git", "flux-system", "--url", r.ClusterURL,
		"--branch", branch, "--interval", "1m", "--export")
	ks := k.flux(r.t, "create", "kustomization", "flux-system", "--source", "GitRepository/flux-system",
		"--path", "./"+clusterPath, "--prune=true", "--interval", "10m", "--export")
	r.write(fluxSysPath+"/gotk-components.yaml", comps+"\n")
	r.write(fluxSysPath+"/gotk-sync.yaml", src+"\n---\n"+ks+"\n")
	r.write(fluxSysPath+"/kustomization.yaml", kustomizationYAML([]string{"gotk-components.yaml", "gotk-sync.yaml"}))
}
