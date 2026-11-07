// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeRegistry is a minimal read-only OCI distribution registry with bearer
// token auth and paginated tag listing.
type fakeRegistry struct {
	mu        sync.Mutex
	repo      string
	blobs     map[string][]byte
	manifests map[string][]byte // by digest
	tags      map[string]string // tag -> digest
	user      string
	pass      string
	srv       *httptest.Server
	tokens    int
}

func newFakeRegistry(t *testing.T, repo string) *fakeRegistry {
	r := &fakeRegistry{repo: repo, blobs: map[string][]byte{}, manifests: map[string][]byte{}, tags: map[string]string{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRegistry) host() string { return strings.TrimPrefix(r.srv.URL, "http://") }

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// push stores a Flux artifact (config + one layer) under tag and returns
// the manifest digest.
func (r *fakeRegistry) push(t *testing.T, tag string, layer []byte) string {
	t.Helper()
	cfg := []byte("{}")
	m := Manifest{
		SchemaVersion: 2, MediaType: mediaTypeOCIManifest,
		Config: Descriptor{MediaType: MediaTypeFluxConfig, Digest: digestOf(cfg), Size: int64(len(cfg))},
		Layers: []Descriptor{{MediaType: MediaTypeFluxContent, Digest: digestOf(layer), Size: int64(len(layer))}},
		Annotations: map[string]string{
			"org.opencontainers.image.revision": tag,
		},
	}
	b, _ := json.Marshal(m)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blobs[digestOf(cfg)] = cfg
	r.blobs[digestOf(layer)] = layer
	r.manifests[digestOf(b)] = b
	r.tags[tag] = digestOf(b)
	return digestOf(b)
}

func (r *fakeRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.URL.Path == "/token" {
		if r.user != "" {
			u, p, ok := req.BasicAuth()
			if !ok || u != r.user || p != r.pass {
				http.Error(w, "bad credentials", http.StatusUnauthorized)
				return
			}
		}
		if req.URL.Query().Get("scope") != "repository:"+r.repo+":pull" {
			http.Error(w, "bad scope "+req.URL.Query().Get("scope"), http.StatusBadRequest)
			return
		}
		r.tokens++
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
		return
	}
	if req.Header.Get("Authorization") != "Bearer tok" {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake",scope="repository:%s:pull"`, r.srv.URL, r.repo))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	prefix := "/v2/" + r.repo + "/"
	if !strings.HasPrefix(req.URL.Path, prefix) {
		http.NotFound(w, req)
		return
	}
	rest := strings.TrimPrefix(req.URL.Path, prefix)
	switch {
	case rest == "tags/list":
		var tags []string
		for k := range r.tags {
			tags = append(tags, k)
		}
		sort.Strings(tags)
		// Two tags per page to exercise pagination.
		last := req.URL.Query().Get("last")
		var page []string
		for _, tg := range tags {
			if tg > last && len(page) < 2 {
				page = append(page, tg)
			}
		}
		if len(page) == 2 && page[1] != tags[len(tags)-1] {
			w.Header().Set("Link", fmt.Sprintf(`<%stags/list?n=2&last=%s>; rel="next"`, prefix, page[1]))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": r.repo, "tags": page})
	case strings.HasPrefix(rest, "manifests/"):
		ref := strings.TrimPrefix(rest, "manifests/")
		if d, ok := r.tags[ref]; ok {
			ref = d
		}
		b, ok := r.manifests[ref]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", mediaTypeOCIManifest)
		w.Header().Set("Docker-Content-Digest", ref)
		if req.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(b)
	case strings.HasPrefix(rest, "blobs/"):
		b, ok := r.blobs[strings.TrimPrefix(rest, "blobs/")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(b)
	default:
		http.NotFound(w, req)
	}
}

type entry struct {
	name, body string
	typ        byte
}

func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: typ}
		if typ != tar.TypeReg {
			hdr.Size = 0
			hdr.Linkname = e.body
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestFetchSemVerAndCache(t *testing.T) {
	ctx := context.Background()
	reg := newFakeRegistry(t, "org/manifests")
	reg.user, reg.pass = "bob", "secret"
	reg.push(t, "1.0.0", tarball(t, entry{name: "app/deploy.yaml", body: "v: 1.0.0"}))
	d11 := reg.push(t, "1.1.0", tarball(t, entry{name: "./app/deploy.yaml", body: "v: 1.1.0"}))
	reg.push(t, "2.0.0", tarball(t, entry{name: "app/deploy.yaml", body: "v: 2.0.0"}))
	reg.push(t, "latest", tarball(t, entry{name: "app/deploy.yaml", body: "latest"}))

	f := &Fetcher{
		URL: "oci://" + reg.host() + "/org/manifests", Ref: Ref{SemVer: "1.x"}, Insecure: true,
		Creds: Credentials{Username: "bob", Password: "secret"}, Dir: t.TempDir(),
	}
	art, err := f.Fetch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "1.1.0@" + d11; art.Revision != want || art.Commit != d11 {
		t.Fatalf("revision %s, want %s", art.Revision, want)
	}
	if b, _ := os.ReadFile(filepath.Join(art.Dir, "app", "deploy.yaml")); string(b) != "v: 1.1.0" {
		t.Fatalf("content %q", b)
	}
	again, err := f.Fetch(ctx, art)
	if err != nil || again != art {
		t.Fatalf("unchanged digest should return the cached artifact: %v", err)
	}

	// A new matching tag moves the source.
	d19 := reg.push(t, "1.9.0", tarball(t, entry{name: "app/deploy.yaml", body: "v: 1.9.0"}))
	next, err := f.Fetch(ctx, art)
	if err != nil || next.Revision != "1.9.0@"+d19 {
		t.Fatalf("got %+v, %v", next, err)
	}
	if _, err := os.Stat(art.Dir); !os.IsNotExist(err) {
		t.Fatalf("old revision directory should be removed: %v", err)
	}
	if reg.tokens != 1 {
		t.Fatalf("token should be fetched once and reused, got %d", reg.tokens)
	}

	// Default ref is latest; a digest ref has no tag in the revision.
	f2 := &Fetcher{URL: f.URL, Insecure: true, Creds: f.Creds, Dir: t.TempDir()}
	if a, err := f2.Fetch(ctx, nil); err != nil || !strings.HasPrefix(a.Revision, "latest@sha256:") {
		t.Fatalf("latest: %+v, %v", a, err)
	}
	f3 := &Fetcher{URL: f.URL, Ref: Ref{Digest: d11}, Insecure: true, Creds: f.Creds, Dir: t.TempDir()}
	if a, err := f3.Fetch(ctx, nil); err != nil || a.Revision != d11 {
		t.Fatalf("digest: %+v, %v", a, err)
	}

	bad := &Fetcher{URL: f.URL, Ref: Ref{Tag: "1.0.0"}, Insecure: true, Creds: Credentials{Username: "bob", Password: "wrong"}, Dir: t.TempDir()}
	if _, err := bad.Fetch(ctx, nil); err == nil || !strings.Contains(err.Error(), "token request") {
		t.Fatalf("want auth error, got %v", err)
	}
}

func TestFetchRejectsBadArtifacts(t *testing.T) {
	ctx := context.Background()
	reg := newFakeRegistry(t, "r")
	reg.push(t, "escape", tarball(t, entry{name: "../evil.yaml", body: "x"}))
	reg.push(t, "link", tarball(t, entry{name: "l", body: "/etc/passwd", typ: tar.TypeSymlink}))
	reg.push(t, "plain", []byte("not gzip"))
	tampered := tarball(t, entry{name: "a.yaml", body: "a"})
	reg.push(t, "tampered", tampered)
	reg.blobs[digestOf(tampered)] = tarball(t, entry{name: "a.yaml", body: "b"})

	for tag, want := range map[string]string{
		"escape":   "escapes the artifact root",
		"link":     "unsupported type",
		"plain":    "not gzip",
		"tampered": "digest mismatch",
		"missing":  "404",
	} {
		f := &Fetcher{URL: "oci://" + reg.host() + "/r", Ref: Ref{Tag: tag}, Insecure: true, Dir: t.TempDir()}
		if _, err := f.Fetch(ctx, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error containing %q, got %v", tag, want, err)
		}
	}
	f := &Fetcher{URL: "oci://" + reg.host() + "/r", Ref: Ref{Tag: "plain"}, MediaType: "application/x-other", Insecure: true, Dir: t.TempDir()}
	if _, err := f.Fetch(ctx, nil); err == nil || !strings.Contains(err.Error(), "no layer with media type") {
		t.Errorf("layer selector: %v", err)
	}
}

func TestParseURL(t *testing.T) {
	for in, want := range map[string]string{
		"oci://ghcr.io/org/app":    "ghcr.io org/app",
		"oci://localhost:5000/app": "localhost:5000 app",
		"oci://docker.io/app":      "registry-1.docker.io library/app",
	} {
		h, r, err := ParseURL(in)
		if err != nil || h+" "+r != want {
			t.Errorf("ParseURL(%s) = %s %s, %v", in, h, r, err)
		}
	}
	for _, in := range []string{"https://ghcr.io/org/app", "oci://ghcr.io", "oci://ghcr.io/app:1.0"} {
		if _, _, err := ParseURL(in); err == nil {
			t.Errorf("ParseURL(%s) should fail", in)
		}
	}
}

func TestCredentialsFromSecret(t *testing.T) {
	cfg := []byte(`{"auths":{"https://ghcr.io":{"auth":"Ym9iOnNlY3JldA=="},"other.io":{"username":"x","password":"y"}}}`)
	c, err := CredentialsFromSecret(map[string][]byte{".dockerconfigjson": cfg}, "ghcr.io")
	if err != nil || c.Username != "bob" || c.Password != "secret" {
		t.Fatalf("%+v %v", c, err)
	}
	c, _ = CredentialsFromSecret(map[string][]byte{".dockerconfigjson": cfg}, "nope.io")
	if c != (Credentials{}) {
		t.Fatalf("unknown host should be anonymous: %+v", c)
	}
	c, _ = CredentialsFromSecret(map[string][]byte{"username": []byte("u"), "password": []byte("p")}, "x")
	if c.Username != "u" || c.Password != "p" {
		t.Fatalf("%+v", c)
	}
}

func TestParseChallenge(t *testing.T) {
	s, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`)
	if s != "Bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" || p["scope"] != "repository:library/nginx:pull" {
		t.Fatalf("%s %v", s, p)
	}
}
