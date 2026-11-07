//go:build e2e

package e2e

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	fixtureGreeting = "hello config"
	fixtureSecret   = "s3cr3t-in-swarm-only"
)

// createStackService creates a Swarm service that belongs to stack ns (the
// way `docker stack deploy` labels it), running imageV1.
func createStackService(t *testing.T, ns, name string, args ...string) string {
	t.Helper()
	svc := swarmServiceName(ns, name)
	base := []string{"service", "create", "--detach", "--name", svc,
		"--label", e2eLabel, "--label", stackKey + "=" + ns, "--container-label", stackKey + "=" + ns}
	base = append(base, args...)
	docker(t, append(base, imageV1)...)
	return svc
}

// createImportFixture creates, directly in Swarm, a service web in stack ns
// (label, env, 1 replica) that mounts a config and a secret.
func createImportFixture(t *testing.T, ns string) string {
	t.Helper()
	cleanupStack(t, ns)
	cfg, sec := ns+"_greeting", ns+"_token"
	runIn(t, fixtureGreeting, "docker", "config", "create", "--label", e2eLabel, "--label", stackKey+"="+ns, cfg, "-")
	runIn(t, fixtureSecret, "docker", "secret", "create", "--label", e2eLabel, "--label", stackKey+"="+ns, sec, "-")
	svc := createStackService(t, ns, "web",
		"--label", "app=web", "--env", "GREETING=hello", "--replicas", "1",
		"--config", "source="+cfg+",target="+configMountPath+"/greeting",
		"--secret", "source="+sec+",target=token")
	waitServiceRunning(t, svc, 1)
	return svc
}

// assertImportedTree checks the files import must write for the fixture.
func assertImportedTree(t *testing.T, repo *gitRepo, ns string) {
	t.Helper()
	for _, f := range []string{fluxSysPath + "/gotk-sync.yaml", fluxSysPath + "/kustomization.yaml",
		appDir(ns) + "/namespace.yaml", appDir(ns) + "/kustomization.yaml", appDir(ns) + "/secretstore.yaml"} {
		if _, ok := repo.read(f); !ok {
			t.Errorf("missing %s", f)
		}
	}
	content := repo.mustRead(importedDeployment(ns, "web"))
	for _, want := range []string{"apiVersion: apps/v1", "kind: Deployment", "namespace: " + ns, imageV1, "GREETING", "hello"} {
		if !strings.Contains(content, want) {
			t.Errorf("%s does not contain %q:\n%s", importedDeployment(ns, "web"), want, content)
		}
	}
	if !regexp.MustCompile(`replicas:\s*1\b`).MatchString(content) {
		t.Errorf("%s does not contain replicas: 1:\n%s", importedDeployment(ns, "web"), content)
	}
	if !anyFileContains(repo, appDir(ns)+"/configmap-*.yaml", "kind: ConfigMap", fixtureGreeting) {
		t.Errorf("no %s/configmap-*.yaml with kind ConfigMap and data %q; tree:\n%s", appDir(ns), fixtureGreeting, repo.git("ls-files"))
	}
	if !anyFileContains(repo, appDir(ns)+"/externalsecret-*.yaml", "kind: ExternalSecret", "external-secrets.io/v1") {
		t.Errorf("no %s/externalsecret-*.yaml with an external-secrets.io/v1 ExternalSecret; tree:\n%s", appDir(ns), repo.git("ls-files"))
	}
	// Swarm never reveals secret data, and plaintext must not end up in Git.
	if out, _ := run(t, time.Minute, repo.Work, "git", "grep", "-l", fixtureSecret); out != "" {
		t.Errorf("secret value found in Git: %s", out)
	}
}

func anyFileContains(repo *gitRepo, pattern string, wants ...string) bool {
	for _, f := range repo.glob(pattern) {
		s, _ := repo.read(f)
		all := true
		for _, w := range wants {
			all = all && strings.Contains(s, w)
		}
		if all {
			return true
		}
	}
	return false
}

// TestImport: objects created directly in Swarm are written to Git as
// standard Kubernetes manifests in the Flux bootstrap layout.
func TestImport(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	ns := uniq("imp")
	createImportFixture(t, ns)

	importToGit(t, repo, importFlags...)
	repo.pull()
	assertImportedTree(t, repo, ns)
}

// TestRoundTripNoop: `import --adopt` relabels the live service as managed,
// so the reconcile that follows must not change it at all (contract: only
// services that use secrets are still updated by the first reconcile).
func TestRoundTripNoop(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	ns := uniq("rt")
	cleanupStack(t, ns)
	svc := createStackService(t, ns, "web", "--replicas", "1")
	waitServiceRunning(t, svc, 1)

	importToGit(t, repo, importFlags...)
	assertReconcileIsNoop(t, repo, svc)
}

// assertReconcileIsNoop checks, for a service without secrets, that import
// --adopt already adopted it and that two reconciles leave it untouched
// (same version, same task template, same running tasks).
func assertReconcileIsNoop(t *testing.T, repo *gitRepo, svc string) {
	t.Helper()
	before := mustService(t, svc)
	if before.Spec.Labels[managedByKey] != managedByVal {
		t.Errorf("import --adopt did not relabel %s as managed: labels %v", svc, before.Spec.Labels)
	}
	beforeRaw := docker(t, "service", "inspect", svc, "--format", "{{json .Spec}}")
	tmpl, tasks := taskTemplate(t, svc), runningTasks(t, svc)
	for i := 1; i <= 2; i++ {
		reconcile(t, repo)
		after := mustService(t, svc)
		if after.Version.Index != before.Version.Index || after.UpdatedAt != before.UpdatedAt {
			t.Errorf("reconcile #%d after import changed %s: version %d -> %d, updatedAt %s -> %s\nSpec before: %s\nSpec after:  %s\nPreviousSpec after: %s",
				i, svc, before.Version.Index, after.Version.Index, before.UpdatedAt, after.UpdatedAt, beforeRaw,
				docker(t, "service", "inspect", svc, "--format", "{{json .Spec}}"),
				docker(t, "service", "inspect", svc, "--format", "{{json .PreviousSpec}}"))
			before = after
		}
		if got := taskTemplate(t, svc); got != tmpl {
			t.Errorf("reconcile #%d changed the task template of %s:\nbefore: %s\nafter:  %s", i, svc, tmpl, got)
		}
		if got := runningTasks(t, svc); got != tasks {
			t.Errorf("reconcile #%d replaced the tasks of %s: %s -> %s", i, svc, tasks, got)
		}
	}
}
