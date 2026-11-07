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
	"testing"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

func newTestController(token string) *Controller {
	c := New(Options{APIToken: token, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	c.AddKustomization(&api.Kustomization{
		Metadata: api.ObjectMeta{Name: "apps", Namespace: api.DefaultNamespace},
		Spec:     api.KustomizationSpec{SourceRef: api.CrossNamespaceObjectReference{Kind: api.KindGitRepository, Name: "flux-system"}},
	})
	c.AddKustomization(&api.Kustomization{
		Metadata: api.ObjectMeta{Name: "frozen", Namespace: api.DefaultNamespace},
		Spec:     api.KustomizationSpec{Suspend: true},
	})
	return c
}

func post(t *testing.T, srv *httptest.Server, path, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func statusOf(t *testing.T, srv *httptest.Server, name string) ObjectStatus {
	t.Helper()
	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st []ObjectStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("%s not in /status", name)
	return ObjectStatus{}
}

func TestSuspendResumeAPI(t *testing.T) {
	c := newTestController("tok")
	srv := httptest.NewServer(c.Handler())
	defer srv.Close()

	if code := post(t, srv, "/suspend/ks/flux-system/apps", ""); code != http.StatusUnauthorized {
		t.Fatalf("without token: %d", code)
	}
	if code := post(t, srv, "/suspend/ks/flux-system/apps", "tok"); code != http.StatusOK {
		t.Fatalf("suspend: %d", code)
	}
	st := statusOf(t, srv, "apps")
	if !st.Suspended || st.SuspendedBy != SuspendedByAPI || st.Ready.Reason != ReasonSuspended {
		t.Fatalf("after suspend: %+v", st)
	}
	// A suspended Kustomization is not reconciled: without a source this
	// would otherwise fail with ArtifactFailed.
	c.pass(context.Background(), true)
	if st := statusOf(t, srv, "apps"); st.Ready.Reason != ReasonSuspended {
		t.Fatalf("reconciled while suspended: %+v", st.Ready)
	}
	if code := post(t, srv, "/resume/kustomization/flux-system/apps", "tok"); code != http.StatusOK {
		t.Fatalf("resume: %d", code)
	}
	c.pass(context.Background(), true)
	if st := statusOf(t, srv, "apps"); st.Suspended || st.Ready.Reason != ReasonArtifactFailed {
		t.Fatalf("after resume: %+v", st)
	}

	if code := post(t, srv, "/resume/ks/flux-system/frozen", "tok"); code != http.StatusConflict {
		t.Errorf("resume of spec.suspend: %d", code)
	}
	if code := post(t, srv, "/suspend/ks/flux-system/missing", "tok"); code != http.StatusNotFound {
		t.Errorf("missing object: %d", code)
	}
	if code := post(t, srv, "/suspend/helmrelease/flux-system/apps", "tok"); code != http.StatusNotFound {
		t.Errorf("unknown kind: %d", code)
	}
}

func TestReconcileOpenWithoutToken(t *testing.T) {
	srv := httptest.NewServer(newTestController("").Handler())
	defer srv.Close()
	if code := post(t, srv, "/reconcile", ""); code != http.StatusAccepted {
		t.Errorf("reconcile: %d", code)
	}
}
