// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/olljanat-ai/flux4swarm/internal/git"
)

func deployment(ns, name, image string, replicas int, env string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: %d
  template:
    spec:
      containers:
      - name: %s
        image: %s
        env:
        - name: MODE
          value: %s
`, name, ns, replicas, name, image, env)
}

func configMap(ns, name, data string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: %s\ndata:\n  a: %s\n", name, ns, data)
}

func TestCommitMessage(t *testing.T) {
	ch := func(path, old, cur string) git.FileChange {
		c := git.FileChange{Path: path}
		if old != "" {
			c.Old = []byte(old)
		}
		if cur != "" {
			c.New = []byte(cur)
		}
		return c
	}
	kust := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [a.yaml]\n"
	for _, tc := range []struct {
		name    string
		changes []git.FileChange
		want    string
	}{
		{"image and removal", []git.FileChange{
			ch("c/apps/shop/deployment-api.yaml", deployment("shop", "api", "reg/api:1.4", 2, "x"), deployment("shop", "api", "reg/api:1.5", 2, "x")),
			ch("c/apps/shop/deployment-tmp-debug.yaml", deployment("shop", "tmp-debug", "busybox", 1, "x"), ""),
			ch("c/apps/shop/kustomization.yaml", kust, kust+"# changed\n"),
		}, "shop: api image 1.4→1.5, removed tmp-debug"},
		{"replicas and settings", []git.FileChange{
			ch("c/apps/shop/deployment-api.yaml", deployment("shop", "api", "api:1", 2, "x"), deployment("shop", "api", "api:1", 3, "y")),
		}, "shop: api replicas 2→3 and settings"},
		{"other repository", []git.FileChange{
			ch("c/apps/web/deployment-proxy.yaml", deployment("web", "proxy", "nginx:1", 1, "x"), deployment("web", "proxy", "caddy:2", 1, "x")),
		}, "web: proxy image nginx:1→caddy:2"},
		{"new digest", []git.FileChange{
			ch("c/apps/web/deployment-proxy.yaml", deployment("web", "proxy", "nginx:1@sha256:aaa", 1, "x"), deployment("web", "proxy", "nginx:1@sha256:bbb", 1, "x")),
		}, "web: proxy image 1 (new digest)"},
		{"settings only and non-workloads", []git.FileChange{
			ch("c/apps/web/deployment-proxy.yaml", deployment("web", "proxy", "nginx:1", 1, "x"), deployment("web", "proxy", "nginx:1", 1, "y")),
			ch("c/apps/web/configmap-conf.yaml", configMap("web", "conf", "1"), configMap("web", "conf", "2")),
			ch("c/apps/db/configmap-init.yaml", "", configMap("db", "init", "1")),
		}, "db: added configmap init; web: updated configmap conf, updated proxy"},
		{"nothing described", []git.FileChange{ch("c/apps/kustomization.yaml", kust, kust+"# x\n")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CommitMessage("Sync", tc.changes); got != tc.want {
				t.Fatalf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestCommitMessageLong(t *testing.T) {
	var changes []git.FileChange
	for _, ns := range []string{"a", "b", "c", "d", "e"} {
		changes = append(changes, git.FileChange{Path: ns + ".yaml", New: []byte(deployment(ns, "service-with-a-long-name", "img:1", 1, "x"))})
	}
	got := CommitMessage("Import", changes)
	subject, body, _ := strings.Cut(got, "\n\n")
	if subject != "Import Docker Swarm state: 5 changes in a, b, c and 2 more" {
		t.Fatalf("subject %q", subject)
	}
	if !strings.HasPrefix(body, "- a: added service-with-a-long-name\n- b: added service-with-a-long-name\n") || strings.Count(body, "\n") != 5 {
		t.Fatalf("body %q", body)
	}
}
