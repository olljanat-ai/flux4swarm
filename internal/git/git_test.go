// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
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
