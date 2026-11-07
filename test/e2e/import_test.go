//go:build e2e

package e2e

import (
	"regexp"
	"strings"
	"testing"
)

// TestImport: objects created directly in Swarm are written to Git.
func TestImport(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)

	svc, cfg, sec := uniq("imp-svc"), uniq("imp-cfg"), uniq("imp-sec")
	cleanupObjects(t, "service", svc)
	cleanupObjects(t, "config", cfg)
	cleanupObjects(t, "secret", sec)

	docker(t, "service", "create", "--detach", "--name", svc,
		"--label", e2eLabel, "--label", "app=web", "--env", "GREETING=hello",
		"--replicas", "1", imageV1)
	runIn(t, "hello config", "docker", "config", "create", "--label", e2eLabel, cfg, "-")
	runIn(t, "s3cr3t", "docker", "secret", "create", "--label", e2eLabel, sec, "-")
	waitServiceRunning(t, svc, 1)

	importToGit(t, repo, "--adopt", "--bootstrap",
		"--secret-store", "fake", "--secret-store-provider", "fake")
	repo.pull()

	if _, ok := repo.read(clusterPath + "/flux-system/gotk-sync.yaml"); !ok {
		t.Errorf("missing %s/flux-system/gotk-sync.yaml (--bootstrap)", clusterPath)
	}
	content, ok := repo.read(servicePath(svc))
	if !ok {
		t.Fatalf("missing %s; repo tree:\n%s", servicePath(svc), repo.git("ls-files"))
	}
	for _, want := range []string{"kind: Service", imageV1, "GREETING=hello"} {
		if !strings.Contains(content, want) {
			t.Errorf("%s does not contain %q:\n%s", servicePath(svc), want, content)
		}
	}
	if !regexp.MustCompile(`Replicas:\s*1\b`).MatchString(content) {
		t.Errorf("%s does not contain Replicas: 1:\n%s", servicePath(svc), content)
	}
	if _, ok := repo.read(configPath(cfg)); !ok {
		t.Errorf("missing %s", configPath(cfg))
	}
	// Secret contents are intentionally not asserted (must not be plaintext).
	if _, ok := repo.read(secretPath(sec)); !ok {
		t.Errorf("missing %s", secretPath(sec))
	}
}

// TestRoundTripNoop: import followed by reconcile must not touch the service.
func TestRoundTripNoop(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	svc := uniq("rt")
	cleanupObjects(t, "service", svc)

	docker(t, "service", "create", "--detach", "--name", svc, "--label", e2eLabel,
		"--replicas", "1", imageV1)
	waitServiceRunning(t, svc, 1)

	importToGit(t, repo, "--adopt",
		"--secret-store", "fake", "--secret-store-provider", "fake")
	before := mustService(t, svc) // after import: --adopt may relabel it

	reconcile(t, repo)
	after := mustService(t, svc)
	if after.Version.Index != before.Version.Index || after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("reconcile after import was not a no-op: version %d -> %d, updatedAt %s -> %s",
			before.Version.Index, after.Version.Index, before.UpdatedAt, after.UpdatedAt)
	}
}
