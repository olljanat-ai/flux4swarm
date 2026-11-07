// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
)

// WriteTo writes the files below root, after emptying the Clean
// directories. With keep, a file outside the Clean directories that
// already exists is left alone: in watch mode the export owns only the
// apps directory, and files such as flux-system/kustomization.yaml are
// written once and then belong to whoever edits them.
func (r *Result) WriteTo(root string, keep bool) error {
	for _, c := range r.Clean {
		if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(c))); err != nil {
			return err
		}
	}
	var names []string
	for f := range r.Files {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		p := filepath.Join(root, filepath.FromSlash(f))
		if keep && !r.owned(f) {
			if _, err := os.Stat(p); err == nil {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, r.Files[f], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// owned reports whether file is in one of the Clean directories.
func (r *Result) owned(file string) bool {
	for _, c := range r.Clean {
		if strings.HasPrefix(file, strings.TrimSuffix(c, "/")+"/") {
			return true
		}
	}
	return false
}

// Hash identifies the export, to skip Git when Swarm did not change.
func (r *Result) Hash() string {
	var names []string
	for f := range r.Files {
		names = append(names, f)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, f := range names {
		h.Write([]byte(f))
		h.Write([]byte{0})
		h.Write(r.Files[f])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// reconnectDelay is the wait before resubscribing to Docker events.
var reconnectDelay = 5 * time.Second

// EventSource is the Docker events API.
type EventSource interface {
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
}

// WatchOptions configures Watch.
type WatchOptions struct {
	// Interval is the full resync period.
	Interval time.Duration
	// Debounce is how long to wait after the first Swarm event before
	// exporting, so a `docker stack deploy` gives one commit.
	Debounce time.Duration
	Log      *slog.Logger
}

// Watch runs sync once at start, after Swarm service, config and secret
// events and every Interval, until ctx is done. full is true for the
// first run, the periodic resyncs and the retries after an error or a
// lost event stream; sync may skip Git when it is false and the export
// did not change.
func Watch(ctx context.Context, es EventSource, o WatchOptions, sync func(ctx context.Context, full bool) error) error {
	if o.Interval <= 0 {
		o.Interval = 10 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	f := filters.NewArgs(
		filters.Arg("type", string(events.ServiceEventType)),
		filters.Arg("type", string(events.ConfigEventType)),
		filters.Arg("type", string(events.SecretEventType)),
	)
	subscribe := func() (<-chan events.Message, <-chan error) {
		return es.Events(ctx, events.ListOptions{Filters: f})
	}
	evs, errs := subscribe()
	run := time.NewTimer(0)
	scheduled, full := true, true
	reconnect := time.NewTimer(0)
	reconnect.Stop()
	resync := time.NewTicker(o.Interval)
	defer resync.Stop()
	var backoff time.Duration
	for {
		select {
		case <-ctx.Done():
			return nil
		case m := <-evs:
			o.Log.Debug("swarm event", "type", m.Type, "action", m.Action, "name", m.Actor.Attributes["name"])
			if !scheduled {
				run.Reset(o.Debounce)
				scheduled = true
			}
		case err := <-errs:
			if ctx.Err() != nil {
				return nil
			}
			o.Log.Warn("docker event stream lost, reconnecting", "error", err)
			evs, errs = nil, nil
			reconnect.Reset(reconnectDelay)
		case <-reconnect.C:
			evs, errs = subscribe()
			// Events may have been missed.
			full = true
			if !scheduled {
				run.Reset(o.Debounce)
				scheduled = true
			}
		case <-resync.C:
			full = true
			run.Reset(0)
			scheduled = true
		case <-run.C:
			scheduled = false
			if err := sync(ctx, full); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				backoff = min(max(2*backoff, 2*time.Second), o.Interval)
				o.Log.Error("export failed", "error", err, "retry", backoff)
				full = true
				run.Reset(backoff)
				scheduled = true
				continue
			}
			backoff, full = 0, false
		}
	}
}
