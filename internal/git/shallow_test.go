// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package git

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitHTTPServer serves a bare repository through the real "git http-backend",
// the same smart HTTP protocol GitHub and Azure DevOps speak, so pushes are
// checked by a real git receive-pack rather than go-git's own server.
func gitHTTPServer(t *testing.T, wrap func(http.Handler) http.Handler) (url, bare string) {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary not available")
	}
	root := t.TempDir()
	bare = filepath.Join(root, "repo.git")
	for _, args := range [][]string{
		{"init", "--bare", "--initial-branch=main", bare},
		{"-C", bare, "config", "http.receivepack", "true"},
	} {
		if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	var h http.Handler = &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL + "/repo.git", bare
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func pushN(t *testing.T, url string, n int) []string {
	t.Helper()
	var dirs []string
	for i := 0; i < n; i++ {
		w := filepath.Join(t.TempDir(), "w")
		r, err := CommitAndPush(context.Background(), w, PushOptions{
			URL: url, Branch: "main", Message: fmt.Sprintf("sync %d", i),
			Write: func(root string) error {
				if err := os.MkdirAll(filepath.Join(root, "apps"), 0o755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(root, "apps", fmt.Sprintf("s%d.yaml", i)), []byte(fmt.Sprint(i)), 0o644)
			},
		})
		if err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		if !r.Changed {
			t.Fatalf("push %d did not change", i)
		}
		dirs = append(dirs, w)
	}
	return dirs
}

// TestShallowPushHTTP pushes from depth-1 clones to a real git server and
// checks that the server ends up with complete, connected history.
func TestShallowPushHTTP(t *testing.T) {
	url, bare := gitHTTPServer(t, nil)
	dirs := pushN(t, url, 4)
	for _, d := range dirs[1:] {
		if _, err := os.Stat(filepath.Join(d, ".git", "shallow")); err != nil {
			t.Fatalf("%s: expected a shallow clone: %v", d, err)
		}
	}
	gitOut(t, bare, "fsck", "--strict", "--no-dangling")
	if n := gitOut(t, bare, "rev-list", "--count", "main"); n != "4" {
		t.Fatalf("history has %s commits, want 4", n)
	}
	files := gitOut(t, bare, "ls-tree", "-r", "--name-only", "main")
	for i := 0; i < 4; i++ {
		if !strings.Contains(files, fmt.Sprintf("apps/s%d.yaml", i)) {
			t.Fatalf("file s%d missing from main:\n%s", i, files)
		}
	}
}

// TestShallowPushFile covers go-git's own transport (file://).
func TestShallowPushFile(t *testing.T) {
	_, bare := gitHTTPServer(t, nil)
	dirs := pushN(t, "file://"+bare, 3)
	if _, err := os.Stat(filepath.Join(dirs[2], ".git", "shallow")); err != nil {
		t.Fatalf("expected a shallow clone: %v", err)
	}
	if n := gitOut(t, bare, "rev-list", "--count", "main"); n != "3" {
		t.Fatalf("history has %s commits, want 3", n)
	}
	gitOut(t, bare, "fsck", "--strict", "--no-dangling")
}

// TestShallowFallback makes the server refuse shallow fetches and checks
// that CommitAndPush still syncs through a full clone.
func TestShallowFallback(t *testing.T) {
	refused := 0
	url, bare := gitHTTPServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
				body, _ := io.ReadAll(r.Body)
				if bytes.Contains(body, []byte("deepen")) {
					refused++
					http.Error(w, "shallow not supported", http.StatusInternalServerError)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			next.ServeHTTP(w, r)
		})
	})
	dirs := pushN(t, url, 3)
	if refused == 0 {
		t.Fatal("the shallow clone was never attempted")
	}
	if _, err := os.Stat(filepath.Join(dirs[2], ".git", "shallow")); err == nil {
		t.Fatal("fallback clone should be full")
	}
	if n := gitOut(t, bare, "rev-list", "--count", "main"); n != "3" {
		t.Fatalf("history has %s commits, want 3", n)
	}
}
