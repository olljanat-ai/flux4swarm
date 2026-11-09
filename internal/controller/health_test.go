// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	dswarm "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/errdefs"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/git"
	"github.com/olljanat-ai/flux4swarm/internal/swarm"
)

// fakeDocker answers the health check calls for a set of running
// single-replica services; every other method panics.
type fakeDocker struct {
	swarm.Client
	running map[string]bool // service name -> exists and runs
}

type notFound struct{}

func (notFound) Error() string { return "not found" }

func (f *fakeDocker) ServiceInspectWithRaw(_ context.Context, name string, _ dswarm.ServiceInspectOptions) (dswarm.Service, []byte, error) {
	if _, ok := f.running[name]; !ok {
		return dswarm.Service{}, nil, errdefs.NotFound(notFound{})
	}
	one := uint64(1)
	return dswarm.Service{ID: name, Spec: dswarm.ServiceSpec{
		Annotations: dswarm.Annotations{Name: name},
		Mode:        dswarm.ServiceMode{Replicated: &dswarm.ReplicatedService{Replicas: &one}},
	}}, nil, nil
}

func (f *fakeDocker) TaskList(_ context.Context, o dswarm.TaskListOptions) ([]dswarm.Task, error) {
	name := o.Filters.Get("service")[0]
	state := dswarm.TaskStateStarting
	if f.running[name] {
		state = dswarm.TaskStateRunning
	}
	return []dswarm.Task{{Slot: 1, NodeID: "n1", DesiredState: dswarm.TaskStateRunning, Status: dswarm.TaskStatus{State: state}}}, nil
}

func (f *fakeDocker) NodeList(context.Context, dswarm.NodeListOptions) ([]dswarm.Node, error) {
	return []dswarm.Node{{ID: "n1", Status: dswarm.NodeStatus{State: dswarm.NodeStateReady}}}, nil
}

func ks(name string, deps ...string) *api.Kustomization {
	k := &api.Kustomization{Metadata: api.ObjectMeta{Name: name, Namespace: api.DefaultNamespace}}
	k.Spec.SourceRef = api.CrossNamespaceObjectReference{Kind: api.KindGitRepository, Name: "flux-system"}
	for _, d := range deps {
		k.Spec.DependsOn = append(k.Spec.DependsOn, api.DependencyReference{Name: d})
	}
	return k
}

