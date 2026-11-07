// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/creds"
	"github.com/olljanat-ai/flux4swarm/internal/swarm"
	"github.com/olljanat-ai/flux4swarm/internal/swarm/swarmtest"
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

func deploymentYAML(name, image string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata: {name: ` + name + `, namespace: shop}
spec:
  selector: {matchLabels: {app: ` + name + `}}
  template:
    metadata: {labels: {app: ` + name + `}}
    spec:
      containers: [{name: ` + name + `, image: "` + image + `"}]
`
}

func kustomization(name, path string, mut ...func(*api.KustomizationSpec)) *api.Kustomization {
	k := &api.Kustomization{
		TypeMeta: api.TypeMeta{APIVersion: api.KustomizeGroupVersion, Kind: api.KindKustomization},
		Metadata: api.ObjectMeta{Name: name, Namespace: api.DefaultNamespace},
		Spec: api.KustomizationSpec{
			Interval:  api.Duration{Duration: time.Minute},
			Path:      path,
			Prune:     true,
			SourceRef: api.CrossNamespaceObjectReference{Kind: LocalSourceKind, Name: "local"},
		},
	}
	for _, m := range mut {
		m(&k.Spec)
	}
	return k
}

type env struct {
	t      *testing.T
	root   string
	docker *swarmtest.Fake
	c      *Controller
}

func newEnv(t *testing.T, mut ...func(*Options)) *env {
	t.Helper()
	e := &env{t: t, root: t.TempDir(), docker: swarmtest.New()}
	o := Options{
		Docker:   e.docker,
		Creds:    creds.New(t.TempDir()),
		CacheDir: t.TempDir(),
		Log:      slog.New(slog.DiscardHandler),
	}
	for _, m := range mut {
		m(&o)
	}
	e.c = New(o)
	e.c.AddLocalSource("local", e.root)
	return e
}

func (e *env) status(kind, name string) ObjectStatus {
	e.t.Helper()
	for _, s := range e.c.Status() {
		if s.Kind == kind && s.Name == name {
			return s
		}
	}
	e.t.Fatalf("no status for %s %s", kind, name)
	return ObjectStatus{}
}

func (e *env) reconcile() error {
	return e.c.ReconcileOnce(context.Background())
}

func changed(changes []swarm.Change) []string {
	var out []string
	for _, c := range changes {
		if c.Action != swarm.Unchanged {
			out = append(out, c.String())
		}
	}
	return out
}

func TestReconcileAppliesAndConverges(t *testing.T) {
	e := newEnv(t)
	write(t, e.root, "apps/web.yaml", deploymentYAML("web", "nginx:1.27"))
	e.c.AddKustomization(kustomization("apps", "./apps"))
	if err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	st := e.status(api.KindKustomization, "apps")
	if !st.IsReady() || st.Ready.Reason != ReasonSucceeded || st.LastAppliedRevision != "local@"+e.root {
		t.Errorf("status %+v", st)
	}
	if strings.Join(st.Inventory, ",") != "Network/shop_default,Service/shop_web" {
		t.Errorf("inventory %v", st.Inventory)
	}
	svc, ok := e.docker.Service("shop_web")
	if !ok || svc.Spec.Labels[api.LabelKustomizeName] != "apps" || svc.Spec.Labels[api.LabelKustomizeNamespace] != api.DefaultNamespace {
		t.Fatalf("service %+v", svc.Spec.Labels)
	}
	if src := e.status(LocalSourceKind, "local"); !src.IsReady() {
		t.Errorf("source %+v", src)
	}
	e.docker.TakeCalls()

	if err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if c := changed(e.status(api.KindKustomization, "apps").LastChanges); len(c) != 0 {
		t.Errorf("second reconcile changed %v", c)
	}
	if c := e.docker.TakeCalls(); len(c) != 0 {
		t.Errorf("second reconcile made calls %v", c)
	}

	// A new image in the source updates the service.
	write(t, e.root, "apps/web.yaml", deploymentYAML("web", "nginx:1.28"))
	if err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	if c := changed(e.status(api.KindKustomization, "apps").LastChanges); strings.Join(c, ",") != "Service/shop_web configured (spec changed)" {
		t.Errorf("changes %v", c)
	}
}

func TestReconcilePrunes(t *testing.T) {
	for _, prune := range []bool{true, false} {
		e := newEnv(t)
		write(t, e.root, "apps/web.yaml", deploymentYAML("web", "nginx"))
		write(t, e.root, "apps/api.yaml", deploymentYAML("api", "api"))
		e.c.AddKustomization(kustomization("apps", "./apps", func(s *api.KustomizationSpec) { s.Prune = prune }))
		if err := e.reconcile(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(e.root, "apps/api.yaml")); err != nil {
			t.Fatal(err)
		}
		if err := e.reconcile(); err != nil {
			t.Fatal(err)
		}
		_, exists := e.docker.Service("shop_api")
		if exists == prune {
			t.Errorf("prune=%v: service exists=%v", prune, exists)
		}
		if prune {
			if c := changed(e.status(api.KindKustomization, "apps").LastChanges); strings.Join(c, ",") != "Service/shop_api deleted" {
				t.Errorf("changes %v", c)
			}
		}
	}
}

func TestReconcileFailures(t *testing.T) {
	cases := map[string]struct {
		files  map[string]string
		ks     *api.Kustomization
		reason string
		msg    string
	}{
		"missing path": {
			ks:     kustomization("apps", "./nope"),
			reason: ReasonBuildFailed,
		},
		"untranslatable workload": {
			files: map[string]string{"apps/a.yaml": `apiVersion: apps/v1
kind: Deployment
metadata: {name: a, namespace: shop}
spec:
  selector: {matchLabels: {app: a}}
  template: {metadata: {labels: {app: a}}, spec: {containers: [{name: a, image: a}, {name: b, image: b}]}}
`},
			ks:     kustomization("apps", "./apps"),
			reason: ReasonBuildFailed,
			msg:    "pods with 2 containers",
		},
		"missing source": {
			ks: kustomization("apps", "./apps", func(s *api.KustomizationSpec) {
				s.SourceRef = api.CrossNamespaceObjectReference{Kind: api.KindGitRepository, Name: "nope"}
			}),
			reason: ReasonArtifactFailed,
			msg:    "source GitRepository 'nope' not found",
		},
		"missing substituteFrom": {
			files: map[string]string{"apps/a.yaml": deploymentYAML("a", "a")},
			ks: kustomization("apps", "./apps", func(s *api.KustomizationSpec) {
				s.PostBuild = &api.PostBuild{SubstituteFrom: []api.SubstituteReference{{Kind: "Secret", Name: "vars"}}}
			}),
			reason: ReasonBuildFailed,
			msg:    "substituteFrom Secret vars",
		},
		"missing dependency": {
			files: map[string]string{"apps/a.yaml": deploymentYAML("a", "a")},
			ks: kustomization("apps", "./apps", func(s *api.KustomizationSpec) {
				s.DependsOn = []api.DependencyReference{{Name: "infra"}}
			}),
			reason: ReasonDependencyNotReady,
			msg:    "dependency 'flux-system/infra' not found",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			for p, c := range tc.files {
				write(t, e.root, p, c)
			}
			e.c.AddKustomization(tc.ks)
			err := e.reconcile()
			if err == nil || !strings.Contains(err.Error(), "not ready") {
				t.Errorf("ReconcileOnce error %v", err)
			}
			st := e.status(api.KindKustomization, "apps")
			if st.Ready.Status != ConditionFalse || st.Ready.Reason != tc.reason || !strings.Contains(st.Ready.Message, tc.msg) {
				t.Errorf("ready %+v, want reason %s message containing %q", st.Ready, tc.reason, tc.msg)
			}
			if n := e.docker.Names(); len(n) != 0 {
				t.Errorf("objects created: %v", n)
			}
		})
	}
}

func TestSubstituteFromCredentials(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "vars/TAG", "1.29")
	e := newEnv(t, func(o *Options) { o.Creds = creds.New(dir) })
	write(t, e.root, "apps/web.yaml", deploymentYAML("web", "nginx:${TAG}"))
	e.c.AddKustomization(kustomization("apps", "./apps", func(s *api.KustomizationSpec) {
		s.PostBuild = &api.PostBuild{SubstituteFrom: []api.SubstituteReference{
			{Kind: "Secret", Name: "vars"},
			{Kind: "ConfigMap", Name: "absent", Optional: true},
		}}
	}))
	if err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	svc, _ := e.docker.Service("shop_web")
	if img := svc.Spec.TaskTemplate.ContainerSpec.Image; img != "nginx:1.29" {
		t.Errorf("image %s", img)
	}
}

func TestDependsOnOrder(t *testing.T) {
	e := newEnv(t)
	write(t, e.root, "infra/db.yaml", deploymentYAML("db", "postgres"))
	write(t, e.root, "apps/web.yaml", deploymentYAML("web", "nginx"))
	// "apps" sorts first but depends on "infra".
	e.c.AddKustomization(kustomization("apps", "./apps", func(s *api.KustomizationSpec) {
		s.DependsOn = []api.DependencyReference{{Name: "infra"}}
	}))
	e.c.AddKustomization(kustomization("infra", "./infra"))
	if err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	calls := e.docker.TakeCalls()
	db, web := -1, -1
	for i, c := range calls {
		switch c {
		case "ServiceCreate shop_db":
			db = i
		case "ServiceCreate shop_web":
			web = i
		}
	}
	if db < 0 || web < 0 || db > web {
		t.Errorf("calls %v: infra must be applied before apps", calls)
	}
}

func TestSuspended(t *testing.T) {
	e := newEnv(t)
	write(t, e.root, "apps/web.yaml", deploymentYAML("web", "nginx"))
	e.c.AddKustomization(kustomization("apps", "./apps", func(s *api.KustomizationSpec) { s.Suspend = true }))
	if err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	st := e.status(api.KindKustomization, "apps")
	if !st.Suspended || st.Ready.Reason != ReasonSuspended {
		t.Errorf("status %+v", st)
	}
	if n := e.docker.Names(); len(n) != 0 {
		t.Errorf("suspended Kustomization applied %v", n)
	}
}

func TestDryRun(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.DryRun = true })
	write(t, e.root, "apps/web.yaml", deploymentYAML("web", "nginx"))
	e.c.AddKustomization(kustomization("apps", "./apps"))
	if err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	st := e.status(api.KindKustomization, "apps")
	if !strings.HasPrefix(st.Ready.Message, "Dry run for revision") {
		t.Errorf("message %q", st.Ready.Message)
	}
	if c := changed(st.LastChanges); len(c) != 2 {
		t.Errorf("changes %v", c)
	}
	if c := e.docker.TakeCalls(); len(c) != 0 {
		t.Errorf("dry run made calls %v", c)
	}
}

func TestNestedKustomizations(t *testing.T) {
	e := newEnv(t)
	child := func(name string) string {
		return `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: ` + name + `, namespace: flux-system}
