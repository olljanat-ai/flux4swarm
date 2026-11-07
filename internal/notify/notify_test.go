// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/creds"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func init() { retryWait = time.Millisecond }

type captured struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

type recorder struct {
	mu   sync.Mutex
	reqs []captured
	// reply returns status and body for a request.
	reply func(r *http.Request) (int, string)
}

func newServer(t *testing.T, rec *recorder) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, captured{Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(), Body: b})
		rec.mu.Unlock()
		code, body := http.StatusOK, ""
		if rec.reply != nil {
			code, body = rec.reply(r)
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *recorder) all() []captured {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]captured(nil), r.reqs...)
}

func provider(name, typ, addr string) *api.Provider {
	return &api.Provider{
		TypeMeta: api.TypeMeta{APIVersion: api.NotificationGroupVersion, Kind: api.KindProvider},
		Metadata: api.ObjectMeta{Name: name, Namespace: "flux-system"},
		Spec:     api.ProviderSpec{Type: typ, Address: addr},
	}
}

func alert(name, providerName string, sources ...api.NotificationObjectRef) *api.Alert {
	return &api.Alert{
		TypeMeta: api.TypeMeta{APIVersion: api.NotificationGroupVersion, Kind: api.KindAlert},
		Metadata: api.ObjectMeta{Name: name, Namespace: "flux-system"},
		Spec:     api.AlertSpec{ProviderRef: api.LocalObjectReference{Name: providerName}, EventSources: sources},
	}
}

func ksEvent(severity, reason, msg string, commitStatus bool) Event {
	md := map[string]string{"kustomize.toolkit.fluxcd.io/revision": "main@sha1:" + sha}
	if commitStatus {
		md["kustomize.toolkit.fluxcd.io/commit_status"] = "update"
	}
	return Event{
		InvolvedObject:      ObjectReference{Kind: "Kustomization", Namespace: "flux-system", Name: "apps", APIVersion: "kustomize.toolkit.fluxcd.io/v1"},
		Severity:            severity,
		Timestamp:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Message:             msg,
		Reason:              reason,
		Action:              ActionReconciled,
		Metadata:            md,
		ReportingController: "kustomize-controller",
	}
}

func dispatch(t *testing.T, d *Dispatcher, ev Event, obj ObjectInfo, alerts []*api.Alert, providers ...*api.Provider) []Result {
	t.Helper()
	var mu sync.Mutex
	var results []Result
	d.OnResult = func(r Result) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	}
	pm := map[string]*api.Provider{}
	for _, p := range providers {
		pm[p.Metadata.NamespacedName()] = p
	}
	d.Dispatch(ev, obj, alerts, pm)
	d.Wait()
	return results
}

var allKs = api.NotificationObjectRef{Kind: "Kustomization", Name: "*"}

func TestSlack(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec)
	d := &Dispatcher{Creds: creds.New("")}
	p := provider("slack", TypeSlack, srv.URL+"/services/T/B/X")
	p.Spec.Channel = "general"
	a := alert("a", "slack", allKs)
	a.Spec.EventMetadata = map[string]string{"cluster": "prod"}
	res := dispatch(t, d, ksEvent(SeverityError, "BuildFailed", "kustomize build failed\nline 2", false), ObjectInfo{
		Annotations: map[string]string{"event.toolkit.fluxcd.io/team": "ops"},
	}, []*api.Alert{a}, p)
	if len(res) != 1 || res[0].Err != nil {
		t.Fatalf("results: %+v", res)
	}
	reqs := rec.all()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests", len(reqs))
	}
	var got slackPayload
	if err := json.Unmarshal(reqs[0].Body, &got); err != nil {
		t.Fatal(err)
	}
	at := got.Attachments[0]
	if got.Channel != "general" || got.Username != "kustomize-controller" || at.Color != "danger" ||
		at.AuthorName != "kustomization/apps.flux-system" || at.Fallback != "kustomization/apps.flux-system: kustomize build failed" {
		t.Errorf("payload: %s", reqs[0].Body)
	}
	fields := map[string]string{}
	for _, f := range at.Fields {
		fields[f.Title] = f.Value
	}
	want := map[string]string{"revision": "main@sha1:" + sha, "cluster": "prod", "team": "ops"}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("field %s = %q, want %q (fields %v)", k, fields[k], v, fields)
		}
	}
}

