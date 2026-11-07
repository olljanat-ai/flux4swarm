// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
)

func TestExportIsDeterministic(t *testing.T) {
	o := Options{Cluster: "prod", Bootstrap: true, SecretStoreProvider: ProviderFake}
	a, err := Export(context.Background(), stackService(), o)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		b, err := Export(context.Background(), stackService(), o)
		if err != nil {
			t.Fatal(err)
		}
		if a.Hash() != b.Hash() {
			t.Fatalf("export %d differs from the first one", i)
		}
	}
}

func TestWriteToKeepsFilesOutsideApps(t *testing.T) {
	res, err := Export(context.Background(), stackService(), Options{Cluster: "prod", Bootstrap: true, SecretStoreProvider: ProviderFake})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	write := func(rel, s string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The platform team's edits and an object removed from Swarm.
	write("clusters/prod/flux-system/kustomization.yaml", "edited\n")
	write("clusters/prod/kustomization.yaml", "platform\n")
	write("clusters/prod/platform/hpa.yaml", "hpa\n")
	write("clusters/prod/apps/old/deployment-gone.yaml", "gone\n")

	if err := res.WriteTo(root, true); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) (string, bool) {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		return string(b), err == nil
	}
	for rel, want := range map[string]string{
		"clusters/prod/flux-system/kustomization.yaml": "edited\n",
		"clusters/prod/kustomization.yaml":             "platform\n",
		"clusters/prod/platform/hpa.yaml":              "hpa\n",
	} {
		if got, _ := read(rel); got != want {
			t.Errorf("%s = %q, want %q (kept)", rel, got, want)
		}
	}
	if _, ok := read("clusters/prod/flux-system/gotk-sync.yaml"); !ok {
		t.Error("missing gotk-sync.yaml is not created")
	}
	if got, _ := read("clusters/prod/apps/shop/deployment-web.yaml"); got != string(res.Files["clusters/prod/apps/shop/deployment-web.yaml"]) {
		t.Error("apps are not written")
	}
	if _, ok := read("clusters/prod/apps/old/deployment-gone.yaml"); ok {
		t.Error("apps directory is not cleaned")
	}

	// Without keep (one-shot import) everything is overwritten.
	if err := res.WriteTo(root, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := read("clusters/prod/flux-system/kustomization.yaml"); got == "edited\n" {
		t.Error("one-shot import kept flux-system/kustomization.yaml")
	}
}

type fakeEvents struct {
	mu   sync.Mutex
	subs int
	msgs chan events.Message
	errs chan error
}

func (f *fakeEvents) Events(context.Context, events.ListOptions) (<-chan events.Message, <-chan error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs++
	return f.msgs, f.errs
}

type syncCall struct {
	full bool
	at   time.Time
}

func startWatch(t *testing.T, es EventSource, o WatchOptions, fail func(n int) error) chan syncCall {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan syncCall, 100)
	done := make(chan struct{})
	n := 0
	go func() {
		defer close(done)
		_ = Watch(ctx, es, o, func(_ context.Context, full bool) error {
			n++
			calls <- syncCall{full, time.Now()}
			if fail != nil {
				return fail(n)
			}
			return nil
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	return calls
}

func next(t *testing.T, calls chan syncCall) syncCall {
	t.Helper()
	select {
	case c := <-calls:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no sync")
		return syncCall{}
	}
}

func none(t *testing.T, calls chan syncCall, d time.Duration) {
	t.Helper()
	select {
	case c := <-calls:
		t.Fatalf("unexpected sync (full=%v)", c.full)
	case <-time.After(d):
	}
}

func TestWatchDebouncesEvents(t *testing.T) {
	es := &fakeEvents{msgs: make(chan events.Message), errs: make(chan error)}
	calls := startWatch(t, es, WatchOptions{Interval: time.Hour, Debounce: 200 * time.Millisecond}, nil)
	if c := next(t, calls); !c.full {
		t.Error("first sync is not full")
	}
	start := time.Now()
	for i := 0; i < 5; i++ {
		es.msgs <- events.Message{Type: events.ServiceEventType, Action: events.ActionUpdate}
	}
	c := next(t, calls)
	if c.full {
		t.Error("event sync is full")
	}
	if d := c.at.Sub(start); d < 150*time.Millisecond {
		t.Errorf("sync after %s, before the debounce", d)
	}
	none(t, calls, 400*time.Millisecond)
}

func TestWatchResyncsAndRetries(t *testing.T) {
	es := &fakeEvents{msgs: make(chan events.Message), errs: make(chan error)}
	calls := startWatch(t, es, WatchOptions{Interval: 300 * time.Millisecond, Debounce: time.Millisecond}, func(n int) error {
		if n == 2 {
			return errors.New("push rejected")
		}
		return nil
	})
	next(t, calls)
	if c := next(t, calls); !c.full {
		t.Error("periodic resync is not full")
	}
	// The failed sync is retried (after 2s), as a full sync.
	if c := next(t, calls); !c.full {
		t.Error("retry is not full")
	}
}

func TestWatchReconnectsEventStream(t *testing.T) {
	reconnectDelay = 100 * time.Millisecond
	t.Cleanup(func() { reconnectDelay = 5 * time.Second })
	es := &fakeEvents{msgs: make(chan events.Message), errs: make(chan error, 1)}
	calls := startWatch(t, es, WatchOptions{Interval: time.Hour, Debounce: time.Millisecond}, nil)
	next(t, calls)
	es.errs <- errors.New("unexpected EOF")
	// Resubscribes and resyncs, since events may have been lost.
	if c := next(t, calls); !c.full {
		t.Error("sync after reconnect is not full")
	}
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.subs != 2 {
		t.Errorf("subscribed %d times, want 2", es.subs)
	}
}
