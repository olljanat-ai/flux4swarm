//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestFlux2Smoke proves the Kubernetes side of the harness on its own (no
// flux4swarm needed): stock Flux2 in kind fetches a test repo from the e2e
// git server and applies a plain Deployment + ConfigMap, then prunes them.
func TestFlux2Smoke(t *testing.T) {
	k := requireK8s(t)
	repo := newGitRepo(t)
	ns := uniq("smoke")
	repo.setApp(ns, map[string]string{
		"configmap-web-config.yaml": configMapYAML(ns, "web-config", "greeting", "hello from flux"),
		"deployment-web.yaml":       deploymentYAML(app{NS: ns, Name: "web", Image: imageV1, Replicas: 1}),
	})
	repo.commitPush("smoke app")

	s := k.sync(t, repo, fluxSyncOpts{Wait: true})
	s.reconcile(t)
	k.waitDeploymentAvailable(t, ns, "web", imageV1, 1)
	var cm struct{ Data map[string]string }
	if !k.getJSON(t, ns, "configmap", "web-config", &cm) || cm.Data["greeting"] != "hello from flux" {
		t.Fatalf("configmap %s/web-config: %+v", ns, cm)
	}

	repo.setApp(ns, nil)
	repo.commitPush("remove smoke app")
	s.reconcile(t)
	eventually(t, 2*time.Minute, "namespace "+ns+" pruned", func() (bool, string) {
		return !k.exists(t, "", "namespace", ns), "still exists"
	})
}

// TestSameRepoSwarmAndFlux2 is the compatibility test: ONE repo in the Flux
// bootstrap layout (real `flux install --export` gotk-components.yaml and a
// gotk-sync.yaml) is reconciled by flux4swarm into Swarm and by stock Flux2
// into kind. Both must agree on create, update and prune, including a
// ConfigMap and an ESO ExternalSecret mounted by the Deployment.
func TestSameRepoSwarmAndFlux2(t *testing.T) {
	requireController(t)
	k := requireK8s(t)
	repo := newGitRepo(t)
	ns := uniq("same")
	svc := swarmServiceName(ns, "web")
	cleanupStack(t, ns)

	const key, cfgVal, secVal = "greeting", "hello from one repo", "s3cr3t-from-fake-store"
	appFiles := func(image string, replicas int) map[string]string {
		return map[string]string{
			"configmap-web-config.yaml":      configMapYAML(ns, "web-config", key, cfgVal),
			"secretstore.yaml":               secretStoreYAML(ns, "e2e-key", secVal),
			"externalsecret-web-secret.yaml": externalSecretYAML(ns, "web-secret", key, "e2e-key"),
			"deployment-web.yaml": deploymentYAML(app{NS: ns, Name: "web", Image: image, Replicas: replicas,
				ConfigMap: "web-config", Secret: "web-secret", Key: key}),
		}
	}
	repo.writeBootstrap(k)
	repo.setApp(ns, appFiles(imageV1, 1))
	repo.commitPush("bootstrap + app")
	k.validateBootstrap(t, repo)
	k.validateApps(t, repo)
	s := k.sync(t, repo, fluxSyncOpts{Wait: true})

	both := func(t *testing.T, image string, replicas int) {
		t.Helper()
		reconcile(t, repo, "--prune")
		s.reconcile(t)

		// Swarm
		sv := mustService(t, svc)
		if sv.image() != image || int(sv.replicas()) != replicas {
			t.Errorf("swarm: %s image=%q replicas=%d, want %s/%d", svc, sv.image(), sv.replicas(), image, replicas)
		}
		waitServiceRunning(t, svc, replicas)
		if got := versionedObjects(t, "config", ns, "web-config", key); len(got) != 1 {
			t.Errorf("swarm: want 1 config %s, got %v", versionedNameRE(ns, "web-config", key), got)
		}
		if got := versionedObjects(t, "secret", ns, "web-secret", key); len(got) != 1 {
			t.Errorf("swarm: want 1 secret %s, got %v", versionedNameRE(ns, "web-secret", key), got)
		}
		if got := serviceFile(t, svc, configMountPath+"/"+key); got != cfgVal {
			t.Errorf("swarm: config file = %q, want %q", got, cfgVal)
		}
		if got := serviceFile(t, svc, secretMountPath+"/"+key); got != secVal {
			t.Errorf("swarm: secret file = %q, want %q", got, secVal)
		}

		// Kubernetes
		k.waitDeploymentAvailable(t, ns, "web", image, replicas)
		var cm struct{ Data map[string]string }
		if !k.getJSON(t, ns, "configmap", "web-config", &cm) || cm.Data[key] != cfgVal {
			t.Errorf("k8s: configmap web-config = %+v", cm.Data)
		}
		eventually(t, time.Minute, "ESO secret "+ns+"/web-secret", func() (bool, string) {
			v, ok := k.secretValue(t, ns, "web-secret", key)
			return ok && v == secVal, v
		})
	}

	if !t.Run("create", func(t *testing.T) { both(t, imageV1, 1) }) {
		t.FailNow()
	}
	if !t.Run("update", func(t *testing.T) {
		repo.setApp(ns, appFiles(imageV2, 2))
		repo.commitPush("update image and replicas")
		both(t, imageV2, 2)
	}) {
		t.FailNow()
	}
	t.Run("prune", func(t *testing.T) {
		repo.setApp(ns, nil)
		repo.commitPush("remove app")
		reconcile(t, repo, "--prune")
		s.reconcile(t)

		waitServiceGone(t, svc)
		eventually(t, time.Minute, "swarm configs/secrets of "+ns+" pruned", func() (bool, string) {
			c := versionedObjects(t, "config", ns, "web-config", key)
			sc := versionedObjects(t, "secret", ns, "web-secret", key)
			return len(c)+len(sc) == 0, strings.Join(append(c, sc...), ",")
		})
		eventually(t, 2*time.Minute, "k8s namespace "+ns+" pruned", func() (bool, string) {
			return !k.exists(t, "", "namespace", ns), "still exists"
		})
	})
}

