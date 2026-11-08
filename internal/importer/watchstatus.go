// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// WatchStatus records the outcome of the exports of import --watch and
// serves it as /healthz, /readyz, /status and /metrics, so a watcher that
// keeps failing (an expired Git token, a rejected push) is noticed instead
// of only logging.
type WatchStatus struct {
	// UnhealthyAfter is how long without a successful export makes
	// /healthz fail. The time since start counts until the first success.
	UnhealthyAfter time.Duration

	now func() time.Time

	mu          sync.Mutex
	start       time.Time
	lastAttempt time.Time
	lastSuccess time.Time
	lastCommit  time.Time
	commit      string
	lastError   string
	consecutive int
	successes   int
	failures    int
	commits     int
}

// NewWatchStatus returns a WatchStatus that becomes unhealthy after
// unhealthyAfter without a successful export.
func NewWatchStatus(unhealthyAfter time.Duration) *WatchStatus {
	s := &WatchStatus{UnhealthyAfter: unhealthyAfter, now: time.Now}
	s.start = s.now()
	return s
}

// Record stores the result of one export.
func (s *WatchStatus) Record(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAttempt = s.now()
	if err != nil {
		s.failures++
		s.consecutive++
		s.lastError = err.Error()
		return
	}
	s.successes++
	s.consecutive = 0
	s.lastError = ""
	s.lastSuccess = s.lastAttempt
}

// Committed stores a commit pushed to Git.
func (s *WatchStatus) Committed(commit string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits++
	s.commit = commit
	s.lastCommit = s.now()
}

// WatchState is the JSON of /status.
type WatchState struct {
	Healthy             bool       `json:"healthy"`
	Message             string     `json:"message,omitempty"`
	LastAttempt         *time.Time `json:"lastAttempt,omitempty"`
	LastSuccess         *time.Time `json:"lastSuccess,omitempty"`
	LastCommit          *time.Time `json:"lastCommitTime,omitempty"`
	Commit              string     `json:"lastCommit,omitempty"`
	LastError           string     `json:"lastError,omitempty"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
}

// State returns the current status.
func (s *WatchStatus) State() WatchState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := WatchState{
		LastAttempt: timePtr(s.lastAttempt), LastSuccess: timePtr(s.lastSuccess),
		LastCommit: timePtr(s.lastCommit), Commit: s.commit,
		LastError: s.lastError, ConsecutiveFailures: s.consecutive,
	}
	st.Healthy, st.Message = s.healthLocked()
	return st
}

func (s *WatchStatus) healthLocked() (bool, string) {
	since, what := s.lastSuccess, "last successful export"
	if since.IsZero() {
		since, what = s.start, "start without a successful export"
	}
	age := s.now().Sub(since)
	if s.UnhealthyAfter <= 0 || age <= s.UnhealthyAfter {
		return true, ""
	}
	msg := fmt.Sprintf("%s %s ago (limit %s)", what, age.Round(time.Second), s.UnhealthyAfter)
	if s.lastError != "" {
		msg += ": " + s.lastError
	}
	return false, msg
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// Handler serves /healthz (503 after UnhealthyAfter without a successful
// export), /readyz (503 until the first successful export), /status and
// /metrics, with the extra collectors added to /metrics.
func (s *WatchStatus) Handler(extra ...prometheus.Collector) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		watchCollector{s},
	)
	reg.MustRegister(extra...)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		st := s.State()
		if !st.Healthy {
			http.Error(w, st.Message, http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.State().LastSuccess == nil {
			http.Error(w, "no successful export yet", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(s.State())
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	return mux
}

var (
	watchHealthyDesc = prometheus.NewDesc("flux4swarm_watch_healthy",
		"1 when the last successful export is not older than --unhealthy-after (the /healthz result).", nil, nil)
	watchSyncsDesc = prometheus.NewDesc("flux4swarm_watch_syncs_total",
		"Exports by result (success, error).", []string{"result"}, nil)
	watchConsecutiveDesc = prometheus.NewDesc("flux4swarm_watch_consecutive_failures",
		"Failed exports since the last successful one.", nil, nil)
	watchCommitsDesc = prometheus.NewDesc("flux4swarm_watch_commits_total",
		"Commits pushed to Git.", nil, nil)
	watchLastSuccessDesc = prometheus.NewDesc("flux4swarm_watch_last_success_timestamp_seconds",
		"Unix time of the last successful export (0 before the first one).", nil, nil)
	watchLastAttemptDesc = prometheus.NewDesc("flux4swarm_watch_last_attempt_timestamp_seconds",
		"Unix time of the last export, successful or not.", nil, nil)
	watchLastCommitDesc = prometheus.NewDesc("flux4swarm_watch_last_commit_timestamp_seconds",
		"Unix time of the last commit pushed to Git.", nil, nil)
	watchCommitInfoDesc = prometheus.NewDesc("flux4swarm_watch_last_commit_info",
		"Always 1; the commit label is the last commit pushed to Git.", []string{"commit"}, nil)
	watchUnhealthyAfterDesc = prometheus.NewDesc("flux4swarm_watch_unhealthy_after_seconds",
		"The --unhealthy-after limit.", nil, nil)
)

// watchCollector reads the WatchStatus at scrape time.
type watchCollector struct{ s *WatchStatus }

func (watchCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{watchHealthyDesc, watchSyncsDesc, watchConsecutiveDesc, watchCommitsDesc,
		watchLastSuccessDesc, watchLastAttemptDesc, watchLastCommitDesc, watchCommitInfoDesc, watchUnhealthyAfterDesc} {
		ch <- d
	}
}

func (c watchCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.s
	s.mu.Lock()
	defer s.mu.Unlock()
	healthy, _ := s.healthLocked()
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	gauge(watchHealthyDesc, boolGauge(healthy))
	ch <- prometheus.MustNewConstMetric(watchSyncsDesc, prometheus.CounterValue, float64(s.successes), "success")
	ch <- prometheus.MustNewConstMetric(watchSyncsDesc, prometheus.CounterValue, float64(s.failures), "error")
	ch <- prometheus.MustNewConstMetric(watchCommitsDesc, prometheus.CounterValue, float64(s.commits))
	gauge(watchConsecutiveDesc, float64(s.consecutive))
	gauge(watchLastSuccessDesc, unixSeconds(s.lastSuccess))
	gauge(watchLastAttemptDesc, unixSeconds(s.lastAttempt))
	gauge(watchLastCommitDesc, unixSeconds(s.lastCommit))
	if s.commit != "" {
		gauge(watchCommitInfoDesc, 1, s.commit)
	}
	gauge(watchUnhealthyAfterDesc, s.UnhealthyAfter.Seconds())
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}
