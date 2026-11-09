// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package swarm_test

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/network"
	dswarm "github.com/docker/docker/api/types/swarm"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/swarm"
	"github.com/olljanat-ai/flux4swarm/internal/swarm/swarmtest"
)

var _ swarm.Client = (*swarmtest.Fake)(nil)

var owner = swarm.Owner{Name: "apps", Namespace: "flux-system"}

func applier(f *swarmtest.Fake) *swarm.Applier {
	return &swarm.Applier{Client: f, Owner: owner}
}

func replicas(n uint64) dswarm.ServiceMode {
	return dswarm.ServiceMode{Replicated: &dswarm.ReplicatedService{Replicas: &n}}
}

// desired is one service using a config, a secret and a network.
func desired(config, image string) swarm.Desired {
	return swarm.Desired{
		Networks: []*swarm.Network{{Name: "shop_default", Labels: map[string]string{api.LabelStackNamespace: "shop"}}},
		Configs:  []*swarm.Config{{Name: "shop_nginx", Data: []byte(config)}},
		Secrets:  []swarm.Secret{{Name: "shop_db", Data: []byte("pw"), Source: "shop/db"}},
		Services: []*swarm.Service{{Name: "shop_web", Spec: dswarm.ServiceSpec{
			Annotations: dswarm.Annotations{Labels: map[string]string{"app": "web"}},
			TaskTemplate: dswarm.TaskSpec{
				ContainerSpec: &dswarm.ContainerSpec{
					Image:   image,
					Configs: []*dswarm.ConfigReference{{ConfigName: "shop_nginx", File: &dswarm.ConfigReferenceFileTarget{Name: "/etc/nginx.conf"}}},
					Secrets: []*dswarm.SecretReference{{SecretName: "shop_db", File: &dswarm.SecretReferenceFileTarget{Name: "/run/secrets/db"}}},
				},
				Networks: []dswarm.NetworkAttachmentConfig{{Target: "shop_default"}},
			},
			Mode: replicas(2),
		}}},
	}
}

func actions(changes []swarm.Change) []string {
	var out []string
	for _, c := range changes {
		out = append(out, c.String())
	}
	return out
}

func mustApply(t *testing.T, a *swarm.Applier, d swarm.Desired) *swarm.Result {
	t.Helper()
	res, err := a.Apply(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got %#v\nwant %#v", what, got, want)
	}
}

func TestApplyCreatesThenIsIdempotent(t *testing.T) {
	f := swarmtest.New()
	a := applier(f)
	res := mustApply(t, a, desired("server {}", "nginx:1.27"))
	eq(t, "changes", actions(res.Changes), []string{
		"Network/shop_default created",
		"Config/shop_nginx created",
		"Secret/shop_db created",
		"Service/shop_web created",
	})
	eq(t, "inventory", res.Inventory, []string{"Config/shop_nginx", "Network/shop_default", "Secret/shop_db", "Service/shop_web"})
	calls := f.TakeCalls()
	if len(calls) != 4 {
		t.Fatalf("calls %v", calls)
	}

	svc, ok := f.Service("shop_web")
	if !ok {
		t.Fatal("service not created")
	}
	if res.ServiceIDs["shop_web"] != svc.ID {
		t.Errorf("service ID %q, want %q", res.ServiceIDs["shop_web"], svc.ID)
	}
	l := svc.Spec.Labels
	if l["app"] != "web" || l[api.LabelKustomizeName] != "apps" || l[api.LabelKustomizeNamespace] != "flux-system" ||
		l[api.LabelManagedBy] != api.ManagedByValue || l[api.LabelSpecHash] == "" {
		t.Errorf("service labels %v", l)
	}
	cs := svc.Spec.TaskTemplate.ContainerSpec
	cfg, sec := cs.Configs[0], cs.Secrets[0]
	if !strings.HasPrefix(cfg.ConfigName, "shop_nginx-") || cfg.ConfigID != f.Configs[0].ID || cfg.ConfigName != f.Configs[0].Spec.Name {
		t.Errorf("config reference %+v, configs %+v", cfg, f.Configs)
	}
	if !strings.HasPrefix(sec.SecretName, "shop_db-") || sec.SecretID != f.Secrets[0].ID {
		t.Errorf("secret reference %+v", sec)
	}
	if f.Secrets[0].Spec.Labels[api.LabelExternalSecret] != "shop.db" || f.Secrets[0].Spec.Labels[api.LabelName] != "shop_db" {
		t.Errorf("secret labels %v", f.Secrets[0].Spec.Labels)
	}
	n := f.Networks[0]
	if n.Driver != "overlay" || n.Scope != "swarm" || n.Labels[api.LabelStackNamespace] != "shop" || n.Labels[api.LabelKustomizeName] != "apps" {
		t.Errorf("network %+v", n)
	}
	eq(t, "versions", res.Versions, []string{sec.SecretName, cfg.ConfigName})

	res = mustApply(t, a, desired("server {}", "nginx:1.27"))
	for _, c := range res.Changes {
		if c.Action != swarm.Unchanged || c.Reason != "" {
			t.Errorf("second apply: %s", c)
		}
	}
	if calls := f.TakeCalls(); len(calls) != 0 {
		t.Errorf("second apply made calls %v", calls)
	}
	// Nothing to prune.
	pruned, err := a.Prune(context.Background(), res, true)
	if err != nil || len(pruned) != 0 {
		t.Errorf("prune: %v, %v", actions(pruned), err)
	}
}

