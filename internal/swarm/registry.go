// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package swarm

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/docker/docker/api/types/registry"
)

// LoadDockerConfig reads registry credentials from a Docker config.json
// ({"auths": {"host": {"auth": base64(user:pass)}}}). A missing file gives
// an empty map. Credential helpers are not supported.
func LoadDockerConfig(path string) (map[string]registry.AuthConfig, error) {
	out := map[string]registry.AuthConfig{}
	if path == "" {
		return out, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			Username      string `json:"username"`
			Password      string `json:"password"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for host, a := range cfg.Auths {
		ac := registry.AuthConfig{Username: a.Username, Password: a.Password, IdentityToken: a.IdentityToken, ServerAddress: host}
		if a.Auth != "" {
			dec, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				return nil, fmt.Errorf("%s: auth for %s: %w", path, host, err)
			}
			user, pass, _ := strings.Cut(string(dec), ":")
			ac.Username, ac.Password = user, pass
		}
		key := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/")
		if host == "https://index.docker.io/v1/" {
			key = host
		}
		out[key] = ac
	}
	return out, nil
}
