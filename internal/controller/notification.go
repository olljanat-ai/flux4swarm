// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/notify"
)

// Event reasons for notifications, following Flux's controllers.
const (
	ReasonNewArtifact = "NewArtifact"
	ReasonInitialized = "Initialized"
	ReasonInvalid     = "InvalidConfig"
)

// Reporting controllers of events, so that Alerts and webhook consumers
// see the same values as with Flux.
const (
	sourceController    = "source-controller"
	kustomizeController = "kustomize-controller"
)

// notifState holds a Provider, Alert or Receiver.
type notifState struct {
	provider *api.Provider
	alert    *api.Alert
	receiver *api.Receiver
	origin   string
	status   ObjectStatus
	// token and path of a Receiver that validated.
	token string
	path  string
}

func notifKey(kind, ns, name string) string { return kind + "/" + ns + "/" + name }

func (n *notifState) meta() api.ObjectMeta {
	switch {
	case n.provider != nil:
		return n.provider.Metadata
	case n.alert != nil:
		return n.alert.Metadata
	}
	return n.receiver.Metadata
}

// AddNotification registers a bootstrap Provider, Alert or Receiver.
func (c *Controller) AddNotification(obj any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.upsertNotificationLocked(obj, bootstrapOrigin) == "" {
		return fmt.Errorf("%T is not a notification object", obj)
	}
	return nil
}

func (c *Controller) upsertNotificationLocked(obj any, origin string) string {
	var kind string
	var meta api.ObjectMeta
	switch o := obj.(type) {
	case *api.Provider:
		kind, meta = api.KindProvider, o.Metadata
	case *api.Alert:
		kind, meta = api.KindAlert, o.Metadata
	case *api.Receiver:
		kind, meta = api.KindReceiver, o.Metadata
	default:
		return ""
	}
	k := notifKey(kind, meta.Namespace, meta.Name)
	n, ok := c.notif[k]
	if !ok {
		n = &notifState{origin: origin, status: ObjectStatus{Kind: kind, Namespace: meta.Namespace, Name: meta.Name}}
		c.notif[k] = n
	}
	if n.origin != bootstrapOrigin {
		n.origin = origin
	}
	n.provider, n.alert, n.receiver = nil, nil, nil
	switch o := obj.(type) {
	case *api.Provider:
		n.provider = o
	case *api.Alert:
		n.alert = o
	case *api.Receiver:
		n.receiver = o
	}
	c.validateNotificationLocked(n)
	return k
}

func (c *Controller) removeNotificationLocked(key string) {
	if n, ok := c.notif[key]; ok {
		if n.path != "" && c.hooks[n.path] == key {
			delete(c.hooks, n.path)
		}
		delete(c.notif, key)
	}
}

// validateNotificationLocked sets the status of a notification object.
// Credentials are re-read every time, so a Docker secret added later is
// picked up by the next pass.
func (c *Controller) validateNotificationLocked(n *notifState) {
	n.status.LastReconcile = time.Now().UTC()
	switch {
	case n.provider != nil:
		n.status.Suspended = n.provider.Spec.Suspend
		var warn []string
		if n.provider.Spec.CommitStatusExpr != "" {
			warn = append(warn, "spec.commitStatusExpr is not supported, the default commit status is used")
		}
		n.status.Warnings = warn
		if err := c.dispatcher.ValidateProvider(n.provider); err != nil {
			n.status.setReady(ConditionFalse, ReasonInvalid, err.Error())
			return
		}
		n.status.setReady(ConditionTrue, ReasonSucceeded, "Provider is ready")
	case n.alert != nil:
		n.status.Suspended = n.alert.Spec.Suspend
		a := n.alert
		if len(a.Spec.EventSources) == 0 {
			n.status.setReady(ConditionFalse, ReasonInvalid, "spec.eventSources is required")
			return
		}
		for _, l := range [][]string{a.Spec.InclusionList, a.Spec.ExclusionList} {
			for _, exp := range l {
				if _, err := regexp.Compile(exp); err != nil {
					n.status.setReady(ConditionFalse, ReasonInvalid, fmt.Sprintf("invalid regular expression %q: %v", exp, err))
					return
				}
			}
		}
		pk := notifKey(api.KindProvider, a.Metadata.Namespace, a.Spec.ProviderRef.Name)
		if _, ok := c.notif[pk]; !ok {
			n.status.setReady(ConditionFalse, ReasonInvalid, fmt.Sprintf("Provider '%s/%s' not found", a.Metadata.Namespace, a.Spec.ProviderRef.Name))
			return
		}
		n.status.setReady(ConditionTrue, ReasonSucceeded, "Alert is ready")
	case n.receiver != nil:
		r := n.receiver
		n.status.Suspended = r.Spec.Suspend
		if n.path != "" && c.hooks[n.path] == notifKey(api.KindReceiver, r.Metadata.Namespace, r.Metadata.Name) {
			delete(c.hooks, n.path)
		}
		n.path, n.token, n.status.WebhookPath = "", "", ""
		if err := notify.ValidateReceiverSpec(r); err != nil {
			n.status.setReady(ConditionFalse, ReasonInvalid, err.Error())
			return
		}
		tok, err := c.opts.Creds.Get(r.Spec.SecretRef.Name, "token")
		if err != nil {
			n.status.setReady(ConditionFalse, ReasonInvalid,
				fmt.Sprintf("unable to read token from secret '%s/%s': %v", r.Metadata.Namespace, r.Spec.SecretRef.Name, err))
			return
		}
		n.token = string(tok)
		n.path = notify.WebhookPath(n.token, r.Metadata.Name, r.Metadata.Namespace)
		n.status.WebhookPath = n.path
		c.hooks[n.path] = notifKey(api.KindReceiver, r.Metadata.Namespace, r.Metadata.Name)
		n.status.setReady(ConditionTrue, ReasonSucceeded, "Receiver initialized for path: "+n.path)
	}
}