func TestConfigChangeRollsVersions(t *testing.T) {
	f := swarmtest.New()
	a := applier(f)
	ctx := context.Background()
	mustApply(t, a, desired("v1", "nginx"))
	v1 := f.Configs[0].Spec.Name
	f.TakeCalls()

	res := mustApply(t, a, desired("v2", "nginx"))
	eq(t, "changes", actions(res.Changes), []string{
		"Network/shop_default unchanged",
		"Config/shop_nginx configured",
		"Secret/shop_db unchanged",
		"Service/shop_web configured (spec changed)",
	})
	svc, _ := f.Service("shop_web")
	v2 := svc.Spec.TaskTemplate.ContainerSpec.Configs[0].ConfigName
	if v2 == v1 || !strings.HasPrefix(v2, "shop_nginx-") {
		t.Fatalf("config version %s after %s", v2, v1)
	}
	eq(t, "calls", f.TakeCalls(), []string{"ConfigCreate " + v2, "ServiceUpdate shop_web"})

	// v1 is still in the service's previous spec (for rollback): keep it.
	pruned, err := a.Prune(ctx, res, false)
	if err != nil || len(pruned) != 0 {
		t.Fatalf("prune after first change: %v, %v", actions(pruned), err)
	}

	res = mustApply(t, a, desired("v3", "nginx"))
	pruned, err = a.Prune(ctx, res, false)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "pruned", actions(pruned), []string{"Config/" + v1 + " deleted (old version)"})
	if len(f.Configs) != 2 {
		t.Errorf("configs left: %v", f.Names())
	}
}

func TestSecretNamesDependOnCluster(t *testing.T) {
	names := map[string]string{}
	for _, id := range []string{"cluster-a", "cluster-b"} {
		f := swarmtest.New()
		f.ClusterID = id
		mustApply(t, applier(f), desired("x", "nginx"))
		names[id] = f.Secrets[0].Spec.Name
	}
	if names["cluster-a"] == names["cluster-b"] {
		t.Errorf("secret version names do not depend on the cluster: %v", names)
	}
	if strings.Contains(names["cluster-a"], "pw") {
		t.Errorf("secret name leaks data: %s", names["cluster-a"])
	}
}

func TestPruneRemoved(t *testing.T) {
	f := swarmtest.New()
	a := applier(f)
	ctx := context.Background()
	mustApply(t, a, desired("x", "nginx"))

	// Another owner's service and an unmanaged config are never touched.
	other := &swarm.Applier{Client: f, Owner: swarm.Owner{Name: "other", Namespace: "flux-system"}}
	mustApply(t, other, swarm.Desired{Services: []*swarm.Service{{Name: "other_svc", Spec: dswarm.ServiceSpec{
		TaskTemplate: dswarm.TaskSpec{ContainerSpec: &dswarm.ContainerSpec{Image: "busybox"}}}}}})
	f.Configs = append(f.Configs, dswarm.Config{ID: "manual", Spec: dswarm.ConfigSpec{Annotations: dswarm.Annotations{Name: "manual"}}})
	f.TakeCalls()

	// With prune disabled, removing objects from the source keeps them.
	res := mustApply(t, a, swarm.Desired{})
	pruned, err := a.Prune(ctx, res, false)
	if err != nil || len(pruned) != 0 {
		t.Fatalf("prune=false removed %v, %v", actions(pruned), err)
	}

	pruned, err = a.Prune(ctx, res, true)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, c := range pruned {
		kinds = append(kinds, c.Kind)
	}
	eq(t, "pruned kinds", kinds, []string{swarm.KindService, swarm.KindConfig, swarm.KindSecret, swarm.KindNetwork})
	eq(t, "left", f.Names(), []string{"Config/manual", "Service/other_svc"})
}

