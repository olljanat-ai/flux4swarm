// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package controller runs the GitRepository and Kustomization reconcilers.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/registry"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/build"
	"github.com/olljanat-ai/flux4swarm/internal/creds"
	"github.com/olljanat-ai/flux4swarm/internal/git"
	"github.com/olljanat-ai/flux4swarm/internal/notify"
	"github.com/olljanat-ai/flux4swarm/internal/render"
	"github.com/olljanat-ai/flux4swarm/internal/secrets"
	"github.com/olljanat-ai/flux4swarm/internal/swarm"
	"github.com/olljanat-ai/flux4swarm/internal/translate"
)

// LocalSourceKind is a source that reads a local directory instead of Git,
// used by `flux4swarm reconcile --path DIR` without --url.
const LocalSourceKind = "LocalDirectory"

// Options configures a Controller.
type Options struct {
	Docker       swarm.Client
	Creds        *creds.Store
	CacheDir     string
	RegistryAuth map[string]registry.AuthConfig
	DryRun       bool
	Log          *slog.Logger
	// NotifyRateLimit suppresses identical notification events
	// (notify.DefaultRateLimit when 0, disabled when negative).
	NotifyRateLimit time.Duration
	// Instance is the reportingInstance of events (usually the hostname).
	Instance string
}

type sourceState struct {
	obj      *api.GitRepository
	localDir string
	origin   string
	fetcher  *git.Fetcher
	artifact *git.Artifact
	status   ObjectStatus
	next     time.Time
}

type ksState struct {
	obj    *api.Kustomization
	origin string
	last   *swarm.Result
	// children are Flux objects this Kustomization registered.
	children []string
	status   ObjectStatus
	next     time.Time
	// appliedRevision of the source at last successful apply.
	appliedRevision string
	// workloads maps "Kind/namespace/name" of the workloads of the last
	// build to their Swarm service names, so health checks in other
	// Kustomizations can find them.
	workloads map[string]string
}

// Controller holds the objects and their status.
type Controller struct {
	opts Options

	mu            sync.Mutex
	sources       map[string]*sourceState
	kustomization map[string]*ksState
	clusterStores map[string]*api.SecretStore
	trigger       chan struct{}
	ready         bool

	// Flux notification objects, Receiver webhook paths and the dispatcher.
	notif        map[string]*notifState
	hooks        map[string]string
	dispatcher   *notify.Dispatcher
	notifChecked time.Time
	instance     string

	metrics *metrics
}

// New returns a Controller.
func New(o Options) *Controller {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	c := &Controller{
		opts:          o,
		sources:       map[string]*sourceState{},
		kustomization: map[string]*ksState{},
		clusterStores: map[string]*api.SecretStore{},
		trigger:       make(chan struct{}, 1),
		notif:         map[string]*notifState{},
		hooks:         map[string]string{},
		instance:      o.Instance,
	}
	c.dispatcher = &notify.Dispatcher{Creds: o.Creds, Log: o.Log, RateLimit: o.NotifyRateLimit, OnResult: c.onNotifyResult}
	c.metrics = newMetrics(c)
	return c
}

const bootstrapOrigin = "bootstrap"

func srcKey(kind, ns, name string) string { return kind + "/" + ns + "/" + name }
func ksKey(ns, name string) string        { return api.KindKustomization + "/" + ns + "/" + name }

// AddGitRepository registers a bootstrap GitRepository.
func (c *Controller) AddGitRepository(r *api.GitRepository) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.upsertSource(r, bootstrapOrigin)
}

// AddLocalSource registers a local directory as a source.
func (c *Controller) AddLocalSource(name, dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := srcKey(LocalSourceKind, api.DefaultNamespace, name)
	c.sources[k] = &sourceState{
		obj:      &api.GitRepository{Metadata: api.ObjectMeta{Name: name, Namespace: api.DefaultNamespace}},
		localDir: dir,
		origin:   bootstrapOrigin,
		status:   ObjectStatus{Kind: LocalSourceKind, Namespace: api.DefaultNamespace, Name: name},
	}
}

// AddKustomization registers a bootstrap Kustomization.
func (c *Controller) AddKustomization(k *api.Kustomization) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.upsertKustomization(k, bootstrapOrigin)
}

