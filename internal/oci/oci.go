// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package oci pulls Flux OCI artifacts (`flux push artifact`) from an OCI
// distribution registry, like Flux's OCIRepository. It speaks the
// distribution HTTP API directly: anonymous, basic and bearer token auth,
// tag listing for semver selection, and extraction of one tar+gzip layer.
package oci

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/olljanat-ai/flux4swarm/internal/source"
)

// Media types of Flux artifacts.
const (
	MediaTypeFluxConfig  = "application/vnd.cncf.flux.config.v1+json"
	MediaTypeFluxContent = "application/vnd.cncf.flux.content.v1.tar+gzip"

	mediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

// Limits protecting the controller from huge or hostile artifacts.
const (
	maxManifestSize = 4 << 20
	// MaxUnpackedSize is the largest total size of the extracted files.
	MaxUnpackedSize = 1 << 30
)

// Ref selects the artifact. Precedence is Digest, SemVer, Tag; without any
// of them the tag "latest" is used.
type Ref struct {
	Digest       string
	SemVer       string
	SemverFilter string
	Tag          string
}

// Credentials authenticate to the registry. An empty value is anonymous.
type Credentials struct {
	Username, Password string
	// IdentityToken is an OAuth2 refresh token (docker config "identitytoken").
	IdentityToken string
	// RegistryToken is a bearer token sent as is.
	RegistryToken string
}

// Fetcher keeps one unpacked artifact per source in a cache directory.
type Fetcher struct {
	// URL is oci://<host>/<repository>.
	URL string
	Ref Ref
	// MediaType of the layer to extract; empty means the first layer.
	MediaType string
	Creds     Credentials
	// Insecure uses plain HTTP.
	Insecure bool
	// TLS configures HTTPS (custom CA, client certificate); may be nil.
	TLS *tls.Config
	// Dir is the cache directory for this source.
	Dir string
	// Client overrides the HTTP client (tests).
	Client *http.Client

	mu    sync.Mutex
	token string
}

// Descriptor is an OCI content descriptor.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Manifest is an OCI image manifest.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// ParseURL splits oci://host/repo into the registry host and repository.
func ParseURL(u string) (host, repo string, err error) {
	rest, ok := strings.CutPrefix(u, "oci://")
	if !ok {
		return "", "", fmt.Errorf("url %q must start with oci://", u)
	}
	host, repo, ok = strings.Cut(rest, "/")
	if !ok || host == "" || repo == "" {
		return "", "", fmt.Errorf("url %q must be oci://<host>/<repository>", u)
	}
	if strings.ContainsAny(repo, ":@") {
		return "", "", fmt.Errorf("url %q must not contain a tag or digest; use spec.ref", u)
	}
	if host == "docker.io" || host == "index.docker.io" {
		host = "registry-1.docker.io"
		if !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	}
	return host, repo, nil
}

func (f *Fetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if f.TLS != nil {
		tr.TLSClientConfig = f.TLS
	}
	return &http.Client{Transport: tr, Timeout: 10 * time.Minute}
}

func (f *Fetcher) base() (string, string, error) {
	host, repo, err := ParseURL(f.URL)
	if err != nil {
		return "", "", err
	}
	scheme := "https"
	if f.Insecure {
		scheme = "http"
	}
	return scheme + "://" + host + "/v2/" + repo, repo, nil
}

// Resolve returns the tag (empty for a digest ref) and the manifest digest
// the ref points at.
func (f *Fetcher) Resolve(ctx context.Context) (tag, digest string, err error) {
	switch {
	case f.Ref.Digest != "":
		if _, _, err := splitDigest(f.Ref.Digest); err != nil {
			return "", "", err
		}
		return "", f.Ref.Digest, nil
	case f.Ref.SemVer != "":
		tags, err := f.Tags(ctx)
		if err != nil {
			return "", "", err
		}
		if tag, err = source.LatestVersion(tags, f.Ref.SemVer, f.Ref.SemverFilter); err != nil {
			return "", "", fmt.Errorf("%s: %w", f.URL, err)
		}
	case f.Ref.Tag != "":
		tag = f.Ref.Tag
	default:
		tag = "latest"
	}
	_, digest, err = f.manifest(ctx, tag, http.MethodHead)
	return tag, digest, err
}

// Fetch makes sure the cache holds the artifact the ref points at and
// returns it. If last is not nil and the digest has not changed, last is
// returned as is.
func (f *Fetcher) Fetch(ctx context.Context, last *source.Artifact) (*source.Artifact, error) {
	tag, digest, err := f.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	rev := digest
	if tag != "" {
		rev = tag + "@" + digest
	}
	if last != nil && last.Commit == digest && last.Revision == rev {
		if _, err := os.Stat(last.Dir); err == nil {
			return last, nil
		}
	}
	m, _, err := f.manifest(ctx, digest, http.MethodGet)
	if err != nil {
		return nil, err
	}
	layer, err := selectLayer(m, f.MediaType)
	if err != nil {
		return nil, fmt.Errorf("%s@%s: %w", f.URL, digest, err)
	}
	if err := os.MkdirAll(f.Dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(f.Dir, "pull-")
	if err != nil {
		return nil, err
	}
	if err := f.extractBlob(ctx, layer, tmp); err != nil {
		os.RemoveAll(tmp)
		return nil, fmt.Errorf("%s@%s: layer %s: %w", f.URL, digest, layer.Digest, err)
	}
	_, hexDigest, _ := splitDigest(digest)
	final := filepath.Join(f.Dir, "rev-"+hexDigest[:12])
	os.RemoveAll(final)
	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	entries, _ := os.ReadDir(f.Dir)
	for _, e := range entries {
		if p := filepath.Join(f.Dir, e.Name()); p != final {
			os.RemoveAll(p)
		}
	}
	return &source.Artifact{Dir: final, Revision: rev, Commit: digest, FetchedAt: time.Now()}, nil
}

func selectLayer(m *Manifest, mediaType string) (Descriptor, error) {
	if len(m.Layers) == 0 {
		return Descriptor{}, errors.New("artifact has no layers")
	}
	if mediaType == "" {
		return m.Layers[0], nil
	}
	for _, l := range m.Layers {
		if l.MediaType == mediaType {
			return l, nil
		}
	}
	return Descriptor{}, fmt.Errorf("no layer with media type %s", mediaType)
}

// Tags lists the repository's tags, following pagination.
func (f *Fetcher) Tags(ctx context.Context) ([]string, error) {
	base, _, err := f.base()
	if err != nil {
		return nil, err
	}
	var tags []string
	next := base + "/tags/list?n=1000"
	for next != "" {
		resp, err := f.do(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, maxManifestSize)).Decode(&page)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list tags of %s: %w", f.URL, err)
		}
		tags = append(tags, page.Tags...)
		next = ""
		if link != "" {
			if next, err = nextLink(base, link); err != nil {
				return nil, err
			}
		}
	}
	return tags, nil
}

