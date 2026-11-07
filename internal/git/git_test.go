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

func TestPushDescribe(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "repo.git")
	if _, err := gogit.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	url := "file://" + bare
	var got []FileChange
	push := func(write func(root string) error) string {
		t.Helper()
		r, err := CommitAndPush(ctx, filepath.Join(t.TempDir(), "w"), PushOptions{
			URL: url, Message: "fallback", Write: write,
			Describe: func(c []FileChange) string {
				got = c
				if len(c) == 1 {
					return ""
				}
				return "described"
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		repo, _ := gogit.PlainOpen(bare)
		c, err := repo.CommitObject(plumbing.NewHash(r.Commit))
		if err != nil {
			t.Fatal(err)
		}
		return c.Message
	}
	file := func(name, content string) func(string) error {
		return func(root string) error { return os.WriteFile(filepath.Join(root, name), []byte(content), 0o644) }
	}
	if m := push(file("a.yaml", "one")); m != "fallback" {
		t.Fatalf("empty Describe should fall back to Message, got %q", m)
	}
	if len(got) != 1 || got[0].Path != "a.yaml" || got[0].Old != nil || string(got[0].New) != "one" {
		t.Fatalf("added file: %+v", got)
	}
	m := push(func(root string) error {
		if err := os.Remove(filepath.Join(root, "a.yaml")); err != nil {
			return err
		}
		return file("b.yaml", "two")(root)
	})
	if m != "described" {
		t.Fatalf("message %q", m)
	}
	if len(got) != 2 || got[0].Path != "a.yaml" || string(got[0].Old) != "one" || got[0].New != nil ||
		got[1].Path != "b.yaml" || got[1].Old != nil || string(got[1].New) != "two" {
		t.Fatalf("changes: %+v", got)
	}
}

func TestPushRetriesWhenBranchMoved(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "repo.git")
	if _, err := gogit.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	url := "file://" + bare
	write := func(name, content string) func(string) error {
		return func(root string) error {
			return os.WriteFile(filepath.Join(root, name), []byte(content), 0o644)
		}
	}
	if _, err := CommitAndPush(ctx, filepath.Join(t.TempDir(), "w"), PushOptions{
		URL: url, Branch: "main", Message: "seed", Write: write("seed.yaml", "seed"),
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r, err := CommitAndPush(ctx, filepath.Join(t.TempDir(), "w"), PushOptions{
		URL: url, Branch: "main", Message: "a",
		Write: func(root string) error {
			calls++
			if calls == 1 {
				// Another watcher pushes between our clone and our push.
				if _, err := CommitAndPush(ctx, filepath.Join(t.TempDir(), "w"), PushOptions{
					URL: url, Branch: "main", Message: "b", Write: write("b.yaml", "b"),
				}); err != nil {
					return err
				}
			}
			return write("a.yaml", "a")(root)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Changed || calls != 2 {
		t.Fatalf("changed=%v calls=%d, want a retry", r.Changed, calls)
	}
	art, err := (&Fetcher{URL: url, Ref: Ref{Branch: "main"}, Dir: t.TempDir()}).Fetch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if art.Commit != r.Commit {
		t.Fatalf("branch at %s, want %s", art.Commit, r.Commit)
	}
	for f, want := range map[string]string{"seed.yaml": "seed", "a.yaml": "a", "b.yaml": "b"} {
		if b, _ := os.ReadFile(filepath.Join(art.Dir, f)); string(b) != want {
			t.Errorf("%s = %q, want %q", f, b, want)
		}
	}
}