func TestPruneEverythingOnDelete(t *testing.T) {
	// An empty Result (the Kustomization was deleted) removes everything the
	// owner has.
	f := swarmtest.New()
	a := applier(f)
	mustApply(t, a, desired("x", "nginx"))
	if _, err := a.Prune(context.Background(), &swarm.Result{}, true); err != nil {
		t.Fatal(err)
	}
	if n := f.Names(); len(n) != 0 {
		t.Errorf("left %v", n)
	}
}

func TestDryRun(t *testing.T) {
	f := swarmtest.New()
	a := applier(f)
	a.DryRun = true
	res := mustApply(t, a, desired("x", "nginx"))
	if len(res.Changes) != 4 || res.Changes[3].Action != swarm.Created {
		t.Errorf("changes %v", actions(res.Changes))
	}
	if c := f.TakeCalls(); len(c) != 0 {
		t.Errorf("dry run made calls %v", c)
	}

	a.DryRun = false
	res = mustApply(t, a, desired("x", "nginx"))
	f.TakeCalls()
	a.DryRun = true
	mustApply(t, a, desired("y", "nginx:2"))
	if _, err := a.Prune(context.Background(), &swarm.Result{}, true); err != nil {
		t.Fatal(err)
	}
	if c := f.TakeCalls(); len(c) != 0 {
		t.Errorf("dry run made calls %v", c)
	}
	if err := a.Wait(context.Background(), res.ServiceIDs, 0); err != nil {
		t.Errorf("dry run wait: %v", err)
	}
}

func TestExistingNetworks(t *testing.T) {
	f := swarmtest.New()
	f.Networks = []network.Summary{
		{ID: "n1", Name: "manual", Driver: "overlay"},
		{ID: "n2", Name: "bridged", Driver: "bridge", Labels: owner.Labels()},
	}
	res := mustApply(t, applier(f), swarm.Desired{Networks: []*swarm.Network{
		{Name: "manual"},
		{Name: "bridged", Spec: network.CreateOptions{Driver: "overlay"}},
	}})
	eq(t, "changes", actions(res.Changes), []string{
		"Network/manual unchanged (exists, not managed)",
		"Network/bridged unchanged (driver is bridge, want overlay; networks are immutable, delete it to recreate)",
	})
	if c := f.TakeCalls(); len(c) != 0 {
		t.Errorf("calls %v", c)
	}
}

func TestReuseUnmanagedConfig(t *testing.T) {
	// After `flux4swarm import`, identical configs created by `docker stack
	// deploy` are reused instead of rolling the services.
	f := swarmtest.New()
	f.Configs = []dswarm.Config{{ID: "stack1", Spec: dswarm.ConfigSpec{
		Annotations: dswarm.Annotations{Name: "shop_nginx", Labels: map[string]string{api.LabelStackNamespace: "shop"}},
		Data:        []byte("server {}"),
	}}}
	res := mustApply(t, applier(f), desired("server {}", "nginx"))
	if c := res.Changes[1]; c.Action != swarm.Unchanged || c.Reason != "reusing existing config shop_nginx" {
		t.Errorf("config change %s", c)
	}
	svc, _ := f.Service("shop_web")
	if ref := svc.Spec.TaskTemplate.ContainerSpec.Configs[0]; ref.ConfigID != "stack1" || ref.ConfigName != "shop_nginx" {
		t.Errorf("config reference %+v", ref)
	}
	if len(f.Configs) != 1 {
		t.Errorf("configs %v", f.Names())
	}
}