func (c *Controller) upsertSource(r *api.GitRepository, origin string) string {
	k := srcKey(api.KindGitRepository, r.Metadata.Namespace, r.Metadata.Name)
	s, ok := c.sources[k]
	if !ok {
		s = &sourceState{origin: origin, status: ObjectStatus{Kind: api.KindGitRepository, Namespace: r.Metadata.Namespace, Name: r.Metadata.Name}}
		c.sources[k] = s
	} else if specChanged(s.obj, r) {
		s.next = time.Time{}
		s.fetcher = nil
	}
	if s.origin != bootstrapOrigin {
		s.origin = origin
	}
	s.obj = r
	return k
}

func (c *Controller) upsertKustomization(ks *api.Kustomization, origin string) string {
	k := ksKey(ks.Metadata.Namespace, ks.Metadata.Name)
	s, ok := c.kustomization[k]
	if !ok {
		s = &ksState{origin: origin, status: ObjectStatus{Kind: api.KindKustomization, Namespace: ks.Metadata.Namespace, Name: ks.Metadata.Name}}
		c.kustomization[k] = s
	} else if specChanged(s.obj, ks) {
		s.next = time.Time{}
	}
	if s.origin != bootstrapOrigin {
		s.origin = origin
	}
	s.obj = ks
	return k
}

func specChanged(a, b any) bool {
	ja, _ := api.MarshalDocs(a)
	jb, _ := api.MarshalDocs(b)
	return string(ja) != string(jb)
}

// Trigger asks the loop to reconcile everything now.
func (c *Controller) Trigger() {
	c.mu.Lock()
	for _, s := range c.sources {
		s.next = time.Time{}
	}
	for _, k := range c.kustomization {
		k.next = time.Time{}
	}
	c.mu.Unlock()
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx is done.
func (c *Controller) Run(ctx context.Context) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		c.pass(ctx, false)
		c.mu.Lock()
		c.ready = true
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			c.dispatcher.Wait()
			return nil
		case <-tick.C:
		case <-c.trigger:
		}
	}
}

// ReconcileOnce fetches every source and reconciles every Kustomization,
// repeating while new Flux objects are discovered. It returns an error if
// any object is not Ready.
func (c *Controller) ReconcileOnce(ctx context.Context) error {
	for i := 0; i < 10; i++ {
		before := c.objectCount()
		c.pass(ctx, true)
		if c.objectCount() == before && c.allAttempted() {
			break
		}
	}
	c.dispatcher.Wait()
	var failed []string
	for _, s := range c.Status() {
		if !s.Suspended && !s.IsReady() && !isNotificationKind(s.Kind) {
			failed = append(failed, fmt.Sprintf("%s/%s/%s: %s", s.Kind, s.Namespace, s.Name, s.Ready.Message))
		}
	}
	if len(failed) > 0 {
		return errors.New("not ready:\n  " + strings.Join(failed, "\n  "))
	}
	return nil
}

func (c *Controller) objectCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sources) + len(c.kustomization)
}

func (c *Controller) allAttempted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range c.kustomization {
		if k.status.LastReconcile.IsZero() && !k.obj.Spec.Suspend {
			return false
		}
	}
	return true
}

// pass runs every due source and Kustomization once. With force, due
// times are ignored and each object runs once.
func (c *Controller) pass(ctx context.Context, force bool) {
	now := time.Now()
	c.mu.Lock()
	c.revalidateNotificationsLocked(force)
	var srcKeys []string
	for k, s := range c.sources {
		if force || !now.Before(s.next) {
			srcKeys = append(srcKeys, k)
		}
	}
	c.mu.Unlock()
	sort.Strings(srcKeys)
	for _, k := range srcKeys {
		c.timed(k, func() { c.reconcileSource(ctx, k) })
	}

	for _, k := range c.ksOrder() {
		c.mu.Lock()
		s, ok := c.kustomization[k]
		due := ok && (force || !now.Before(s.next) || c.sourceMovedLocked(s))
		c.mu.Unlock()
		if due {
			c.timed(k, func() { c.reconcileKustomization(ctx, k, force) })
		}
	}
}