func TestGenericHMACAndHeaders(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec)
	store := creds.New("")
	store.Set("hook", "token", []byte("s3cret"))
	store.Set("hook", "headers", []byte("X-Team: ops\n"))
	d := &Dispatcher{Creds: store}
	p := provider("hook", TypeGenericHMAC, srv.URL+"/events")
	p.Spec.SecretRef = &api.LocalObjectReference{Name: "hook"}
	res := dispatch(t, d, ksEvent(SeverityInfo, "ReconciliationSucceeded", "done", true), ObjectInfo{}, []*api.Alert{alert("a", "hook", allKs)}, p)
	if len(res) != 1 || res[0].Err != nil {
		t.Fatalf("results: %+v", res)
	}
	r := rec.all()[0]
	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write(r.Body)
	if want := "sha256=" + hex.EncodeToString(m.Sum(nil)); r.Header.Get("X-Signature") != want {
		t.Errorf("X-Signature = %q, want %q", r.Header.Get("X-Signature"), want)
	}
	if r.Header.Get("X-Team") != "ops" || r.Header.Get(NotificationHeader) != "kustomize-controller" {
		t.Errorf("headers: %v", r.Header)
	}
	var ev Event
	if err := json.Unmarshal(r.Body, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.InvolvedObject.Name != "apps" || ev.Metadata["revision"] != "main@sha1:"+sha || ev.Severity != "info" {
		t.Errorf("event: %s", r.Body)
	}
	if !strings.Contains(string(r.Body), `"timestamp":"2026-01-02T03:04:05Z"`) {
		t.Errorf("timestamp format: %s", r.Body)
	}
}

func TestMSTeams(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec)
	d := &Dispatcher{Creds: creds.New("")}
	p := provider("teams", TypeMSTeams, srv.URL+"/workflows/x")
	res := dispatch(t, d, ksEvent(SeverityError, "HealthCheckFailed", "timeout", false), ObjectInfo{}, []*api.Alert{alert("a", "teams", allKs)}, p)
	if len(res) != 1 || res[0].Err != nil {
		t.Fatalf("results: %+v", res)
	}
	// Commit status updates are not sent to chat providers, like Flux.
	res = dispatch(t, d, ksEvent(SeverityInfo, "ReconciliationSucceeded", "done", true), ObjectInfo{}, []*api.Alert{alert("a", "teams", allKs)}, p)
	if len(res) != 0 || len(rec.all()) != 1 {
		t.Fatalf("commit status event was sent to msteams: %+v", res)
	}
	body := string(rec.all()[0].Body)
	for _, s := range []string{`"type":"message"`, `"contentType":"application/vnd.microsoft.card.adaptive"`, `"color":"attention"`, `"text":"kustomization/apps.flux-system"`} {
		if !strings.Contains(body, s) {
			t.Errorf("payload has no %s: %s", s, body)
		}
	}
	s, _ := newSender(&target{Type: TypeMSTeams, URL: "https://acme.webhook.office.com/webhookb2/x"})
	if !s.(*msteams).legacy {
		t.Error("office.com webhooks should use the MessageCard schema")
	}
}

func TestGitHubCommitStatus(t *testing.T) {
	rec := &recorder{}
	existing := "[]"
	rec.reply = func(r *http.Request) (int, string) {
		if r.Method == http.MethodGet {
			return 200, existing
		}
		return 201, "{}"
	}
	srv := newServer(t, rec)
	store := creds.New("")
	store.Set("gh", "token", []byte("ghp_x"))
	d := &Dispatcher{Creds: store, RateLimit: -1}
	p := provider("github", TypeGitHub, srv.URL+"/acme/fleet.git")
	p.Spec.SecretRef = &api.LocalObjectReference{Name: "gh"}
	alerts := []*api.Alert{alert("a", "github", allKs)}

	// Progressing and info events without commit_status are not sent.
	dispatch(t, d, ksEvent(SeverityInfo, ReasonProgressing, "Service/x created", false), ObjectInfo{}, alerts, p)
	dispatch(t, d, ksEvent(SeverityInfo, "ReconciliationSucceeded", "done", false), ObjectInfo{}, alerts, p)
	if n := len(rec.all()); n != 0 {
		t.Fatalf("got %d requests, want none", n)
	}

	res := dispatch(t, d, ksEvent(SeverityInfo, "ReconciliationSucceeded", "done", true), ObjectInfo{}, alerts, p)
	if len(res) != 1 || res[0].Err != nil {
		t.Fatalf("results: %+v", res)
	}
	reqs := rec.all()
	if len(reqs) != 2 || reqs[0].Path != "/api/v3/repos/acme/fleet/commits/"+sha+"/statuses?per_page=50" ||
		reqs[1].Path != "/api/v3/repos/acme/fleet/statuses/"+sha || reqs[1].Header.Get("Authorization") != "Bearer ghp_x" {
		t.Fatalf("requests: %+v", reqs)
	}
	var st githubStatus
	_ = json.Unmarshal(reqs[1].Body, &st)
	id := CommitStatusID(p, ksEvent("", "", "", false))
	if st.State != "success" || st.Description != "reconciliation succeeded" || st.Context != id || !strings.HasPrefix(id, "kustomization/apps/") {
		t.Errorf("status: %+v", st)
	}

	// Same status already present: nothing is posted.
	b, _ := json.Marshal([]githubStatus{st})
	existing = string(b)
	dispatch(t, d, ksEvent(SeverityInfo, "ReconciliationSucceeded", "done", true), ObjectInfo{}, alerts, p)
	if n := len(rec.all()); n != 3 {
		t.Errorf("got %d requests, want 3 (only the list call)", n)
	}

	// Failures are posted as failure.
	dispatch(t, d, ksEvent(SeverityError, "HealthCheckFailed", "boom", true), ObjectInfo{}, alerts, p)
	reqs = rec.all()
	_ = json.Unmarshal(reqs[len(reqs)-1].Body, &st)
	if st.State != "failure" || st.Description != "health check failed" {
		t.Errorf("failure status: %+v", st)
	}
}

