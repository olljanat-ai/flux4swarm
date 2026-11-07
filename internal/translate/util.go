// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package translate

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/docker/docker/api/types/container"
	"sigs.k8s.io/yaml"
)

type osFileMode = os.FileMode

type containerHealth = container.HealthConfig

func decodeAuth(s string) (string, string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", "", err
	}
	u, p, ok := strings.Cut(string(b), ":")
	if !ok {
		return "", "", fmt.Errorf("auth is not user:password")
	}
	return u, p, nil
}

// toJSON accepts JSON or YAML.
func toJSON(b []byte) ([]byte, error) {
	return yaml.YAMLToJSON(b)
}
