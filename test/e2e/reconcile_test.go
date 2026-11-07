//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestReconcileService walks one service through create -> update -> prune.
func TestReconcileService(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	svc := uniq("rec")
	unmanaged := uniq("unmanaged")
	cleanupObjects(t, "service", svc, unmanaged)

	ok := t.Run("create", func(t *testing.T) {
		repo.write(servicePath(svc), serviceYAML(svc, imageV1, 1))
		repo.commitPush("add " + svc)
		reconcile(t, repo)

		s := mustService(t, svc)
		if s.image() != imageV1 || s.replicas() != 1 {
			t.Fatalf("got image=%q replicas=%d, want %s/1", s.image(), s.replicas(), imageV1)
		}
		if s.Spec.Labels["app.kubernetes.io/managed-by"] != "flux4swarm" {
			t.Errorf("missing label %s; labels: %v", managedByLabel, s.Spec.Labels)
		}
		waitServiceRunning(t, svc, 1)
	})
	if !ok {
		t.FailNow()
	}

	ok = t.Run("update", func(t *testing.T) {
		repo.write(servicePath(svc), serviceYAML(svc, imageV2, 2))
		repo.commitPush("update " + svc)
		reconcile(t, repo)

		s := mustService(t, svc)
		if s.image() != imageV2 || s.replicas() != 2 {
			t.Fatalf("got image=%q replicas=%d, want %s/2", s.image(), s.replicas(), imageV2)
		}
		waitServiceRunning(t, svc, 2)
	})
	if !ok {
		t.FailNow()
	}

	t.Run("prune", func(t *testing.T) {
		// An object not managed by flux4swarm must survive --prune.
		docker(t, "service", "create", "--detach", "--name", unmanaged,
			"--label", e2eLabel, "--replicas", "0", imageV1)

		repo.remove(servicePath(svc))
		repo.commitPush("remove " + svc)
		reconcile(t, repo, "--prune")

		eventually(t, time.Minute, "service "+svc+" pruned", func() (bool, string) {
			_, exists := inspectService(t, svc)
			return !exists, "still exists"
		})
		if _, exists := inspectService(t, unmanaged); !exists {
			t.Fatalf("unmanaged service %s was pruned", unmanaged)
		}
	})
}

// TestReconcileConfigAndSecret: a Config and an ExternalSecret backed by the
// fake SecretStore become Swarm objects (named NAME-<hash>, found by label).
func TestReconcileConfigAndSecret(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	cfg, sec := uniq("rec-cfg"), uniq("rec-sec")
	cleanupObjects(t, "config", cfg)
	cleanupObjects(t, "secret", sec)

	repo.write(configPath(cfg), configYAML(cfg, "hello from git\n"))
	repo.write(secretStorePath(), secretStoreYAML("e2e-key", "e2e-value"))
	repo.write(secretPath(sec), externalSecretYAML(sec, "e2e-key"))
	repo.commitPush("add config and secret")
	reconcile(t, repo)

	if got := objectsByLabel(t, "config", nameLabel+"="+cfg); len(got) != 1 {
		t.Errorf("want 1 config labelled %s=%s, got %v", nameLabel, cfg, got)
	}
	if got := objectsByLabel(t, "secret", nameLabel+"="+sec); len(got) != 1 {
		t.Errorf("want 1 secret labelled %s=%s, got %v", nameLabel, sec, got)
	}
}
