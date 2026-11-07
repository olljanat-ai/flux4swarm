// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package notify implements Flux's notification-controller for flux4swarm:
// Alerts that forward controller events to Providers (Slack, Microsoft
// Teams, generic webhooks, GitHub and Azure DevOps commit statuses) and
// Receivers that turn incoming webhooks into reconcile requests.
package notify

import (
	"time"
)

// Event severities, as in Flux's event API.
const (
	SeverityInfo  = "info"
	SeverityError = "error"
)

// Event metadata keys (event.toolkit.fluxcd.io/v1). The controller sets them
// prefixed with the API group of the involved object, for example
// "kustomize.toolkit.fluxcd.io/revision"; the prefix is removed before the
// event reaches a Provider, like notification-controller does.
const (
	MetaRevisionKey       = "revision"
	MetaOriginRevisionKey = "originRevision"
	MetaCommitKey         = "commit"
	MetaCommitStatusKey   = "commit_status"
	MetaCommitStatusValue = "update"
	MetaTokenKey          = "token"
	MetaSummaryKey        = "summary"

	// EventGroup prefixes metadata set through annotations on the
	// involved object ("event.toolkit.fluxcd.io/<key>").
	EventGroup = "event.toolkit.fluxcd.io"
)

// Reasons and actions used by events.
const (
	ReasonProgressing = "Progressing"

	ActionReconciling = "Reconciling"
	ActionReconciled  = "Reconciled"
	ActionFailed      = "Failed"
)

// ObjectReference is the involved object of an event (the subset of
// corev1.ObjectReference that Flux fills).
type ObjectReference struct {
	Kind            string `json:"kind,omitempty"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name,omitempty"`
	UID             string `json:"uid,omitempty"`
	APIVersion      string `json:"apiVersion,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// Group returns the API group of the object.
func (o ObjectReference) Group() string {
	for i := 0; i < len(o.APIVersion); i++ {
		if o.APIVersion[i] == '/' {
			return o.APIVersion[:i]
		}
	}
	return ""
}

// Event has the JSON shape of Flux's event.toolkit.fluxcd.io/v1 Event, so
// generic webhook receivers written for Flux work unchanged.
type Event struct {
	InvolvedObject      ObjectReference   `json:"involvedObject"`
	Severity            string            `json:"severity"`
	Timestamp           time.Time         `json:"timestamp"`
	Message             string            `json:"message"`
	Reason              string            `json:"reason"`
	Action              string            `json:"action"`
	Metadata            map[string]string `json:"metadata,omitempty"`
	ReportingController string            `json:"reportingController"`
	ReportingInstance   string            `json:"reportingInstance,omitempty"`
}

// Revision returns the commit or revision metadata of a dispatched event.
func (e *Event) Revision() (string, bool) {
	for _, k := range []string{MetaCommitKey, MetaOriginRevisionKey, MetaRevisionKey} {
		if v, ok := e.Metadata[k]; ok && v != "" {
			return v, true
		}
	}
	return "", false
}

func (e *Event) clone() Event {
	c := *e
	c.Metadata = make(map[string]string, len(e.Metadata))
	for k, v := range e.Metadata {
		c.Metadata[k] = v
	}
	return c
}
