// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package volumebackup

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

type fake struct {
	services []swarm.Service
	tasks    map[string][]swarm.Task // by service ID
	nodes    []swarm.Node
	// jobState is the state of the backup job's task.
	jobState swarm.TaskState
	created  []swarm.ServiceSpec
	removed  []string
	secrets  map[string][]byte
}

func (f *fake) ServiceList(context.Context, swarm.ServiceListOptions) ([]swarm.Service, error) {
	return f.services, nil
}
func (f *fake) ServiceCreate(_ context.Context, s swarm.ServiceSpec, _ swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error) {
	f.created = append(f.created, s)
	return swarm.ServiceCreateResponse{ID: "job"}, nil
}
func (f *fake) ServiceRemove(_ context.Context, id string) error {
	f.removed = append(f.removed, "service "+id)
	return nil
}
func (f *fake) ServiceLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("Fatal: wrong password")), nil
}
func (f *fake) TaskList(_ context.Context, o swarm.TaskListOptions) ([]swarm.Task, error) {
	id := o.Filters.Get("service")[0]
	if id == "job" {
		return []swarm.Task{{Status: swarm.TaskStatus{State: f.jobState, Err: "exit 1"}}}, nil
	}
	return f.tasks[id], nil
}
func (f *fake) NodeList(context.Context, swarm.NodeListOptions) ([]swarm.Node, error) {
	return f.nodes, nil
}
func (f *fake) SecretCreate(_ context.Context, s swarm.SecretSpec) (swarm.SecretCreateResponse, error) {
	if f.secrets == nil {
		f.secrets = map[string][]byte{}
	}
	f.secrets[s.Name] = s.Data
	return swarm.SecretCreateResponse{ID: "sec"}, nil
}
func (f *fake) SecretRemove(_ context.Context, id string) error {
	f.removed = append(f.removed, "secret "+id)
	return nil
}

func dbService(m mount.Mount) swarm.Service {
	return swarm.Service{ID: "db", Spec: swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: "shop_db", Labels: map[string]string{api.LabelStackNamespace: "shop"}},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Mounts: []mount.Mount{m}}},
	}}
}

func task(node string, state swarm.TaskState, age time.Duration) swarm.Task {
	return swarm.Task{NodeID: node, Status: swarm.TaskStatus{State: state, Timestamp: time.Now().Add(-age)}}
}

func TestPlan(t *testing.T) {
	local := mount.Mount{Type: mount.TypeVolume, Source: "shop_db", Target: "/var/lib/db",
		VolumeOptions: &mount.VolumeOptions{Subpath: "pg"}}
	nfs := mount.Mount{Type: mount.TypeVolume, Source: "shop_db", Target: "/var/lib/db",
		VolumeOptions: &mount.VolumeOptions{DriverConfig: &mount.Driver{Name: "local", Options: map[string]string{"type": "nfs"}}}}
	for _, c := range []struct {
		name    string
		m       mount.Mount
		tasks   []swarm.Task
		nodes   []swarm.Node
		pin     string
		node    string
		running bool
		err     string
	}{
		{name: "running", m: local, tasks: []swarm.Task{task("n1", swarm.TaskStateRunning, 0), task("n2", swarm.TaskStateShutdown, time.Hour)}, node: "n1", running: true},
		{name: "stopped: newest task", m: local, tasks: []swarm.Task{task("n2", swarm.TaskStateShutdown, time.Minute), task("n1", swarm.TaskStateShutdown, time.Hour)}, node: "n2"},
		{name: "scaled to 0, one node", m: local, nodes: []swarm.Node{{ID: "only"}}, node: "only"},
		{name: "scaled to 0, many nodes", m: local, nodes: []swarm.Node{{ID: "a"}, {ID: "b"}}, err: "give --node"},
		{name: "two local copies", m: local, tasks: []swarm.Task{task("n1", swarm.TaskStateRunning, 0), task("n2", swarm.TaskStateRunning, 0)}, err: "each has its own copy"},
		{name: "two nodes, shared", m: nfs, tasks: []swarm.Task{task("n2", swarm.TaskStateRunning, 0), task("n1", swarm.TaskStateRunning, 0)}, node: "n1", running: true},
		{name: "--node", m: local, pin: "worker-3", node: "worker-3"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fake{services: []swarm.Service{dbService(c.m)}, tasks: map[string][]swarm.Task{"db": c.tasks}, nodes: c.nodes}
			tg, err := Plan(context.Background(), f, "shop_db", c.pin)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tg.Namespace != "shop" || tg.Claim != "db" || tg.Node != c.node || (len(tg.Running) > 0) != c.running {
				t.Errorf("got %+v", tg)
			}
		})
	}
}

