// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package git fetches and pushes Git repositories with go-git. It supports
// the authentication options of Flux's GitRepository: basic auth / PAT,
// bearer token, SSH, GitHub App (provider: github) and Azure Entra ID
// service principals (provider: azure) for Azure DevOps.
package git

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/golang-jwt/jwt/v5"
)

// Providers, matching Flux's GitRepository spec.provider values.
const (
	ProviderGeneric = "generic"
	ProviderGitHub  = "github"
	ProviderAzure   = "azure"
)

// Secret keys, matching Flux's GitRepository secret keys.
const (
	KeyUsername          = "username"
	KeyPassword          = "password"
	KeyBearerToken       = "bearerToken"
	KeyIdentity          = "identity"
	KeyKnownHosts        = "known_hosts"
	KeyGitHubAppID       = "githubAppID"
	KeyGitHubInstallID   = "githubAppInstallationID"
	KeyGitHubPrivateKey  = "githubAppPrivateKey"
	KeyGitHubAppBaseURL  = "githubAppBaseURL"
	KeyAzureTenantID     = "tenantId"
	KeyAzureClientID     = "clientId"
	KeyAzureClientSecret = "clientSecret"
	KeyAzureAuthorityURL = "authorityHost"
)

// SecretKeys lists every key flux4swarm reads from a Git secretRef.
var SecretKeys = []string{
	KeyUsername, KeyPassword, KeyBearerToken, KeyIdentity, KeyKnownHosts,
	KeyGitHubAppID, KeyGitHubInstallID, KeyGitHubPrivateKey, KeyGitHubAppBaseURL,
	KeyAzureTenantID, KeyAzureClientID, KeyAzureClientSecret, KeyAzureAuthorityURL,
}

// AzureDevOpsScope is the Entra ID resource scope of Azure DevOps.
const AzureDevOpsScope = "499b84ac-1321-427f-aa17-267ca6975798/.default"

// HTTPClient is used for token exchanges; tests may replace it.
var HTTPClient = &http.Client{Timeout: 30 * time.Second}

// AuthProvider returns a go-git auth method, refreshing tokens as needed.
type AuthProvider struct {
	Provider string
	URL      string
	Secret   map[string][]byte

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (a *AuthProvider) get(key string) string {
	return strings.TrimSpace(string(a.Secret[key]))
}

// Method returns the auth method for the next Git operation, or nil for
// anonymous access.
func (a *AuthProvider) Method(ctx context.Context) (transport.AuthMethod, error) {
	if a == nil {
		return nil, nil
	}
	ep, err := transport.NewEndpoint(a.URL)
	if err != nil {
		return nil, err
	}
	switch a.Provider {
	case "", ProviderGeneric:
	case ProviderGitHub:
		tok, err := a.cachedToken(ctx, a.githubAppToken)
		if err != nil {
			return nil, fmt.Errorf("github app: %w", err)
		}
		return &githttp.BasicAuth{Username: "x-access-token", Password: tok}, nil
	case ProviderAzure:
		tok, err := a.cachedToken(ctx, a.azureToken)
		if err != nil {
			return nil, fmt.Errorf("azure: %w", err)
		}
		return &githttp.TokenAuth{Token: tok}, nil
	default:
		return nil, fmt.Errorf("unsupported git provider %q", a.Provider)
	}

	if ep.Protocol == "ssh" {
		identity := a.Secret[KeyIdentity]
		if len(identity) == 0 {
			return nil, fmt.Errorf("ssh url %s needs an %q key in its secret", a.URL, KeyIdentity)
		}
		user := ep.User
		if user == "" {
			user = "git"
		}
		pk, err := gitssh.NewPublicKeys(user, identity, a.get(KeyPassword))
		if err != nil {
			return nil, err
		}
		if kh := a.Secret[KeyKnownHosts]; len(kh) > 0 {
			f, err := os.CreateTemp("", "known_hosts")
			if err != nil {
				return nil, err
			}
			defer os.Remove(f.Name())
			if _, err := f.Write(kh); err != nil {
				f.Close()
				return nil, err
			}
			f.Close()
			cb, err := gitssh.NewKnownHostsCallback(f.Name())
			if err != nil {
				return nil, err
			}
			pk.HostKeyCallback = cb
		}
		return pk, nil
	}

	if tok := a.get(KeyBearerToken); tok != "" {
		return &githttp.TokenAuth{Token: tok}, nil
	}
	user, pass := a.get(KeyUsername), a.get(KeyPassword)
	if pass != "" {
		if user == "" {
			// GitHub and Azure DevOps accept any non-empty user with a PAT.
			user = "git"
		}
		return &githttp.BasicAuth{Username: user, Password: pass}, nil
	}
	return nil, nil
}

func (a *AuthProvider) cachedToken(ctx context.Context, fetch func(context.Context) (string, time.Time, error)) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Until(a.expires) > 5*time.Minute {
		return a.token, nil
	}
	tok, exp, err := fetch(ctx)
	if err != nil {
		return "", err
	}
	a.token, a.expires = tok, exp
	return tok, nil
}