spec:
  interval: 1m
  path: ./` + name + `
  prune: true
  sourceRef: {kind: LocalDirectory, name: local}
`
	}
	write(t, e.root, "clusters/apps.yaml", child("apps"))
	write(t, e.root, "clusters/infra.yaml", child("infra"))
	write(t, e.root, "apps/web.yaml", deploymentYAML("web", "nginx"))
	write(t, e.root, "infra/db.yaml", deploymentYAML("db", "postgres"))
	e.c.AddKustomization(kustomization("root", "./clusters"))
	if err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	root := ksKey(api.DefaultNamespace, "root")
	for _, name := range []string{"apps", "infra"} {
		st := e.status(api.KindKustomization, name)
		if !st.IsReady() || st.ManagedBy != root {
			t.Errorf("%s: %+v", name, st)
		}
	}
	if _, ok := e.docker.Service("shop_web"); !ok {
		t.Fatalf("child not applied: %v", e.docker.Names())
	}

	// Removing a child from the source deletes it and garbage-collects
	// its Swarm objects; the sibling stays.
	if err := os.Remove(filepath.Join(e.root, "clusters/apps.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := e.reconcile(); err != nil {
		t.Fatal(err)
	}
	for _, s := range e.c.Status() {
		if s.Name == "apps" {
			t.Errorf("child still registered: %+v", s)
		}
	}
	if _, ok := e.docker.Service("shop_web"); ok {
		t.Errorf("child's service not garbage-collected: %v", e.docker.Names())
	}
	if _, ok := e.docker.Service("shop_db"); !ok {
		t.Errorf("sibling's service removed: %v", e.docker.Names())
	}
}
