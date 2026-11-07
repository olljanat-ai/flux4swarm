//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fluxEvent is the subset of Flux's event.toolkit.fluxcd.io/v1 Event the
// generic provider posts.
type fluxEvent struct {
	InvolvedObject      struct{ Kind, Namespace, Name, APIVersion string }
	Severity            string
	Message             string
	Reason              string
	Metadata            map[string]string
	ReportingController string
}

// TestNotifications: a repo with genuine notification.toolkit.fluxcd.io
// Provider, Alert and Receiver objects. flux4swarm sends the
// Kustomization's events to the generic webhook and initializes the
// Receiver at Flux's webhook path; stock Flux2 accepts the same objects and
// computes the same Receiver path.
func TestNotifications(t *testing.T) {
	requireController(t)
	repo := newGitRepo(t)
	ns := uniq("notif")
	name := uniq("hook")
	svc := swarmServiceName(ns, "web")
	cleanupStack(t, ns)

	var mu sync.Mutex
	var events []fluxEvent
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var ev fluxEvent
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Errorf("webhook got invalid JSON: %v\n%s", err, b)
		}
		if r.Header.Get("gotk-component") != ev.ReportingController {
			t.Errorf("gotk-component header %q, reportingController %q", r.Header.Get("gotk-component"), ev.ReportingController)
		}
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}))
	defer hook.Close()

	const token = "e2e-webhook-token"
	credDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(credDir, name+"-token"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, name+"-token", "token"), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	notifications := fmt.Sprintf(`apiVersion: notification.toolkit.fluxcd.io/v1
kind: Provider
metadata:
  name: %[1]s
  namespace: flux-system
spec:
  type: generic
  address: %[2]s
---
apiVersion: notification.toolkit.fluxcd.io/v1
kind: Alert
metadata:
  name: %[1]s
  namespace: flux-system
spec:
  providerRef:
    name: %[1]s
  eventSeverity: info
  eventSources:
    - kind: Kustomization
      name: '*'
    - kind: GitRepository
      name: '*'
---
apiVersion: notification.toolkit.fluxcd.io/v1
kind: Receiver
metadata:
  name: %[1]s
  namespace: flux-system
spec:
  type: github
  events: ["ping", "push"]
  secretRef:
    name: %[1]s-token
  resources:
    - kind: GitRepository
      name: flux-system
`, name, hook.URL)
	repo.setApp(ns, map[string]string{
		"notifications.yaml":  notifications,
		"deployment-web.yaml": deploymentYAML(app{NS: ns, Name: "web", Image: imageV1, Replicas: 1}),
	})
	repo.commitPush("app with notifications")

	out := reconcile(t, repo, "--prune", "--credentials-dir", credDir)
	waitServiceRunning(t, svc, 1)
	sum := sha256.Sum256([]byte(token + name + "flux-system"))
	path := "/hook/" + hex.EncodeToString(sum[:])
	if !strings.Contains(out, "Receiver/flux-system/"+name+"\tready=True\tReceiver initialized for path: "+path) {
		t.Errorf("reconcile output lacks a ready Receiver at %s:\n%s", path, out)
	}

	head := repo.head()
	eventually(t, 30*time.Second, "Kustomization events at the generic provider", func() (bool, string) {
		mu.Lock()
		defer mu.Unlock()
		var changes, finished bool
		for _, ev := range events {
			if ev.InvolvedObject.Kind != "Kustomization" || ev.InvolvedObject.Name != "flux-system" ||
				ev.ReportingController != "kustomize-controller" || !strings.HasSuffix(ev.Metadata["revision"], head) {
				continue
			}
			if ev.Reason == "Progressing" && strings.Contains(ev.Message, svc) {
				changes = true
			}
			if ev.Reason == "ReconciliationSucceeded" && ev.Metadata["commit_status"] == "update" &&
				strings.HasPrefix(ev.Message, "Reconciliation finished in ") {
				finished = true
			}
		}
		return changes && finished, fmt.Sprintf("%+v", events)
	})

	t.Run("flux2", func(t *testing.T) {
		k := requireK8s(t)
		k.kubectl(t, "-n", "flux-system", "create", "secret", "generic", name+"-token", "--from-literal=token="+token)
		t.Cleanup(func() { _, _ = k.try(t, "kubectl", "-n", "flux-system", "delete", "secret", name+"-token") })
		k.kubectl(t, "apply", "--dry-run=server", "-f", filepath.Join(repo.Work, appDir(ns), "notifications.yaml"))
		s := k.sync(t, repo, fluxSyncOpts{Wait: false})
		s.reconcile(t)
		eventually(t, 2*time.Minute, "Flux Receiver "+name+" ready", func() (bool, string) {
			var r struct {
				Status struct {
					WebhookPath string
					Conditions  []struct{ Type, Status string }
				}
			}
			if !k.getJSON(t, "flux-system", "receiver", name, &r) {
				return false, "not found"
			}
			for _, c := range r.Status.Conditions {
				if c.Type == "Ready" && c.Status == "True" {
					if r.Status.WebhookPath != path {
						t.Fatalf("Flux webhookPath %s, flux4swarm %s", r.Status.WebhookPath, path)
					}
					return true, ""
				}
			}
			return false, fmt.Sprintf("%+v", r.Status)
		})
	})
}
