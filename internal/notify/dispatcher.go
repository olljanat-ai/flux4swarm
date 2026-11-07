// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/creds"
	"github.com/olljanat-ai/flux4swarm/internal/git"
)

// DefaultRateLimit is how long an identical event is suppressed, like
// notification-controller's --rate-limit-interval.
const DefaultRateLimit = 5 * time.Minute

// DefaultTimeout is the Provider timeout when spec.timeout is not set.
const DefaultTimeout = 15 * time.Second

// Secret keys read from a Provider's secretRef, proxySecretRef and
// certSecretRef.
var (
	providerSecretKeys = append([]string{"address", "token", "username", "password", "headers", "proxy"},
		git.KeyGitHubAppID, git.KeyGitHubInstallID, git.KeyGitHubPrivateKey, git.KeyGitHubAppBaseURL,
		git.KeyAzureTenantID, git.KeyAzureClientID, git.KeyAzureClientSecret, git.KeyAzureAuthorityURL)
	proxySecretKeys = []string{"address", "username", "password"}
	certSecretKeys  = []string{"ca.crt", "tls.crt", "tls.key", "caFile", "certFile", "keyFile"}
)

// ObjectInfo is the metadata of the involved object that Alerts can match
// (matchLabels) and that adds event metadata (event.toolkit.fluxcd.io/*
// annotations).
type ObjectInfo struct {
	Labels      map[string]string
	Annotations map[string]string
}

// Result reports the outcome of sending one event through one Alert.
type Result struct {
	Alert    string // namespace/name
	Provider string // namespace/name
	Event    Event
	Err      error
}

// Dispatcher matches events against Alerts and sends them to Providers.
type Dispatcher struct {
	Creds *creds.Store
	Log   *slog.Logger
	// RateLimit suppresses identical events; 0 means DefaultRateLimit,
	// a negative value disables it.
	RateLimit time.Duration
	// OnResult, when set, is called after every send attempt.
	OnResult func(Result)
	// Transport is the base HTTP transport (tests may replace it).
	Transport *http.Transport

	mu   sync.Mutex
	seen map[string]time.Time
	auth map[string]*cachedAuth
	wg   sync.WaitGroup
}

type cachedAuth struct {
	fingerprint string
	provider    *git.AuthProvider
}

// Wait blocks until every notification in flight has been sent.
func (d *Dispatcher) Wait() { d.wg.Wait() }

func (d *Dispatcher) log() *slog.Logger {
	if d.Log == nil {
		return slog.Default()
	}
	return d.Log
}

// Dispatch sends ev to every Provider whose Alert matches it. providers is
// keyed by "namespace/name". Sending and OnResult happen in the
// background, so Dispatch may be called with the caller's locks held; call
// Wait to block until everything is sent.
func (d *Dispatcher) Dispatch(ev Event, obj ObjectInfo, alerts []*api.Alert, providers map[string]*api.Provider) {
	if d == nil {
		return
	}
	group := ev.InvolvedObject.Group()
	delete(ev.Metadata, group+"/"+MetaTokenKey)
	if d.limited(ev) {
		d.log().Debug("discarding event, rate limited", "object", involved(ev), "message", ev.Message)
		return
	}
	for _, a := range alerts {
		if a.Spec.Suspend || !alertMatches(a, ev, obj) {
			continue
		}
		providerKey := a.Metadata.Namespace + "/" + a.Spec.ProviderRef.Name
		p := providers[providerKey]
		res := Result{Alert: a.Metadata.NamespacedName(), Provider: providerKey}
		if p == nil {
			res.Err = fmt.Errorf("failed to read provider: Provider '%s' not found", providerKey)
			res.Event = ev
			d.wg.Add(1)
			go func(res Result) {
				defer d.wg.Done()
				d.report(res)
			}(res)
			continue
		}
		if p.Spec.Suspend {
			continue
		}
		commitStatusProvider := p.Spec.Type == TypeGitHub || p.Spec.Type == TypeAzureDevOps
		generic := p.Spec.Type == TypeGeneric || p.Spec.Type == TypeGenericHMAC
		update := ev.Metadata[group+"/"+MetaCommitStatusKey] == MetaCommitStatusValue
		if !commitStatusProvider && !generic && update {
			continue
		}
		if commitStatusProvider && !update && ev.Metadata[EventGroup+"/"+MetaCommitKey] == "" && ev.Severity != SeverityError {
			continue
		}
		n := ev.clone()
		n.Metadata = combineMetadata(ev, obj, a)
		res.Event = n
		d.wg.Add(1)
		go func(p *api.Provider, res Result) {
			defer d.wg.Done()
			res.Err = d.send(p, res.Event)
			d.report(res)
		}(p, res)
	}
}

