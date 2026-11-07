// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package swarm

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/errdefs"

	"github.com/olljanat-ai/flux4swarm/internal/swarm/tfdocker"
)

// JobSettle is how long a job may sit with failed tasks and no task left
// running before it counts as failed. Swarm schedules the replacement of a
// failed task (restart policy) shortly after the failure, so a short quiet
// period tells "retrying" from "gave up".
var JobSettle = 10 * time.Second

// Health is the result of one health check.
type Health struct {
	// Ready is true when the object is healthy (a service converged, a job
	// completed).
	Ready bool
	// Message describes the progress.
	Message string
}

// CheckService checks a service by ID or name. Replicated and global
// services are healthy when they converged; replicated and global jobs
// (Kubernetes Jobs) when their current iteration completed, like a Job's
// Complete condition in Kubernetes. A missing service is not healthy yet.
// A job that ran out of retries and a rolled back update are errors.
func CheckService(ctx context.Context, c tfdocker.Client, idOrName string) (Health, error) {
	svc, _, err := c.ServiceInspectWithRaw(ctx, idOrName, swarm.ServiceInspectOptions{})
	if errdefs.IsNotFound(err) {
		return Health{Message: "service not found"}, nil
	}
	if err != nil {
		return Health{}, err
	}
	if svc.Spec.Mode.ReplicatedJob != nil || svc.Spec.Mode.GlobalJob != nil {
		return checkJob(ctx, c, svc)
	}
	state, msg, err := tfdocker.CheckService(ctx, c, svc.ID)
	if err != nil {
		return Health{}, err
	}
	return Health{Ready: state == tfdocker.StateConverged, Message: msg}, nil
}

func checkJob(ctx context.Context, c tfdocker.Client, svc swarm.Service) (Health, error) {
	if svc.JobStatus == nil {
		return Health{Message: "job not started"}, nil
	}
	iter := svc.JobStatus.JobIteration.Index
	tasks, err := c.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", svc.ID))})
	if err != nil {
		return Health{}, err
	}
	var completed, live, failed, seen int
	var lastErr string
	var lastFail, lastUpdate time.Time
	nodes, doneNodes := map[string]bool{}, map[string]bool{}
	for _, t := range tasks {
		if t.JobIteration == nil || t.JobIteration.Index != iter {
			continue
		}
		seen++
		if t.UpdatedAt.After(lastUpdate) {
			lastUpdate = t.UpdatedAt
		}
		if t.NodeID != "" {
			nodes[t.NodeID] = true
		}
		switch t.Status.State {
		case swarm.TaskStateComplete:
			completed++
			doneNodes[t.NodeID] = true
		case swarm.TaskStateFailed, swarm.TaskStateRejected:
			failed++
			if t.Status.Timestamp.After(lastFail) || lastErr == "" {
				lastFail = t.Status.Timestamp
				lastErr = t.Status.Err
				if lastErr == "" {
					lastErr = t.Status.Message
				}
			}
		case swarm.TaskStateShutdown, swarm.TaskStateRemove, swarm.TaskStateOrphaned:
		default:
			live++
		}
	}
	settled := time.Since(lastUpdate) > JobSettle

	var done bool
	var msg string
	if j := svc.Spec.Mode.ReplicatedJob; j != nil {
		want := 1
		if j.TotalCompletions != nil {
			want = int(*j.TotalCompletions)
		}
		done = completed >= want
		msg = fmt.Sprintf("%d/%d completions", completed, want)
	} else {
		done = completed > 0 && live == 0 && len(doneNodes) == len(nodes)
		msg = fmt.Sprintf("completed on %d/%d nodes", len(doneNodes), len(nodes))
	}
	switch {
	case done:
		return Health{Ready: true, Message: msg}, nil
	case seen == 0 && settled && time.Since(svc.JobStatus.LastExecution) > JobSettle && time.Since(svc.UpdatedAt) > JobSettle:
		// Swarm creates the tasks of an iteration right away, so none
		// at all long after the last execution means the task history
		// was trimmed after the job finished.
		return Health{Ready: true, Message: "job ran, task history trimmed"}, nil
	case live == 0 && failed > 0 && settled:
		return Health{}, fmt.Errorf("job failed (%s, %d failed task(s)): %s", msg, failed, lastErr)
	}
	return Health{Message: msg}, nil
}

// WaitHealthy polls check every interval until it reports healthy, returns
// an error, ctx is done or timeout passes.
func WaitHealthy(ctx context.Context, timeout, interval time.Duration, check func(context.Context) (Health, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	for {
		h, err := check(ctx)
		if err != nil && ctx.Err() == nil {
			return err
		}
		if err == nil {
			if h.Ready {
				return nil
			}
			last = h.Message
		}
		select {
		case <-ctx.Done():
			if last == "" {
				last = "not ready"
			}
			return fmt.Errorf("timeout after %s: %s", timeout.Round(time.Second), last)
		case <-time.After(interval):
		}
	}
}
