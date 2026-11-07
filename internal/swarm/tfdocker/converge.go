// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// SPDX-License-Identifier: MPL-2.0
//
// Derived from kreuzwerker/terraform-provider-docker,
// internal/provider/resource_docker_service_funcs.go
// (https://github.com/kreuzwerker/terraform-provider-docker).
// Changes: removed the Terraform SDK (schema.ResourceData, retry.StateChangeConf)
// and replaced it with a plain polling loop driven by context; the
// convergence logic (task-by-slot tracking, update/rollback state machine,
// numbered task states) is unchanged.

// Package tfdocker holds Docker Swarm helpers adapted from
// terraform-provider-docker. Files in this package are MPL-2.0.
package tfdocker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
)

// Client is the Docker API subset used for convergence checks.
type Client interface {
	ServiceInspectWithRaw(ctx context.Context, serviceID string, options swarm.ServiceInspectOptions) (swarm.Service, []byte, error)
	TaskList(ctx context.Context, options swarm.TaskListOptions) ([]swarm.Task, error)
	NodeList(ctx context.Context, options swarm.NodeListOptions) ([]swarm.Node, error)
}

// DidNotConvergeError is the error returned when a the service does not converge in
// the defined time
type DidNotConvergeError struct {
	ServiceID string
	Timeout   time.Duration
	Err       error
}

// Error the custom error if a service does not converge
func (err *DidNotConvergeError) Error() string {
	if err.Err != nil {
		return err.Err.Error()
	}
	return "Service with ID (" + err.ServiceID + ") did not converge after " + err.Timeout.String()
}

// State is the result of one convergence check.
type State string

const (
	StatePending   State = "pending"
	StateConverged State = "converged"
)

// CheckService returns whether the service has converged: an update (if
// any) completed and all replicas are running. Rollbacks and paused updates
// are returned as errors. Global and job services are considered converged
// when every desired task is running or complete.
func CheckService(ctx context.Context, client Client, serviceID string) (State, string, error) {
	service, _, err := client.ServiceInspectWithRaw(ctx, serviceID, swarm.ServiceInspectOptions{})
	if err != nil {
		return "", "", err
	}

	rollback := false
	if service.UpdateStatus != nil {
		switch service.UpdateStatus.State {
		case swarm.UpdateStateUpdating:
			rollback = false
		case swarm.UpdateStateCompleted:
			// fall through to the task check below
		case swarm.UpdateStateRollbackStarted:
			rollback = true
		case swarm.UpdateStateRollbackCompleted:
			return "", "", fmt.Errorf("service rollback completed: %s", service.UpdateStatus.Message)
		case swarm.UpdateStatePaused:
			return "", "", fmt.Errorf("service update paused: %s", service.UpdateStatus.Message)
		case swarm.UpdateStateRollbackPaused:
			return "", "", fmt.Errorf("service rollback paused: %s", service.UpdateStatus.Message)
		}
	}

	f := filters.NewArgs()
	f.Add("service", serviceID)
	f.Add("desired-state", "running")
	tasks, err := client.TaskList(ctx, swarm.TaskListOptions{Filters: f})
	if err != nil {
		return "", "", err
	}
	activeNodes, err := getActiveNodes(ctx, client)
	if err != nil {
		return "", "", err
	}

	if service.Spec.Mode.Replicated == nil || service.Spec.Mode.Replicated.Replicas == nil {
		// Not covered by the upstream updater: treat global services as
		// converged when every task on an active node runs.
		for _, t := range tasks {
			if _, ok := activeNodes[t.NodeID]; t.NodeID != "" && !ok {
				continue
			}
			if t.Status.State != swarm.TaskStateRunning {
				return StatePending, fmt.Sprintf("task %s is %s", t.ID, t.Status.State), nil
			}
		}
		return StateConverged, "", nil
	}

	u := &replicatedConsoleLogUpdater{}
	done, running, replicas := u.update(&service, tasks, activeNodes)
	msg := fmt.Sprintf("%d/%d replicas running", running, replicas)
	if done {
		if rollback {
			return "", "", fmt.Errorf("service rollback completed: %s", service.UpdateStatus.Message)
		}
		if service.UpdateStatus != nil && service.UpdateStatus.State == swarm.UpdateStateUpdating {
			return StatePending, "update in progress, " + msg, nil
		}
		return StateConverged, msg, nil
	}
	return StatePending, msg, nil
}

// WaitForService polls CheckService until the service converges, ctx is
// done or timeout passes.
func WaitForService(ctx context.Context, client Client, serviceID string, timeout, interval time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if interval <= 0 {
		interval = 2 * time.Second
	}
	last := ""
	for {
		state, msg, err := CheckService(ctx, client, serviceID)
		if err != nil && ctx.Err() == nil {
			return err
		}
		if state == StateConverged {
			return nil
		}
		if msg != last {
			slog.Debug("waiting for service", "service", serviceID, "progress", msg)
			last = msg
		}
		select {
		case <-ctx.Done():
			return &DidNotConvergeError{ServiceID: serviceID, Timeout: timeout, Err: fmt.Errorf("service %s did not converge after %s: %s", serviceID, timeout, last)}
		case <-time.After(interval):
		}
	}
}