// nextLink resolves a `Link: </v2/...>; rel="next"` header.
func nextLink(base, link string) (string, error) {
	start, end := strings.Index(link, "<"), strings.Index(link, ">")
	if start < 0 || end < start || !strings.Contains(link[end:], `rel="next"`) {
		return "", nil
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u, err := b.Parse(link[start+1 : end])
	if err != nil {
		return "", fmt.Errorf("bad Link header %q: %w", link, err)
	}
	return u.String(), nil
}

// manifest fetches (GET) or resolves (HEAD) a manifest by tag or digest and
// returns it (GET only) with its digest.
func (f *Fetcher) manifest(ctx context.Context, ref, method string) (*Manifest, string, error) {
	base, _, err := f.base()
	if err != nil {
		return nil, "", err
	}
	h := http.Header{"Accept": []string{mediaTypeOCIManifest + ", " + mediaTypeDockerManifest}}
	resp, err := f.do(ctx, method, base+"/manifests/"+url.PathEscape(ref), h)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	digest := resp.Header.Get("Docker-Content-Digest")
	if method == http.MethodHead {
		if digest == "" {
			// Some registries omit the header on HEAD; fall back to GET.
			m, d, err := f.manifest(ctx, ref, http.MethodGet)
			return m, d, err
		}
		return nil, digest, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxManifestSize {
		return nil, "", fmt.Errorf("manifest %s of %s is too large", ref, f.URL)
	}
	sum := sha256.Sum256(body)
	computed := "sha256:" + hex.EncodeToString(sum[:])
	if strings.HasPrefix(ref, "sha256:") && ref != computed {
		return nil, "", fmt.Errorf("manifest %s of %s has digest %s", ref, f.URL, computed)
	}
	if digest == "" || strings.HasPrefix(digest, "sha256:") {
		digest = computed
	}
	m := &Manifest{}
	if err := json.Unmarshal(body, m); err != nil {
		return nil, "", fmt.Errorf("manifest %s of %s: %w", ref, f.URL, err)
	}
	if m.MediaType == "application/vnd.oci.image.index.v1+json" || m.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		return nil, "", fmt.Errorf("%s:%s is an image index, not a Flux artifact", f.URL, ref)
	}
	return m, digest, nil
}

// extractBlob downloads a tar+gzip blob, verifies its digest and unpacks it
// into dir.
func (f *Fetcher) extractBlob(ctx context.Context, d Descriptor, dir string) error {
	algo, want, err := splitDigest(d.Digest)
	if err != nil {
		return err
	}
	if algo != "sha256" {
		return fmt.Errorf("unsupported digest algorithm %s", algo)
	}
	base, _, err := f.base()
	if err != nil {
		return err
	}
	resp, err := f.do(ctx, http.MethodGet, base+"/blobs/"+d.Digest, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	h := sha256.New()
	if err := untar(io.TeeReader(resp.Body, h), dir); err != nil {
		return err
	}
	// Drain what the tar reader did not consume so the digest covers the
	// whole blob.
	if _, err := io.Copy(h, resp.Body); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("digest mismatch: got sha256:%s", got)
	}
	return nil
}

func splitDigest(d string) (algo, hexPart string, err error) {
	algo, hexPart, ok := strings.Cut(d, ":")
	if !ok || algo == "" || len(hexPart) < 12 {
		return "", "", fmt.Errorf("invalid digest %q", d)
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return "", "", fmt.Errorf("invalid digest %q", d)
	}
	return algo, hexPart, nil
}

// untar extracts a gzip compressed tarball into dir. Entries that would
// escape dir, links and special files are rejected.
func untar(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("layer is not gzip compressed: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(strings.TrimPrefix(hdr.Name, "/"))
		clean := filepath.Clean(name)
		if clean == "." {
			continue
		}
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
			return fmt.Errorf("tar entry %q escapes the artifact root", hdr.Name)
		}
		target := filepath.Join(dir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > MaxUnpackedSize {
				return fmt.Errorf("artifact is larger than %d bytes", MaxUnpackedSize)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(tr, hdr.Size))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
		default:
			return fmt.Errorf("tar entry %q has unsupported type %q (links and special files are not allowed)", hdr.Name, hdr.Typeflag)
		}
	}
}