func TestAzureDevOpsCommitStatus(t *testing.T) {
	rec := &recorder{}
	rec.reply = func(r *http.Request) (int, string) {
		if r.Method == http.MethodGet {
			return 200, `{"value":[]}`
		}
		return 201, "{}"
	}
	srv := newServer(t, rec)
	store := creds.New("")
	store.Set("ado", "token", []byte("pat"))
	d := &Dispatcher{Creds: store}
	p := provider("ado", TypeAzureDevOps, srv.URL+"/acme/platform/_git/fleet")
	p.Spec.SecretRef = &api.LocalObjectReference{Name: "ado"}
	a := alert("a", "ado", allKs)
	a.Spec.EventMetadata = map[string]string{"summary": "prod"}
	res := dispatch(t, d, ksEvent(SeverityError, "BuildFailed", "boom", true), ObjectInfo{}, []*api.Alert{a}, p)
	if len(res) != 1 || res[0].Err != nil {
		t.Fatalf("results: %+v", res)
	}
	reqs := rec.all()
	wantPath := "/acme/platform/_apis/git/repositories/fleet/commits/" + sha + "/statuses?api-version=6.0"
	if len(reqs) != 2 || reqs[1].Path != wantPath || reqs[1].Method != http.MethodPost {
		t.Fatalf("requests: %+v", reqs)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte(":pat")); reqs[1].Header.Get("Authorization") != want {
		t.Errorf("auth %q", reqs[1].Header.Get("Authorization"))
	}
	var st azureStatus
	_ = json.Unmarshal(reqs[1].Body, &st)
	if st.State != "error" || st.Context.Genre != "fluxcd:prod" || st.Description != "build failed" || !strings.HasPrefix(st.Context.Name, "kustomization/apps/") {
		t.Errorf("status: %s", reqs[1].Body)
	}
}

