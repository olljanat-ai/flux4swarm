// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package merge

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/build"
	"github.com/olljanat-ai/flux4swarm/internal/importer"
	"github.com/olljanat-ai/flux4swarm/internal/render"
)

type fakeSwarm struct {
	services []swarm.Service
	configs  []swarm.Config
	networks []network.Summary
}

func (f *fakeSwarm) ServiceList(context.Context, swarm.ServiceListOptions) ([]swarm.Service, error) {
	return f.services, nil
}
func (f *fakeSwarm) ConfigList(context.Context, swarm.ConfigListOptions) ([]swarm.Config, error) {
	return f.configs, nil
}
func (f *fakeSwarm) ConfigInspectWithRaw(_ context.Context, id string) (swarm.Config, []byte, error) {
	for _, c := range f.configs {
		if c.ID == id {
			return c, nil, nil
		}
	}
	return swarm.Config{}, nil, os.ErrNotExist
}
func (f *fakeSwarm) SecretList(context.Context, swarm.SecretListOptions) ([]swarm.Secret, error) {
	return nil, nil
}
func (f *fakeSwarm) NetworkList(context.Context, network.ListOptions) ([]network.Summary, error) {
	return f.networks, nil
}

// swarmEnv is one environment's Swarm: the same stack with its own image
// tag, replicas, env, config data and Traefik host.
func swarmEnv(tag string, replicas uint64, level, host, conf string, extra bool) *fakeSwarm {
	stack := map[string]string{"com.docker.stack.namespace": "shop"}
	svc := func(name, image string, labels map[string]string, env []string, cfg bool) swarm.Service {
		l := map[string]string{"com.docker.stack.namespace": "shop"}
		for k, v := range labels {
			l[k] = v
		}
		r := replicas
		cs := &swarm.ContainerSpec{Image: image, Env: env, Labels: stack}
		if cfg {
			cs.Configs = []*swarm.ConfigReference{{ConfigID: "cfg1", ConfigName: "shop_nginx",
				File: &swarm.ConfigReferenceFileTarget{Name: "/etc/nginx/conf.d/default.conf", UID: "0", GID: "0", Mode: 0o444}}}
		}
		return swarm.Service{ID: name, Spec: swarm.ServiceSpec{
			Annotations:  swarm.Annotations{Name: "shop_" + name, Labels: l},
			TaskTemplate: swarm.TaskSpec{ContainerSpec: cs, Networks: []swarm.NetworkAttachmentConfig{{Target: "net1", Aliases: []string{name}}}},
			Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &r}},
		}}
	}
	f := &fakeSwarm{
		networks: []network.Summary{{ID: "net1", Name: "shop_default", Scope: "swarm", Driver: "overlay", Labels: stack}},
		configs: []swarm.Config{{ID: "cfg1", Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: "shop_nginx", Labels: stack}, Data: []byte(conf)}}},
		services: []swarm.Service{
			svc("web", "nginx:"+tag, map[string]string{
				"traefik.enable":                                     "true",
				"traefik.http.routers.web.rule":                      "Host(`" + host + "`)",
				"traefik.http.routers.web.entrypoints":               "websecure",
				"traefik.http.routers.web.tls":                       "true",
				"traefik.http.services.web.loadbalancer.server.port": "80",
			}, []string{"TZ=UTC", "LOG_LEVEL=" + level}, true),
			svc("api", "example/api:"+tag, nil, []string{"DB=db:5432"}, false),
		},
	}
	if extra {
		f.services = append(f.services, svc("debug", "busybox:1", nil, nil, false))
	}
	return f
}

func TestMergeImports(t *testing.T) {
	sw := map[string]*fakeSwarm{
		"development": swarmEnv("2.0", 1, "debug", "shop.dev.example.com", "server { listen 80; }\n", true),
		"acceptance":  swarmEnv("1.9", 1, "info", "shop.acc.example.com", "server { listen 80; }\n", false),
		"production":  swarmEnv("1.9", 3, "info", "shop.example.com", "server { listen 443; }\n", false),
	}
	dir := t.TempDir()
	var envs []Env
	for _, name := range []string{"development", "acceptance", "production"} {
		res, err := importer.Export(context.Background(), sw[name], importer.Options{Cluster: name, Bootstrap: true})
		if err != nil {
			t.Fatal(err)
		}
		writeFiles(t, dir, res.Files)
		out, err := build.Render(dir, build.Options{Spec: api.KustomizationSpec{Path: "clusters/" + name + "/apps"}})
		if err != nil {
			t.Fatal(err)
		}
		objs, err := Load(out)
		if err != nil {
			t.Fatal(err)
		}
		envs = append(envs, Env{Name: name, Path: "clusters/" + name + "/apps", Objects: objs})
	}
	want := map[string]string{}
	for _, e := range envs {
		want[e.Name] = desired(t, dir, e.Name)
		if !strings.Contains(want[e.Name], `"shop_web"`) {
			t.Fatalf("%s: no shop_web in %s", e.Name, want[e.Name])
		}
	}

	res, err := Merge(envs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Clean {
		if err := os.RemoveAll(dir + "/" + c); err != nil {
			t.Fatal(err)
		}
	}
	writeFiles(t, dir, res.Files)
	if _, err := os.Stat(dir + "/clusters/production/flux-system/gotk-sync.yaml"); err != nil {
		t.Error("merge removed flux-system:", err)
	}
	base := string(res.Files["apps/base/shop/deployment-web.yaml"])
	for _, w := range []string{"nginx:1.9", "TZ", "LOG_LEVEL"} {
		if !strings.Contains(base, w) {
			t.Errorf("base lacks %q:\n%s", w, base)
		}
	}
	if _, ok := res.Files["clusters/development/apps/shop/deployment-debug.yaml"]; !ok {
		t.Errorf("development-only service not in its overlay: %v", keys(res.Files))
	}
	route := string(res.Files["clusters/production/apps/shop/httproute-web-patch.yaml"])
	if !strings.Contains(route, "shop.example.com") {
		t.Errorf("production route patch:\n%s", route)
	}
	// The whole cluster path (flux-system + overlay) gives the same
	// Swarm state as before the merge.
	for _, e := range envs {
		if got := desired(t, dir, e.Name); got != want[e.Name] {
			t.Errorf("%s: Swarm state changed by the merge:\n%s\nwant:\n%s", e.Name, got, want[e.Name])
		}
	}
}

// desired is the Swarm state reconciling ./clusters/<env> gives.
func desired(t *testing.T, dir, env string) string {
	t.Helper()
	set, err := build.Build(dir, build.Options{Spec: api.KustomizationSpec{Path: "./clusters/" + env}})
	if err != nil {
		t.Fatalf("%s: %v", env, err)
	}
	out, err := render.Render(set, nil)
	if err != nil {
		t.Fatalf("%s: %v", env, err)
	}
	b, err := json.MarshalIndent(out.Desired, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for f, b := range files {
		p := dir + "/" + f
		if err := os.MkdirAll(p[:strings.LastIndex(p, "/")], 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
