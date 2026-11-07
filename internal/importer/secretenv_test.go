// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"context"
	"strings"
	"testing"
)

func TestIsSecretEnv(t *testing.T) {
	p := Options{SecretEnvNames: []string{"license"}}.secretEnvPatterns()
	for name, want := range map[string]bool{
		"DB_PASSWORD":         true,
		"postgres_password":   true,
		"STRIPE_API_KEY":      true,
		"GITHUB_TOKEN":        true,
		"AZURE_CLIENT_SECRET": true,
		"APP_LICENSE":         true,
		"DB_PASSWORD_FILE":    false,
		"TLS_SECRET_PATH":     false,
		"LOG_LEVEL":           false,
		"DB_HOST":             false,
	} {
		if got := isSecretEnv(p, name, "x"); got != want {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	if isSecretEnv(p, "DB_PASSWORD", "") {
		t.Error("empty value should stay plain")
	}
	if isSecretEnv(p, "DB_PASSWORD", "{{.Task.Name}}") {
		t.Error("template value should stay plain")
	}
	if p := (Options{NoDefaultSecretEnvNames: true}).secretEnvPatterns(); len(p) != 0 {
		t.Errorf("patterns with defaults disabled: %v", p)
	}
}

func TestExportSecretEnv(t *testing.T) {
	fd := stackService()
	cs := fd.services[0].Spec.TaskTemplate.ContainerSpec
	cs.Env = append(cs.Env, "DB_PASSWORD=hunter2", "API_KEY=k-123456", "DB_PASSWORD_FILE=/run/secrets/dbpw")
	res, err := Export(context.Background(), fd, Options{Cluster: "prod", SecretStore: "fake", SecretStoreProvider: ProviderFake, SecretKeyPrefix: "p/"})
	if err != nil {
		t.Fatal(err)
	}
	for f, b := range res.Files {
		if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "k-123456") {
			t.Errorf("%s contains a secret env value:\n%s", f, b)
		}
	}
	es := string(res.Files["clusters/prod/apps/shop/externalsecret-web-env.yaml"])
	for _, want := range []string{"name: web-env", "secretKey: API_KEY", "key: p/shop_web-API_KEY", "secretKey: DB_PASSWORD", "key: p/shop_web-DB_PASSWORD"} {
		if !strings.Contains(es, want) {
			t.Errorf("ExternalSecret lacks %q:\n%s", want, es)
		}
	}
	dep := string(res.Files["clusters/prod/apps/shop/deployment-web.yaml"])
	for _, want := range []string{"secretKeyRef:", "name: web-env", "value: /run/secrets/dbpw", "value: two words"} {
		if !strings.Contains(dep, want) {
			t.Errorf("Deployment lacks %q:\n%s", want, dep)
		}
	}
	store := string(res.Files["clusters/prod/apps/shop/secretstore.yaml"])
	if !strings.Contains(store, "p/shop_web-DB_PASSWORD") {
		t.Errorf("SecretStore lacks the env key:\n%s", store)
	}
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "env API_KEY, DB_PASSWORD look like secrets") {
			found = true
		}
	}
	if !found {
		t.Errorf("no note about moved env vars: %q", res.Notes)
	}

	res, err = Export(context.Background(), fd, Options{Cluster: "prod", SecretStore: "fake", SecretStoreProvider: ProviderFake, NoDefaultSecretEnvNames: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Files["clusters/prod/apps/shop/externalsecret-web-env.yaml"]; ok {
		t.Error("ExternalSecret for env written with the list disabled")
	}
	if dep := string(res.Files["clusters/prod/apps/shop/deployment-web.yaml"]); !strings.Contains(dep, "value: hunter2") {
		t.Errorf("env value not kept with the list disabled:\n%s", dep)
	}
}