func TestAlertMatching(t *testing.T) {
	ev := ksEvent(SeverityInfo, "ReconciliationSucceeded", "Reconciliation finished", true)
	cases := []struct {
		name string
		mod  func(a *api.Alert)
		obj  ObjectInfo
		want bool
	}{
		{"wildcard", func(a *api.Alert) {}, ObjectInfo{}, true},
		{"by name", func(a *api.Alert) { a.Spec.EventSources[0].Name = "apps" }, ObjectInfo{}, true},
		{"other name", func(a *api.Alert) { a.Spec.EventSources[0].Name = "infra" }, ObjectInfo{}, false},
		{"other kind", func(a *api.Alert) { a.Spec.EventSources[0].Kind = "GitRepository" }, ObjectInfo{}, false},
		{"other namespace", func(a *api.Alert) { a.Spec.EventSources[0].Namespace = "apps" }, ObjectInfo{}, false},
		{"error only", func(a *api.Alert) { a.Spec.EventSeverity = "error" }, ObjectInfo{}, false},
		{"labels match", func(a *api.Alert) { a.Spec.EventSources[0].MatchLabels = map[string]string{"team": "ops"} },
			ObjectInfo{Labels: map[string]string{"team": "ops"}}, true},
		{"labels differ", func(a *api.Alert) { a.Spec.EventSources[0].MatchLabels = map[string]string{"team": "ops"} },
			ObjectInfo{Labels: map[string]string{"team": "dev"}}, false},
		{"included", func(a *api.Alert) { a.Spec.InclusionList = []string{"^Reconciliation"} }, ObjectInfo{}, true},
		{"not included", func(a *api.Alert) { a.Spec.InclusionList = []string{"created"} }, ObjectInfo{}, false},
		{"excluded", func(a *api.Alert) { a.Spec.ExclusionList = []string{"finished"} }, ObjectInfo{}, false},
		{"suspended", func(a *api.Alert) { a.Spec.Suspend = true }, ObjectInfo{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			srv := newServer(t, rec)
			a := alert("a", "hook", allKs)
			tc.mod(a)
			d := &Dispatcher{Creds: creds.New("")}
			dispatch(t, d, ev, tc.obj, []*api.Alert{a}, provider("hook", TypeGeneric, srv.URL))
			if got := len(rec.all()) == 1; got != tc.want {
				t.Errorf("sent = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRateLimitAndErrors(t *testing.T) {
	rec := &recorder{}
	srv := newServer(t, rec)
	d := &Dispatcher{Creds: creds.New("")}
	p := provider("hook", TypeGeneric, srv.URL)
	alerts := []*api.Alert{alert("a", "hook", allKs)}
	ev := ksEvent(SeverityError, "BuildFailed", "boom", true)
	dispatch(t, d, ev, ObjectInfo{}, alerts, p)
	dispatch(t, d, ev, ObjectInfo{}, alerts, p)
	if n := len(rec.all()); n != 1 {
		t.Errorf("identical events sent %d times, want 1", n)
	}

	res := dispatch(t, d, ksEvent(SeverityError, "BuildFailed", "other", true), ObjectInfo{}, []*api.Alert{alert("a", "missing", allKs)})
	if len(res) != 1 || res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "not found") {
		t.Errorf("missing provider: %+v", res)
	}

	rec.reply = func(*http.Request) (int, string) { return 500, "down" }
	p2 := provider("hook2", TypeGeneric, srv.URL+"/secret-path")
	res = dispatch(t, d, ksEvent(SeverityError, "BuildFailed", "third", true), ObjectInfo{}, []*api.Alert{alert("b", "hook2", allKs)}, p2)
	if len(res) != 1 || res[0].Err == nil || strings.Contains(res[0].Err.Error(), "secret-path") {
		t.Errorf("server error: %+v", res)
	}
	if n := len(rec.all()); n != 1+maxAttempts {
		t.Errorf("got %d requests, want %d (retries)", n, 1+maxAttempts)
	}
}

func TestValidateProvider(t *testing.T) {
	store := creds.New("")
	d := &Dispatcher{Creds: store}
	if err := d.ValidateProvider(provider("x", "discord", "https://example.com")); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("unsupported type: %v", err)
	}
	if err := d.ValidateProvider(provider("x", TypeSlack, "")); err == nil {
		t.Error("slack without address should fail")
	}
	gh := provider("x", TypeGitHub, "https://github.com/acme/fleet")
	if err := d.ValidateProvider(gh); err == nil {
		t.Error("github without token should fail")
	}
	gh.Spec.SecretRef = &api.LocalObjectReference{Name: "gh"}
	store.Set("gh", "token", []byte("t"))
	if err := d.ValidateProvider(gh); err != nil {
		t.Errorf("github: %v", err)
	}
	if s, _ := newGitHub(&target{URL: "git@github.com:acme/fleet.git", CommitStatus: "x"}); s.(*github).api != "https://api.github.com" || s.(*github).repo != "fleet" {
		t.Errorf("github api: %+v", s)
	}
}

func TestHelpers(t *testing.T) {
	for rev, want := range map[string]string{
		"main@sha1:" + sha:   sha,
		"main/" + sha:        sha,
		sha:                  sha,
		"v1.0.0@sha1:" + sha: sha,
	} {
		if got, err := parseRevision(rev); err != nil || got != want {
			t.Errorf("parseRevision(%q) = %q, %v", rev, got, err)
		}
	}
	if _, err := parseRevision("local@/tmp/x"); err == nil {
		t.Error("local revision should not parse")
	}
	for in, want := range map[string]string{
		"ReconciliationSucceeded": "reconciliation succeeded",
		"HealthCheckFailed":       "health check failed",
		"GitOperationFailed":      "git operation failed",
	} {
		if got := statusDescription(Event{Reason: in}); got != want {
			t.Errorf("%s -> %q, want %q", in, got, want)
		}
	}
}
