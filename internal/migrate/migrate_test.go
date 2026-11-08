// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/importer"
	"github.com/olljanat-ai/flux4swarm/internal/volumebackup"
)

type fake struct {
	services []swarm.Service
	created  []swarm.ServiceSpec
	updates  int
}

func (f *fake) ServiceList(context.Context, swarm.ServiceListOptions) ([]swarm.Service, error) {
	return append([]swarm.Service(nil), f.services...), nil
}
func (f *fake) ConfigList(context.Context, swarm.ConfigListOptions) ([]swarm.Config, error) {
	return nil, nil
}
func (f *fake) ConfigInspectWithRaw(context.Context, string) (swarm.Config, []byte, error) {
	return swarm.Config{}, nil, nil
}
func (f *fake) SecretList(context.Context, swarm.SecretListOptions) ([]swarm.Secret, error) {
	return nil, nil
}
func (f *fake) NetworkList(context.Context, network.ListOptions) ([]network.Summary, error) {
	return nil, nil
}
func (f *fake) ServiceCreate(_ context.Context, s swarm.ServiceSpec, _ swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error) {
	f.created = append(f.created, s)
	return swarm.ServiceCreateResponse{ID: "job"}, nil
}
func (f *fake) ServiceRemove(context.Context, string) error { return nil }
func (f *fake) ServiceLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *fake) TaskList(_ context.Context, o swarm.TaskListOptions) ([]swarm.Task, error) {
	id := o.Filters.Get("service")[0]
	if id == "job" {
		return []swarm.Task{{Status: swarm.TaskStatus{State: swarm.TaskStateComplete}}}, nil
	}
	for _, s := range f.services {
		if s.ID == id && *s.Spec.Mode.Replicated.Replicas > 0 {
			return []swarm.Task{{NodeID: "n1", Status: swarm.TaskStatus{State: swarm.TaskStateRunning, Timestamp: time.Now()}}}, nil
		}
	}
	return []swarm.Task{{NodeID: "n1", Status: swarm.TaskStatus{State: swarm.TaskStateShutdown, Timestamp: time.Now()}}}, nil
}
func (f *fake) NodeList(context.Context, swarm.NodeListOptions) ([]swarm.Node, error) {
	return []swarm.Node{{ID: "n1"}}, nil
}
func (f *fake) SecretCreate(context.Context, swarm.SecretSpec) (swarm.SecretCreateResponse, error) {
	return swarm.SecretCreateResponse{ID: "sec"}, nil
}
func (f *fake) SecretRemove(context.Context, string) error { return nil }
func (f *fake) ServiceUpdate(_ context.Context, id string, _ swarm.Version, spec swarm.ServiceSpec, _ swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error) {
	f.updates++
	for i := range f.services {
		if f.services[i].ID == id {
			f.services[i].Spec = spec
		}
	}
	return swarm.ServiceUpdateResponse{}, nil
}

var _ Client = (*fake)(nil)

func service(id, name string, replicas uint64, labels map[string]string, mounts ...mount.Mount) swarm.Service {
	l := map[string]string{api.LabelStackNamespace: "shop"}
	for k, v := range labels {
		l[k] = v
	}
	return swarm.Service{ID: id, Spec: swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: name, Labels: l},
		Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "alpine:3.22", Mounts: mounts}},
	}}
}

func options(log *bytes.Buffer) Options {
	return Options{
		Export: importer.Options{Cluster: "dev", VolSync: true, VolSyncSchedule: "*/15 * * * *"},
		Backup: volumebackup.Options{Repository: "rest:http://repo:8000/", Env: map[string]string{"RESTIC_PASSWORD": "pw"}, Poll: time.Millisecond},
		Poll:   time.Millisecond,
		Log:    slog.New(slog.NewTextHandler(log, nil)),
		Now:    func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) },
	}
}