// revalidateNotificationsLocked re-reads the credentials of notification
// objects at most every 30 seconds.
func (c *Controller) revalidateNotificationsLocked(force bool) {
	if !force && time.Since(c.notifChecked) < 30*time.Second {
		return
	}
	c.notifChecked = time.Now()
	keys := make([]string, 0, len(c.notif))
	for k := range c.notif {
		keys = append(keys, k)
	}
	// Providers first, so Alerts see them.
	sort.Slice(keys, func(i, j int) bool {
		pi, pj := strings.HasPrefix(keys[i], api.KindProvider+"/"), strings.HasPrefix(keys[j], api.KindProvider+"/")
		if pi != pj {
			return pi
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		c.validateNotificationLocked(c.notif[k])
	}
}

// onNotifyResult records the last dispatch error of an Alert as a warning.
func (c *Controller) onNotifyResult(r notify.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ns, name, _ := strings.Cut(r.Alert, "/")
	n := c.notif[notifKey(api.KindAlert, ns, name)]
	if n == nil {
		return
	}
	if r.Err != nil {
		n.status.Warnings = []string{time.Now().UTC().Format(time.RFC3339) + " " + r.Err.Error()}
	} else {
		n.status.Warnings = nil
	}
}

// emitLocked sends an event about a Flux object to matching Alerts. c.mu
// must be held. Nothing is sent in dry-run mode.
func (c *Controller) emitLocked(ref notify.ObjectReference, obj api.ObjectMeta, controller, severity, reason, action, msg string, metadata map[string]string) {
	if c.opts.DryRun || len(c.notif) == 0 {
		return
	}
	var alerts []*api.Alert
	providers := map[string]*api.Provider{}
	for _, n := range c.notif {
		switch {
		case n.alert != nil:
			alerts = append(alerts, n.alert)
		case n.provider != nil:
			providers[n.provider.Metadata.NamespacedName()] = n.provider
		}
	}
	if len(alerts) == 0 {
		return
	}
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].Metadata.NamespacedName() < alerts[j].Metadata.NamespacedName() })
	ref.Namespace, ref.Name = obj.Namespace, obj.Name
	ev := notify.Event{
		InvolvedObject:      ref,
		Severity:            severity,
		Timestamp:           time.Now().UTC().Truncate(time.Second),
		Message:             msg,
		Reason:              reason,
		Action:              action,
		Metadata:            metadata,
		ReportingController: controller,
		ReportingInstance:   c.instance,
	}
	c.dispatcher.Dispatch(ev, notify.ObjectInfo{Labels: obj.Labels, Annotations: obj.Annotations}, alerts, providers)
}

func ksRef() notify.ObjectReference {
	return notify.ObjectReference{Kind: api.KindKustomization, APIVersion: api.KustomizeGroupVersion}
}

