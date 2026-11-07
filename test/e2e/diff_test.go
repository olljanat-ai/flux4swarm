//go:build e2e

package e2e

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// diff runs `flux4swarm diff` against the Git repo and returns its output
// and exit code.
func diff(t *testing.T, repo *gitRepo) (string, int) {
	t.Helper()
	out, err := run(t, 5*time.Minute, "", requireController(t), "diff",
		"--url", repo.remoteURL(), "--branch", branch, "--path", "./"+clusterPath)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out, 0
	case errors.As(err, &ee):
		return out, ee.ExitCode()
	}
	t.Fatalf("flux4swarm diff: %v\n%s", err, out)
	return "", 0
}

// TestDiff: `flux4swarm diff` exits 0 when Swarm matches Git, and 1 with a
// unified spec diff when Git changed, without changing Swarm.
func TestDiff(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	ns := uniq("diff")
	svc := swarmServiceName(ns, "web")
	cleanupStack(t, ns)

	repo.setApp(ns, map[string]string{"deployment-web.yaml": deploymentYAML(app{NS: ns, Name: "web", Image: imageV1, Replicas: 1})})
	repo.commitPush("add " + ns + "/web")
	reconcile(t, repo)

	if out, code := diff(t, repo); code != 0 {
		t.Fatalf("diff after reconcile: exit %d, want 0\n%s", code, out)
	}

	repo.setApp(ns, map[string]string{"deployment-web.yaml": deploymentYAML(app{NS: ns, Name: "web", Image: imageV1, Replicas: 2})})
	repo.commitPush("scale " + ns + "/web")
	out, code := diff(t, repo)
	if code != 1 {
		t.Fatalf("diff after change: exit %d, want 1\n%s", code, out)
	}
	for _, want := range []string{"► Service/" + svc + " drifted", "-    Replicas: 1", "+    Replicas: 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("diff output has no %q:\n%s", want, out)
		}
	}
	if s := mustService(t, svc); s.replicas() != 1 {
		t.Errorf("diff changed Swarm: replicas %d, want 1", s.replicas())
	}
}
