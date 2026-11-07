// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

// Metrics use the names and labels of the Flux controllers
// (fluxcd/pkg/runtime/metrics, controller-runtime and the gotk_resource_info
// series of the flux2-monitoring-example kube-state-metrics config), so the
// standard Flux Grafana dashboards and alerts work for Swarm as well.

// fluxGroupVersion maps kinds to their Flux API group and version.
var fluxGroupVersion = map[string][2]string{
	api.KindGitRepository: {"source.toolkit.fluxcd.io", "v1"},
	api.KindKustomization: {"kustomize.toolkit.fluxcd.io", "v1"},
}

// controller-runtime reconcile results.
const (
	resultSuccess      = "success"
	resultError        = "error"
	resultRequeueAfter = "requeue_after"
)

var (
	conditionDesc = prometheus.NewDesc("gotk_reconcile_condition",
		"The current condition status of a GitOps Toolkit resource reconciliation.",
		[]string{"kind", "name", "namespace", "type", "status"}, nil)
	suspendDesc = prometheus.NewDesc("gotk_suspend_status",
		"The current suspend status of a GitOps Toolkit resource.",
		[]string{"kind", "name", "namespace"}, nil)
	resourceInfoDesc = prometheus.NewDesc("gotk_resource_info",
		"The current state of a Flux resource.",
		[]string{"customresource_group", "customresource_kind", "customresource_version",
			"exported_namespace", "name", "ready", "reason", "suspended", "revision", "url", "source_name"}, nil)
	maxConcurrentDesc = prometheus.NewDesc("controller_runtime_max_concurrent_reconciles",
		"Maximum number of concurrent reconciles per controller",
		[]string{"controller"}, nil)
)

type metrics struct {
	registry      *prometheus.Registry
	duration      *prometheus.HistogramVec
	reconcileTime *prometheus.HistogramVec
	total         *prometheus.CounterVec
	errors        *prometheus.CounterVec
}

func newMetrics(c *Controller) *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gotk_reconcile_duration_seconds",
			Help:    "The duration in seconds of a GitOps Toolkit resource reconciliation.",
			Buckets: prometheus.ExponentialBuckets(10e-3, 10, 5),
		}, []string{"kind", "name", "namespace"}),
		reconcileTime: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "controller_runtime_reconcile_time_seconds",
			Help: "Length of time per reconciliation per controller",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0,
				1.25, 1.5, 1.75, 2.0, 2.5, 3.0, 3.5, 4.0, 4.5, 5, 6, 7, 8, 9, 10, 15, 20, 25, 30, 40, 50, 60},
		}, []string{"controller"}),
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "controller_runtime_reconcile_total",
			Help: "Total number of reconciliations per controller",
		}, []string{"controller", "result"}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "controller_runtime_reconcile_errors_total",
			Help: "Total number of reconciliation errors per controller",
		}, []string{"controller"}),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.duration, m.reconcileTime, m.total, m.errors,
		stateCollector{c},
	)
	// Pre-create the series like controller-runtime does, so rate() and
	// absent() based alerts work before the first error.
	for _, ctrl := range []string{controllerName(api.KindGitRepository), controllerName(api.KindKustomization)} {
		for _, r := range []string{resultSuccess, resultError, resultRequeueAfter} {
			m.total.WithLabelValues(ctrl, r)
		}
		m.errors.WithLabelValues(ctrl)
	}
	return m
}

// controllerName is the controller-runtime controller name Flux uses: the
// lowercased kind.
func controllerName(kind string) string { return strings.ToLower(kind) }

// observe records one reconciliation of an object, using its status after
// the reconcile to pick the controller-runtime result like Flux returns it:
// suspended objects return no requeue, failures return an error, and
// everything else (including waiting for dependencies) requeues after an
// interval.
func (m *metrics) observe(st ObjectStatus, d time.Duration) {
	ctrl := controllerName(st.Kind)
	result := resultRequeueAfter
	switch {
	case st.Suspended:
		result = resultSuccess
	case st.Ready.Status == ConditionFalse && st.Ready.Reason != ReasonDependencyNotReady:
		result = resultError
		m.errors.WithLabelValues(ctrl).Inc()
	}
	m.total.WithLabelValues(ctrl, result).Inc()
	m.reconcileTime.WithLabelValues(ctrl).Observe(d.Seconds())
	if !st.Suspended {
		m.duration.WithLabelValues(st.Kind, st.Name, st.Namespace).Observe(d.Seconds())
	}
}

// forget drops the per-object series of a removed object.
func (m *metrics) forget(kind, namespace, name string) {
	m.duration.DeleteLabelValues(kind, name, namespace)
}

func (m *metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// stateCollector exports the gauges derived from the current object status
// at scrape time, so removed objects disappear without bookkeeping.
type stateCollector struct{ c *Controller }

func (stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- conditionDesc
	ch <- suspendDesc
	ch <- resourceInfoDesc
	ch <- maxConcurrentDesc
}

func (s stateCollector) Collect(ch chan<- prometheus.Metric) {
	for _, ctrl := range []string{controllerName(api.KindGitRepository), controllerName(api.KindKustomization)} {
		ch <- prometheus.MustNewConstMetric(maxConcurrentDesc, prometheus.GaugeValue, 1, ctrl)
	}
	for _, o := range s.c.resourceInfo() {
		st := o.status
		status := st.Ready.Status
		if status == "" {
			status = ConditionUnknown
		}
		for _, v := range []string{ConditionTrue, ConditionFalse, ConditionUnknown} {
			val := 0.0
			if status == v {
				val = 1
			}
			ch <- prometheus.MustNewConstMetric(conditionDesc, prometheus.GaugeValue, val,
				st.Kind, st.Name, st.Namespace, "Ready", v)
		}
		ch <- prometheus.MustNewConstMetric(suspendDesc, prometheus.GaugeValue, boolValue(st.Suspended),
			st.Kind, st.Name, st.Namespace)

		gv := fluxGroupVersion[st.Kind]
		revision := st.Revision
		if st.Kind == api.KindKustomization {
			revision = st.LastAppliedRevision
		}
		ch <- prometheus.MustNewConstMetric(resourceInfoDesc, prometheus.GaugeValue, 1,
			gv[0], st.Kind, gv[1], st.Namespace, st.Name, status, st.Ready.Reason,
			boolString(st.Suspended), revision, o.url, o.sourceName)
	}
}

type resourceState struct {
	status     ObjectStatus
	url        string
	sourceName string
}

// resourceInfo returns every object's status with the spec fields used as
// gotk_resource_info labels.
func (c *Controller) resourceInfo() []resourceState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]resourceState, 0, len(c.sources)+len(c.kustomization))
	for _, s := range c.sources {
		r := resourceState{status: s.status}
		switch o := s.obj.(type) {
		case *api.GitRepository:
			r.url = o.Spec.URL
		case *api.OCIRepository:
			r.url = o.Spec.URL
		}
		out = append(out, r)
	}
	for _, k := range c.kustomization {
		out = append(out, resourceState{status: k.status, sourceName: k.obj.Spec.SourceRef.Name})
	}
	return out
}

// statusOf returns a copy of an object's status.
func (c *Controller) statusOf(key string) (ObjectStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.sources[key]; ok {
		return s.status, true
	}
	if k, ok := c.kustomization[key]; ok {
		return k.status, true
	}
	return ObjectStatus{}, false
}

// timed runs one reconcile of key and records its metrics.
func (c *Controller) timed(key string, reconcile func()) {
	start := time.Now()
	reconcile()
	if st, ok := c.statusOf(key); ok {
		c.metrics.observe(st, time.Since(start))
	}
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