// ---- HTTP and auth ----

// do sends a request, answering a 401 challenge once (basic or bearer).
func (f *Fetcher) do(ctx context.Context, method, u string, h http.Header) (*http.Response, error) {
	send := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range h {
			req.Header[k] = v
		}
		f.mu.Lock()
		tok := f.token
		f.mu.Unlock()
		switch {
		case tok != "":
			req.Header.Set("Authorization", tok)
		case f.Creds.RegistryToken != "":
			req.Header.Set("Authorization", "Bearer "+f.Creds.RegistryToken)
		}
		return f.client().Do(req)
	}
	resp, err := send()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, redact(u), err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		if err := f.authenticate(ctx, challenge); err != nil {
			return nil, fmt.Errorf("%s %s: %w", method, redact(u), err)
		}
		if resp, err = send(); err != nil {
			return nil, fmt.Errorf("%s %s: %w", method, redact(u), err)
		}
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("%s %s: %s %s", method, redact(u), resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

// authenticate answers a WWW-Authenticate challenge and stores the
// Authorization header to use.
func (f *Fetcher) authenticate(ctx context.Context, challenge string) error {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if f.Creds.Username == "" && f.Creds.Password == "" {
			return errors.New("registry requires credentials (set spec.secretRef)")
		}
		f.setToken("Basic " + base64.StdEncoding.EncodeToString([]byte(f.Creds.Username+":"+f.Creds.Password)))
		return nil
	case "bearer":
	default:
		return fmt.Errorf("unsupported auth challenge %q", challenge)
	}
	realm := params["realm"]
	if realm == "" {
		return fmt.Errorf("bearer challenge without realm: %q", challenge)
	}
	_, repo, err := f.base()
	if err != nil {
		return err
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)

	var req *http.Request
	if f.Creds.IdentityToken != "" {
		// OAuth2 refresh token flow (docker config identitytoken).
		q.Set("grant_type", "refresh_token")
		q.Set("refresh_token", f.Creds.IdentityToken)
		q.Set("client_id", "flux4swarm")
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, realm, strings.NewReader(q.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		sep := "?"
		if strings.Contains(realm, "?") {
			sep = "&"
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, realm+sep+q.Encode(), nil)
		if err == nil && (f.Creds.Username != "" || f.Creds.Password != "") {
			req.SetBasicAuth(f.Creds.Username, f.Creds.Password)
		}
	}
	if err != nil {
		return err
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("token request to %s: %s %s", realm, resp.Status, strings.TrimSpace(string(msg)))
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return fmt.Errorf("token response: %w", err)
	}
	tok := tr.Token
	if tok == "" {
		tok = tr.AccessToken
	}
	if tok == "" {
		return errors.New("token response has no token")
	}
	f.setToken("Bearer " + tok)
	return nil
}

func (f *Fetcher) setToken(t string) {
	f.mu.Lock()
	f.token = t
	f.mu.Unlock()
}

// parseChallenge parses `Bearer realm="...",service="...",scope="..."`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(v, `"`) {
			end := strings.Index(v[1:], `"`)
			if end < 0 {
				params[k] = v[1:]
				break
			}
			params[k] = v[1 : end+1]
			rest = v[end+2:]
		} else {
			val, r, _ := strings.Cut(v, ",")
			params[k] = strings.TrimSpace(val)
			rest = r
		}
	}
	return scheme, params
}