func (c *Controller) sourceMovedLocked(s *ksState) bool {
	src := c.sourceForLocked(s.obj)
	return src != nil && src.artifact != nil && src.artifact.Revision != s.appliedRevision && s.status.IsReady()
}

// ksOrder sorts Kustomizations so that dependencies come first.
func (c *Controller) ksOrder() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var keys []string
	for k := range c.kustomization {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	visited := map[string]int{}
	var visit func(string)
	visit = func(k string) {
		if visited[k] != 0 {
			return
		}
		visited[k] = 1
		if s := c.kustomization[k]; s != nil {
			for _, d := range s.obj.Spec.DependsOn {
				ns := d.Namespace
				if ns == "" {
					ns = s.obj.Metadata.Namespace
				}
				visit(ksKey(ns, d.Name))
			}
			out = append(out, k)
		}
		visited[k] = 2
	}
	for _, k := range keys {
		visit(k)
	}
	return out
}

func (c *Controller) sourceForLocked(ks *api.Kustomization) *sourceState {
	ref := ks.Spec.SourceRef
	ns := ref.Namespace
	if ns == "" {
		ns = ks.Metadata.Namespace
	}
	kind := ref.Kind
	if kind == "" {
		kind = api.KindGitRepository
	}
	return c.sources[srcKey(kind, ns, ref.Name)]
}

// ---- GitRepository ----

func (c *Controller) reconcileSource(ctx context.Context, key string) {
	c.mu.Lock()
	s := c.sources[key]
	if s == nil {
		c.mu.Unlock()
		return
	}
	obj := s.obj
	s.status.Suspended = obj.Spec.Suspend
	if obj.Spec.Suspend {
		s.status.setReady(ConditionUnknown, ReasonSuspended, "suspended")
		s.next = time.Now().Add(interval(obj.Spec.Interval.Duration))
		c.mu.Unlock()
		return
	}
	if s.localDir != "" {
		s.artifact = &git.Artifact{Dir: s.localDir, Revision: "local@" + s.localDir}
		s.status.Revision = s.artifact.Revision
		s.status.LastReconcile = time.Now().UTC()
		s.status.setReady(ConditionTrue, ReasonSucceeded, "local directory "+s.localDir)
		s.next = time.Now().Add(time.Minute)
		c.mu.Unlock()
		return
	}
	if s.fetcher == nil {
		var auth *git.AuthProvider
		secret := map[string][]byte{}
		if obj.Spec.SecretRef != nil {
			secret = c.opts.Creds.Lookup(obj.Spec.SecretRef.Name, git.SecretKeys...)
		}
		if obj.Spec.SecretRef != nil || obj.Spec.Provider != "" {
			auth = &git.AuthProvider{Provider: obj.Spec.Provider, URL: obj.Spec.URL, Secret: secret}
		}
		ref := git.Ref{}
		if r := obj.Spec.Ref; r != nil {
			ref = git.Ref{Branch: r.Branch, Tag: r.Tag, Name: r.Name, Commit: r.Commit}
		}
		s.fetcher = &git.Fetcher{
			URL:  obj.Spec.URL,
			Ref:  ref,
			Auth: auth,
			Dir:  filepath.Join(c.opts.CacheDir, "gitrepository", obj.Metadata.Namespace+"-"+obj.Metadata.Name),
		}
	}
	f, last := s.fetcher, s.artifact
	c.mu.Unlock()

	start := time.Now()
	timeout := time.Minute
	if obj.Spec.Timeout != nil {
		timeout = obj.Spec.Timeout.Duration
	}
	fctx, cancel := context.WithTimeout(ctx, timeout)
	var art *git.Artifact
	var err error
	if obj.Spec.Ref != nil && obj.Spec.Ref.SemVer != "" {
		err = fmt.Errorf("spec.ref.semver is not supported")
	} else {
		art, err = f.Fetch(fctx, last)
	}
	cancel()

	c.mu.Lock()
	defer c.mu.Unlock()
	s.status.LastReconcile = time.Now().UTC()
	s.status.Duration = time.Since(start).Seconds()
	s.next = time.Now().Add(interval(obj.Spec.Interval.Duration))
	if err != nil {
		c.opts.Log.Error("fetch failed", "gitrepository", obj.Metadata.NamespacedName(), "error", err)
		s.status.setReady(ConditionFalse, ReasonGitOperationFailed, err.Error())
		c.emitSourceLocked(s, notify.SeverityError, ReasonGitOperationFailed, err.Error(), "")
		return
	}
	if last == nil || last.Revision != art.Revision {
		c.opts.Log.Info("fetched revision", "gitrepository", obj.Metadata.NamespacedName(), "revision", art.Revision)
		c.emitSourceLocked(s, notify.SeverityInfo, ReasonNewArtifact, "stored artifact for revision '"+art.Revision+"'", art.Revision)
	}
	s.artifact = art
	s.status.Revision = art.Revision
	s.status.setReady(ConditionTrue, ReasonSucceeded, "stored artifact for revision '"+art.Revision+"'")
}

