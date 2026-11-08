// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics records the cutovers of Run for Prometheus. A nil *Metrics
// records nothing.
type Metrics struct {
	mu        sync.Mutex
	ok, fail  int
	migrated  int
	waiting   int
	again     map[string]int
	cutovers  map[string]Cutover
	collected bool
}

var (
	migServicesDesc = prometheus.NewDesc("flux4swarm_migration_services",
		"Swarm services by migration state: migrated (recorded in Git) or waiting (labelled, not cut over yet; a failed cutover stays waiting).", []string{"state"}, nil)
	migCutoversDesc = prometheus.NewDesc("flux4swarm_migration_cutovers_total",
		"Cutover attempts since start, by result (success, error).", []string{"result"}, nil)
	migDowntimeDesc = prometheus.NewDesc("flux4swarm_migration_swarm_downtime_seconds",
		"Time from scaling the service to 0 on Swarm until its manifests were ready for Git (the Swarm part of the downtime), for the cutovers since start.", []string{"service", "namespace", "workload"}, nil)
	migDoneDesc = prometheus.NewDesc("flux4swarm_migration_handed_over_timestamp_seconds",
		"Unix time the service was handed over to Kubernetes, for the cutovers since start.", []string{"service", "namespace", "workload"}, nil)
	migAgainDesc = prometheus.NewDesc("flux4swarm_migration_stopped_again_total",
		"Times a migrated service was found running on Swarm and scaled back to 0.", []string{"service"}, nil)
)

func (m *Metrics) cutover(svc string, c Cutover, err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.fail++
		return
	}
	m.ok++
	if m.cutovers == nil {
		m.cutovers = map[string]Cutover{}
	}
	m.cutovers[svc] = Cutover{Dir: c.Dir, Namespace: c.Namespace, Workload: c.Workload, Stopped: c.Stopped, HandedOver: c.HandedOver}
}

func (m *Metrics) stoppedAgain(svc string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.again == nil {
		m.again = map[string]int{}
	}
	m.again[svc]++
}

func (m *Metrics) services(migrated, waiting int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.migrated, m.waiting, m.collected = migrated, waiting, true
}

// Describe implements prometheus.Collector.
func (m *Metrics) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{migServicesDesc, migCutoversDesc, migDowntimeDesc, migDoneDesc, migAgainDesc} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (m *Metrics) Collect(ch chan<- prometheus.Metric) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.collected {
		ch <- prometheus.MustNewConstMetric(migServicesDesc, prometheus.GaugeValue, float64(m.migrated), "migrated")
		ch <- prometheus.MustNewConstMetric(migServicesDesc, prometheus.GaugeValue, float64(m.waiting), "waiting")
	}
	ch <- prometheus.MustNewConstMetric(migCutoversDesc, prometheus.CounterValue, float64(m.ok), "success")
	ch <- prometheus.MustNewConstMetric(migCutoversDesc, prometheus.CounterValue, float64(m.fail), "error")
	for svc, c := range m.cutovers {
		ch <- prometheus.MustNewConstMetric(migDowntimeDesc, prometheus.GaugeValue, c.HandedOver.Sub(c.Stopped).Seconds(), svc, c.Namespace, c.Workload)
		ch <- prometheus.MustNewConstMetric(migDoneDesc, prometheus.GaugeValue, float64(c.HandedOver.UnixNano())/1e9, svc, c.Namespace, c.Workload)
	}
	for svc, n := range m.again {
		ch <- prometheus.MustNewConstMetric(migAgainDesc, prometheus.CounterValue, float64(n), svc)
	}
}