// TestImportIsFlux2Valid: what `flux4swarm import` writes from a live Swarm
// must be deployable by stock Flux2 as-is: the flux-system directory must
// validate against the Flux CRDs, every kustomization must build, and a
// Kustomization over clusters/e2e must become Ready and create the objects.
func TestImportIsFlux2Valid(t *testing.T) {
	requireController(t)
	k := requireK8s(t)
	repo := newGitRepo(t)
	ns := uniq("impk")
	createImportFixture(t, ns)

	importToGit(t, repo, importFlags...)
	repo.pull()
	assertImportedTree(t, repo, ns)
	k.validateBootstrap(t, repo)
	k.validateApps(t, repo)

	// Imported images may be pinned to registry digests (--adopt), which are
	// not preloaded into kind, so only require that Flux applies everything.
	s := k.sync(t, repo, fluxSyncOpts{Wait: false})
	s.reconcile(t)

	d := k.mustDeployment(t, ns, "web")
	if img, _, _ := strings.Cut(d.image(), "@"); img != imageV1 || d.replicas() != 1 {
		t.Errorf("k8s: deployment %s/web image=%q replicas=%d, want %s/1", ns, d.image(), d.replicas(), imageV1)
	}
	if out := k.kubectl(t, "get", "configmaps", "-n", ns, "-o", "json"); !strings.Contains(out, fixtureGreeting) {
		t.Errorf("k8s: no ConfigMap in %s holds %q:\n%s", ns, fixtureGreeting, out)
	}
	for _, kind := range []string{"externalsecrets.external-secrets.io", "secretstores.external-secrets.io"} {
		if out := k.kubectl(t, "get", kind, "-n", ns, "-o", "name"); out == "" {
			t.Errorf("k8s: no %s in %s", kind, ns)
		}
	}
}
