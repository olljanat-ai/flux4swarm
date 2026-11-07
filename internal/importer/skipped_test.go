// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"context"
	"regexp"
	"testing"

	"github.com/docker/docker/api/types/swarm"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

func TestExportReportsSkipped(t *testing.T) {
	d := stackService()
	d.services = append(d.services, swarm.Service{ID: "svc2", Spec: swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: "tmp_debug"},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "busybox"}},
	}})
	d.configs = append(d.configs, swarm.Config{ID: "old", Spec: swarm.ConfigSpec{
		Annotations: swarm.Annotations{Name: "app-0123456789", Labels: map[string]string{api.LabelName: "app"}}}})
	d.secrets = append(d.secrets, swarm.Secret{ID: "tmpsec", Spec: swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: "tmp_token"}}})
	res, err := Export(context.Background(), d, Options{Exclude: regexp.MustCompile(`^tmp_`)})
	if err != nil {
		t.Fatal(err)
	}
	want := map[Skipped]bool{
		{Kind: "Service", Name: "tmp_debug", Reason: SkippedExcluded}:   true,
		{Kind: "Config", Name: "app-0123456789", Reason: SkippedUnused}: true,
		{Kind: "Secret", Name: "tmp_token", Reason: SkippedExcluded}:   true,
	}
	for _, s := range res.Skipped {
		if !want[s] {
			t.Errorf("unexpected skipped %v", s)
		}
		delete(want, s)
	}
	for s := range want {
		t.Errorf("missing skipped %v", s)
	}
}
