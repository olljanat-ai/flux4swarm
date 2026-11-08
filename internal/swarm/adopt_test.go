// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package swarm_test

import (
	"context"
	"testing"

	dswarm "github.com/docker/docker/api/types/swarm"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/swarm"
	"github.com/olljanat-ai/flux4swarm/internal/swarm/swarmtest"
)

// unmanaged applies d, then drops the ownership labels again, as if the
// service had been created by `docker service create`.
func unmanaged(t *testing.T, f *swarmtest.Fake, d swarm.Desired) {
	t.Helper()
	mustApply(t, applier(f), d)
	for i := range f.Services {
		for k := range owner.Labels() {
			delete(f.Services[i].Spec.Labels, k)
		}
		delete(f.Services[i].Spec.Labels, api.LabelSpecHash)
	}
	f.TakeCalls()
}

func plain(image string) swarm.Desired {
	return swarm.Desired{Services: []*swarm.Service{{Name: "shop_web", Spec: dswarm.ServiceSpec{
		Annotations:  dswarm.Annotations{Labels: map[string]string{"app": "web"}},
		TaskTemplate: dswarm.TaskSpec{ContainerSpec: &dswarm.ContainerSpec{Image: image}},
		Mode:         replicas(1),
	}}}}
}

func TestAdoptLabels(t *testing.T) {
	f := swarmtest.New()
	unmanaged(t, f, plain("nginx:1"))

	changes, err := applier(f).AdoptLabels(context.Background(), plain("nginx:1"))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "changes", actions(changes), []string{"Service/shop_web configured (adopted (labels only))"})
	s, _ := f.Service("shop_web")
	if s.Spec.Labels[api.LabelSpecHash] == "" || s.Spec.Labels[api.LabelKustomizeName] != owner.Name {
		t.Errorf("labels %v", s.Spec.Labels)
	}
	if res := mustApply(t, applier(f), plain("nginx:1")); actions(res.Changes)[0] != "Service/shop_web unchanged" {
		t.Errorf("reconcile after adopt: %v", actions(res.Changes))
	}
}

// A rolling update that completes between the list and the relabel bumps
// the service version; the relabel must retry at the new version.
func TestAdoptLabelsRetriesOutOfSequence(t *testing.T) {
	f := swarmtest.New()
	unmanaged(t, f, plain("nginx:1"))
	bumps := 2
	f.BeforeServiceUpdate = func(s *dswarm.Service) {
		if bumps > 0 {
			bumps--
			s.Version.Index++
			s.UpdateStatus = &dswarm.UpdateStatus{State: dswarm.UpdateStateCompleted}
		}
	}

	changes, err := applier(f).AdoptLabels(context.Background(), plain("nginx:1"))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "changes", actions(changes), []string{"Service/shop_web configured (adopted (labels only))"})
	eq(t, "calls", f.TakeCalls(), []string{"ServiceUpdate shop_web"})
}

// A real change between the list and the relabel is never overwritten.
func TestAdoptLabelsKeepsConcurrentChange(t *testing.T) {
	f := swarmtest.New()
	unmanaged(t, f, plain("nginx:1"))
	f.BeforeServiceUpdate = func(s *dswarm.Service) {
		if s.Spec.TaskTemplate.ContainerSpec.Image == "nginx:1" {
			s.Version.Index++
			s.Spec.TaskTemplate.ContainerSpec.Image = "nginx:2"
		}
	}

	changes, err := applier(f).AdoptLabels(context.Background(), plain("nginx:1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Action != swarm.Unchanged {
		t.Fatalf("changes %v", actions(changes))
	}
	s, _ := f.Service("shop_web")
	if s.Spec.TaskTemplate.ContainerSpec.Image != "nginx:2" || s.Spec.Labels[api.LabelSpecHash] != "" {
		t.Errorf("service changed: %+v", s.Spec)
	}
}
