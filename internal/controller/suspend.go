// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"fmt"
	"time"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

// Errors returned by SetSuspended.
var (
	ErrNotFound        = errors.New("not found")
	ErrSuspendedInSpec = errors.New("suspended by spec.suspend in the source; change it in Git")
)

// Suspend sources:
const (
	SuspendedBySpec = "spec"
	SuspendedByAPI  = "api"
)

func (c *Controller) isSuspendedLocked(key string, spec bool) bool {
	return spec || c.suspended[key]
}

// markSuspendedLocked updates st for a suspended object and reports
// whether it is suspended.
func (c *Controller) markSuspendedLocked(key string, spec bool, st *ObjectStatus) bool {
	st.Suspended = c.isSuspendedLocked(key, spec)
	switch {
	case spec:
		st.SuspendedBy = SuspendedBySpec
		st.setReady(ConditionUnknown, ReasonSuspended, "suspended")
	case st.Suspended:
		st.SuspendedBy = SuspendedByAPI
		st.setReady(ConditionUnknown, ReasonSuspended, "suspended through the API (until resumed or restarted)")
	default:
		st.SuspendedBy = ""
	}
	return st.Suspended
}

// SetSuspended suspends or resumes a GitRepository or Kustomization, like
// `flux suspend` and `flux resume`. The suspension is kept in memory: it
// lasts until resume or a controller restart. Resuming triggers a
// reconcile of the object.
func (c *Controller) SetSuspended(kind, namespace, name string, suspend bool) error {
	if namespace == "" {
		namespace = api.DefaultNamespace
	}
	c.mu.Lock()
	var (
		key  string
		spec bool
		st   *ObjectStatus
		next *time.Time
	)
	switch kind {
	case api.KindKustomization:
		key = ksKey(namespace, name)
		if s, ok := c.kustomization[key]; ok {
			spec, st, next = s.obj.Spec.Suspend, &s.status, &s.next
		}
	case api.KindGitRepository, api.KindOCIRepository, LocalSourceKind:
		key = srcKey(kind, namespace, name)
		if s, ok := c.sources[key]; ok {
			if s.localDir == "" {
				spec = commonOf(s.obj).suspend
			}
			st, next = &s.status, &s.next
		}
	default:
		c.mu.Unlock()
		return fmt.Errorf("kind %q: %w", kind, ErrNotFound)
	}
	if st == nil {
		c.mu.Unlock()
		return fmt.Errorf("%s %s/%s: %w", kind, namespace, name, ErrNotFound)
	}
	if !suspend && spec {
		c.mu.Unlock()
		return fmt.Errorf("%s %s/%s: %w", kind, namespace, name, ErrSuspendedInSpec)
	}
	if suspend {
		c.suspended[key] = true
		c.markSuspendedLocked(key, spec, st)
		c.opts.Log.Info("suspended", "object", key)
		c.mu.Unlock()
		return nil
	}
	wasSuspended := c.suspended[key]
	delete(c.suspended, key)
	if wasSuspended {
		st.Suspended, st.SuspendedBy = false, ""
		st.setReady(ConditionUnknown, ReasonProgressing, "reconciliation in progress")
		c.opts.Log.Info("resumed", "object", key)
	}
	*next = time.Time{}
	c.mu.Unlock()
	select {
	case c.trigger <- struct{}{}:
	default:
	}
	return nil
}