func TestPlanErrors(t *testing.T) {
	f := &fake{services: []swarm.Service{dbService(mount.Mount{Type: mount.TypeVolume, Source: "legacy", Target: "/x"})}}
	if _, err := Plan(context.Background(), f, "other", ""); err == nil || !strings.Contains(err.Error(), "no service mounts it") {
		t.Errorf("err = %v", err)
	}
	if _, err := Plan(context.Background(), f, "legacy", ""); err == nil || !strings.Contains(err.Error(), "no PersistentVolumeClaim") {
		t.Errorf("err = %v", err)
	}
}

func TestJobSpec(t *testing.T) {
	m := mount.Mount{Type: mount.TypeVolume, Source: "shop_db", Target: "/var/lib/db",
		VolumeOptions: &mount.VolumeOptions{Subpath: "pg", DriverConfig: &mount.Driver{Name: "rexray"}}}
	tg := &Target{Volume: "shop_db", Namespace: "shop", Claim: "db", Mount: m, Node: "abcdefghijklmnopqrstuvwxy"}
	s := JobSpec(tg, DefaultImage, "sec", "secname", []string{"backup"})
	got := s.TaskTemplate.ContainerSpec.Mounts[0]
	if got.Target != "/data" || !got.ReadOnly || got.VolumeOptions.Subpath != "" || got.VolumeOptions.DriverConfig.Name != "rexray" {
		t.Errorf("mount %+v", got)
	}
	if m.VolumeOptions.Subpath != "pg" {
		t.Error("JobSpec changed the service's mount")
	}
	script := s.TaskTemplate.ContainerSpec.Command[2]
	if !strings.Contains(script, "cd /data\nrestic backup --host volsync --exclude lost+found .") {
		t.Errorf("script:\n%s", script)
	}
	if s.Name != "flux4swarm-backup-shop-db" || s.Labels[api.LabelVolumeBackup] != "shop_db" || s.Mode.ReplicatedJob == nil {
		t.Errorf("spec %+v", s)
	}
	if c := s.TaskTemplate.Placement.Constraints; len(c) != 1 || c[0] != "node.id==abcdefghijklmnopqrstuvwxy" {
		t.Errorf("constraints %v", c)
	}
	if n := s.TaskTemplate.Networks; len(n) != 1 || n[0].Target != "backup" {
		t.Errorf("networks %v", n)
	}
	tg.Node = "worker-1"
	if c := JobSpec(tg, DefaultImage, "sec", "secname", nil).TaskTemplate.Placement.Constraints[0]; c != "node.hostname==worker-1" {
		t.Errorf("constraint %s", c)
	}
}

func TestEnvFile(t *testing.T) {
	got := string(EnvFile(map[string]string{"RESTIC_PASSWORD": "it's", "A": "b c"}))
	if want := "A='b c'\nRESTIC_PASSWORD='it'\\''s'\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBackup(t *testing.T) {
	newFake := func(state swarm.TaskState) *fake {
		return &fake{
			services: []swarm.Service{dbService(mount.Mount{Type: mount.TypeVolume, Source: "shop_db", Target: "/db"})},
			tasks:    map[string][]swarm.Task{"db": {task("n1", swarm.TaskStateRunning, 0)}},
			jobState: state,
		}
	}
	o := Options{Repository: "s3:https://s3.example.com/bucket/prod/", Env: map[string]string{"RESTIC_PASSWORD": "pw"}, Poll: time.Millisecond}
	f := newFake(swarm.TaskStateComplete)
	r, err := Backup(context.Background(), f, "shop_db", o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Repository != "s3:https://s3.example.com/bucket/prod/shop/db" || r.Node != "n1" || len(r.Running) != 1 {
		t.Errorf("result %+v", r)
	}
	for _, data := range f.secrets {
		if s := string(data); !strings.Contains(s, "RESTIC_REPOSITORY='s3:https://s3.example.com/bucket/prod/shop/db'") || !strings.Contains(s, "RESTIC_PASSWORD='pw'") {
			t.Errorf("secret %q", s)
		}
	}
	if strings.Join(f.removed, ",") != "service job,secret sec" {
		t.Errorf("removed %v", f.removed)
	}

	f = newFake(swarm.TaskStateFailed)
	if _, err := Backup(context.Background(), f, "shop_db", o); err == nil || !strings.Contains(err.Error(), "wrong password") {
		t.Errorf("err = %v", err)
	}
	if len(f.removed) != 2 {
		t.Errorf("failed job not cleaned up: %v", f.removed)
	}
	if _, err := Backup(context.Background(), newFake(swarm.TaskStateComplete), "shop_db", Options{Repository: "x"}); err == nil {
		t.Error("no error without RESTIC_PASSWORD")
	}
}