func write(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for f, b := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRun(t *testing.T) {
	vol := mount.Mount{Type: mount.TypeVolume, Source: "shop_db", Target: "/var/lib/db"}
	f := &fake{services: []swarm.Service{
		service("db", "shop_db", 2, map[string]string{api.LabelMigrate: api.MigrateKubernetes}, vol),
		service("web", "shop_web", 1, nil),
	}}
	var log bytes.Buffer
	root := t.TempDir()
	ctx := context.Background()
	if need, _ := NeedsRun(ctx, f, nil); !need {
		t.Error("NeedsRun = false with a labelled service")
	}
	r, err := Run(ctx, f, root, options(&log))
	if err != nil {
		t.Fatal(err)
	}
	dir := "clusters/dev/migrated/shop/db"
	if r.New["shop_db"].Dir != dir || !r.Migrated["shop_db"] || r.Migrated["shop_web"] {
		t.Fatalf("result %+v", r)
	}
	// Stopped on Swarm, with its replica count kept.
	db := f.services[0].Spec
	if *db.Mode.Replicated.Replicas != 0 || db.Labels[labelReplicas] != "2" {
		t.Errorf("db not stopped: replicas %d, labels %v", *db.Mode.Replicated.Replicas, db.Labels)
	}
	// The final backup.
	if len(f.created) != 1 || f.created[0].Name != "flux4swarm-backup-shop-db" {
		t.Errorf("created %v", f.created)
	}
	for _, want := range []string{"migration 1/4: started", "migration 2/4: stopped on Swarm", "migration 3/4: final backup done", "migration 4/4: handed over to Kubernetes in Git"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log misses %q:\n%s", want, log.String())
		}
	}
	get := func(f string) string {
		t.Helper()
		b, ok := r.Files[f]
		if !ok {
			var names []string
			for n := range r.Files {
				names = append(names, n)
			}
			t.Fatalf("no %s in %v", f, names)
		}
		return string(b)
	}
	if s := get(dir + "/restore/replicationdestination-db.yaml"); !strings.Contains(s, "manual: final") {
		t.Errorf("restore not final:\n%s", s)
	}
	get(dir + "/restore/persistentvolumeclaim-db.yaml")
	get(dir + "/restore/externalsecret-db-volsync-restic.yaml")
	if s := get(dir + "/app/deployment-db.yaml"); !strings.Contains(s, "replicas: 2") || strings.Contains(s, api.LabelMigrate) || strings.Contains(s, api.AnnotationHeld) {
		t.Errorf("app deployment:\n%s", s)
	}
	ks := get(dir + "/flux.yaml")
	for _, want := range []string{"name: migrate-shop-db-restore", "path: ./" + dir + "/restore", "prune: false",
		"status.lastManualSync == spec.trigger.manual", "dependsOn:", "path: ./" + dir + "/app", api.AnnotationIgnore} {
		if !strings.Contains(ks, want) {
			t.Errorf("flux.yaml misses %q:\n%s", want, ks)
		}
	}
	st := get(dir + "/" + StatusFile)
	for _, want := range []string{"- **Swarm service:** `shop_db`", "- **Final backup:** volume `shop_db` to `rest:http://repo:8000/shop/db`", "## Roll back to Swarm", "docker service scale shop_db=2", "--label-rm flux4swarm.io/migrate", "scale deployment/db --replicas 0"} {
		if !strings.Contains(st, want) {
			t.Errorf("%s misses %q:\n%s", StatusFile, want, st)
		}
	}
	if s := get("clusters/dev/migrated/kustomization.yaml"); !strings.Contains(s, "- shop/db") || len(r.Owned) != 1 {
		t.Errorf("migrated/kustomization.yaml:\n%s", s)
	}
	for f := range r.Files {
		if strings.Contains(f, "/web") {
			t.Errorf("unlabelled service written: %s", f)
		}
	}

	// In Git now: recorded, not cut over again, and kept stopped.
	write(t, root, r.Files)
	if need, _ := NeedsRun(ctx, f, r.Migrated); need {
		t.Error("NeedsRun = true with nothing to do")
	}
	one := uint64(1)
	f.services[0].Spec.Mode.Replicated.Replicas = &one
	if need, _ := NeedsRun(ctx, f, r.Migrated); !need {
		t.Error("NeedsRun = false with a migrated service running")
	}
	log.Reset()
	r2, err := Run(ctx, f, root, options(&log))
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.New) != 0 || !r2.Migrated["shop_db"] || len(f.created) != 1 {
		t.Errorf("second run %+v, created %d", r2, len(f.created))
	}
	if *f.services[0].Spec.Mode.Replicated.Replicas != 0 || !strings.Contains(log.String(), "started on Swarm again") {
		t.Errorf("migrated service not stopped again:\n%s", log.String())
	}
}

func TestRunPending(t *testing.T) {
	f := &fake{services: []swarm.Service{service("db", "shop_db", 0, map[string]string{api.LabelMigrate: api.MigrateKubernetes, labelReplicas: "2"})}}
	var log bytes.Buffer
	o := options(&log)
	o.Pending = map[string]Cutover{"shop_db": {Dir: "clusters/dev/migrated/shop/db", Files: map[string][]byte{"clusters/dev/migrated/shop/db/MIGRATED.md": []byte("x")}}}
	r, err := Run(context.Background(), f, t.TempDir(), o)
	if err != nil {
		t.Fatal(err)
	}
	if f.updates != 0 || string(r.Files["clusters/dev/migrated/shop/db/MIGRATED.md"]) != "x" || r.New["shop_db"].Dir == "" {
		t.Errorf("pending cutover not kept: updates %d, %+v", f.updates, r)
	}
}

