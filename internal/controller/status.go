// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"time"

	"github.com/olljanat-ai/flux4swarm/internal/swarm"
)

// Condition statuses and reasons, following Flux.
const (
	ConditionTrue    = "True"
	ConditionFalse   = "False"
	ConditionUnknown = "Unknown"

	ReasonSucceeded           = "ReconciliationSucceeded"
	ReasonFailed              = "ReconciliationFailed"
	ReasonProgressing         = "Progressing"
	ReasonDependencyNotReady  = "DependencyNotReady"
	ReasonArtifactFailed      = "ArtifactFailed"
	ReasonBuildFailed         = "BuildFailed"
	ReasonHealthCheckFailed   = "HealthCheckFailed"
	ReasonGitOperationFailed  = "GitOperationFailed"
	ReasonSuspended           = "Suspended"
	ReasonSecretResolveFailed = "ExternalSecretFailed"
)

// Condition is a Flux-style Ready condition.
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	Message            string    `json:"message"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// ObjectStatus is the observed state of a GitRepository or Kustomization,
// similar to what `flux get` shows.
type ObjectStatus struct {
	Kind                  string         `json:"kind"`
	Namespace             string         `json:"namespace"`
	Name                  string         `json:"name"`
	Suspended             bool           `json:"suspended"`
	Ready                 Condition      `json:"ready"`
	Revision              string         `json:"revision,omitempty"`
	LastAppliedRevision   string         `json:"lastAppliedRevision,omitempty"`
	LastAttemptedRevision string         `json:"lastAttemptedRevision,omitempty"`
	LastReconcile         time.Time      `json:"lastReconcile,omitempty"`
	Duration              float64        `json:"durationSeconds,omitempty"`
	Inventory             []string       `json:"inventory,omitempty"`
	LastChanges           []swarm.Change `json:"lastChanges,omitempty"`
	Warnings              []string       `json:"warnings,omitempty"`
	Ignored               int            `json:"ignoredObjects,omitempty"`
	ManagedBy             string         `json:"managedBy,omitempty"`
	// WebhookPath of a Receiver.
	WebhookPath string `json:"webhookPath,omitempty"`
}

func (s *ObjectStatus) setReady(status, reason, msg string) {
	if s.Ready.Status != status || s.Ready.Reason != reason {
		s.Ready.LastTransitionTime = time.Now().UTC()
	}
	s.Ready.Type = "Ready"
	s.Ready.Status, s.Ready.Reason, s.Ready.Message = status, reason, msg
}

// IsReady reports Ready=True.
func (s ObjectStatus) IsReady() bool { return s.Ready.Status == ConditionTrue }
