// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
	"sigs.k8s.io/yaml"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b)
}

func TestWatchStatusHealth(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewWatchStatus(30 * time.Minute)
	s.now = func() time.Time { return now }
	s.start = now
	h := s.Handler()

	if code, _ := get(t, h, "/healthz"); code != http.StatusOK {
		t.Errorf("healthz at start = %d, want 200", code)
	}
	if code, _ := get(t, h, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("readyz before the first export = %d, want 503", code)
	}

	// Never succeeded: unhealthy once the limit passes after start.
	now = now.Add(31 * time.Minute)
	s.Record(errors.New("authentication required"))
	code, body := get(t, h, "/healthz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "authentication required") {
		t.Errorf("healthz = %d %q, want 503 with the last error", code, body)
	}

	s.Record(nil)
	s.Committed("abc123")
	if code, _ := get(t, h, "/healthz"); code != http.StatusOK {
		t.Errorf("healthz after success = %d, want 200", code)
	}
	if code, _ := get(t, h, "/readyz"); code != http.StatusOK {
		t.Errorf("readyz after success = %d, want 200", code)
	}

	// Failing for longer than the limit since the last success.
	for i := 0; i < 3; i++ {
		now = now.Add(15 * time.Minute)
		s.Record(errors.New("push rejected"))
	}
	if code, _ := get(t, h, "/healthz"); code != http.StatusServiceUnavailable {
		t.Errorf("healthz 45m after the last success = %d, want 503", code)
	}
	_, body = get(t, h, "/status")
	var st WatchState
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Healthy || st.ConsecutiveFailures != 3 || st.Commit != "abc123" || st.LastError != "push rejected" {
		t.Errorf("status = %+v", st)
	}

	_, metrics := get(t, h, "/metrics")
	for _, want := range []string{
		"flux4swarm_watch_healthy 0",
		"flux4swarm_watch_consecutive_failures 3",
		`flux4swarm_watch_syncs_total{result="error"} 4`,
		`flux4swarm_watch_syncs_total{result="success"} 1`,
		"flux4swarm_watch_commits_total 1",
		`flux4swarm_watch_last_commit_info{commit="abc123"} 1`,
		"flux4swarm_watch_last_success_timestamp_seconds 1.80000186e+09",
		"flux4swarm_watch_unhealthy_after_seconds 1800",
		"go_goroutines",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func TestWatchRecordsStatus(t *testing.T) {
	s := NewWatchStatus(time.Hour)
	es := &fakeEvents{msgs: make(chan events.Message), errs: make(chan error)}
	calls := startWatch(t, es, WatchOptions{Interval: 300 * time.Millisecond, Debounce: time.Millisecond, Status: s}, func(n int) error {
		if n == 1 {
			return errors.New("push rejected")
		}
		return nil
	})
	next(t, calls)
	next(t, calls) // the retry
	deadline := time.Now().Add(time.Second)
	for {
		st := s.State()
		if st.LastSuccess != nil {
			if st.ConsecutiveFailures != 0 || st.LastError != "" {
				t.Errorf("status after success = %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("success not recorded: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures != 1 {
		t.Errorf("failures = %d, want 1", s.failures)
	}
}

// TestAlertRulesUseWatchMetrics keeps deploy/monitoring/alerts.yml in step
// with the metrics names.
func TestAlertRulesUseWatchMetrics(t *testing.T) {
	b, err := os.ReadFile("../../deploy/monitoring/alerts.yml")
	if err != nil {
		t.Fatal(err)
	}
	var rules struct {
		Groups []struct {
			Name  string `json:"name"`
			Rules []struct {
				Alert string `json:"alert"`
				Expr  string `json:"expr"`
			} `json:"rules"`
		} `json:"groups"`
	}
	if err := yaml.Unmarshal(b, &rules); err != nil {
		t.Fatal(err)
	}
	if len(rules.Groups) == 0 {
		t.Fatal("no rule groups")
	}
	s := NewWatchStatus(time.Hour)
	s.Record(nil)
	s.Committed("abc")
	_, metrics := get(t, s.Handler(), "/metrics")
	re := regexp.MustCompile(`flux4swarm_watch_[a-z_]+`)
	for _, g := range rules.Groups {
		for _, r := range g.Rules {
			if r.Alert == "" || r.Expr == "" {
				t.Errorf("group %s: rule without alert or expr", g.Name)
			}
			for _, m := range re.FindAllString(r.Expr, -1) {
				if !strings.Contains(metrics, "\n"+m) {
					t.Errorf("alert %s uses unknown metric %s", r.Alert, m)
				}
			}
		}
	}
}