func (d *Dispatcher) report(r Result) {
	if r.Err != nil {
		d.log().Error("failed to send notification", "alert", r.Alert, "provider", r.Provider,
			"object", involved(r.Event), "error", r.Err)
	} else {
		d.log().Debug("notification sent", "alert", r.Alert, "provider", r.Provider, "object", involved(r.Event))
	}
	if d.OnResult != nil {
		d.OnResult(r)
	}
}

func involved(ev Event) string {
	o := ev.InvolvedObject
	return o.Kind + "/" + o.Namespace + "/" + o.Name
}

// limited reports whether an identical event was dispatched within the
// rate limit interval, and records ev otherwise.
func (d *Dispatcher) limited(ev Event) bool {
	interval := d.RateLimit
	if interval == 0 {
		interval = DefaultRateLimit
	}
	if interval < 0 {
		return false
	}
	key := eventKey(ev)
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil {
		d.seen = map[string]time.Time{}
	}
	for k, t := range d.seen {
		if now.Sub(t) >= interval {
			delete(d.seen, k)
		}
	}
	if _, ok := d.seen[key]; ok {
		return true
	}
	d.seen[key] = now
	return false
}

// eventKey is notification-controller's EventKey.
func eventKey(ev Event) string {
	o := ev.InvolvedObject
	comps := []string{"event", "name=" + o.Name, "namespace=" + o.Namespace, "kind=" + o.Kind, "message=" + ev.Message}
	g := o.Group()
	for _, k := range []string{MetaOriginRevisionKey, MetaRevisionKey, MetaTokenKey} {
		if v, ok := ev.Metadata[g+"/"+k]; ok {
			comps = append(comps, k+"="+v)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(comps, "/")))
	return hex.EncodeToString(sum[:])
}

func alertMatches(a *api.Alert, ev Event, obj ObjectInfo) bool {
	matched := false
	for _, src := range a.Spec.EventSources {
		ns := src.Namespace
		if ns == "" {
			ns = a.Metadata.Namespace
		}
		if ev.InvolvedObject.Namespace != ns || ev.InvolvedObject.Kind != src.Kind {
			continue
		}
		sev := a.Spec.EventSeverity
		if sev == "" {
			sev = SeverityInfo
		}
		if ev.Severity != sev && sev != SeverityInfo {
			continue
		}
		if src.Name != "*" && src.Name != ev.InvolvedObject.Name {
			continue
		}
		if src.MatchLabels != nil && !labelsMatch(src.MatchLabels, obj.Labels) {
			continue
		}
		matched = true
		break
	}
	if !matched {
		return false
	}
	if len(a.Spec.InclusionList) > 0 {
		inc := false
		for _, exp := range a.Spec.InclusionList {
			if r, err := regexp.Compile(exp); err == nil && r.MatchString(ev.Message) {
				inc = true
				break
			}
		}
		if !inc {
			return false
		}
	}
	for _, exp := range a.Spec.ExclusionList {
		if r, err := regexp.Compile(exp); err == nil && r.MatchString(ev.Message) {
			return false
		}
	}
	return true
}

func labelsMatch(want, have map[string]string) bool {
	for k, v := range want {
		if hv, ok := have[k]; !ok || hv != v {
			return false
		}
	}
	return true
}

