//go:build e2e

package e2e

import (
	"testing"
)

// TestReconcileService walks one Deployment through create -> update -> prune
// on Swarm. Deployment web in namespace NS becomes Swarm service NS_web.
func TestReconcileService(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	ns := uniq("rec")
	svc := swarmServiceName(ns, "web")
	unmanaged := uniq("unmanaged")
	cleanupStack(t, ns)
	cleanupServices(t, unmanaged)

	ok := t.Run("create", func(t *testing.T) {
		repo.setApp(ns, map[string]string{"deployment-web.yaml": deploymentYAML(app{NS: ns, Name: "web", Image: imageV1, Replicas: 1})})
		repo.commitPush("add " + ns + "/web")
		reconcile(t, repo)

		s := mustService(t, svc)
		if s.image() != imageV1 || s.replicas() != 1 {
			t.Fatalf("got image=%q replicas=%d, want %s/1", s.image(), s.replicas(), imageV1)
		}
		if s.Spec.Labels[managedByKey] != managedByVal {
			t.Errorf("missing label %s=%s; labels: %v", managedByKey, managedByVal, s.Spec.Labels)
		}
		if s.Spec.Labels[stackKey] != ns {
			t.Errorf("missing label %s=%s; labels: %v", stackKey, ns, s.Spec.Labels)
		}
		waitServiceRunning(t, svc, 1)
	})
	if !ok {
		t.FailNow()
	}

	ok = t.Run("update", func(t *testing.T) {
		repo.setApp(ns, map[string]string{"deployment-web.yaml": deploymentYAML(app{NS: ns, Name: "web", Image: imageV2, Replicas: 2})})
		repo.commitPush("update " + ns + "/web")
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

		repo.setApp(ns, nil)
		repo.commitPush("remove " + ns)
		reconcile(t, repo, "--prune")

		waitServiceGone(t, svc)
		if _, exists := inspectService(t, unmanaged); !exists {
			t.Fatalf("unmanaged service %s was pruned", unmanaged)
		}
	})
}

// TestReconcileConfigAndSecret: a ConfigMap and an ExternalSecret (ESO fake
// SecretStore), both mounted by a Deployment, become one Swarm config and one
// Swarm secret (NS_OBJECT_KEY-HASH10, labelled flux4swarm.io/name), and the
// task container sees the right file contents.
func TestReconcileConfigAndSecret(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	ns := uniq("cfg")
	cleanupStack(t, ns)

	const key, cfgVal, secVal = "greeting", "hello from git", "s3cr3t-from-store"
	repo.setApp(ns, map[string]string{
		"configmap-web-config.yaml":      configMapYAML(ns, "web-config", key, cfgVal),
		"secretstore.yaml":               secretStoreYAML(ns, "e2e-key", secVal),
		"externalsecret-web-secret.yaml": externalSecretYAML(ns, "web-secret", key, "e2e-key"),
		"deployment-web.yaml": deploymentYAML(app{NS: ns, Name: "web", Image: imageV1, Replicas: 1,
			ConfigMap: "web-config", Secret: "web-secret", Key: key}),
	})
	repo.commitPush("add config and secret")
	reconcile(t, repo)

	if got := versionedObjects(t, "config", ns, "web-config", key); len(got) != 1 {
		t.Errorf("want 1 Swarm config %s, got %v (all labelled: %v)", versionedNameRE(ns, "web-config", key),
			got, objectsByLabel(t, "config", nameLabel))
	}
	if got := versionedObjects(t, "secret", ns, "web-secret", key); len(got) != 1 {
		t.Errorf("want 1 Swarm secret %s, got %v (all labelled: %v)", versionedNameRE(ns, "web-secret", key),
			got, objectsByLabel(t, "secret", nameLabel))
	}
	svc := swarmServiceName(ns, "web")
	waitServiceRunning(t, svc, 1)
	if got := serviceFile(t, svc, configMountPath+"/"+key); got != cfgVal {
		t.Errorf("%s in %s = %q, want %q", configMountPath+"/"+key, svc, got, cfgVal)
	}
	if got := serviceFile(t, svc, secretMountPath+"/"+key); got != secVal {
		t.Errorf("%s in %s = %q, want %q", secretMountPath+"/"+key, svc, got, secVal)
	}
}
