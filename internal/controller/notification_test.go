// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/creds"
	"github.com/olljanat-ai/flux4swarm/internal/notify"
)

func testController(t *testing.T) (*Controller, *creds.Store) {
	t.Helper()
	store := creds.New("")
	c := New(Options{Creds: store, CacheDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return c, store
}

func meta(name string) api.ObjectMeta {
	return api.ObjectMeta{Name: name, Namespace: api.DefaultNamespace}
}

func TestReceiverRequestsReconcile(t *testing.T) {
	c, store := testController(t)
	store.Set("webhook-token", "token", []byte("s3cret"))
	c.AddGitRepository(&api.GitRepository{Metadata: meta("flux-system"), Spec: api.GitRepositorySpec{URL: "https://example.com/x"}})
	c.AddKustomization(&api.Kustomization{Metadata: api.ObjectMeta{Name: "apps", Namespace: api.DefaultNamespace, Labels: map[string]string{"tier": "apps"}}})
	rcv := &api.Receiver{Metadata: meta("github"), Spec: api.ReceiverSpec{
		Type:      notify.ReceiverGeneric,
		SecretRef: &api.LocalObjectReference{Name: "webhook-token"},
		Resources: []api.ReceiverResource{
			{NotificationObjectRef: api.NotificationObjectRef{Kind: api.KindGitRepository, Name: "flux-system"}},
			{NotificationObjectRef: api.NotificationObjectRef{Kind: api.KindKustomization, Name: "*", MatchLabels: map[string]string{"tier": "apps"}}},
		},
	}}
	if err := c.AddNotification(rcv); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	c.sources[srcKey(api.KindGitRepository, api.DefaultNamespace, "flux-system")].next = future
	c.kustomization[ksKey(api.DefaultNamespace, "apps")].next = future

	path := notify.WebhookPath("s3cret", "github", api.DefaultNamespace)
	var st ObjectStatus
	for _, s := range c.Status() {
		if s.Kind == api.KindReceiver {
			st = s
		}
	}
	if st.WebhookPath != path || !st.IsReady() || !strings.Contains(st.Ready.Message, path) {
		t.Fatalf("receiver status: %+v", st)
	}

	srv := httptest.NewServer(c.ReceiverHandler())
	defer srv.Close()
	post := func(p string) int {
		resp, err := http.Post(srv.URL+p, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("/hook/unknown"); code != http.StatusNotFound {
		t.Errorf("unknown path: %d", code)
	}
	if code := post(path); code != http.StatusOK {
		t.Fatalf("webhook: %d", code)
	}
	if !c.sources[srcKey(api.KindGitRepository, api.DefaultNamespace, "flux-system")].next.IsZero() ||
		!c.kustomization[ksKey(api.DefaultNamespace, "apps")].next.IsZero() {
		t.Error("resources were not marked for reconciliation")
	}
	select {
	case <-c.trigger:
	default:
		t.Error("loop was not triggered")
	}

	// A resource that does not exist makes the request fail, like Flux.
	rcv2 := *rcv
	rcv2.Metadata = meta("broken")
	rcv2.Spec.Resources = []api.ReceiverResource{{NotificationObjectRef: api.NotificationObjectRef{Kind: api.KindKustomization, Name: "missing"}}}
	_ = c.AddNotification(&rcv2)
	if code := post(notify.WebhookPath("s3cret", "broken", api.DefaultNamespace)); code != http.StatusInternalServerError {
		t.Errorf("missing resource: %d", code)
	}

	// Suspended receivers answer 503.
	rcv3 := *rcv
	rcv3.Spec.Suspend = true
	_ = c.AddNotification(&rcv3)
	if code := post(path); code != http.StatusServiceUnavailable {
		t.Errorf("suspended: %d", code)
	}
}

func TestReceiverWithoutSecretIsNotReady(t *testing.T) {
	c, _ := testController(t)
	_ = c.AddNotification(&api.Receiver{Metadata: meta("github"), Spec: api.ReceiverSpec{
		Type: notify.ReceiverGitHub, SecretRef: &api.LocalObjectReference{Name: "missing"},
	}})
	st := c.Status()
	if len(st) != 1 || st[0].IsReady() || !strings.Contains(st[0].Ready.Message, "unable to read token") {
		t.Fatalf("status: %+v", st)
	}
}

func TestKustomizationFailureIsNotified(t *testing.T) {
	var mu sync.Mutex
	var events []notify.Event
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev notify.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}))
	defer hook.Close()

	c, _ := testController(t)
	for _, o := range []any{
		&api.Provider{Metadata: meta("hook"), Spec: api.ProviderSpec{Type: notify.TypeGeneric, Address: hook.URL}},
		&api.Alert{Metadata: meta("all"), Spec: api.AlertSpec{
			ProviderRef:  api.LocalObjectReference{Name: "hook"},
			EventSources: []api.NotificationObjectRef{{Kind: api.KindKustomization, Name: "*"}},
		}},
	} {
		if err := c.AddNotification(o); err != nil {
			t.Fatal(err)
		}
	}
	c.AddKustomization(&api.Kustomization{Metadata: meta("apps"), Spec: api.KustomizationSpec{
		SourceRef: api.CrossNamespaceObjectReference{Kind: api.KindGitRepository, Name: "missing"},
	}})
	err := c.ReconcileOnce(context.Background())
	if err == nil || strings.Contains(err.Error(), "Provider") || strings.Contains(err.Error(), "Alert") {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (rate limited): %+v", len(events), events)
	}
	ev := events[0]
	if ev.Severity != notify.SeverityError || ev.Reason != ReasonArtifactFailed || ev.ReportingController != "kustomize-controller" ||
		ev.InvolvedObject.Kind != api.KindKustomization || ev.InvolvedObject.Name != "apps" ||
		ev.InvolvedObject.APIVersion != api.KustomizeGroupVersion {
		t.Errorf("event: %+v", ev)
	}
	for _, s := range c.Status() {
		if (s.Kind == api.KindProvider || s.Kind == api.KindAlert) && !s.IsReady() {
			t.Errorf("%s not ready: %s", s.Kind, s.Ready.Message)
		}
	}
}

func TestNotificationChildrenArePruned(t *testing.T) {
	c, _ := testController(t)
	parent := &api.Kustomization{Metadata: meta("flux-system")}
	c.AddKustomization(parent)
	key := ksKey(api.DefaultNamespace, "flux-system")
	s := c.kustomization[key]
	set := &api.Set{
		Providers: []*api.Provider{{Metadata: meta("slack"), Spec: api.ProviderSpec{Type: notify.TypeSlack, Address: "https://hooks.slack.com/services/x"}}},
		Alerts: []*api.Alert{{Metadata: meta("slack"), Spec: api.AlertSpec{
			ProviderRef: api.LocalObjectReference{Name: "slack"}, EventSources: []api.NotificationObjectRef{{Kind: api.KindKustomization, Name: "*"}},
		}}},
	}
	c.mu.Lock()
	c.registerChildren(context.Background(), s, key, set, true)
	c.mu.Unlock()
	if len(c.notif) != 2 {
		t.Fatalf("registered %d notification objects", len(c.notif))
	}
	for _, st := range c.Status() {
		if st.Kind == api.KindAlert && !st.IsReady() {
			t.Errorf("alert not ready: %s", st.Ready.Message)
		}
	}
	c.mu.Lock()
	c.registerChildren(context.Background(), s, key, &api.Set{}, true)
	c.mu.Unlock()
	if len(c.notif) != 0 {
		t.Errorf("%d notification objects left after prune", len(c.notif))
	}
}

func TestDecodeNotificationObjects(t *testing.T) {
	doc := `apiVersion: notification.toolkit.fluxcd.io/v1beta3
kind: Provider
metadata:
  name: slack
spec:
  type: slack
  channel: general
  secretRef:
    name: slack-url
---
apiVersion: notification.toolkit.fluxcd.io/v1
kind: Alert
metadata:
  name: on-call
  namespace: flux-system
spec:
  providerRef:
    name: slack
  eventSeverity: error
  eventSources:
    - kind: Kustomization
      name: '*'
      matchLabels:
        team: ops
---
apiVersion: notification.toolkit.fluxcd.io/v1
kind: Receiver
metadata:
  name: github
spec:
  type: github
  events: ["ping", "push"]
  secretRef:
    name: webhook-token
  resources:
    - kind: GitRepository
      name: flux-system
`
	set := &api.Set{}
	if err := set.Decode("test", []byte(doc), api.DefaultNamespace); err != nil {
		t.Fatal(err)
	}
	if len(set.Providers) != 1 || len(set.Alerts) != 1 || len(set.Receivers) != 1 || len(set.Ignored) != 0 {
		t.Fatalf("decoded: %+v", set)
	}
	if p := set.Providers[0]; p.Metadata.Namespace != api.DefaultNamespace || p.Spec.SecretRef.Name != "slack-url" || p.Spec.Channel != "general" {
		t.Errorf("provider: %+v", p)
	}
	if a := set.Alerts[0]; a.Spec.EventSources[0].MatchLabels["team"] != "ops" || a.Spec.EventSeverity != "error" {
		t.Errorf("alert: %+v", a)
	}
	if r := set.Receivers[0]; r.Spec.Resources[0].Kind != api.KindGitRepository || len(r.Spec.Events) != 2 {
		t.Errorf("receiver: %+v", r)
	}
}