// combineMetadata builds the metadata a Provider receives, from lowest to
// highest precedence: event.toolkit.fluxcd.io/ annotations of the involved
// object, the Alert's eventMetadata (and deprecated summary) and the
// controller's <group>/ metadata. Prefixes are removed and other keys
// dropped, like notification-controller.
func combineMetadata(ev Event, obj ObjectInfo, a *api.Alert) map[string]string {
	out := map[string]string{}
	const evPrefix = EventGroup + "/"
	for k, v := range obj.Annotations {
		if strings.HasPrefix(k, evPrefix) {
			out[strings.TrimPrefix(k, evPrefix)] = v
		}
	}
	for k, v := range ev.Metadata {
		if strings.HasPrefix(k, evPrefix) {
			out[strings.TrimPrefix(k, evPrefix)] = v
		}
	}
	if a.Spec.Summary != "" {
		out[MetaSummaryKey] = a.Spec.Summary
	}
	for k, v := range a.Spec.EventMetadata {
		out[k] = v
	}
	prefix := ev.InvolvedObject.Group() + "/"
	for k, v := range ev.Metadata {
		if strings.HasPrefix(k, prefix) {
			out[strings.TrimPrefix(k, prefix)] = v
		}
	}
	return out
}

// CommitStatusID is Flux's default commit status context
// "<kind>/<name>/<provider id>". Swarm objects have no UID, so the id is
// derived from the Provider's namespace and name.
func CommitStatusID(p *api.Provider, ev Event) string {
	sum := sha256.Sum256([]byte(p.Metadata.Namespace + "/" + p.Metadata.Name))
	return strings.ToLower(fmt.Sprintf("%s/%s/%s", ev.InvolvedObject.Kind, ev.InvolvedObject.Name, hex.EncodeToString(sum[:4])))
}

