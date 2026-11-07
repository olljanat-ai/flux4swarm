//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestHealthChecksAndJobs: Flux Kustomizations in the repo with dependsOn,
// spec.wait and spec.healthChecks behave as in Flux on Swarm:
//   - NS-migrate (wait: true) applies a Job and is Ready only once it
//     completed;
//   - NS-db applies Deployment db without waiting;
//   - NS-app dependsOn both and health-checks Deployment NS/db, which another
//     Kustomization applied.
//
// Then a failing Job makes NS-migrate fail (so NS-app's dependency is not
// ready), and a health check on a missing Deployment fails NS-probe.
func TestHealthChecksAndJobs(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	ns := uniq("hc")
	cleanupStack(t, ns)
	job := swarmServiceName(ns, "migrate")

	setTree := func(jobCmd string, probe bool) {
		files := map[string]string{"flux-kustomizations.yaml": healthKustomizationsYAML(ns, probe)}
		repo.setApp(ns, files)
		repo.write(hcPath(ns, "db")+"/deployment-db.yaml", deploymentYAML(app{NS: ns, Name: "db", Image: imageV1, Replicas: 1}))
		repo.write(hcPath(ns, "app")+"/deployment-web.yaml", deploymentYAML(app{NS: ns, Name: "web", Image: imageV1, Replicas: 1}))
		repo.write(hcPath(ns, "migrate")+"/job-migrate.yaml", jobYAML(ns, "migrate", imageV1, jobCmd))
		repo.write(hcPath(ns, "probe")+"/configmap-probe.yaml", configMapYAML(ns, "probe", "k", "v"))
	}

	ok := t.Run("ready", func(t *testing.T) {
		setTree("sleep 5; echo migrated", false)
		repo.commitPush("add " + ns + " with healthChecks")
		start := time.Now()
		reconcile(t, repo)
		// Ready means the job finished: its task must be Complete now.
		states := docker(t, "service", "ps", "--format", "{{.CurrentState}}", job)
		if !strings.Contains(states, "Complete") {
			t.Fatalf("reconcile returned after %s but job %s is not complete:\n%s", time.Since(start), job, states)
		}
		for _, s := range []string{swarmServiceName(ns, "db"), swarmServiceName(ns, "web")} {
			mustService(t, s)
		}
		waitServiceRunning(t, swarmServiceName(ns, "web"), 1)
	})
	if !ok {
		t.FailNow()
	}

	t.Run("failing", func(t *testing.T) {
		setTree("echo failing; exit 3", true)
		repo.commitPush("break " + ns + " migrate job, add probe")
		args := []string{"reconcile", "--url", repo.remoteURL(), "--branch", branch, "--path", "./" + clusterPath,
			"--wait", "--timeout", "3m"}
		out, err := run(t, 5*time.Minute, "", requireController(t), args...)
		if err == nil {
			t.Fatalf("reconcile succeeded with a failing job and a missing health check target:\n%s", out)
		}
		for _, want := range []string{
			"Kustomization/flux-system/" + ns + "-migrate: ", "job failed",
			"Kustomization/flux-system/" + ns + "-app: dependency 'flux-system/" + ns + "-migrate' is not ready",
			"Kustomization/flux-system/" + ns + "-probe: ", "Deployment/" + ns + "/missing", "service not found",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("reconcile output lacks %q:\n%s", want, out)
			}
		}
	})
}

func hcPath(ns, part string) string { return "hc/" + ns + "/" + part }

// healthKustomizationsYAML: NS-db, NS-migrate (wait) and NS-app (dependsOn
// both, healthChecks on Deployment NS/db); with probe also NS-probe, which
// health-checks a Deployment nobody applies.
func healthKustomizationsYAML(ns string, probe bool) string {
	ks := func(name, path, extra string) string {
		return fmt.Sprintf(`apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %s-%s
  namespace: flux-system
spec:
  interval: 10m
  path: ./%s
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
%s`, ns, name, path, extra)
	}
	docs := []string{
		ks("db", hcPath(ns, "db"), ""),
		ks("migrate", hcPath(ns, "migrate"), "  wait: true\n  timeout: 2m\n"),
		ks("app", hcPath(ns, "app"), fmt.Sprintf(`  timeout: 2m
  dependsOn:
    - name: %[1]s-db
    - name: %[1]s-migrate
  healthChecks:
    - apiVersion: apps/v1
      kind: Deployment
      name: db
      namespace: %[1]s
`, ns)),
	}
	if probe {
		docs = append(docs, ks("probe", hcPath(ns, "probe"), fmt.Sprintf(`  timeout: 15s
  healthChecks:
    - apiVersion: apps/v1
      kind: Deployment
      name: missing
      namespace: %s
`, ns)))
	}
	return strings.Join(docs, "---\n")
}

// jobYAML is a Job running sh -c cmd once (restartPolicy Never, no retries).
func jobYAML(ns, name, image, cmd string) string {
	return fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    io.flux4swarm.e2e: "true"
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: %[1]s
          image: %[3]s
          command: ["sh", "-c", %[4]q]
`, name, ns, image, cmd)
}
