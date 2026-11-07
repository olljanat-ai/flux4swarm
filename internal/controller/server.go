// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Handler serves /healthz, /readyz, /status, /metrics and POST /reconcile.
func (c *Controller) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !c.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(c.Status())
	})
	mux.HandleFunc("POST /reconcile", func(w http.ResponseWriter, _ *http.Request) {
		c.Trigger()
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintln(w, "reconcile triggered")
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		var b strings.Builder
		b.WriteString("# HELP gotk_reconcile_condition The current condition status of a GitOps Toolkit resource reconciliation.\n")
		b.WriteString("# TYPE gotk_reconcile_condition gauge\n")
		st := c.Status()
		for _, s := range st {
			for _, v := range []string{ConditionTrue, ConditionFalse, ConditionUnknown} {
				val := 0
				if s.Ready.Status == v {
					val = 1
				}
				fmt.Fprintf(&b, "gotk_reconcile_condition{kind=%q,name=%q,namespace=%q,type=\"Ready\",status=%q} %d\n",
					s.Kind, s.Name, s.Namespace, v, val)
			}
		}
		b.WriteString("# HELP gotk_suspend_status The current suspend status of a GitOps Toolkit resource.\n")
		b.WriteString("# TYPE gotk_suspend_status gauge\n")
		for _, s := range st {
			val := 0
			if s.Suspended {
				val = 1
			}
			fmt.Fprintf(&b, "gotk_suspend_status{kind=%q,name=%q,namespace=%q} %d\n", s.Kind, s.Name, s.Namespace, val)
		}
		b.WriteString("# HELP gotk_reconcile_duration_seconds Duration of the last reconciliation.\n")
		b.WriteString("# TYPE gotk_reconcile_duration_seconds gauge\n")
		for _, s := range st {
			fmt.Fprintf(&b, "gotk_reconcile_duration_seconds{kind=%q,name=%q,namespace=%q} %g\n", s.Kind, s.Name, s.Namespace, s.Duration)
		}
		_, _ = w.Write([]byte(b.String()))
	})
	return mux
}
