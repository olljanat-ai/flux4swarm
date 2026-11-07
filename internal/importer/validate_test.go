// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, p, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exportTo(t *testing.T) string {
	t.Helper()
	res, err := Export(context.Background(), stackService(), Options{Cluster: "prod", Bootstrap: true, SecretStore: "fake", SecretStoreProvider: ProviderFake})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := res.WriteTo(dir, true); err != nil {
		t.Fatal(err)
	}
	return dir
}

func smp(name string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + name + `
  namespace: shop
spec:
  replicas: 3
`
}

func TestValidateExport(t *testing.T) {
	if err := Validate(exportTo(t), "clusters/prod"); err != nil {
		t.Fatal(err)
	}
}

// A Flux Kustomization that a platform team added next to the export
// patches a service; the patch breaks once the service is gone.
func TestValidateFollowsFluxKustomizations(t *testing.T) {
	platform := func(name string) string {
		return `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  interval: 10m
  path: ./clusters/prod/apps
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  patches:
  - patch: |
` + indent(smp(name), "      ")
	}
	dir := exportTo(t)
	writeFile(t, dir, "clusters/prod/platform.yaml", platform("web"))
	if err := Validate(dir, "clusters/prod"); err != nil {
		t.Fatalf("patch of an existing service: %v", err)
	}
	writeFile(t, dir, "clusters/prod/platform.yaml", platform("gone"))
	err := Validate(dir, "clusters/prod")
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
	if ve.Path != "clusters/prod/apps" || !strings.Contains(err.Error(), "not pushing") {
		t.Errorf("error %q (path %q)", err, ve.Path)
	}
}

// A plain kustomize overlay in the cluster directory.
func TestValidateKustomizeOverlay(t *testing.T) {
	dir := exportTo(t)
	writeFile(t, dir, "clusters/prod/kustomization.yaml", `resources: [flux-system, apps]
patches:
- path: replicas.yaml
`)
	writeFile(t, dir, "clusters/prod/replicas.yaml", smp("gone"))
	var ve *ValidationError
	if err := Validate(dir, "clusters/prod"); !errors.As(err, &ve) || ve.Path != "clusters/prod" {
		t.Fatalf("want ValidationError for clusters/prod, got %v", err)
	}
	writeFile(t, dir, "clusters/prod/replicas.yaml", smp("web"))
	if err := Validate(dir, "clusters/prod"); err != nil {
		t.Fatal(err)
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// When the last service leaves Swarm, apps/ must still exist so an
// overlay that lists it keeps building.
func TestValidateEmptySwarm(t *testing.T) {
	res, err := Export(context.Background(), &fakeDocker{}, Options{Cluster: "prod", Bootstrap: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := res.WriteTo(dir, true); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "clusters/prod/kustomization.yaml", "resources: [flux-system, apps]\n")
	if err := Validate(dir, "clusters/prod"); err != nil {
		t.Fatal(err)
	}
}
