// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/olljanat-ai/flux4swarm/internal/importer"
	"github.com/olljanat-ai/flux4swarm/internal/merge"
)

// envExport is what "import --cluster env" gives for a one-service Swarm.
func envExport(env, tag string, replicas string) *importer.Result {
	apps := "clusters/" + env + "/" + importer.AppsDir
	return &importer.Result{Root: "clusters/" + env, Clean: []string{apps}, Files: map[string][]byte{
		"clusters/" + env + "/flux-system/kustomization.yaml": []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"),
		apps + "/kustomization.yaml":                          []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- deployment-web.yaml\n"),
		apps + "/deployment-web.yaml": []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
spec:
  replicas: ` + replicas + `
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      containers:
      - name: web
        image: nginx:` + tag + `
`),
	}}
}

func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWriteMergedWatchers(t *testing.T) {
	dir := t.TempDir()
	envs := []string{"development", "acceptance", "production"}
	notes, err := writeMerged(dir, envExport("acceptance", "1.9", "1"), true, envs, merge.DefaultBasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, merge.DefaultBasePath)); !os.IsNotExist(err) {
		t.Fatalf("one environment should not get a base: %v", err)
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "development") {
		t.Errorf("notes %q", notes)
	}

	dev := envExport("development", "2.0", "1")
	if _, err := writeMerged(dir, dev, true, envs, merge.DefaultBasePath); err != nil {
		t.Fatal(err)
	}
	files := readTree(t, dir)
	base := files["apps/base/shop/deployment-web.yaml"]
	if !strings.Contains(base, "name: web") {
		t.Errorf("no shared base:\n%v", files)
	}
	for _, env := range []string{"development", "acceptance"} {
		k := files["clusters/"+env+"/apps/kustomization.yaml"]
		if !strings.Contains(k, "apps/base") {
			t.Errorf("%s is not an overlay of the base:\n%s", env, k)
		}
	}
	for _, env := range []string{"development", "acceptance"} {
		// import --validate runs on the merged tree before every push.
		if err := importer.Validate(dir, "clusters/"+env); err != nil {
			t.Errorf("%s overlay does not validate: %v", env, err)
		}
	}
	if !strings.Contains(strings.Join(mapValues(files, "clusters/acceptance/apps/"), "\n"), "nginx:1.9") {
		t.Errorf("acceptance overlay lost its image tag: %v", files)
	}

	// The same Swarm exported again changes nothing, byte for byte.
	if _, err := writeMerged(dir, envExport("development", "2.0", "1"), true, envs, merge.DefaultBasePath); err != nil {
		t.Fatal(err)
	}
	if again := readTree(t, dir); !reflect.DeepEqual(again, files) {
		t.Errorf("unchanged export rewrote files:\n%v\nwant:\n%v", again, files)
	}

	// A third environment joins; the others still build to their exports.
	if _, err := writeMerged(dir, envExport("production", "1.9", "3"), true, envs, merge.DefaultBasePath); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadEnvs(dir, []envSpec{{name: "development"}, {name: "acceptance"}, {name: "production"}})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"nginx:2.0", "nginx:1.9", "nginx:1.9"} {
		img := loaded[i].Objects[0]["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"]
		if img != want {
			t.Errorf("%s: image %v, want %s", loaded[i].Name, img, want)
		}
	}
}

func mapValues(m map[string]string, prefix string) []string {
	var out []string
	for k, v := range m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, v)
		}
	}
	return out
}

func TestCheckMergeEnvs(t *testing.T) {
	for _, c := range []struct {
		names   []string
		cluster string
		ok      bool
	}{
		{[]string{"dev", "prod"}, "dev", true},
		{[]string{"dev", "prod"}, "acc", false},
		{[]string{"dev"}, "dev", false},
		{[]string{"dev", "prod=x"}, "dev", false},
	} {
		if err := checkMergeEnvs(c.names, c.cluster); (err == nil) != c.ok {
			t.Errorf("%v %s: %v", c.names, c.cluster, err)
		}
	}
}