// getActiveNodes gets the actives nodes within a swarm
func getActiveNodes(ctx context.Context, client Client) (map[string]struct{}, error) {
	nodes, err := client.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		return nil, err
	}

	activeNodes := make(map[string]struct{})
	for _, n := range nodes {
		if n.Status.State != swarm.NodeStateDown {
			activeNodes[n.ID] = struct{}{}
		}
	}
	return activeNodes, nil
}

// replicatedConsoleLogUpdater console log updater for replicated services
type replicatedConsoleLogUpdater struct {
	// used for mapping slots to a contiguous space
	// this also causes progress bars to appear in order
	slotMap map[int]int

	initialized bool
	done        bool
}

// update is the concrete implementation of updating replicated services
func (u *replicatedConsoleLogUpdater) update(service *swarm.Service, tasks []swarm.Task, activeNodes map[string]struct{}) (bool, uint64, uint64) {
	replicas := *service.Spec.Mode.Replicated.Replicas

	if !u.initialized {
		u.slotMap = make(map[int]int)
		u.initialized = true
	}

	// get the task for each slot. there can be multiple slots on one node
	tasksBySlot := u.tasksBySlot(tasks, activeNodes)

	// if a converged state is reached, check if is still converged.
	if u.done {
		for _, task := range tasksBySlot {
			if task.Status.State != swarm.TaskStateRunning {
				u.done = false
				break
			}
		}
	}

	running := uint64(0)

	// map the slots to keep track of their state individually
	for _, task := range tasksBySlot {
		mappedSlot := u.slotMap[task.Slot]
		if mappedSlot == 0 {
			mappedSlot = len(u.slotMap) + 1
			u.slotMap[task.Slot] = mappedSlot
		}

		// if a task is in the desired state count it as running
		if !terminalState(task.DesiredState) && task.Status.State == swarm.TaskStateRunning {
			running++
		}
	}

	// check if all tasks the same amount of tasks is running than replicas defined
	if !u.done && running == replicas {
		u.done = true
	}

	return running == replicas, running, replicas
}

// tasksBySlot maps the tasks to slots on active nodes. There can be multiple slots on active nodes.
// A task is analogous to a “slot” where (on a node) the scheduler places a container.
func (u *replicatedConsoleLogUpdater) tasksBySlot(tasks []swarm.Task, activeNodes map[string]struct{}) map[int]swarm.Task {
	// if there are multiple tasks with the same slot number, favor the one
	// with the *lowest* desired state. This can happen in restart
	// scenarios.
	tasksBySlot := make(map[int]swarm.Task)
	for _, task := range tasks {
		if numberedStates[task.DesiredState] == 0 || numberedStates[task.Status.State] == 0 {
			continue
		}
		if existingTask, ok := tasksBySlot[task.Slot]; ok {
			if numberedStates[existingTask.DesiredState] < numberedStates[task.DesiredState] {
				continue
			}
			// if the desired states match, observed state breaks
			// ties. This can happen with the "start first" service
			// update mode.
			if numberedStates[existingTask.DesiredState] == numberedStates[task.DesiredState] &&
				numberedStates[existingTask.Status.State] <= numberedStates[task.Status.State] {
				continue
			}
		}
		// if the task is on a node and this node is active, then map this task to a slot
		if task.NodeID != "" {
			if _, nodeActive := activeNodes[task.NodeID]; !nodeActive {
				continue
			}
		}
		tasksBySlot[task.Slot] = task
	}

	return tasksBySlot
}

// terminalState determines if the given state is a terminal state
// meaning 'higher' than running (see numberedStates)
func terminalState(state swarm.TaskState) bool {
	return numberedStates[state] > numberedStates[swarm.TaskStateRunning]
}

//////// States

// numberedStates are ascending sorted states for docker tasks
// meaning they appear internally in this order in the statemachine
var (
	numberedStates = map[swarm.TaskState]int64{
		swarm.TaskStateNew:       1,
		swarm.TaskStateAllocated: 2,
		swarm.TaskStatePending:   3,
		swarm.TaskStateAssigned:  4,
		swarm.TaskStateAccepted:  5,
		swarm.TaskStatePreparing: 6,
		swarm.TaskStateReady:     7,
		swarm.TaskStateStarting:  8,
		swarm.TaskStateRunning:   9,

		// The following states are not actually shown in progress
		// output, but are used internally for ordering.
		swarm.TaskStateComplete: 10,
		swarm.TaskStateShutdown: 11,
		swarm.TaskStateFailed:   12,
		swarm.TaskStateRejected: 13,
	}
)
