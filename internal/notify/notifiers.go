// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Provider types flux4swarm can send to. They use the payloads of Flux's
// notification-controller.
const (
	TypeGeneric     = "generic"
	TypeGenericHMAC = "generic-hmac"
	TypeSlack       = "slack"
	TypeMSTeams     = "msteams"
	TypeGitHub      = "github"
	TypeAzureDevOps = "azuredevops"
)

// SupportedProviderTypes lists the Provider types flux4swarm implements.
var SupportedProviderTypes = []string{TypeGeneric, TypeGenericHMAC, TypeSlack, TypeMSTeams, TypeGitHub, TypeAzureDevOps}

// NotificationHeader is the header the generic provider sets to the
// reporting controller, like Flux.
const NotificationHeader = "gotk-component"

// sender posts one event.
type sender interface {
	Post(ctx context.Context, ev Event) error
}

// target is the resolved configuration of a Provider.
type target struct {
	Type         string
	URL          string
	Token        string
	Username     string
	Password     string
	Channel      string
	Headers      map[string]string
	CommitStatus string
	// TokenFunc returns a token when the secret has no "token" (GitHub App
	// or Entra ID service principal). Bearer reports whether the token is
	// an OAuth token rather than a personal access token.
	TokenFunc func(context.Context) (string, error)
	Bearer    bool
	Client    *http.Client
}

func newSender(t *target) (sender, error) {
	switch t.Type {
	case TypeGeneric, TypeGenericHMAC:
		if _, err := url.ParseRequestURI(t.URL); err != nil {
			return nil, fmt.Errorf("invalid hook URL %q: %w", redactURL(t.URL), err)
		}
		f := &forwarder{t: t}
		if t.Type == TypeGenericHMAC {
			if t.Token == "" {
				return nil, errors.New("generic-hmac needs a token in its secret")
			}
			f.key = []byte(t.Token)
		}
		return f, nil
	case TypeSlack:
		if _, err := url.ParseRequestURI(t.URL); err != nil {
			return nil, fmt.Errorf("invalid Slack hook URL %q: %w", redactURL(t.URL), err)
		}
		return &slack{t: t}, nil
	case TypeMSTeams:
		u, err := url.ParseRequestURI(t.URL)
		if err != nil {
			return nil, fmt.Errorf("invalid MS Teams webhook URL %q: %w", redactURL(t.URL), err)
		}
		return &msteams{t: t, legacy: strings.HasSuffix(u.Hostname(), ".webhook.office.com")}, nil
	case TypeGitHub:
		return newGitHub(t)
	case TypeAzureDevOps:
		return newAzureDevOps(t)
	}
	return nil, fmt.Errorf("provider type %q is not supported by flux4swarm (supported: %s)", t.Type, strings.Join(SupportedProviderTypes, ", "))
}

// ---- HTTP ----

// retryWait is the base backoff between attempts; tests lower it.
var retryWait = 2 * time.Second

const maxAttempts = 4

type request struct {
	method   string
	url      string
	body     any
	header   map[string]string
	validate func(*http.Response) error
	out      any
}

// do sends a JSON request, retrying network errors, 429 and 5xx like
// Flux's retryable client.
func do(ctx context.Context, c *http.Client, r request) error {
	var data []byte
	if r.body != nil {
		var err error
		if data, err = json.Marshal(r.body); err != nil {
			return fmt.Errorf("marshalling notification payload failed: %w", err)
		}
	}
	if r.method == "" {
		r.method = http.MethodPost
	}
	var last error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w (last error: %v)", ctx.Err(), last)
			case <-time.After(retryWait * time.Duration(1<<(attempt-1))):
			}
		}
		var body io.Reader
		if data != nil {
			body = bytes.NewReader(data)
		}
		req, err := http.NewRequestWithContext(ctx, r.method, r.url, body)
		if err != nil {
			return err
		}
		if data != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("User-Agent", "flux4swarm-notification")
		for k, v := range r.header {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		if err != nil {
			last = err
			if ctx.Err() != nil {
				return err
			}
			continue
		}
		err = func() error {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				return &retryable{statusError(resp)}
			}
			if r.validate != nil {
				return r.validate(resp)
			}
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				return statusError(resp)
			}
			if r.out != nil {
				return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(r.out)
			}
			return nil
		}()
		var re *retryable
		if errors.As(err, &re) {
			last = re.err
			continue
		}
		return err
	}
	return fmt.Errorf("giving up after %d attempts: %w", maxAttempts, last)
}

type retryable struct{ err error }

func (r *retryable) Error() string { return r.err.Error() }

