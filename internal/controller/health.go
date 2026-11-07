// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/swarm"
	"github.com/olljanat-ai/flux4swarm/internal/translate"
)

// healthCheckInterval is how often health checks poll Swarm.
var healthCheckInterval = 2 * time.Second

func workloadKey(kind, ns, name string) string { return kind + "/" + ns + "/" + name }

// dependenciesNotReadyLocked returns why the dependsOn of ks are not
// satisfied, or "" when they are. Like Flux, a dependency must be Ready and,
// when it uses the same source, must have applied the source's current
// revision. c.mu must be held.
func (c *Controller) dependenciesNotReadyLocked(ks *api.Kustomization, src *sourceState) string {
	for _, d := range ks.Spec.DependsOn {
		ns := d.Namespace
		if ns == "" {
			ns = ks.Metadata.Namespace
		}
		dep := c.kustomization[ksKey(ns, d.Name)]
		if dep == nil {
			return fmt.Sprintf("dependency '%s/%s' not found", ns, d.Name)
		}
		if !dep.status.IsReady() {
			return fmt.Sprintf("dependency '%s/%s' is not ready", ns, d.Name)
		}
		if src != nil && src.artifact != nil && c.sourceForLocked(dep.obj) == src && dep.appliedRevision != src.artifact.Revision {
			return fmt.Sprintf("dependency '%s/%s' revision is not up to date", ns, d.Name)
		}
	}
	return ""
}

// workloadKinds are the Kubernetes kinds that become Swarm services.
var workloadKinds = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true, "Job": true}

// healthCheck is one resolved entry of spec.healthChecks.
type healthCheck struct {
	name  string
	check func(context.Context) (swarm.Health, error)
}

// healthChecks waits until every object in spec.healthChecks of ks is
// healthy:
//   - Deployment, StatefulSet, DaemonSet: the Swarm service converged;
//   - Job: the replicated job completed;
//   - Kustomization, GitRepository: Ready (their status cannot change while
//     this Kustomization reconciles, so they are checked once).
//
// Workloads may belong to any Kustomization; their Swarm service name comes
// from the build that applied them (swarm.flux4swarm.io/name included),
// else from the namespace_name convention. Objects with no Swarm meaning
// (Service, ConfigMap, ...) are healthy as soon as they are listed, as
// kstatus treats them in Flux.
func (c *Controller) healthChecks(ctx context.Context, key string, ks *api.Kustomization, own map[string]string, timeout time.Duration) error {
	var checks []healthCheck
	var notReady []string
	for _, ref := range ks.Spec.HealthChecks {
		ns := ref.Namespace
		if ns == "" {
			ns = ks.Metadata.Namespace
		}
		id := fmt.Sprintf("%s/%s/%s", ref.Kind, ns, ref.Name)
		group, _, _ := strings.Cut(ref.APIVersion, "/")
		switch {
		case (workloadKinds[ref.Kind] && (group == "" || group == "apps" || group == "batch" || group == "v1")) ||
			(ref.Kind == api.KindRollout && group == "argoproj.io"):
			svc := c.serviceFor(key, own, ref.Kind, ns, ref.Name)
			checks = append(checks, healthCheck{name: id + " (service " + svc + ")", check: func(ctx context.Context) (swarm.Health, error) {
				return swarm.CheckService(ctx, c.opts.Docker, svc)
			}})
		case ref.Kind == api.KindKustomization || ref.Kind == api.KindGitRepository || ref.Kind == LocalSourceKind:
			if msg := c.fluxObjectNotReady(key, ref.Kind, ns, ref.Name); msg != "" {
				notReady = append(notReady, id+": "+msg)
			}
		}
		// Anything else (HelmRelease, custom resources) is not applied to
		// Swarm; skippedHealthChecks reports it as a warning.
	}
	if len(notReady) > 0 {
		return errors.New("health check failed: " + strings.Join(notReady, "; "))
	}
	if len(checks) == 0 {
		return nil
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].name < checks[j].name })
	deadline := time.Now().Add(timeout)
	for _, hc := range checks {
		left := time.Until(deadline)
		if left <= 0 {
			left = time.Millisecond
		}
		if err := swarm.WaitHealthy(ctx, left, healthCheckInterval, hc.check); err != nil {
			return fmt.Errorf("health check %s: %w", hc.name, err)
		}
	}
	return nil
}

// skippedHealthChecks lists warnings for the health checks of ks that
// name Flux kinds Swarm does not run, such as HelmRelease. Like the objects
// themselves they are skipped instead of failing the Kustomization.
func skippedHealthChecks(ks *api.Kustomization) []string {
	var out []string
	for _, ref := range ks.Spec.HealthChecks {
		group, _, _ := strings.Cut(ref.APIVersion, "/")
		if !strings.HasSuffix(group, ".fluxcd.io") || ref.Kind == api.KindKustomization || ref.Kind == api.KindGitRepository {
			continue
		}
		ns := ref.Namespace
		if ns == "" {
			ns = ks.Metadata.Namespace
		}
		out = append(out, fmt.Sprintf("health check %s/%s/%s: kind %s is not supported on Swarm and was skipped", ref.Kind, ns, ref.Name, ref.Kind))
	}
	return out
}

// serviceFor returns the Swarm service name of a workload: from this
// Kustomization's build, from another Kustomization's last build, else by
// convention.
func (c *Controller) serviceFor(key string, own map[string]string, kind, ns, name string) string {
	wk := workloadKey(kind, ns, name)
	if n, ok := own[wk]; ok {
		return n
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.kustomization))
	for k := range c.kustomization {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == key {
			continue
		}
		if n, ok := c.kustomization[k].workloads[wk]; ok {
			return n
		}
	}
	return translate.SwarmName(ns, name)
}

// fluxObjectNotReady returns why a Kustomization or source is not Ready, or
// "" when it is.
func (c *Controller) fluxObjectNotReady(self, kind, ns, name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var st *ObjectStatus
	switch kind {
	case api.KindKustomization:
		k := ksKey(ns, name)
		if k == self {
			return ""
		}
		if s := c.kustomization[k]; s != nil {
			st = &s.status
		}
	default:
		if s := c.sources[srcKey(kind, ns, name)]; s != nil {
			st = &s.status
		}
	}
	switch {
	case st == nil:
		return "not found"
	case !st.IsReady():
		if st.Ready.Message != "" {
			return "not ready: " + st.Ready.Message
		}
		return "not ready"
	}
	return ""
}