func interval(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Minute
	}
	return d
}

// ---- Kustomization ----

func (c *Controller) reconcileKustomization(ctx context.Context, key string, once bool) {
	c.mu.Lock()
	s := c.kustomization[key]
	if s == nil {
		c.mu.Unlock()
		return
	}
	obj := s.obj
	log := c.opts.Log.With("kustomization", obj.Metadata.NamespacedName())
	s.status.Suspended = obj.Spec.Suspend
	retry := interval(obj.Spec.Interval.Duration)
	if obj.Spec.RetryInterval != nil {
		retry = obj.Spec.RetryInterval.Duration
	}
	fail := func(reason string, err error) {
		s.status.LastReconcile = time.Now().UTC()
		s.status.setReady(ConditionFalse, reason, err.Error())
		s.next = time.Now().Add(retry)
		log.Error("reconciliation failed", "reason", reason, "error", err)
		c.emitKustomizationFailureLocked(obj, s.status.LastAttemptedRevision, reason, err.Error())
	}
	if obj.Spec.Suspend {
		s.status.setReady(ConditionUnknown, ReasonSuspended, "suspended")
		s.next = time.Now().Add(interval(obj.Spec.Interval.Duration))
		c.mu.Unlock()
		return
	}
	src := c.sourceForLocked(obj)
	if msg := c.dependenciesNotReadyLocked(obj, src); msg != "" {
		s.status.LastReconcile = time.Now().UTC()
		s.status.setReady(ConditionFalse, ReasonDependencyNotReady, msg)
		s.next = time.Now().Add(retry)
		if once {
			s.next = time.Time{}
		}
		c.mu.Unlock()
		return
	}
	if src == nil || src.artifact == nil {
		msg := fmt.Sprintf("source %s '%s' not found or has no artifact", obj.Spec.SourceRef.Kind, obj.Spec.SourceRef.Name)
		fail(ReasonArtifactFailed, errors.New(msg))
		c.mu.Unlock()
		return
	}
	art := *src.artifact
	clusterStores := map[string]*api.SecretStore{}
	for k, v := range c.clusterStores {
		clusterStores[k] = v
	}
	s.status.LastAttemptedRevision = art.Revision
	last := s.last
	c.mu.Unlock()

	start := time.Now()
	timeout := interval(obj.Spec.Interval.Duration)
	if obj.Spec.Timeout != nil {
		timeout = obj.Spec.Timeout.Duration
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	vars, err := c.substituteFrom(obj)
	if err != nil {
		c.mu.Lock()
		fail(ReasonBuildFailed, err)
		c.mu.Unlock()
		return
	}
	set, err := build.Build(art.Dir, build.Options{Spec: obj.Spec, Vars: vars})
	if err != nil {
		c.mu.Lock()
		fail(ReasonBuildFailed, err)
		c.mu.Unlock()
		return
	}
	for _, st := range set.SecretStores {
		if st.Kind == api.KindClusterSecretStore {
			clusterStores[st.Metadata.Name] = st
		}
	}
	resolver := &secrets.Resolver{Creds: c.opts.Creds, ClusterStores: clusterStores}
	resolved, err := resolver.Resolve(rctx, set.ExternalSecrets, set.SecretStores)
	if err != nil {
		c.mu.Lock()
		fail(ReasonSecretResolveFailed, err)
		c.mu.Unlock()
		return
	}
	out, err := render.Render(set, resolved)
	if err != nil {
		c.mu.Lock()
		fail(ReasonBuildFailed, err)
		c.mu.Unlock()
		return
	}
	for _, w := range out.Warnings {
		log.Warn(w)
	}
	workloads := map[string]string{}
	for _, w := range translate.Workloads(set) {
		workloads[workloadKey(w.Kind, w.Namespace, w.Name)] = w.ServiceName()
	}

	applier := &swarm.Applier{
		Client:       c.opts.Docker,
		Owner:        swarm.Owner{Name: obj.Metadata.Name, Namespace: obj.Metadata.Namespace},
		RegistryAuth: c.opts.RegistryAuth,
		DryRun:       c.opts.DryRun,
		Force:        obj.Spec.Force,
		Log:          log,
	}
	res, err := applier.Apply(rctx, out.Desired)
	if err != nil {
		c.mu.Lock()
		fail(ReasonFailed, err)
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	s.workloads = workloads
	c.mu.Unlock()
	changes := res.Changes
	for _, ch := range changes {
		if ch.Action != swarm.Unchanged {
			log.Info("applied", "object", ch.Kind+"/"+ch.Name, "action", ch.Action, "reason", ch.Reason)
		}
	}

	if obj.Spec.Wait && len(res.ServiceIDs) > 0 {
		if err := applier.Wait(rctx, res.ServiceIDs, time.Until(deadline(rctx, timeout))); err != nil {
			c.mu.Lock()
			s.status.LastChanges = changes
			fail(ReasonHealthCheckFailed, err)
			c.mu.Unlock()
			return
		}
	}
	if !obj.Spec.Wait && len(obj.Spec.HealthChecks) > 0 && !c.opts.DryRun {
		if err := c.healthChecks(rctx, key, obj, workloads, time.Until(deadline(rctx, timeout))); err != nil {
			c.mu.Lock()
			s.status.LastChanges = changes
			fail(ReasonHealthCheckFailed, err)
			c.mu.Unlock()
			return
		}
	}

	pruned, err := applier.Prune(rctx, res, obj.Spec.Prune)
	for _, ch := range pruned {
		log.Info("pruned", "object", ch.Kind+"/"+ch.Name, "reason", ch.Reason)
	}
	changes = append(changes, pruned...)
	if err != nil {
		c.mu.Lock()
		s.status.LastChanges = changes
		fail(ReasonFailed, fmt.Errorf("prune: %w", err))
		c.mu.Unlock()
		return
	}
	_ = last

	c.mu.Lock()
	defer c.mu.Unlock()
	c.registerChildren(ctx, s, key, set, obj.Spec.Prune)
	for _, st := range set.SecretStores {
		if st.Kind == api.KindClusterSecretStore {
			c.clusterStores[st.Metadata.Name] = st
		}
	}
	s.last = res
	s.appliedRevision = art.Revision
	s.status.LastAppliedRevision = art.Revision
	s.status.Inventory = res.Inventory
	s.status.LastChanges = changes
	s.status.Warnings = out.Warnings
	s.status.Ignored = len(set.Ignored)
	s.status.LastReconcile = time.Now().UTC()
	s.status.Duration = time.Since(start).Seconds()
	msg := "Applied revision: " + art.Revision
	if c.opts.DryRun {
		msg = "Dry run for revision: " + art.Revision
	}
	s.status.setReady(ConditionTrue, ReasonSucceeded, msg)
	s.next = time.Now().Add(interval(obj.Spec.Interval.Duration))

	var changeSet []string
	for _, ch := range changes {
		if ch.Action != swarm.Unchanged {
			changeSet = append(changeSet, ch.String())
		}
	}
	if len(changeSet) > 0 {
		c.emitLocked(ksRef(), obj.Metadata, kustomizeController, notify.SeverityInfo, notify.ReasonProgressing,
			notify.ActionReconciling, strings.Join(changeSet, "\n"), ksMetadata(art.Revision, false))
	}
	// Like kustomize-controller, every successful reconciliation ends with
	// an event that commit status (and generic) Providers receive.
	done := fmt.Sprintf("Reconciliation finished in %s, next run in %s",
		time.Since(start).Round(time.Millisecond), interval(obj.Spec.Interval.Duration))
	c.emitLocked(ksRef(), obj.Metadata, kustomizeController, notify.SeverityInfo, ReasonSucceeded,
		notify.ActionReconciled, done, ksMetadata(art.Revision, true))
}

func deadline(ctx context.Context, fallback time.Duration) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(fallback)
}

// registerChildren adds the Flux objects found in a build and removes the
// ones that disappeared (only when prune is set, like Flux's garbage
// collection). c.mu must be held.
func (c *Controller) registerChildren(ctx context.Context, s *ksState, key string, set *api.Set, prune bool) {
	now := map[string]bool{}
	for _, r := range set.GitRepositories {
		now[c.upsertSource(r, key)] = true
	}
	for _, k := range set.Kustomizations {
		now[c.upsertKustomization(k, key)] = true
	}
	for _, p := range set.Providers {
		now[c.upsertNotificationLocked(p, key)] = true
	}
	for _, r := range set.Receivers {
		now[c.upsertNotificationLocked(r, key)] = true
	}
	for _, a := range set.Alerts {
		now[c.upsertNotificationLocked(a, key)] = true
	}
	var keep []string
	for _, child := range s.children {
		if now[child] {
			continue
		}
		if !prune {
			keep = append(keep, child)
			continue
		}
		if n, ok := c.notif[child]; ok && n.origin == key {
			c.removeNotificationLocked(child)
			c.opts.Log.Info("removed notification object", "object", child)
		}
		if src, ok := c.sources[child]; ok && src.origin == key {
			delete(c.sources, child)
			c.metrics.forget(src.status.Kind, src.status.Namespace, src.status.Name)
			c.opts.Log.Info("removed source", "object", child)
		}
		if ks, ok := c.kustomization[child]; ok && ks.origin == key {
			delete(c.kustomization, child)
			c.metrics.forget(ks.status.Kind, ks.status.Namespace, ks.status.Name)
			c.opts.Log.Info("removed kustomization", "object", child)
			if ks.obj.Spec.Prune {
				a := &swarm.Applier{
					Client: c.opts.Docker, DryRun: c.opts.DryRun, Log: c.opts.Log,
					Owner: swarm.Owner{Name: ks.obj.Metadata.Name, Namespace: ks.obj.Metadata.Namespace},
				}
				if _, err := a.Prune(ctx, &swarm.Result{}, true); err != nil {
					c.opts.Log.Error("garbage collection failed", "object", child, "error", err)
				}
			}
		}
	}
	for k := range now {
		if k != key {
			keep = append(keep, k)
		}
	}
	sort.Strings(keep)
	s.children = keep
}

// Status returns a snapshot of every object's status.
func (c *Controller) Status() []ObjectStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ObjectStatus
	for _, s := range c.sources {
		st := s.status
		st.ManagedBy = s.origin
		out = append(out, st)
	}
	for _, k := range c.kustomization {
		st := k.status
		st.ManagedBy = k.origin
		out = append(out, st)
	}
	for _, n := range c.notif {
		st := n.status
		st.ManagedBy = n.origin
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Ready reports whether the first pass completed.
func (c *Controller) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

// substituteFrom reads postBuild.substituteFrom. On Swarm, ConfigMaps and
// Secrets with variables are directories in the credentials directory
// (<dir>/<name>/<VAR>), typically Docker secrets mounted into the
// controller.
func (c *Controller) substituteFrom(k *api.Kustomization) (map[string]string, error) {
	if k.Spec.PostBuild == nil || len(k.Spec.PostBuild.SubstituteFrom) == 0 {
		return nil, nil
	}
	vars := map[string]string{}
	for _, ref := range k.Spec.PostBuild.SubstituteFrom {
		vals, err := c.opts.Creds.All(ref.Name)
		if err != nil {
			if ref.Optional {
				continue
			}
			return nil, fmt.Errorf("substituteFrom %s %s: %w", ref.Kind, ref.Name, err)
		}
		for key, v := range vals {
			vars[key] = string(v)
		}
	}
	return vars, nil
}
