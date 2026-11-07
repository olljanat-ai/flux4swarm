// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
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
	mux.Handle("GET /metrics", c.metrics.handler())
	return mux
}
