// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/creds"
	"github.com/olljanat-ai/flux4swarm/internal/git"
)

// azureClouds maps ESO environmentType to the Entra ID authority and the
// Key Vault token scope.
var azureClouds = map[string][2]string{
	"":                  {"https://login.microsoftonline.com", "https://vault.azure.net/.default"},
	"PublicCloud":       {"https://login.microsoftonline.com", "https://vault.azure.net/.default"},
	"USGovernmentCloud": {"https://login.microsoftonline.us", "https://vault.usgovcloudapi.net/.default"},
	"ChinaCloud":        {"https://login.chinacloudapi.cn", "https://vault.azure.cn/.default"},
	"GermanCloud":       {"https://login.microsoftonline.de", "https://vault.microsoftazure.de/.default"},
}

type azureKV struct {
	cfg          *api.AzureKVProvider
	clientID     string
	clientSecret string
	authority    string
	scope        string
	http         *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newAzureKV(cfg *api.AzureKVProvider, c *creds.Store) (*azureKV, error) {
	if cfg.VaultURL == "" {
		return nil, fmt.Errorf("azurekv: vaultUrl is required")
	}
	if cfg.AuthType != "" && cfg.AuthType != "ServicePrincipal" {
		return nil, fmt.Errorf("azurekv: authType %q is not supported on Swarm, use ServicePrincipal", cfg.AuthType)
	}
	a := cfg.AuthSecretRef
	if cfg.TenantID == "" || a == nil || a.ClientID == nil || a.ClientSecret == nil {
		return nil, fmt.Errorf("azurekv: tenantId and authSecretRef.clientId/clientSecret are required")
	}
	id, err := c.GetString(a.ClientID.Name, a.ClientID.Key)
	if err != nil {
		return nil, err
	}
	secret, err := c.GetString(a.ClientSecret.Name, a.ClientSecret.Key)
	if err != nil {
		return nil, err
	}
	cloud, ok := azureClouds[cfg.EnvironmentType]
	if !ok {
		return nil, fmt.Errorf("azurekv: unknown environmentType %q", cfg.EnvironmentType)
	}
	return &azureKV{cfg: cfg, clientID: id, clientSecret: secret, authority: cloud[0], scope: cloud[1],
		http: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (z *azureKV) accessToken(ctx context.Context) (string, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.token != "" && time.Until(z.expires) > 5*time.Minute {
		return z.token, nil
	}
	tok, exp, err := git.AzureClientCredentialsToken(ctx, z.authority, z.cfg.TenantID, z.clientID, z.clientSecret, z.scope)
	if err != nil {
		return "", err
	}
	z.token, z.expires = tok, exp
	return tok, nil
}

func (z *azureKV) Get(ctx context.Context, ref api.RemoteReference) ([]byte, error) {
	name := ref.Key
	// ESO accepts "secret/<name>"; keys and certs are not supported here.
	if kind, rest, ok := strings.Cut(name, "/"); ok {
		if kind != "secret" {
			return nil, fmt.Errorf("azurekv: only secret/<name> objects are supported, got %q", name)
		}
		name = rest
	}
	tok, err := z.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	u := strings.TrimRight(z.cfg.VaultURL, "/") + "/secrets/" + url.PathEscape(name)
	if ref.Version != "" {
		u += "/" + url.PathEscape(ref.Version)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?api-version=7.4", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := z.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("azurekv: get %s: %s", name, resp.Status)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return property([]byte(out.Value), ref.Property)
}
