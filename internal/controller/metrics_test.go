// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

func TestMetricsFluxCompatible(t *testing.T) {
	c := New(Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	c.AddLocalSource("flux-system", t.TempDir())
	c.AddGitRepository(&api.GitRepository{
		Metadata: api.ObjectMeta{Name: "paused", Namespace: "flux-system"},
		Spec:     api.GitRepositorySpec{URL: "https://example.com/repo.git", Suspend: true},
	})
	c.AddOCIRepository(&api.OCIRepository{
		Metadata: api.ObjectMeta{Name: "artifacts", Namespace: "flux-system"},
		Spec:     api.OCIRepositorySpec{URL: "oci://example.com/manifests", Suspend: true},
	})
	c.AddKustomization(&api.Kustomization{
		Metadata: api.ObjectMeta{Name: "apps", Namespace: "flux-system"},
		Spec: api.KustomizationSpec{SourceRef: api.CrossNamespaceObjectReference{
			Kind: api.KindGitRepository, Name: "missing"}},
	})
	c.pass(context.Background(), true)

	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`gotk_reconcile_condition{kind="Kustomization",name="apps",namespace="flux-system",status="False",type="Ready"} 1`,
		`gotk_reconcile_condition{kind="LocalDirectory",name="flux-system",namespace="flux-system",status="True",type="Ready"} 1`,
		`gotk_suspend_status{kind="GitRepository",name="paused",namespace="flux-system"} 1`,
		`gotk_reconcile_duration_seconds_count{kind="Kustomization",name="apps",namespace="flux-system"} 1`,
		`gotk_reconcile_duration_seconds_bucket{kind="Kustomization",name="apps",namespace="flux-system",le="0.01"}`,
		`gotk_resource_info{customresource_group="kustomize.toolkit.fluxcd.io",customresource_kind="Kustomization",customresource_version="v1",exported_namespace="flux-system",name="apps",ready="False",reason="ArtifactFailed",revision="",source_name="missing",suspended="false",url=""} 1`,
		`gotk_resource_info{customresource_group="source.toolkit.fluxcd.io",customresource_kind="GitRepository",customresource_version="v1",exported_namespace="flux-system",name="paused",ready="Unknown",reason="Suspended",revision="",source_name="",suspended="true",url="https://example.com/repo.git"} 1`,
		`gotk_resource_info{customresource_group="source.toolkit.fluxcd.io",customresource_kind="OCIRepository",customresource_version="v1",exported_namespace="flux-system",name="artifacts",ready="Unknown",reason="Suspended",revision="",source_name="",suspended="true",url="oci://example.com/manifests"} 1`,
		`controller_runtime_reconcile_total{controller="ocirepository",result="success"} 1`,
		`controller_runtime_reconcile_total{controller="ocirepository",result="error"} 0`,
		`controller_runtime_reconcile_errors_total{controller="ocirepository"} 0`,
		`controller_runtime_max_concurrent_reconciles{controller="ocirepository"} 1`,
		`controller_runtime_reconcile_total{controller="kustomization",result="error"} 1`,
		`controller_runtime_reconcile_total{controller="gitrepository",result="success"} 1`,
		`controller_runtime_reconcile_errors_total{controller="kustomization"} 1`,
		`controller_runtime_reconcile_time_seconds_count{controller="kustomization"} 1`,
		`controller_runtime_max_concurrent_reconciles{controller="kustomization"} 1`,
		"go_goroutines ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, `gotk_reconcile_duration_seconds_count{kind="GitRepository",name="paused"`) {
		t.Error("suspended object has a duration")
	}
}
