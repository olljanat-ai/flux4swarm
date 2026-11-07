//go:build e2e

// Package e2e holds end-to-end tests that run the flux4swarm binary against a
// real single-node Docker Swarm (see scripts/up.sh) and a local bare Git repo.
//
// Every assumption about the controller's CLI lives in the flux4swarm* helpers
// below; adjust them there if the CLI changes.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	clusterName = "e2e"
	clusterPath = "clusters/" + clusterName
	swarmPath   = clusterPath + "/swarm"
	branch      = "main"

	// Label put on every object the tests create directly, so leftovers can be
	// cleaned (scripts/down.sh) without touching anything else.
	e2eLabel = "io.flux4swarm.e2e=true"
	// Labels flux4swarm puts on objects it manages.
	managedByLabel = "app.kubernetes.io/managed-by=flux4swarm"
	nameLabel      = "flux4swarm.io/name"

	imageV1 = "nginx:alpine"
	imageV2 = "nginx:stable-alpine"
)

// ---------------------------------------------------------------------------
// flux4swarm invocation (the only place that knows the CLI)
// ---------------------------------------------------------------------------

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

// importToGit exports the current Swarm state into the Git repo (Swarm -> Git).
func importToGit(t *testing.T, repo *gitRepo, extra ...string) string {
	t.Helper()
	args := []string{"import",
		"--cluster", clusterName,
		"--git-url", repo.URL, "--branch", branch,
		"--path", clusterPath,
	}
	return flux4swarm(t, append(args, extra...)...)
}

// reconcile applies the Git repo to Swarm once (Git -> Swarm).
func reconcile(t *testing.T, repo *gitRepo, extra ...string) string {
	t.Helper()
	args := []string{"reconcile",
		"--url", repo.URL, "--branch", branch,
		"--path", "./" + clusterPath,
		"--wait", "--timeout", "3m",
	}
	return flux4swarm(t, append(args, extra...)...)
}

// ---------------------------------------------------------------------------
// process helpers
// ---------------------------------------------------------------------------

func run(t *testing.T, timeout time.Duration, dir, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return strings.TrimSpace(buf.String()), err
}

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

// ---------------------------------------------------------------------------
// swarm helpers
// ---------------------------------------------------------------------------

type serviceInfo struct {
	Version   struct{ Index uint64 }
	UpdatedAt string
	Spec      struct {
		Name         string
		Labels       map[string]string
		TaskTemplate struct {
			ContainerSpec struct {
				Image string
				Env   []string
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

// objectsByLabel lists config/secret/service names carrying a label.
func objectsByLabel(t *testing.T, kind, label string) []string {
	t.Helper()
	out := docker(t, kind, "ls", "--filter", "label="+label, "--format", "{{.Name}}")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// cleanupObjects removes services/configs/secrets by name (or by flux4swarm
// name label, since flux4swarm hashes config/secret names) at test end.
func cleanupObjects(t *testing.T, kind string, names ...string) {
	t.Cleanup(func() {
		for _, n := range names {
			dockerTry(t, kind, "rm", n)
			if kind != "service" {
				for _, id := range objectsByLabel(t, kind, nameLabel+"="+n) {
					dockerTry(t, kind, "rm", id)
				}
			}
		}
	})
}

// uniq returns a short, per-run unique object name.
func uniq(prefix string) string {
	return fmt.Sprintf("e2e-%s-%d", prefix, time.Now().UnixNano()%1_000_000)
}

// ---------------------------------------------------------------------------
// git helpers
// ---------------------------------------------------------------------------

// gitRepo is a bare repo (the GitOps source, reachable via file://) plus a
// working clone used by the tests to commit desired state.
type gitRepo struct {
	URL  string
	Work string
	t    *testing.T
}

func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	return gitIn(r.t, r.Work, args...)
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{"-c", "user.name=e2e", "-c", "user.email=e2e@example.invalid",
		"-c", "init.defaultBranch=" + branch, "-c", "commit.gpgsign=false"}
	out, err := run(t, time.Minute, dir, "git", append(base, args...)...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// newGitRepo creates a bare repo with an initial commit on main.
func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "source.git")
	gitIn(t, root, "init", "--bare", "-b", branch, bare)
	r := &gitRepo{URL: "file://" + bare, Work: filepath.Join(root, "work"), t: t}
	gitIn(t, root, "clone", r.URL, r.Work)
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

func (r *gitRepo) remove(rel string) {
	r.t.Helper()
	r.git("rm", "-q", rel)
}

func (r *gitRepo) commitPush(msg string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", msg)
	r.git("push", "-q", "origin", "HEAD:"+branch)
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

// ---------------------------------------------------------------------------
// manifests (layout: clusters/<cluster>/swarm/<kind>s/<name>.yaml)
// ---------------------------------------------------------------------------

func servicePath(name string) string { return swarmPath + "/services/" + name + ".yaml" }
func configPath(name string) string  { return swarmPath + "/configs/" + name + ".yaml" }
func secretPath(name string) string  { return swarmPath + "/secrets/" + name + ".yaml" }
func secretStorePath() string        { return swarmPath + "/secrets/secret-store.yaml" }

// serviceYAML renders a Service manifest; spec is a Docker ServiceSpec.
func serviceYAML(name, image string, replicas int) string {
	return fmt.Sprintf(`apiVersion: swarm.flux4swarm.io/v1alpha1
kind: Service
metadata:
  name: %[1]s
spec:
  Name: %[1]s
  Labels:
    io.flux4swarm.e2e: "true"
  TaskTemplate:
    ContainerSpec:
      Image: %[2]s
      Env:
        - E2E=1
  Mode:
    Replicated:
      Replicas: %[3]d
`, name, image, replicas)
}

func configYAML(name, data string) string {
	return fmt.Sprintf(`apiVersion: swarm.flux4swarm.io/v1alpha1
kind: Config
metadata:
  name: %s
spec:
  data: %q
`, name, data)
}

func secretStoreYAML(key, value string) string {
	return fmt.Sprintf(`apiVersion: external-secrets.io/v1
kind: SecretStore
metadata:
  name: fake
spec:
  provider:
    fake:
      data:
        - key: %s
          value: %q
`, key, value)
}

func externalSecretYAML(name, key string) string {
	return fmt.Sprintf(`apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: %[1]s
spec:
  secretStoreRef:
    kind: SecretStore
    name: fake
  target:
    name: %[1]s
  data:
    - secretKey: %[1]s
      remoteRef:
        key: %[2]s
`, name, key)
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