func TestHealthChecks(t *testing.T) {
	old := healthCheckInterval
	healthCheckInterval = time.Millisecond
	t.Cleanup(func() { healthCheckInterval = old })

	docker := &fakeDocker{running: map[string]bool{"custom-db": true, "demo_cache": true, "demo_slow": false}}
	c := New(Options{Docker: docker})
	c.AddKustomization(ks("db"))
	c.AddKustomization(ks("broken"))
	c.AddKustomization(ks("app", "db"))
	// db applied Deployment demo/db with swarm.flux4swarm.io/name: custom-db.
	db := c.kustomization[ksKey(api.DefaultNamespace, "db")]
	db.workloads = map[string]string{workloadKey("Deployment", "demo", "db"): "custom-db"}
	db.status.setReady(ConditionTrue, ReasonSucceeded, "ok")
	c.kustomization[ksKey(api.DefaultNamespace, "broken")].status.setReady(ConditionFalse, ReasonBuildFailed, "boom")
	c.AddOCIRepository(&api.OCIRepository{Metadata: api.ObjectMeta{Name: "artifacts", Namespace: api.DefaultNamespace}})
	c.AddOCIRepository(&api.OCIRepository{Metadata: api.ObjectMeta{Name: "unreachable", Namespace: api.DefaultNamespace}})
	c.sources[srcKey(api.KindOCIRepository, api.DefaultNamespace, "artifacts")].status.setReady(ConditionTrue, ReasonSucceeded, "ok")
	c.sources[srcKey(api.KindOCIRepository, api.DefaultNamespace, "unreachable")].status.setReady(ConditionFalse, ReasonOCIPullFailed, "no route")
	self := ksKey(api.DefaultNamespace, "app")

	ref := func(apiVersion, kind, ns, name string) api.CrossNamespaceObjectReference {
		return api.CrossNamespaceObjectReference{APIVersion: apiVersion, Kind: kind, Namespace: ns, Name: name}
	}
	for _, tc := range []struct {
		name    string
		checks  []api.CrossNamespaceObjectReference
		wantErr string
	}{
		{name: "workload of another kustomization", checks: []api.CrossNamespaceObjectReference{ref("apps/v1", "Deployment", "demo", "db")}},
		{name: "workload by naming convention", checks: []api.CrossNamespaceObjectReference{ref("apps/v1", "StatefulSet", "demo", "cache")}},
		{name: "ready kustomization", checks: []api.CrossNamespaceObjectReference{ref(api.KustomizeGroupVersion, api.KindKustomization, "", "db")}},
		{name: "objects with no swarm meaning", checks: []api.CrossNamespaceObjectReference{ref("v1", "Service", "demo", "db"), ref("v1", "ConfigMap", "demo", "x")}},
		{name: "not converged", checks: []api.CrossNamespaceObjectReference{ref("apps/v1", "Deployment", "demo", "slow")}, wantErr: "demo_slow"},
		{name: "missing service", checks: []api.CrossNamespaceObjectReference{ref("batch/v1", "Job", "demo", "gone")}, wantErr: "service not found"},
		{name: "kustomization not ready", checks: []api.CrossNamespaceObjectReference{ref(api.KustomizeGroupVersion, api.KindKustomization, "", "broken")}, wantErr: "boom"},
		{name: "kustomization missing", checks: []api.CrossNamespaceObjectReference{ref(api.KustomizeGroupVersion, api.KindKustomization, "", "nope")}, wantErr: "not found"},
		{name: "ready ocirepository", checks: []api.CrossNamespaceObjectReference{ref(api.SourceGroupVersion, api.KindOCIRepository, "", "artifacts")}},
		{name: "ocirepository not ready", checks: []api.CrossNamespaceObjectReference{ref(api.SourceGroupVersion, api.KindOCIRepository, "", "unreachable")}, wantErr: "no route"},
		{name: "flux kind swarm does not run is skipped", checks: []api.CrossNamespaceObjectReference{ref("helm.toolkit.fluxcd.io/v2", "HelmRelease", "demo", "x")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := ks("app", "db")
			k.Spec.HealthChecks = tc.checks
			err := c.healthChecks(context.Background(), self, k, nil, 50*time.Millisecond)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestDependenciesNotReady(t *testing.T) {
	c := New(Options{})
	c.AddGitRepository(&api.GitRepository{Metadata: api.ObjectMeta{Name: "flux-system", Namespace: api.DefaultNamespace}})
	src := c.sources[srcKey(api.KindGitRepository, api.DefaultNamespace, "flux-system")]
	src.artifact = &git.Artifact{Revision: "main@sha1:new"}
	c.AddKustomization(ks("infra"))
	app := ks("app", "infra")
	infra := c.kustomization[ksKey(api.DefaultNamespace, "infra")]

	check := func(want string) {
		t.Helper()
		c.mu.Lock()
		got := c.dependenciesNotReadyLocked(app, src)
		c.mu.Unlock()
		if !strings.Contains(got, want) || (want == "" && got != "") {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	check("is not ready")
	infra.status.setReady(ConditionTrue, ReasonSucceeded, "ok")
	infra.appliedRevision = "main@sha1:old"
	check("revision is not up to date")
	infra.appliedRevision = "main@sha1:new"
	check("")
	check2 := ks("app", "missing")
	c.mu.Lock()
	got := c.dependenciesNotReadyLocked(check2, src)
	c.mu.Unlock()
	if !strings.Contains(got, "not found") {
		t.Errorf("missing dependency: %q", got)
	}
}

func TestSkippedHealthChecks(t *testing.T) {
	k := ks("app", "db")
	k.Spec.HealthChecks = []api.CrossNamespaceObjectReference{
		{APIVersion: "helm.toolkit.fluxcd.io/v2", Kind: "HelmRelease", Name: "x"},
		{APIVersion: api.KustomizeGroupVersion, Kind: api.KindKustomization, Name: "db"},
		{APIVersion: api.SourceGroupVersion, Kind: api.KindGitRepository, Name: "flux-system"},
		{APIVersion: api.SourceGroupVersion, Kind: api.KindOCIRepository, Name: "artifacts"},
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"},
	}
	got := skippedHealthChecks(k)
	if len(got) != 1 || !strings.Contains(got[0], "HelmRelease/") || !strings.Contains(got[0], "skipped") {
		t.Errorf("got %v", got)
	}
}