func TestReferencesToSwarmObjects(t *testing.T) {
	// References to configs and secrets that are not in the desired state
	// resolve to existing Swarm objects.
	f := swarmtest.New()
	f.Configs = []dswarm.Config{{ID: "c1", Spec: dswarm.ConfigSpec{Annotations: dswarm.Annotations{Name: "external"}}}}
	f.Secrets = []dswarm.Secret{{ID: "s1", Spec: dswarm.SecretSpec{Annotations: dswarm.Annotations{Name: "external"}}}}
	svc := func(name string) swarm.Desired {
		return swarm.Desired{Services: []*swarm.Service{{Name: "s", Spec: dswarm.ServiceSpec{TaskTemplate: dswarm.TaskSpec{
			ContainerSpec: &dswarm.ContainerSpec{
				Image:   "x",
				Configs: []*dswarm.ConfigReference{{ConfigName: name}},
				Secrets: []*dswarm.SecretReference{{SecretName: "external"}},
			}}}}}}
	}
	mustApply(t, applier(f), svc("external"))
	s, _ := f.Service("s")
	cs := s.Spec.TaskTemplate.ContainerSpec
	if cs.Configs[0].ConfigID != "c1" || cs.Secrets[0].SecretID != "s1" {
		t.Errorf("references %+v %+v", cs.Configs[0], cs.Secrets[0])
	}

	_, err := applier(swarmtest.New()).Apply(context.Background(), svc("missing"))
	if err == nil || !strings.Contains(err.Error(), `config "missing" is neither in the source nor in Swarm`) {
		t.Errorf("error %v", err)
	}
}

func TestDrift(t *testing.T) {
	cases := map[string]struct {
		mutate func(*dswarm.Service)
		reason string
	}{
		"image pinned by digest": {
			mutate: func(s *dswarm.Service) { s.Spec.TaskTemplate.ContainerSpec.Image = "nginx:1.27@sha256:abc" },
		},
		"image normalized": {
			mutate: func(s *dswarm.Service) { s.Spec.TaskTemplate.ContainerSpec.Image = "docker.io/library/nginx:1.27" },
		},
		"image changed": {
			mutate: func(s *dswarm.Service) { s.Spec.TaskTemplate.ContainerSpec.Image = "nginx:1.26" },
			reason: "image drifted to nginx:1.26",
		},
		"scaled": {
			mutate: func(s *dswarm.Service) { s.Spec.Mode = replicas(5) },
			reason: "replicas drifted",
		},
		"label removed": {
			mutate: func(s *dswarm.Service) { delete(s.Spec.Labels, "app") },
			reason: "label app changed",
		},
		"not managed yet": {
			mutate: func(s *dswarm.Service) { delete(s.Spec.Labels, api.LabelSpecHash) },
			reason: "adopting",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := swarmtest.New()
			a := applier(f)
			mustApply(t, a, desired("x", "nginx:1.27"))
			tc.mutate(&f.Services[0])
			f.TakeCalls()
			res := mustApply(t, a, desired("x", "nginx:1.27"))
			c := res.Changes[3]
			if tc.reason == "" {
				if c.Action != swarm.Unchanged {
					t.Errorf("change %s, want unchanged", c)
				}
				return
			}
			if c.Action != swarm.Configured || c.Reason != tc.reason {
				t.Errorf("change %s, want configured (%s)", c, tc.reason)
			}
			s, _ := f.Service("shop_web")
			if s.Spec.TaskTemplate.ContainerSpec.Image != "nginx:1.27" || *s.Spec.Mode.Replicated.Replicas != 2 {
				t.Errorf("not corrected: %+v", s.Spec)
			}
			// Converged now.
			if res := mustApply(t, a, desired("x", "nginx:1.27")); res.Changes[3].Action != swarm.Unchanged {
				t.Errorf("after correction: %s", res.Changes[3])
			}
		})
	}
}

func TestForceRecreatesOnUpdateFailure(t *testing.T) {
	f := swarmtest.New()
	a := applier(f)
	mustApply(t, a, desired("x", "nginx:1"))
	old, _ := f.Service("shop_web")
	a.Log = slog.New(slog.DiscardHandler)
	f.UpdateErr = errors.New("rpc error: invalid spec")
	f.TakeCalls()

	if _, err := a.Apply(context.Background(), desired("x", "nginx:2")); err == nil || !strings.Contains(err.Error(), "update service shop_web") {
		t.Fatalf("without force: %v", err)
	}

	a.Force = true
	res := mustApply(t, a, desired("x", "nginx:2"))
	if c := res.Changes[3]; c.Action != swarm.Configured || c.Reason != "recreated: spec changed" {
		t.Errorf("change %s", c)
	}
	s, _ := f.Service("shop_web")
	if s.ID == old.ID || res.ServiceIDs["shop_web"] != s.ID || s.Spec.TaskTemplate.ContainerSpec.Image != "nginx:2" {
		t.Errorf("not recreated: %s %+v", s.ID, s.Spec.TaskTemplate.ContainerSpec)
	}
	eq(t, "calls", f.TakeCalls(), []string{"ServiceRemove shop_web", "ServiceCreate shop_web"})
}

