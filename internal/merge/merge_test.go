// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package merge

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/build"
)

func deployment(image string, replicas int, env map[string]string, extraEnv ...string) string {
	var envs []string
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		envs = append(envs, "        - name: "+k+"\n          value: \""+env[k]+"\"")
	}
	envs = append(envs, extraEnv...)
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
  annotations:
    swarm.flux4swarm.io/service-spec: '{"TaskTemplate":{"RestartPolicy":{"Condition":"any"}}}'
spec:
  replicas: ` + itoa(replicas) + `
  selector:
    matchLabels:
      app.kubernetes.io/name: web
  template:
    metadata:
      labels:
        app.kubernetes.io/name: web
    spec:
      containers:
      - name: web
        image: ` + image + `
        args: ["--port", "80"]
        env:
` + strings.Join(envs, "\n") + `
        volumeMounts:
        - name: config
          mountPath: /etc/nginx/conf.d
      volumes:
      - name: config
        configMap:
          name: nginx
`
}

func itoa(i int) string { b, _ := yaml.Marshal(i); return strings.TrimSpace(string(b)) }

func route(host string) string {
	return `apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: web
  namespace: shop
spec:
  parentRefs:
  - name: traefik
    namespace: traefik
    sectionName: websecure
  hostnames:
  - ` + host + `
  rules:
  - backendRefs:
    - name: web
      port: 80
`
}

const ns = `apiVersion: v1
kind: Namespace
metadata:
  name: shop
`

func configMap(name, data string) string {
	return `apiVersion: v1
kind: ConfigMap
metadata:
  name: ` + name + `
  namespace: shop
data:
  default.conf: "` + data + `"