func (a *AuthProvider) githubAppToken(ctx context.Context) (string, time.Time, error) {
	appID, inst, key := a.get(KeyGitHubAppID), a.get(KeyGitHubInstallID), a.Secret[KeyGitHubPrivateKey]
	if appID == "" || inst == "" || len(key) == 0 {
		return "", time.Time{}, fmt.Errorf("secret needs %s, %s and %s", KeyGitHubAppID, KeyGitHubInstallID, KeyGitHubPrivateKey)
	}
	if _, err := strconv.ParseInt(inst, 10, 64); err != nil {
		return "", time.Time{}, fmt.Errorf("invalid %s: %w", KeyGitHubInstallID, err)
	}
	pk, err := jwt.ParseRSAPrivateKeyFromPEM(key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parse private key: %w", err)
	}
	now := time.Now()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    appID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).SignedString(pk)
	if err != nil {
		return "", time.Time{}, err
	}
	base := a.get(KeyGitHubAppBaseURL)
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/app/installations/"+inst+"/access_tokens", nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+signed)
	req.Header.Set("Accept", "application/vnd.github+json")
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := doJSON(req, &out); err != nil {
		return "", time.Time{}, err
	}
	return out.Token, out.ExpiresAt, nil
}

func (a *AuthProvider) azureToken(ctx context.Context) (string, time.Time, error) {
	tenant, id, secret := a.get(KeyAzureTenantID), a.get(KeyAzureClientID), a.get(KeyAzureClientSecret)
	if tenant == "" || id == "" || secret == "" {
		return "", time.Time{}, fmt.Errorf("secret needs %s, %s and %s", KeyAzureTenantID, KeyAzureClientID, KeyAzureClientSecret)
	}
	tok, exp, err := AzureClientCredentialsToken(ctx, a.get(KeyAzureAuthorityURL), tenant, id, secret, AzureDevOpsScope)
	return tok, exp, err
}

// AzureClientCredentialsToken gets an Entra ID token with the client
// credentials grant. authority defaults to https://login.microsoftonline.com.
func AzureClientCredentialsToken(ctx context.Context, authority, tenant, clientID, clientSecret, scope string) (string, time.Time, error) {
	if authority == "" {
		authority = "https://login.microsoftonline.com"
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"scope":         {scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(authority, "/")+"/"+url.PathEscape(tenant)+"/oauth2/v2.0/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := doJSON(req, &out); err != nil {
		return "", time.Time{}, err
	}
	return out.AccessToken, time.Now().Add(time.Duration(out.ExpiresIn) * time.Second), nil
}

func doJSON(req *http.Request, out any) error {
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", req.Method, req.URL.Redacted(), resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}
