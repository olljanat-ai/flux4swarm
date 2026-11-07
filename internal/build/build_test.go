// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package build

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

func write(t *testing.T, root, p, content string) {
	t.Helper()
	full := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildGeneratesKustomizationAndSubstitutes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "apps/app.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  replicas: ${REPLICAS:=1}
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers: [{name: web, image: "nginx:${TAG}"}]
`)
	write(t, root, "apps/notes.yaml", "just: data\n")
	write(t, root, "apps/sub/kustomization.yaml", "resources: [cm.yaml]\n")
	write(t, root, "apps/sub/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg}\ndata: {a: b}\n")
	write(t, root, "apps/sub/ignored.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: not-listed}\n")

	images, _ := json.Marshal(map[string]string{"name": "nginx", "newTag": "1.27"})
	set, err := Build(root, Options{Spec: api.KustomizationSpec{
		Path:            "./apps",
		TargetNamespace: "shop",
		PostBuild:       &api.PostBuild{Substitute: map[string]string{"TAG": "1.25", "REPLICAS": "3"}},
		Images:          []json.RawMessage{images},
		CommonMetadata:  &api.CommonMetadata{Labels: map[string]string{"team": "a"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Deployments) != 1 || len(set.ConfigMaps) != 1 {
		t.Fatalf("deployments %d configmaps %d", len(set.Deployments), len(set.ConfigMaps))
	}
	d := set.Deployments[0]
	if d.Namespace != "shop" || *d.Spec.Replicas != 3 || d.Labels["team"] != "a" {
		t.Errorf("unexpected deployment %s/%s replicas %d labels %v", d.Namespace, d.Name, *d.Spec.Replicas, d.Labels)
	}
	// Like Flux, kustomize runs before substitution, so a templated tag is not matched by images.
	if img := d.Spec.Template.Spec.Containers[0].Image; img != "nginx:1.25" {
		t.Errorf("image %s", img)
	}
	if set.ConfigMaps[0].Name != "cfg" {
		t.Errorf("configmap %s", set.ConfigMaps[0].Name)
	}
}

func TestBuildRejectsEscape(t *testing.T) {
	if _, err := Build(t.TempDir(), Options{Spec: api.KustomizationSpec{Path: "../etc"}}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestBuildSkipsFluxComponents(t *testing.T) {
	root := t.TempDir()
	write(t, root, "c/gotk-components.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: source-controller
  namespace: flux-system
  labels: {app.kubernetes.io/part-of: flux}
spec:
  selector: {matchLabels: {app: x}}
  template: {metadata: {labels: {app: x}}, spec: {containers: [{name: x, image: x}]}}
`)
	set, err := Build(root, Options{Spec: api.KustomizationSpec{Path: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Deployments) != 0 || len(set.Ignored) != 1 {
		t.Errorf("deployments %d ignored %v", len(set.Deployments), set.Ignored)
	}
}