func TestRunRefusesBeforeStopping(t *testing.T) {
	vol := mount.Mount{Type: mount.TypeVolume, Source: "shop_db", Target: "/var/lib/db"}
	for name, change := range map[string]func(*Options){
		"no volsync":    func(o *Options) { o.Export.VolSync = false },
		"no repository": func(o *Options) { o.Backup.Repository = "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{services: []swarm.Service{service("db", "shop_db", 2, map[string]string{api.LabelMigrate: api.MigrateKubernetes}, vol)}}
			var log bytes.Buffer
			o := options(&log)
			change(&o)
			r, err := Run(context.Background(), f, t.TempDir(), o)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.New) != 0 || f.updates != 0 || !strings.Contains(log.String(), "migration failed") {
				t.Errorf("updates %d, result %+v, log:\n%s", f.updates, r, log.String())
			}
		})
	}
}

func TestRunStatelessService(t *testing.T) {
	f := &fake{services: []swarm.Service{service("web", "shop_web", 3, map[string]string{api.LabelMigrate: api.MigrateKubernetes})}}
	var log bytes.Buffer
	o := options(&log)
	o.Export.VolSync = false
	r, err := Run(context.Background(), f, t.TempDir(), o)
	if err != nil {
		t.Fatal(err)
	}
	dir := "clusters/dev/migrated/shop/web"
	if _, ok := r.Files[dir+"/restore/kustomization.yaml"]; ok {
		t.Error("restore written without volumes")
	}
	ks := string(r.Files[dir+"/flux.yaml"])
	if strings.Contains(ks, "dependsOn") || !strings.Contains(ks, "migrate-shop-web-app") {
		t.Errorf("flux.yaml:\n%s", ks)
	}
	if !strings.Contains(log.String(), "migration 3/4: no volumes to back up") {
		t.Errorf("log:\n%s", log.String())
	}
}

// The export with the migrated service handed over builds, Flux
// Kustomizations included.
func TestRunValidates(t *testing.T) {
	vol := mount.Mount{Type: mount.TypeVolume, Source: "shop_db", Target: "/var/lib/db"}
	f := &fake{services: []swarm.Service{
		service("db", "shop_db", 2, map[string]string{api.LabelMigrate: api.MigrateKubernetes}, vol),
		service("web", "shop_web", 1, nil),
	}}
	var log bytes.Buffer
	root := t.TempDir()
	o := options(&log)
	o.Export.Bootstrap = true
	r, err := Run(context.Background(), f, root, o)
	if err != nil {
		t.Fatal(err)
	}
	eo := o.Export
	eo.Migrated = r.Migrated
	res, err := importer.Export(context.Background(), f, eo)
	if err != nil {
		t.Fatal(err)
	}
	for f, b := range r.Files {
		res.Files[f] = b
	}
	res.Clean = append(res.Clean, r.Owned...)
	if err := res.WriteTo(root, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Files["clusters/dev/apps/shop/deployment-db.yaml"]; ok {
		t.Error("migrated service still in apps/")
	}
	if _, ok := res.Files["clusters/dev/apps/shop/namespace.yaml"]; !ok {
		t.Error("namespace of the migrated service left apps/")
	}
	if err := importer.Validate(root, res.Root); err != nil {
		t.Fatal(err)
	}
}

func TestMetrics(t *testing.T) {
	f := &fake{services: []swarm.Service{
		service("web", "shop_web", 3, map[string]string{api.LabelMigrate: api.MigrateKubernetes}),
		service("api", "shop_api", 1, map[string]string{api.LabelMigrate: "elsewhere"}),
	}}
	var log bytes.Buffer
	o := options(&log)
	o.Metrics = &Metrics{}
	if _, err := Run(context.Background(), f, t.TempDir(), o); err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(o.Metrics)
	want := `
# HELP flux4swarm_migration_cutovers_total Cutover attempts since start, by result (success, error).
# TYPE flux4swarm_migration_cutovers_total counter
flux4swarm_migration_cutovers_total{result="error"} 0
flux4swarm_migration_cutovers_total{result="success"} 1
# HELP flux4swarm_migration_services Swarm services by migration state: migrated (recorded in Git) or waiting (labelled, not cut over yet; a failed cutover stays waiting).
# TYPE flux4swarm_migration_services gauge
flux4swarm_migration_services{state="migrated"} 1
flux4swarm_migration_services{state="waiting"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "flux4swarm_migration_cutovers_total", "flux4swarm_migration_services"); err != nil {
		t.Error(err)
	}
	if n, _ := testutil.GatherAndCount(reg, "flux4swarm_migration_swarm_downtime_seconds"); n != 1 {
		t.Errorf("downtime series %d", n)
	}
}