func TestApplyRejectsInvalidDesired(t *testing.T) {
	for name, d := range map[string]swarm.Desired{
		"duplicate": {Configs: []*swarm.Config{{Name: "a"}, {Name: "a"}}},
		"no name":   {Services: []*swarm.Service{{}}},
	} {
		if _, err := applier(swarmtest.New()).Apply(context.Background(), d); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSpecHashIgnoresIDs(t *testing.T) {
	// The spec hash must not depend on object IDs, which differ between
	// clusters; otherwise moving a source to another cluster or restoring a
	// backup would roll every service.
	hash := func(f *swarmtest.Fake) string {
		mustApply(t, applier(f), desired("x", "nginx"))
		s, _ := f.Service("shop_web")
		return s.Spec.Labels[api.LabelSpecHash]
	}
	shifted := swarmtest.New()
	shifted.Configs = []dswarm.Config{{ID: "cfg100", Spec: dswarm.ConfigSpec{Annotations: dswarm.Annotations{Name: "unrelated"}}}}
	if _, err := shifted.ConfigCreate(context.Background(), dswarm.ConfigSpec{Annotations: dswarm.Annotations{Name: "bump"}}); err != nil {
		t.Fatal(err)
	}
	a, b := hash(swarmtest.New()), hash(shifted)
	if a == "" || a != b {
		t.Errorf("hashes %q and %q differ", a, b)
	}
	sa, _ := shifted.Service("shop_web")
	if sa.Spec.TaskTemplate.ContainerSpec.Configs[0].ConfigID == "cfg1" {
		t.Fatal("test setup: IDs did not shift")
	}
}

func TestVersionName(t *testing.T) {
	a := swarm.VersionName("cfg", []byte("data"), []byte(""))
	if a != swarm.VersionName("cfg", []byte("data"), []byte("")) || !strings.HasPrefix(a, "cfg-") || len(a) != len("cfg-")+10 {
		t.Errorf("VersionName %q", a)
	}
	if a == swarm.VersionName("cfg", []byte("data"), []byte("golang")) {
		t.Error("templating does not change the version name")
	}
}

func TestSuspendedServiceIsKeptAsIs(t *testing.T) {
	f := swarmtest.New()
	a := applier(f)
	ctx := context.Background()
	job := func(image string, suspended bool) swarm.Desired {
		return swarm.Desired{Services: []*swarm.Service{{Name: "shop_migrate", Suspended: suspended, Spec: dswarm.ServiceSpec{
			TaskTemplate: dswarm.TaskSpec{ContainerSpec: &dswarm.ContainerSpec{Image: image}},
			Mode:         dswarm.ServiceMode{ReplicatedJob: &dswarm.ReplicatedJob{}},
		}}}}
	}

	// A suspended job that never ran is not created.
	res := mustApply(t, a, job("migrate:1", true))
	eq(t, "services", f.Names(), []string(nil))
	eq(t, "changes", actions(res.Changes), []string{"Service/shop_migrate unchanged (suspended)"})
	eq(t, "ids", res.ServiceIDs, map[string]string{})

	// Once resumed it runs; suspending it again neither updates nor prunes it.
	mustApply(t, a, job("migrate:1", false))
	f.TakeCalls()
	res = mustApply(t, a, job("migrate:2", true))
	if calls := f.TakeCalls(); len(calls) != 0 {
		t.Errorf("suspended job changed: %v", calls)
	}
	pruned, err := a.Prune(ctx, res, true)
	if err != nil || len(pruned) != 0 {
		t.Fatalf("pruned %v, %v", actions(pruned), err)
	}
	svc, _ := f.Service("shop_migrate")
	eq(t, "image", svc.Spec.TaskTemplate.ContainerSpec.Image, "migrate:1")
}
