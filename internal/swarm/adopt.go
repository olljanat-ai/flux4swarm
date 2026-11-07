// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package swarm

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/swarm"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

// AdoptLabels takes ownership of live services that d describes exactly,
// by adding the owner and spec-hash labels without changing anything
// else. The first reconcile then finds nothing to update. A service is
// left alone (and updated by the first reconcile instead) when it is
// already managed, missing, uses a config that does not exist yet, uses a
// secret from d (secret versions depend on values the import cannot read)
// or would still drift after relabelling.
func (a *Applier) AdoptLabels(ctx context.Context, d Desired) ([]Change, error) {
	res := &Result{}
	dry := *a
	dry.DryRun = true
	configs, err := dry.applyConfigs(ctx, d.Configs, &Result{})
	if err != nil {
		return nil, err
	}
	desiredSecrets := map[string]bool{}
	for _, s := range d.Secrets {
		desiredSecrets[s.Name] = true
	}
	live, err := a.Client.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil {
		return nil, err
	}
	byName := map[string]swarm.Service{}
	for _, s := range live {
		byName[s.Spec.Name] = s
	}
	liveConfigs, liveSecrets := map[string]swarm.Config{}, map[string]swarm.Secret{}
	if needsLookup(d.Services, configs, nil) {
		cl, err := a.Client.ConfigList(ctx, swarm.ConfigListOptions{})
		if err != nil {
			return nil, err
		}
		for _, c := range cl {
			liveConfigs[c.Spec.Name] = c
		}
		sl, err := a.Client.SecretList(ctx, swarm.SecretListOptions{})
		if err != nil {
			return nil, err
		}
		for _, s := range sl {
			liveSecrets[s.Spec.Name] = s
		}
	}
	for _, s := range d.Services {
		cur, ok := byName[s.Name]
		if !ok || cur.Spec.Labels[api.LabelSpecHash] != "" {
			continue
		}
		if reason := a.adoptBlocker(s, configs, desiredSecrets); reason != "" {
			res.add(KindService, s.Name, Unchanged, "not relabelled: "+reason)
			continue
		}
		want, err := a.DesiredServiceSpec(s, configs, nil, liveConfigs, liveSecrets)
		if err != nil {
			res.add(KindService, s.Name, Unchanged, "not relabelled: "+err.Error())
			continue
		}
		spec, err := deepCopy(cur.Spec)
		if err != nil {
			return nil, err
		}
		spec.Labels = merge(spec.Labels, a.Owner.Labels(), map[string]string{api.LabelSpecHash: want.Labels[api.LabelSpecHash]})
		if reason := drift(spec, want); reason != "" {
			res.add(KindService, s.Name, Unchanged, "not relabelled: "+reason)
			continue
		}
		if !a.DryRun {
			// No registry query: the image (and its pinned digest) stays as is.
			if _, err := a.Client.ServiceUpdate(ctx, cur.ID, cur.Version, spec, swarm.ServiceUpdateOptions{}); err != nil {
				return res.Changes, fmt.Errorf("relabel service %s: %w", s.Name, err)
			}
		}
		res.add(KindService, s.Name, Configured, "adopted (labels only)")
	}
	return res.Changes, nil
}

func (a *Applier) adoptBlocker(s *Service, configs map[string]versioned, desiredSecrets map[string]bool) string {
	cs := s.Spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return ""
	}
	for _, r := range cs.Configs {
		if v, ok := configs[r.ConfigName]; ok && v.id == "dry-run" {
			return "config " + r.ConfigName + " gets a new version"
		}
	}
	for _, r := range cs.Secrets {
		if desiredSecrets[r.SecretName] {
			return "secret " + r.SecretName + " is created from the secret store"
		}
	}
	return ""
}
