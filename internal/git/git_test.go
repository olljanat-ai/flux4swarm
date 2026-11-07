// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestPushAndFetch(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "repo.git")
	if _, err := gogit.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	url := "file://" + bare
	push := func(content string) *PushResult {
		t.Helper()
		r, err := CommitAndPush(ctx, filepath.Join(t.TempDir(), "w"), PushOptions{
			URL: url, Branch: "main", Message: "test",
			Write: func(root string) error {
				return os.WriteFile(filepath.Join(root, "a.yaml"), []byte(content), 0o644)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := push("one")
	if !first.Changed {
		t.Fatal("first push should change")
	}
	if again := push("one"); again.Changed {
		t.Fatal("identical content should not create a commit")
	}

	f := &Fetcher{URL: url, Ref: Ref{Branch: "main"}, Dir: t.TempDir()}
	art, err := f.Fetch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if art.Commit != first.Commit || !strings.HasPrefix(art.Revision, "main@sha1:") {
		t.Fatalf("artifact %+v, want commit %s", art, first.Commit)
	}
	b, _ := os.ReadFile(filepath.Join(art.Dir, "a.yaml"))
	if string(b) != "one" {
		t.Fatalf("content %q", b)
	}
	same, err := f.Fetch(ctx, art)
	if err != nil || same != art {
		t.Fatalf("unchanged remote should return the cached artifact: %v", err)
	}
	second := push("two")
	art2, err := f.Fetch(ctx, art)
	if err != nil {
		t.Fatal(err)
	}
	if art2.Commit != second.Commit {
		t.Fatalf("got %s want %s", art2.Commit, second.Commit)
	}
}

func TestFetchSemVer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	commit := func(content string) plumbing.Hash {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add("a.yaml"); err != nil {
			t.Fatal(err)
		}
		h, err := wt.Commit(content, &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.invalid", When: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	tag := func(name string, h plumbing.Hash, annotated bool) {
		t.Helper()
		var opts *gogit.CreateTagOptions
		if annotated {
			opts = &gogit.CreateTagOptions{Message: name, Tagger: &object.Signature{Name: "t", Email: "t@example.invalid", When: time.Now()}}
		}
		if _, err := repo.CreateTag(name, h, opts); err != nil {
			t.Fatal(err)
		}
	}
	tag("v1.0.0", commit("one"), false)
	v11 := commit("one.one")
	tag("v1.1.0", v11, true)
	tag("v2.0.0", commit("two"), false)
	tag("not-a-version", commit("head"), false)

	f := &Fetcher{URL: "file://" + dir, Ref: Ref{SemVer: ">=1.0.0 <2.0.0"}, Dir: t.TempDir()}
	art, err := f.Fetch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "v1.1.0@sha1:" + v11.String(); art.Revision != want || art.Commit != v11.String() {
		t.Fatalf("revision %s, want %s", art.Revision, want)
	}
	if b, _ := os.ReadFile(filepath.Join(art.Dir, "a.yaml")); string(b) != "one.one" {
		t.Fatalf("content %q", b)
	}

	f = &Fetcher{URL: "file://" + dir, Ref: Ref{SemVer: ">=3.0.0"}, Dir: t.TempDir()}
	if _, err := f.Fetch(ctx, nil); err == nil || !strings.Contains(err.Error(), "no tag matches") {
		t.Fatalf("want no-match error, got %v", err)
	}
}
