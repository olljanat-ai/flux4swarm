// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package swarm

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/errdefs"
)

// fakeTasks is a tfdocker.Client with one service and its tasks.
type fakeTasks struct {
	svc   *swarm.Service
	tasks []swarm.Task
}

func (f *fakeTasks) ServiceInspectWithRaw(_ context.Context, id string, _ swarm.ServiceInspectOptions) (swarm.Service, []byte, error) {
	if f.svc == nil || (id != f.svc.ID && id != f.svc.Spec.Name) {
		return swarm.Service{}, nil, errdefs.NotFound(errNotFound{})
	}
	return *f.svc, nil, nil
}

func (f *fakeTasks) TaskList(context.Context, swarm.TaskListOptions) ([]swarm.Task, error) {
	return f.tasks, nil
}

func (f *fakeTasks) NodeList(context.Context, swarm.NodeListOptions) ([]swarm.Node, error) {
	return []swarm.Node{{ID: "n1", Status: swarm.NodeStatus{State: swarm.NodeStateReady}}}, nil
}

type errNotFound struct{}

func (errNotFound) Error() string { return "no such service" }

func u64p(v uint64) *uint64 { return &v }

func jobService(total uint64, iter uint64) *swarm.Service {
	return &swarm.Service{
		ID:   "job1",
		Meta: swarm.Meta{UpdatedAt: time.Now().Add(-time.Hour)},
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{Name: "ns_migrate"},
			Mode:        swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{TotalCompletions: u64p(total)}},
		},
		JobStatus: &swarm.JobStatus{JobIteration: swarm.Version{Index: iter}, LastExecution: time.Now().Add(-time.Hour)},
	}
}

func task(iter uint64, state swarm.TaskState, age time.Duration) swarm.Task {
	return swarm.Task{
		Meta:         swarm.Meta{UpdatedAt: time.Now().Add(-age)},
		NodeID:       "n1",
		JobIteration: &swarm.Version{Index: iter},
		Status:       swarm.TaskStatus{State: state, Err: "exit 1", Timestamp: time.Now().Add(-age)},
	}
}

func TestCheckServiceJob(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		svc     *swarm.Service
		tasks   []swarm.Task
		ready   bool
		wantErr string
	}{
		{name: "missing service", svc: nil},
		{name: "not started", svc: func() *swarm.Service { s := jobService(1, 1); s.JobStatus = nil; return s }()},
		{name: "running", svc: jobService(1, 5), tasks: []swarm.Task{task(5, swarm.TaskStateRunning, 0)}},
		{name: "complete", svc: jobService(1, 5), tasks: []swarm.Task{task(5, swarm.TaskStateComplete, time.Minute)}, ready: true},
		{name: "old iteration complete, new running", svc: jobService(1, 7),
			tasks: []swarm.Task{task(5, swarm.TaskStateComplete, time.Hour), task(7, swarm.TaskStateRunning, 0)}},
		{name: "partial completions", svc: jobService(2, 5), tasks: []swarm.Task{task(5, swarm.TaskStateComplete, 0), task(5, swarm.TaskStateRunning, 0)}},
		{name: "retrying after failure", svc: jobService(1, 5),
			tasks: []swarm.Task{task(5, swarm.TaskStateFailed, time.Minute), task(5, swarm.TaskStatePending, 0)}},
		{name: "just failed, may retry", svc: jobService(1, 5), tasks: []swarm.Task{task(5, swarm.TaskStateFailed, 0)}},
		{name: "failed for good", svc: jobService(1, 5), tasks: []swarm.Task{task(5, swarm.TaskStateFailed, time.Minute)}, wantErr: "job failed"},
		{name: "history trimmed", svc: jobService(1, 5), ready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := CheckService(ctx, &fakeTasks{svc: tc.svc, tasks: tc.tasks}, "ns_migrate")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if h.Ready != tc.ready {
				t.Fatalf("ready = %v (%s), want %v", h.Ready, h.Message, tc.ready)
			}
		})
	}
}

func TestCheckServiceReplicated(t *testing.T) {
	svc := &swarm.Service{ID: "s1", Spec: swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "ns_web"},
		Mode:        swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: u64p(1)}},
	}}
	running := swarm.Task{Slot: 1, NodeID: "n1", DesiredState: swarm.TaskStateRunning, Status: swarm.TaskStatus{State: swarm.TaskStateRunning}}
	starting := running
	starting.Status.State = swarm.TaskStateStarting
	for _, tc := range []struct {
		task  swarm.Task
		ready bool
	}{{running, true}, {starting, false}} {
		h, err := CheckService(context.Background(), &fakeTasks{svc: svc, tasks: []swarm.Task{tc.task}}, "ns_web")
		if err != nil || h.Ready != tc.ready {
			t.Errorf("task %s: ready=%v err=%v, want %v", tc.task.Status.State, h.Ready, err, tc.ready)
		}
	}
}

func TestWaitHealthy(t *testing.T) {
	n := 0
	err := WaitHealthy(context.Background(), time.Second, time.Millisecond, func(context.Context) (Health, error) {
		n++
		return Health{Ready: n == 3}, nil
	})
	if err != nil || n != 3 {
		t.Fatalf("err=%v calls=%d", err, n)
	}
	err = WaitHealthy(context.Background(), 20*time.Millisecond, time.Millisecond, func(context.Context) (Health, error) {
		return Health{Message: "1/2 replicas running"}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "1/2 replicas running") {
		t.Fatalf("err = %v, want timeout with progress", err)
	}
}