func redact(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		return u[:i]
	}
	return u
}

// ---- credentials ----

// SecretKeys are the keys read from spec.secretRef and spec.certSecretRef.
var SecretKeys = []string{".dockerconfigjson", "username", "password", "ca.crt", "tls.crt", "tls.key", "caFile", "certFile", "keyFile"}

// CredentialsFromSecret reads a kubernetes.io/dockerconfigjson secret
// (".dockerconfigjson") or a basic auth secret ("username", "password") for
// host.
func CredentialsFromSecret(secret map[string][]byte, host string) (Credentials, error) {
	if b := secret[".dockerconfigjson"]; len(b) > 0 {
		return credentialsFromDockerConfig(b, host)
	}
	return Credentials{Username: string(secret["username"]), Password: string(secret["password"])}, nil
}

func credentialsFromDockerConfig(b []byte, host string) (Credentials, error) {
	var cfg struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			Username      string `json:"username"`
			Password      string `json:"password"`
			IdentityToken string `json:"identitytoken"`
			RegistryToken string `json:"registrytoken"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Credentials{}, fmt.Errorf(".dockerconfigjson: %w", err)
	}
	for k, a := range cfg.Auths {
		if NormalizeHost(k) != NormalizeHost(host) {
			continue
		}
		c := Credentials{Username: a.Username, Password: a.Password, IdentityToken: a.IdentityToken, RegistryToken: a.RegistryToken}
		if a.Auth != "" {
			dec, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				return Credentials{}, fmt.Errorf(".dockerconfigjson: auth for %s: %w", k, err)
			}
			c.Username, c.Password, _ = strings.Cut(string(dec), ":")
		}
		return c, nil
	}
	return Credentials{}, nil
}

// NormalizeHost strips the scheme and path of a docker config key and maps
// Docker Hub aliases to registry-1.docker.io.
func NormalizeHost(h string) string {
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h, _, _ = strings.Cut(h, "/")
	switch h {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return "registry-1.docker.io"
	}
	return h
}

// TLSFromSecret builds a TLS config from a certSecretRef secret (ca.crt,
// tls.crt and tls.key, or the legacy caFile, certFile and keyFile). It
// returns nil when the secret holds none of them.
func TLSFromSecret(secret map[string][]byte) (*tls.Config, error) {
	pick := func(keys ...string) []byte {
		for _, k := range keys {
			if v := secret[k]; len(v) > 0 {
				return v
			}
		}
		return nil
	}
	ca, crt, key := pick("ca.crt", "caFile"), pick("tls.crt", "certFile"), pick("tls.key", "keyFile")
	if ca == nil && crt == nil {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if ca != nil {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("certSecretRef: ca.crt has no PEM certificates")
		}
		cfg.RootCAs = pool
	}
	if crt != nil {
		pair, err := tls.X509KeyPair(crt, key)
		if err != nil {
			return nil, fmt.Errorf("certSecretRef: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}