`
}

func objs(t *testing.T, docs ...string) []map[string]any {
	t.Helper()
	out, err := Load([]byte(strings.Join(docs, "---\n")))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func threeEnvs(t *testing.T) []Env {
	common := map[string]string{"LOG_FORMAT": "json", "TZ": "UTC"}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range common {
			m[a] = b
		}
		m[k] = v
		return m
	}
	return []Env{
		{Name: "development", Path: "clusters/development/apps", Objects: objs(t, ns,
			deployment("nginx:1.27", 1, with("LOG_LEVEL", "debug")), route("shop.dev.example.com"),
			configMap("nginx", "server {}"), configMap("debug", "on"))},
		{Name: "acceptance", Path: "clusters/acceptance/apps", Objects: objs(t, ns,
			deployment("nginx:1.27", 1, with("LOG_LEVEL", "info")), route("shop.acc.example.com"),
			configMap("nginx", "server {}"))},
		{Name: "production", Path: "clusters/production/apps", Objects: objs(t, ns,
			deployment("nginx:1.26", 3, with("LOG_LEVEL", "info"), "        - name: EXTRA\n          value: prod"),
			route("shop.example.com"), configMap("nginx", "server { listen 443; }"))},
	}
}

func TestMergeThreeEnvironments(t *testing.T) {
	envs := threeEnvs(t)
	want := map[string][]map[string]any{}
	for _, e := range envs {
		want[e.Name] = e.Objects
	}
	res, err := Merge(envs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for f, b := range res.Files {
		files[f] = string(b)
	}
	// The base has the majority values.
	base := files["apps/base/shop/deployment-web.yaml"]
	for _, w := range []string{"nginx:1.27", "replicas: 1", "LOG_FORMAT", "LOG_LEVEL", "info", "--port"} {
		if !strings.Contains(base, w) {
			t.Errorf("base deployment lacks %q:\n%s", w, base)
		}
	}
	if strings.Contains(base, "debug") || strings.Contains(base, "EXTRA") {
		t.Errorf("base deployment has environment-only values:\n%s", base)
	}
	if _, ok := files["apps/base/shop/configmap-debug.yaml"]; ok {
		t.Error("development-only ConfigMap is in the base")
	}
	if _, ok := files["clusters/development/apps/shop/configmap-debug.yaml"]; !ok {
		t.Errorf("development-only ConfigMap not in the overlay; files: %v", keys(files))
	}
	// Acceptance only differs in the route hostname.
	acc := files["clusters/acceptance/apps/kustomization.yaml"]
	if !strings.Contains(acc, "../../../apps/base") || !strings.Contains(acc, "shop/httproute-web-patch.yaml") {
		t.Errorf("acceptance kustomization:\n%s", acc)
	}
	if strings.Contains(acc, "deployment-web-patch") {
		t.Errorf("acceptance patches the deployment although it matches the base:\n%s", acc)
	}
	prod := files["clusters/production/apps/shop/deployment-web-patch.yaml"]
	for _, w := range []string{"nginx:1.26", "replicas: 3", "EXTRA"} {
		if !strings.Contains(prod, w) {
			t.Errorf("production patch lacks %q:\n%s", w, prod)
		}
	}
	if strings.Contains(prod, "--port") || strings.Contains(prod, "volumes") {
		t.Errorf("production patch repeats shared values:\n%s", prod)
	}
	dev := files["clusters/development/apps/shop/deployment-web-patch.yaml"]
	if !strings.Contains(dev, "debug") || strings.Contains(dev, "nginx:1.27") {
		t.Errorf("development patch:\n%s", dev)
	}

	// Write it to disk and build every environment the way the
	// controller (and Flux) does.
	dir := t.TempDir()
	for f, b := range res.Files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range envs {
		out, err := build.Render(dir, build.Options{Spec: api.KustomizationSpec{Path: "./clusters/" + e.Name}})
		if err != nil {
			t.Fatalf("%s: %v", e.Name, err)
		}
		got, err := Load(out)
		if err != nil {
			t.Fatal(err)
		}
		if !sameSet(got, want[e.Name]) {
			t.Errorf("%s builds to something else:\n%s", e.Name, out)
		}
	}

	// Merging the merged tree again gives the same files.
	var again []Env
	for _, e := range envs {
		out, err := build.Render(dir, build.Options{Spec: api.KustomizationSpec{Path: e.Path}})
		if err != nil {
			t.Fatal(err)
		}
		o, _ := Load(out)
		again = append(again, Env{Name: e.Name, Path: e.Path, Objects: o})
	}
	res2, err := Merge(again, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Files, res2.Files) {
		t.Error("merging the merged tree changed it")
	}
}

func TestMergeRemovalsAndJSONPatch(t *testing.T) {
	// A field and an env var removed in one of three environments, and a
	// reordered list strategic merge cannot express.
	a := deployment("nginx:1", 2, map[string]string{"A": "1", "B": "2"})
	b := strings.Replace(a, `args: ["--port", "80"]`, `args: ["80", "--port"]`, 1)
	c := strings.Replace(deployment("nginx:1", 2, map[string]string{"A": "1"}), "  annotations:\n    swarm.flux4swarm.io/service-spec: '{\"TaskTemplate\":{\"RestartPolicy\":{\"Condition\":\"any\"}}}'\n", "", 1)
	envs := []Env{
		{Name: "a", Path: "envs/a", Objects: objs(t, a)},
		{Name: "b", Path: "envs/b", Objects: objs(t, b)},
		{Name: "c", Path: "envs/c", Objects: objs(t, c)},
	}
	res, err := Merge(envs, Options{BasePath: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Files["envs/a/kustomization.yaml"]; !ok {
		t.Fatalf("files: %v", keys(res.Files))
	}
	if k := string(res.Files["envs/a/kustomization.yaml"]); !strings.Contains(k, "../../shared") || strings.Contains(k, "patches") {
		t.Errorf("a should be the base itself:\n%s", k)
	}
	if p := string(res.Files["envs/c/shop/deployment-web-patch.yaml"]); p == "" {
		t.Errorf("no patch for c; files %v", keys(res.Files))
	}
}

func TestMergeRejectsBadInput(t *testing.T) {
	one := []Env{{Name: "a", Path: "a", Objects: objs(t, ns)}}
	if _, err := Merge(one, Options{}); err == nil {
		t.Error("one environment accepted")
	}
	two := []Env{{Name: "a", Path: "apps/base/a", Objects: objs(t, ns)}, {Name: "b", Path: "b", Objects: objs(t, ns)}}
	if _, err := Merge(two, Options{}); err == nil {
		t.Error("overlay inside the base accepted")
	}
}

func sameSet(a, b []map[string]any) bool {
	key := func(l []map[string]any) map[string]any {
		m := map[string]any{}
		for _, o := range l {
			obj, _ := newObject(o)
			m[obj.key] = obj.obj
		}
		return m
	}
	return reflect.DeepEqual(key(a), key(b))
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