func ksMetadata(revision string, commitStatus bool) map[string]string {
	g := strings.Split(api.KustomizeGroupVersion, "/")[0]
	m := map[string]string{}
	if revision != "" {
		m[g+"/"+notify.MetaRevisionKey] = revision
	}
	if commitStatus {
		m[g+"/"+notify.MetaCommitStatusKey] = notify.MetaCommitStatusValue
	}
	return m
}

func (c *Controller) emitKustomizationFailureLocked(obj *api.Kustomization, revision, reason, msg string) {
	c.emitLocked(ksRef(), obj.Metadata, kustomizeController, notify.SeverityError, reason, notify.ActionFailed, msg, ksMetadata(revision, false))
}

func (c *Controller) emitSourceLocked(s *sourceState, severity, reason, msg, revision string) {
	if s.localDir != "" {
		return
	}
	g := strings.Split(api.SourceGroupVersion, "/")[0]
	m := map[string]string{}
	if revision != "" {
		m[g+"/"+notify.MetaRevisionKey] = revision
	}
	action := notify.ActionReconciled
	if severity == notify.SeverityError {
		action = notify.ActionFailed
	}
	c.emitLocked(notify.ObjectReference{Kind: s.status.Kind, APIVersion: api.SourceGroupVersion},
		commonOf(s.obj).meta, sourceController, severity, reason, action, msg, m)
}

// isNotificationKind reports kinds whose status does not fail a one-shot
// reconcile.
func isNotificationKind(kind string) bool {
	return kind == api.KindProvider || kind == api.KindAlert || kind == api.KindReceiver
}

// ---- Receiver webhooks ----

// ReceiverHandler serves Flux Receiver webhooks on /hook/<digest>, like
// notification-controller's webhook-receiver.
func (c *Controller) ReceiverHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(notify.WebhookPathPrefix, c.handleWebhook)
	return mux
}

func (c *Controller) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, notify.MaxPayloadBytes))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	c.mu.Lock()
	var rcv *api.Receiver
	var token string
	var ready bool
	if n := c.notif[c.hooks[r.URL.Path]]; n != nil && n.receiver != nil {
		rcv, token, ready = n.receiver, n.token, n.status.IsReady()
	}
	c.mu.Unlock()
	if rcv == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	log := c.opts.Log.With("receiver", rcv.Metadata.NamespacedName())
	if rcv.Spec.Suspend || !ready {
		log.Info("unable to process request", "suspended", rcv.Spec.Suspend)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if err := notify.ValidateRequest(rcv, token, r, body); err != nil {
		log.Error("unable to validate payload", "error", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	log.Info("handling request")
	var errs []error
	for _, res := range rcv.Spec.Resources {
		ns := res.Namespace
		if ns == "" {
			ns = rcv.Metadata.Namespace
		}
		names, err := c.requestReconcile(res.Kind, ns, res.Name, res.MatchLabels)
		if err != nil {
			log.Error("unable to request reconciliation", "error", err)
			errs = append(errs, err)
			continue
		}
		for _, name := range names {
			log.Info("resource annotated", "object", res.Kind+"/"+name+"."+ns)
		}
	}
	if len(errs) > 0 {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// requestReconcile marks a source or Kustomization (or, with name "*",
// every one matching labels) as due, like Flux's requestedAt annotation.
func (c *Controller) requestReconcile(kind, ns, name string, labels map[string]string) ([]string, error) {
	c.mu.Lock()
	var hit []string
	match := func(st ObjectStatus, meta api.ObjectMeta) bool {
		if st.Kind != kind || meta.Namespace != ns {
			return false
		}
		if name == "*" {
			for k, v := range labels {
				if meta.Labels[k] != v {
					return false
				}
			}
			return true
		}
		return meta.Name == name
	}
	for _, s := range c.sources {
		if s.localDir == "" && match(s.status, commonOf(s.obj).meta) {
			s.next = time.Time{}
			hit = append(hit, commonOf(s.obj).meta.Name)
		}
	}
	for _, k := range c.kustomization {
		if match(k.status, k.obj.Metadata) {
			k.next = time.Time{}
			hit = append(hit, k.obj.Metadata.Name)
		}
	}
	c.mu.Unlock()
	if len(hit) == 0 && name != "*" {
		return nil, fmt.Errorf("unable to read %s '%s/%s': not found", kind, ns, name)
	}
	if len(hit) > 0 {
		select {
		case c.trigger <- struct{}{}:
		default:
		}
	}
	sort.Strings(hit)
	return hit, nil
}
