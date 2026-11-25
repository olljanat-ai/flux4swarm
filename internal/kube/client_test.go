// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olljanat-ai/flux4swarm/internal/importer"
)

func TestClient(t *testing.T) {
	var applied, applyType, fieldManager string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1":
			io.WriteString(w, `{"kind":"APIResourceList","groupVersion":"v1","resources":[
				{"name":"configmaps","namespaced":true,"kind":"ConfigMap","verbs":["list","get"]},
				{"name":"pods/log","namespaced":true,"kind":"Pod","verbs":["get"]},
				{"name":"bindings","namespaced":true,"kind":"Binding","verbs":["create"]}]}`)
		case r.URL.Path == "/apis":
			io.WriteString(w, `{"kind":"APIGroupList","groups":[
				{"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}},
				{"name":"metrics.k8s.io","versions":[{"groupVersion":"metrics.k8s.io/v1beta1","version":"v1beta1"}],"preferredVersion":{"groupVersion":"metrics.k8s.io/v1beta1","version":"v1beta1"}}]}`)
		case r.URL.Path == "/apis/apps/v1":
			io.WriteString(w, `{"kind":"APIResourceList","groupVersion":"apps/v1","resources":[
				{"name":"deployments","namespaced":true,"kind":"Deployment","verbs":["list"]}]}`)
		case r.URL.Path == "/apis/metrics.k8s.io/v1beta1":
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"kind":"Status","message":"service unavailable"}`)
		case r.URL.Path == "/api/v1/namespaces/shop/configmaps" && r.Method == http.MethodGet:
			if r.URL.Query().Get("continue") == "" {
				io.WriteString(w, `{"kind":"ConfigMapList","apiVersion":"v1","metadata":{"continue":"p2"},"items":[{"metadata":{"name":"a","namespace":"shop"}}]}`)
				return
			}
			io.WriteString(w, `{"kind":"ConfigMapList","apiVersion":"v1","metadata":{},"items":[{"metadata":{"name":"b","namespace":"shop"}}]}`)
		case r.URL.Path == "/apis/apps/v1/deployments":
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`)
		case r.URL.Path == "/api/v1/namespaces/shop/configmaps/c" && r.Method == http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			applied, applyType, fieldManager = string(b), r.Header.Get("Content-Type"), r.URL.Query().Get("fieldManager")
			w.Write(b)
		case r.URL.Path == "/api/v1/namespaces/shop/configmaps/gone":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc := filepath.Join(t.TempDir(), "config")
	cfg := "apiVersion: v1\nkind: Config\nclusters: [{name: c, cluster: {server: " + srv.URL + "}}]\n" +
		"users: [{name: u, user: {token: t}}]\ncontexts: [{name: x, context: {cluster: c, user: u}}]\ncurrent-context: x\n"
	if err := os.WriteFile(kc, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(kc, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := c.Resources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range res {
		names = append(names, r.APIVersion()+"/"+r.Kind)
	}
	if got := strings.Join(names, ","); got != "v1/ConfigMap,apps/v1/Deployment" {
		t.Errorf("resources: %s", got)
	}
	cm := importer.KubeResource{Version: "v1", Resource: "configmaps", Kind: "ConfigMap", Namespaced: true}
	l, err := c.List(ctx, cm, "shop")
	if err != nil || len(l) != 2 {
		t.Fatalf("paged list: %v %v", l, err)
	}
	if l, err := c.List(ctx, res[1], ""); err != nil || len(l) != 0 {
		t.Errorf("forbidden list: %v %v", l, err)
	}
	if err := c.Apply(ctx, cm, map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c", "namespace": "shop"}}); err != nil {
		t.Fatal(err)
	}
	if applyType != "application/apply-patch+yaml" || fieldManager != FieldManager || !strings.Contains(applied, `"name":"c"`) {
		t.Errorf("apply: %s %s %s", applyType, fieldManager, applied)
	}
	if o, err := c.Get(ctx, cm, "shop", "gone"); o != nil || err != nil {
		t.Errorf("get missing: %v %v", o, err)
	}
	if err := c.Delete(ctx, cm, "shop", "gone"); err != nil {
		t.Errorf("delete missing: %v", err)
	}
}
