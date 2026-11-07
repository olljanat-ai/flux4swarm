// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/creds"
)

type vault struct {
	cfg   *api.VaultProvider
	creds *creds.Store
	http  *http.Client

	mu    sync.Mutex
	token string
}

func newVault(cfg *api.VaultProvider, c *creds.Store) (*vault, error) {
	if cfg.Server == "" {
		return nil, fmt.Errorf("vault: server is required")
	}
	if cfg.Auth.TokenSecretRef == nil && cfg.Auth.AppRole == nil {
		return nil, fmt.Errorf("vault: auth.tokenSecretRef or auth.appRole is required")
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	if len(cfg.CABundle) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cfg.CABundle) {
			return nil, fmt.Errorf("vault: caBundle has no PEM certificates")
		}
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	return &vault{cfg: cfg, creds: c, http: hc}, nil
}

func (v *vault) do(ctx context.Context, method, path string, body any, token string) (map[string]any, int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(v.cfg.Server, "/")+"/v1/"+strings.TrimLeft(path, "/"), rd)
	if err != nil {
		return nil, 0, err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if v.cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.cfg.Namespace)
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, fmt.Errorf("vault %s %s: %s", method, path, resp.Status)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}

func (v *vault) login(ctx context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token != "" {
		return v.token, nil
	}
	if r := v.cfg.Auth.TokenSecretRef; r != nil {
		t, err := v.creds.GetString(r.Name, r.Key)
		if err != nil {
			return "", err
		}
		v.token = t
		return t, nil
	}
	ar := v.cfg.Auth.AppRole
	roleID := ar.RoleID
	if ar.RoleRef != nil {
		var err error
		if roleID, err = v.creds.GetString(ar.RoleRef.Name, ar.RoleRef.Key); err != nil {
			return "", err
		}
	}
	secretID, err := v.creds.GetString(ar.SecretRef.Name, ar.SecretRef.Key)
	if err != nil {
		return "", err
	}
	mount := ar.Path
	if mount == "" {
		mount = "approle"
	}
	out, _, err := v.do(ctx, http.MethodPost, "auth/"+mount+"/login", map[string]string{"role_id": roleID, "secret_id": secretID}, "")
	if err != nil {
		return "", err
	}
	auth, _ := out["auth"].(map[string]any)
	tok, _ := auth["client_token"].(string)
	if tok == "" {
		return "", fmt.Errorf("vault approle login returned no token")
	}
	v.token = tok
	return tok, nil
}

func (v *vault) Get(ctx context.Context, ref api.RemoteReference) ([]byte, error) {
	tok, err := v.login(ctx)
	if err != nil {
		return nil, err
	}
	mount := v.cfg.Path
	key := strings.TrimLeft(ref.Key, "/")
	var path string
	if v.cfg.Version == "v1" {
		path = mount + "/" + key
		if mount == "" {
			path = key
		}
	} else {
		if mount == "" {
			// ESO: without path the first key segment is the mount.
			mount, key, _ = strings.Cut(key, "/")
		}
		path = mount + "/data/" + key
		if ref.Version != "" {
			path += "?version=" + ref.Version
		}
	}
	out, code, err := v.do(ctx, http.MethodGet, path, nil, tok)
	if code == http.StatusForbidden && v.cfg.Auth.AppRole != nil {
		// Token may have expired; log in again once.
		v.mu.Lock()
		v.token = ""
		v.mu.Unlock()
		if tok, err = v.login(ctx); err == nil {
			out, _, err = v.do(ctx, http.MethodGet, path, nil, tok)
		}
	}
	if err != nil {
		return nil, err
	}
	data, _ := out["data"].(map[string]any)
	if v.cfg.Version != "v1" {
		data, _ = data["data"].(map[string]any)
	}
	if data == nil {
		return nil, fmt.Errorf("vault: no data at %s", path)
	}
	if ref.Property == "" {
		return json.Marshal(data)
	}
	return pick(data, ref.Property)
}