func statusError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("request failed with status code %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

func objectName(ev Event) string {
	return fmt.Sprintf("%s/%s.%s", strings.ToLower(ev.InvolvedObject.Kind), ev.InvolvedObject.Name, ev.InvolvedObject.Namespace)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- generic / generic-hmac ----

type forwarder struct {
	t   *target
	key []byte
}

func (f *forwarder) Post(ctx context.Context, ev Event) error {
	h := map[string]string{NotificationHeader: ev.ReportingController}
	for k, v := range f.t.Headers {
		h[k] = v
	}
	if len(f.key) > 0 {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		h["X-Signature"] = "sha256=" + hmacSHA256(b, f.key)
	}
	return do(ctx, f.t.Client, request{url: f.t.URL, body: ev, header: h})
}

func hmacSHA256(payload, key []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(payload)
	return hex.EncodeToString(m.Sum(nil))
}

// ---- slack ----

type slack struct{ t *target }

type slackPayload struct {
	Channel     string            `json:"channel"`
	Username    string            `json:"username"`
	IconURL     string            `json:"icon_url"`
	IconEmoji   string            `json:"icon_emoji"`
	Text        string            `json:"text,omitempty"`
	Attachments []slackAttachment `json:"attachments,omitempty"`
}

type slackAttachment struct {
	Fallback   string       `json:"fallback"`
	Color      string       `json:"color"`
	AuthorName string       `json:"author_name"`
	Text       string       `json:"text"`
	MrkdwnIn   []string     `json:"mrkdwn_in"`
	Fields     []slackField `json:"fields"`
}

type slackField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// SlackAPIURL is the Slack Web API endpoint used with a bot token.
const SlackAPIURL = "https://slack.com/api/chat.postMessage"

func (s *slack) Post(ctx context.Context, ev Event) error {
	p := slackPayload{Username: s.t.Username, Channel: s.t.Channel}
	if p.Username == "" {
		p.Username = ev.ReportingController
	}
	color := "good"
	if ev.Severity == SeverityError {
		color = "danger"
	}
	fields := make([]slackField, 0, len(ev.Metadata))
	for _, k := range sortedKeys(ev.Metadata) {
		fields = append(fields, slackField{Title: k, Value: ev.Metadata[k]})
	}
	author := objectName(ev)
	p.Attachments = []slackAttachment{{
		Fallback:   author + ": " + strings.Split(ev.Message, "\n")[0],
		Color:      color,
		AuthorName: author,
		Text:       ev.Message,
		MrkdwnIn:   []string{"text"},
		Fields:     fields,
	}}
	r := request{url: s.t.URL, body: p}
	if s.t.Token != "" {
		r.header = map[string]string{"Authorization": "Bearer " + s.t.Token}
	}
	if s.t.URL == SlackAPIURL {
		r.validate = func(resp *http.Response) error {
			var out struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
				return fmt.Errorf("unable to unmarshal response body: %w", err)
			}
			if !out.OK {
				return fmt.Errorf("Slack responded with error: %s", out.Error)
			}
			return nil
		}
	}
	return do(ctx, s.t.Client, r)
}

// ---- msteams ----

type msteams struct {
	t      *target
	legacy bool
}

type teamsFact struct {
	Name  string `json:"name,omitempty"`
	Title string `json:"title,omitempty"`
	Value string `json:"value"`
}

func (m *msteams) Post(ctx context.Context, ev Event) error {
	obj := objectName(ev)
	var payload any
	if m.legacy {
		facts := []teamsFact{}
		for _, k := range sortedKeys(ev.Metadata) {
			facts = append(facts, teamsFact{Name: k, Value: ev.Metadata[k]})
		}
		color := "0076D7"
		if ev.Severity == SeverityError {
			color = "FF0000"
		}
		payload = map[string]any{
			"@type":      "MessageCard",
			"@context":   "http://schema.org/extensions",
			"themeColor": color,
			"summary":    obj,
			"sections": []map[string]any{{
				"activityTitle":    ev.Message,
				"activitySubtitle": obj,
				"facts":            facts,
			}},
		}
	} else {
		facts := []teamsFact{}
		if s, ok := ev.Metadata[MetaSummaryKey]; ok {
			facts = append(facts, teamsFact{Title: MetaSummaryKey, Value: s})
		}
		for _, k := range sortedKeys(ev.Metadata) {
			if k != MetaSummaryKey {
				facts = append(facts, teamsFact{Title: k, Value: ev.Metadata[k]})
			}
		}
		msg := map[string]any{"type": "TextBlock", "text": ev.Message, "wrap": true}
		if ev.Severity == SeverityError {
			msg["color"] = "attention"
		}
		payload = map[string]any{
			"type": "message",
			"attachments": []map[string]any{{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"content": map[string]any{
					"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
					"type":    "AdaptiveCard",
					"version": "1.4",
					"msteams": map[string]string{"width": "Full"},
					"body": []map[string]any{{
						"type": "Container",
						"items": []map[string]any{
							{"type": "TextBlock", "text": obj, "size": "large", "weight": "bolder", "wrap": true},
							msg,
							{"type": "FactSet", "facts": facts},
						},
					}},
				},
			}},
		}
	}
	return do(ctx, m.t.Client, request{url: m.t.URL, body: payload})
}

// ---- commit status helpers ----

// parseGitAddress returns "scheme://host" and the repository path of a Git
// URL (https, ssh:// or scp-like git@host:path).
func parseGitAddress(s string) (string, string, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "://") {
		if at := strings.Index(s, "@"); at >= 0 {
			if colon := strings.Index(s[at:], ":"); colon > 0 {
				s = "ssh://" + s[:at+colon] + "/" + s[at+colon+1:]
			}
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("failed parsing URL %q", redactURL(s))
	}
	scheme := u.Scheme
	if scheme == "ssh" || scheme == "" {
		scheme = "https"
	}
	id := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	return scheme + "://" + u.Host, id, nil
}

// parseRevision extracts the commit SHA of a Flux revision
// ("main@sha1:<sha>", "<sha>" or the legacy "main/<sha>").
func parseRevision(rev string) (string, error) {
	r := rev
	if i := strings.LastIndex(r, "@"); i >= 0 {
		r = r[i+1:]
	} else if i := strings.LastIndex(r, "/"); i >= 0 {
		r = r[i+1:]
	}
	r = strings.TrimPrefix(strings.TrimPrefix(r, "sha1:"), "sha256:")
	if (len(r) == 40 || len(r) == 64) && isHex(r) {
		return r, nil
	}
	return "", fmt.Errorf("failed to extract commit hash from '%s' revision", rev)
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// statusDescription is Flux's commit status description: the event reason
// split into lower case words ("reconciliation succeeded").
func statusDescription(ev Event) string {
	return strings.ToLower(strings.Join(splitCamelcase(ev.Reason), " "))
}

// splitCamelcase splits "ReconciliationSucceeded" into words, keeping
// acronyms together ("HTTPServer" -> "HTTP", "Server").
func splitCamelcase(src string) []string {
	if !utf8.ValidString(src) {
		return []string{src}
	}
	var runes [][]rune
	last := 0
	for _, r := range src {
		class := 4
		switch {
		case unicode.IsLower(r):
			class = 1
		case unicode.IsUpper(r):
			class = 2
		case unicode.IsDigit(r):
			class = 3
		}
		if class == last && len(runes) > 0 {
			runes[len(runes)-1] = append(runes[len(runes)-1], r)
		} else {
			runes = append(runes, []rune{r})
		}
		last = class
	}
	for i := 0; i < len(runes)-1; i++ {
		if unicode.IsUpper(runes[i][0]) && unicode.IsLower(runes[i+1][0]) {
			runes[i+1] = append([]rune{runes[i][len(runes[i])-1]}, runes[i+1]...)
			runes[i] = runes[i][:len(runes[i])-1]
		}
	}
	out := []string{}
	for _, r := range runes {
		if len(r) > 0 {
			out = append(out, string(r))
		}
	}
	return out
}

func (t *target) token(ctx context.Context) (string, error) {
	if t.Token != "" {
		return t.Token, nil
	}
	if t.TokenFunc != nil {
		return t.TokenFunc(ctx)
	}
	return "", errors.New("the provider secret needs a token")
}

// ---- github ----

type github struct {
	t           *target
	api         string
	owner, repo string
}

func newGitHub(t *target) (sender, error) {
	if t.CommitStatus == "" {
		return nil, errors.New("commit status cannot be empty")
	}
	host, id, err := parseGitAddress(t.URL)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(id, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid repository id: '%s'", id)
	}
	api := host + "/api/v3"
	if h := strings.TrimPrefix(host, "https://"); h == "github.com" || h == "www.github.com" {
		api = "https://api.github.com"
	}
	return &github{t: t, api: api, owner: parts[0], repo: parts[1]}, nil
}

type githubStatus struct {
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
}

func (g *github) Post(ctx context.Context, ev Event) error {
	if ev.Reason == ReasonProgressing {
		return nil
	}
	revs, ok := ev.Revision()
	if !ok {
		return errors.New("missing revision metadata")
	}
	sha, err := parseRevision(revs)
	if err != nil {
		return err
	}
	state := "success"
	switch ev.Severity {
	case SeverityInfo:
	case SeverityError:
		state = "failure"
	default:
		return errors.New("can't convert to github state")
	}
	tok, err := g.t.token(ctx)
	if err != nil {
		return err
	}
	h := map[string]string{
		"Authorization":        "Bearer " + tok,
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}
	status := githubStatus{State: state, Context: g.t.CommitStatus, Description: statusDescription(ev)}
	base := fmt.Sprintf("%s/repos/%s/%s", g.api, url.PathEscape(g.owner), url.PathEscape(g.repo))
	var existing []githubStatus
	if err := do(ctx, g.t.Client, request{method: http.MethodGet, url: base + "/commits/" + sha + "/statuses?per_page=50", header: h, out: &existing}); err != nil {
		return fmt.Errorf("could not list commit statuses: %w", err)
	}
	for _, s := range existing {
		if s.Context == status.Context {
			if s.State == status.State && s.Description == status.Description {
				return nil
			}
			break
		}
	}
	if err := do(ctx, g.t.Client, request{url: base + "/statuses/" + sha, body: status, header: h}); err != nil {
		return fmt.Errorf("could not create commit status: %w", err)
	}
	return nil
}

// ---- azure devops ----

type azureDevOps struct {
	t    *target
	base string // https://dev.azure.com/org/project/_apis/git/repositories/repo
}

func newAzureDevOps(t *target) (sender, error) {
	if t.CommitStatus == "" {
		return nil, errors.New("commit status cannot be empty")
	}
	host, id, err := parseGitAddress(t.URL)
	if err != nil {
		return nil, err
	}
	comp := strings.Split(id, "/")
	var org, proj, repo string
	switch {
	case len(comp) == 4 && comp[2] == "_git":
		org, proj, repo = comp[0], comp[1], comp[3]
	case len(comp) == 3 && comp[1] == "_git" && strings.HasSuffix(host, ".visualstudio.com"):
		proj, repo = comp[0], comp[2]
	default:
		return nil, fmt.Errorf("invalid repository id %q", id)
	}
	orgURL := host
	if org != "" {
		orgURL += "/" + url.PathEscape(org)
	}
	return &azureDevOps{t: t, base: fmt.Sprintf("%s/%s/_apis/git/repositories/%s", orgURL, url.PathEscape(proj), url.PathEscape(repo))}, nil
}

type azureStatus struct {
	State       string `json:"state"`
	Description string `json:"description"`
	Context     struct {
		Genre string `json:"genre"`
		Name  string `json:"name"`
	} `json:"context"`
}

func (a *azureDevOps) Post(ctx context.Context, ev Event) error {
	if ev.Reason == ReasonProgressing {
		return nil
	}
	revs, ok := ev.Revision()
	if !ok {
		return errors.New("missing revision metadata")
	}
	sha, err := parseRevision(revs)
	if err != nil {
		return err
	}
	var st azureStatus
	switch ev.Severity {
	case SeverityInfo:
		st.State = "succeeded"
	case SeverityError:
		st.State = "error"
	default:
		return errors.New("can't convert to azure devops state")
	}
	st.Description = statusDescription(ev)
	st.Context.Name = a.t.CommitStatus
	st.Context.Genre = "fluxcd"
	if s, ok := ev.Metadata[MetaSummaryKey]; ok {
		st.Context.Genre = "fluxcd:" + s
	}
	tok, err := a.t.token(ctx)
	if err != nil {
		return err
	}
	h := map[string]string{"Accept": "application/json"}
	if a.t.Bearer {
		h["Authorization"] = "Bearer " + tok
	} else {
		h["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+tok))
	}
	u := a.base + "/commits/" + sha + "/statuses?api-version=6.0"
	var existing struct {
		Value []azureStatus `json:"value"`
	}
	if err := do(ctx, a.t.Client, request{method: http.MethodGet, url: u, header: h, out: &existing}); err != nil {
		return fmt.Errorf("could not list commit statuses: %w", err)
	}
	for _, s := range existing.Value {
		if s.Context.Name == st.Context.Name && s.Context.Genre == st.Context.Genre {
			if s.State == st.State && s.Description == st.Description {
				return nil
			}
			break
		}
	}
	if err := do(ctx, a.t.Client, request{url: u, body: st, header: h}); err != nil {
		return fmt.Errorf("could not create commit status: %w", err)
	}
	return nil
}

// redactURL hides the path and query of a URL, which for webhooks usually
// contains the secret.
func redactURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "***"
	}
	if u.Path == "" && u.RawQuery == "" {
		return u.Scheme + "://" + u.Host
	}
	return u.Scheme + "://" + u.Host + "/***"
}
