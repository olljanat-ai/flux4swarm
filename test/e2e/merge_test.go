//go:build e2e

package e2e

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// importEnv imports the current Swarm state as environment env
// (clusters/<env>) of the repo.
func importEnv(t *testing.T, repo *gitRepo, env string) {
	t.Helper()
	flux4swarm(t, append([]string{"import", "--cluster", env,
		"--git-url", repo.remoteURL(), "--branch", branch}, importFlags...)...)
}

func reconcileEnv(t *testing.T, repo *gitRepo, env string) {
	t.Helper()
	flux4swarm(t, "reconcile", "--url", repo.remoteURL(), "--branch", branch,
		"--path", "./clusters/"+env, "--wait", "--timeout", "3m")
}

func serviceEnv(t *testing.T, svc string) string {
	t.Helper()
	return docker(t, "service", "inspect", svc, "--format", "{{json .Spec.TaskTemplate.ContainerSpec.Env}}")
}

// TestMergeEnvironments simulates two Swarms (development and production)
// with two imports of the same stack in different states, merges them
// into a shared base plus overlays, and checks that each overlay deploys
// its own environment again: on Swarm with flux4swarm and on Kubernetes
// with stock Flux2.
func TestMergeEnvironments(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	ns := uniq("menv")
	cleanupStack(t, ns)

	// "development": one replica, debug logging.
	web := createStackService(t, ns, "web", "--replicas", "1", "--env", "TZ=UTC", "--env", "LEVEL=debug")
	waitServiceRunning(t, web, 1)
	importEnv(t, repo, "development")

	// "production": two replicas, info logging and an extra worker. A
	// separate Swarm would not carry the labels the development import
	// adopted the service with, so drop them.
	docker(t, "service", "update", "--detach", "--replicas", "2", "--env-add", "LEVEL=info",
		"--label-rm", managedByKey, "--label-rm", "flux4swarm.io/spec-hash",
		"--label-rm", "kustomize.toolkit.fluxcd.io/name", "--label-rm", "kustomize.toolkit.fluxcd.io/namespace", web)
	waitServiceRunning(t, web, 2)
	worker := createStackService(t, ns, "worker", "--replicas", "1")
	waitServiceRunning(t, worker, 1)
	importEnv(t, repo, "production")

	flux4swarm(t, "merge", "--env", "development", "--env", "production",
		"--git-url", repo.remoteURL(), "--branch", branch)
	repo.pull()

	base := repo.mustRead("apps/base/" + ns + "/deployment-web.yaml")
	for _, w := range []string{"kind: Deployment", imageV1, "TZ"} {
		if !strings.Contains(base, w) {
			t.Errorf("base deployment lacks %q:\n%s", w, base)
		}
	}
	if strings.Contains(base, "debug") || regexp.MustCompile(`value: info\b`).MatchString(base) {
		t.Errorf("base deployment holds environment-specific values:\n%s", base)
	}
	if _, ok := repo.read("apps/base/" + ns + "/deployment-worker.yaml"); ok {
		t.Error("production-only worker is in the base")
	}
	prodK := repo.mustRead("clusters/production/apps/kustomization.yaml")
	for _, w := range []string{"../../../apps/base", ns + "/deployment-worker.yaml", ns + "/deployment-web-patch.yaml"} {
		if !strings.Contains(prodK, w) {
			t.Errorf("production kustomization lacks %q:\n%s", w, prodK)
		}
	}
	devPatch := repo.mustRead("clusters/development/apps/" + ns + "/deployment-web-patch.yaml")
	if !strings.Contains(devPatch, "debug") {
		t.Errorf("development patch lacks LEVEL=debug:\n%s", devPatch)
	}
	for _, f := range []string{"clusters/development/flux-system/gotk-sync.yaml", "clusters/production/flux-system/gotk-sync.yaml"} {
		if _, ok := repo.read(f); !ok {
			t.Errorf("merge removed %s", f)
		}
	}

	// Swarm is in the production state: reconciling production changes
	// nothing.
	before := mustService(t, web)
	reconcileEnv(t, repo, "production")
	if after := mustService(t, web); after.Version.Index != before.Version.Index {
		t.Errorf("reconciling production after merge changed %s (version %d -> %d)", web, before.Version.Index, after.Version.Index)
	}
	// Reconciling development brings back its state and prunes the worker.
	reconcileEnv(t, repo, "development")
	waitServiceRunning(t, web, 1)
	if env := serviceEnv(t, web); !strings.Contains(env, "LEVEL=debug") || !strings.Contains(env, "TZ=UTC") {
		t.Errorf("development: %s env %s", web, env)
	}
	waitServiceGone(t, worker)
	reconcileEnv(t, repo, "production")
	waitServiceRunning(t, web, 2)
	waitServiceRunning(t, worker, 1)
	if env := serviceEnv(t, web); !strings.Contains(env, "LEVEL=info") {
		t.Errorf("production: %s env %s", web, env)
	}

	// Stock kustomize builds the overlays and stock Flux2 deploys the
	// production one.
	k := requireK8s(t)
	for _, env := range []string{"development", "production"} {
		k.kubectl(t, "kustomize", filepath.Join(repo.Work, "clusters", env, "apps"))
	}
	s := k.sync(t, repo, fluxSyncOpts{Wait: false, Path: "clusters/production"})
	s.reconcile(t)
	if d := k.mustDeployment(t, ns, "web"); d.replicas() != 2 {
		t.Errorf("k8s: deployment %s/web replicas=%d, want 2", ns, d.replicas())
	}
	k.mustDeployment(t, ns, "worker")
}