func (d *Dispatcher) send(p *api.Provider, ev Event) error {
	timeout := DefaultTimeout
	if p.Spec.Timeout != nil && p.Spec.Timeout.Duration > 0 {
		timeout = p.Spec.Timeout.Duration
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	t, err := d.target(p)
	if err != nil {
		return fmt.Errorf("failed to initialize notifier for provider '%s': %w", p.Metadata.Name, err)
	}
	if t.Type == TypeGitHub || t.Type == TypeAzureDevOps {
		if p.Spec.CommitStatusExpr != "" {
			d.log().Warn("spec.commitStatusExpr is not supported by flux4swarm, using the default commit status",
				"provider", p.Metadata.NamespacedName())
		}
		t.CommitStatus = CommitStatusID(p, ev)
	}
	s, err := newSender(t)
	if err != nil {
		return fmt.Errorf("failed to initialize notifier for provider '%s': %w", p.Metadata.Name, err)
	}
	if err := s.Post(ctx, ev); err != nil {
		return maskSecrets(fmt.Errorf("failed to send notification for %s: %w", involved(ev), err), t)
	}
	return nil
}

// ValidateProvider checks that a Provider has a supported type and that its
// secrets, proxy and TLS settings resolve.
func (d *Dispatcher) ValidateProvider(p *api.Provider) error {
	t, err := d.target(p)
	if err != nil {
		return err
	}
	if t.Type == TypeGitHub || t.Type == TypeAzureDevOps {
		t.CommitStatus = "validate"
		if t.Token == "" && t.TokenFunc == nil {
			return fmt.Errorf("%s provider needs a secretRef with a token", t.Type)
		}
	}
	_, err = newSender(t)
	return err
}

// target resolves a Provider's secrets, proxy and TLS settings.
func (d *Dispatcher) target(p *api.Provider) (*target, error) {
	t := &target{
		Type:     p.Spec.Type,
		URL:      strings.TrimSpace(p.Spec.Address),
		Channel:  p.Spec.Channel,
		Username: p.Spec.Username,
	}
	ns := p.Metadata.Namespace
	var secret map[string][]byte
	if p.Spec.SecretRef != nil {
		secret = d.Creds.Lookup(p.Spec.SecretRef.Name, providerSecretKeys...)
		if len(secret) == 0 {
			return nil, fmt.Errorf("failed to get secret '%s/%s': not found in the credentials directory", ns, p.Spec.SecretRef.Name)
		}
		get := func(k string) string { return strings.TrimSpace(string(secret[k])) }
		if v := get("address"); v != "" {
			t.URL = v
		}
		t.Token = get("token")
		if v := get("username"); v != "" {
			t.Username = v
		}
		t.Password = get("password")
		if h, ok := secret["headers"]; ok {
			headers := map[string]string{}
			if err := yaml.Unmarshal(h, &headers); err != nil {
				return nil, fmt.Errorf("failed to read headers from secret: %w", err)
			}
			t.Headers = headers
		}
		if t.Token == "" {
			switch {
			case t.Type == TypeGitHub && get(git.KeyGitHubAppID) != "":
				t.TokenFunc = d.tokenFunc(p, git.ProviderGitHub, secret)
			case t.Type == TypeAzureDevOps && get(git.KeyAzureClientID) != "":
				t.TokenFunc = d.tokenFunc(p, git.ProviderAzure, secret)
				t.Bearer = true
			}
		}
	}
	tr := d.Transport
	if tr == nil {
		tr = http.DefaultTransport.(*http.Transport)
	}
	tr = tr.Clone()
	proxy := p.Spec.Proxy
	if secret != nil && len(secret["proxy"]) > 0 {
		proxy = strings.TrimSpace(string(secret["proxy"]))
	}
	var proxyUser, proxyPass string
	if p.Spec.ProxySecretRef != nil {
		ps := d.Creds.Lookup(p.Spec.ProxySecretRef.Name, proxySecretKeys...)
		if len(ps["address"]) == 0 {
			return nil, fmt.Errorf("proxy secret '%s/%s' needs an 'address' key", ns, p.Spec.ProxySecretRef.Name)
		}
		proxy = strings.TrimSpace(string(ps["address"]))
		proxyUser, proxyPass = strings.TrimSpace(string(ps["username"])), strings.TrimSpace(string(ps["password"]))
	}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("invalid proxy URL")
		}
		if proxyUser != "" {
			u.User = url.UserPassword(proxyUser, proxyPass)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	if p.Spec.CertSecretRef != nil {
		cs := d.Creds.Lookup(p.Spec.CertSecretRef.Name, certSecretKeys...)
		cfg, err := tlsConfig(cs)
		if err != nil {
			return nil, fmt.Errorf("failed to get TLS config from secret '%s/%s': %w", ns, p.Spec.CertSecretRef.Name, err)
		}
		tr.TLSClientConfig = cfg
	}
	t.Client = &http.Client{Transport: tr}
	if t.URL == "" && t.Type != TypeGitHub && t.Type != TypeAzureDevOps {
		return nil, fmt.Errorf("provider has no address (set spec.address or 'address' in the secret)")
	}
	return t, nil
}

func (d *Dispatcher) tokenFunc(p *api.Provider, provider string, secret map[string][]byte) func(context.Context) (string, error) {
	h := sha256.New()
	for _, k := range providerSecretKeys {
		h.Write([]byte(k + "=" + string(secret[k]) + "\n"))
	}
	fp := hex.EncodeToString(h.Sum(nil))
	key := p.Metadata.NamespacedName()
	d.mu.Lock()
	if d.auth == nil {
		d.auth = map[string]*cachedAuth{}
	}
	c := d.auth[key]
	if c == nil || c.fingerprint != fp {
		c = &cachedAuth{fingerprint: fp, provider: &git.AuthProvider{Provider: provider, URL: p.Spec.Address, Secret: secret}}
		d.auth[key] = c
	}
	d.mu.Unlock()
	return c.provider.Token
}

func tlsConfig(s map[string][]byte) (*tls.Config, error) {
	pick := func(a, b string) []byte {
		if v := s[a]; len(v) > 0 {
			return v
		}
		return s[b]
	}
	ca, crt, key := pick("ca.crt", "caFile"), pick("tls.crt", "certFile"), pick("tls.key", "keyFile")
	if len(ca) == 0 && len(crt) == 0 {
		return nil, fmt.Errorf("no ca.crt or tls.crt/tls.key")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(ca) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("ca.crt has no valid PEM certificates")
		}
		cfg.RootCAs = pool
	}
	if len(crt) > 0 || len(key) > 0 {
		pair, err := tls.X509KeyPair(crt, key)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// maskSecrets removes the token and webhook address from an error message.
func maskSecrets(err error, t *target) error {
	msg := err.Error()
	if t.URL != "" && t.Type != TypeGitHub && t.Type != TypeAzureDevOps {
		msg = strings.ReplaceAll(msg, t.URL, redactURL(t.URL))
		if u, perr := url.Parse(t.URL); perr == nil && u.Path != "" {
			msg = strings.ReplaceAll(msg, u.Path, "/***")
		}
	}
	if t.Token != "" {
		msg = strings.ReplaceAll(msg, t.Token, "*****")
	}
	if msg == err.Error() {
		return err
	}
	return fmt.Errorf("%s", msg)
}
