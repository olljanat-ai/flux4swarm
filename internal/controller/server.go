// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// KindFor maps a kind as written in a URL or on the command line
// ("kustomization", "ks", "gitrepository", ...) to the Flux kind.
func KindFor(s string) (string, bool) {
	switch strings.ToLower(s) {
	case "kustomization", "kustomizations", "ks":
		return "Kustomization", true
	case "gitrepository", "gitrepositories", "git":
		return "GitRepository", true
	case "ocirepository", "ocirepositories", "oci":
		return "OCIRepository", true
	case "localdirectory":
		return LocalSourceKind, true
	}
	return "", false
}

// authorized requires the API token, when one is configured.
func (c *Controller) authorized(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c.opts.APIToken != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(c.opts.APIToken)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		h(w, r)
	}
}

// Handler serves /healthz, /readyz, /status, /metrics and the POST
// endpoints /reconcile, /suspend/{kind}/{namespace}/{name} and
// /resume/{kind}/{namespace}/{name}.
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
	mux.HandleFunc("POST /reconcile", c.authorized(func(w http.ResponseWriter, _ *http.Request) {
		c.Trigger()
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintln(w, "reconcile triggered")
	}))
	for action, done := range map[string]string{"suspend": "suspended", "resume": "resumed"} {
		suspend := action == "suspend"
		mux.HandleFunc("POST /"+action+"/{kind}/{namespace}/{name}", c.authorized(func(w http.ResponseWriter, r *http.Request) {
			kind, ok := KindFor(r.PathValue("kind"))
			if !ok {
				http.Error(w, "unknown kind "+r.PathValue("kind"), http.StatusNotFound)
				return
			}
			err := c.SetSuspended(kind, r.PathValue("namespace"), r.PathValue("name"), suspend)
			switch {
			case errors.Is(err, ErrNotFound):
				http.Error(w, err.Error(), http.StatusNotFound)
			case errors.Is(err, ErrSuspendedInSpec):
				http.Error(w, err.Error(), http.StatusConflict)
			case err != nil:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			default:
				fmt.Fprintf(w, "%s %s/%s %s\n", kind, r.PathValue("namespace"), r.PathValue("name"), done)
			}
		}))
	}
	mux.Handle("GET /metrics", c.metrics.handler())
	return mux
}
