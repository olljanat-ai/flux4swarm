// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/build"
	"github.com/olljanat-ai/flux4swarm/internal/render"
	"github.com/olljanat-ai/flux4swarm/internal/secrets"
	"github.com/olljanat-ai/flux4swarm/internal/translate"
)

type fakeDocker struct {
	services []swarm.Service
	configs  []swarm.Config
	secrets  []swarm.Secret
	networks []network.Summary
}

func (f *fakeDocker) ServiceList(context.Context, swarm.ServiceListOptions) ([]swarm.Service, error) {
	return f.services, nil
}
func (f *fakeDocker) ConfigList(context.Context, swarm.ConfigListOptions) ([]swarm.Config, error) {
	return f.configs, nil
}
func (f *fakeDocker) ConfigInspectWithRaw(_ context.Context, id string) (swarm.Config, []byte, error) {
	for _, c := range f.configs {
		if c.ID == id {
			return c, nil, nil
		}
	}
	return swarm.Config{}, nil, os.ErrNotExist
}
func (f *fakeDocker) SecretList(context.Context, swarm.SecretListOptions) ([]swarm.Secret, error) {
	return f.secrets, nil
}
func (f *fakeDocker) NetworkList(context.Context, network.ListOptions) ([]network.Summary, error) {
	return f.networks, nil
}

func stackService() *fakeDocker {
	replicas := uint64(2)
	grace := 20 * time.Second
	stack := map[string]string{"com.docker.stack.namespace": "shop"}
	return &fakeDocker{
		networks: []network.Summary{{ID: "net1", Name: "shop_default", Scope: "swarm", Driver: "overlay", Labels: stack}},
		configs: []swarm.Config{{ID: "cfg1", Spec: swarm.ConfigSpec{
			Annotations: swarm.Annotations{Name: "shop_nginx", Labels: stack}, Data: []byte("server {}\n")}}},
		secrets: []swarm.Secret{{ID: "sec1", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "shop_dbpw", Labels: stack}}}},
		services: []swarm.Service{{ID: "svc1", Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{Name: "shop_web", Labels: map[string]string{
				"com.docker.stack.namespace":                         "shop",
				"com.docker.stack.image":                             "nginx:1.27",
				"traefik.enable":                                     "true",
				"traefik.http.routers.web.rule":                      "Host(`shop.example.com`)",
				"traefik.http.routers.web.entrypoints":               "websecure",
				"traefik.http.routers.web.tls":                       "true",
				"traefik.http.routers.web.service":                   "web",
				"traefik.http.services.web.loadbalancer.server.port": "80",
				"traefik.swarm.network":                              "shop_default",
			}},
			TaskTemplate: swarm.TaskSpec{
				ContainerSpec: &swarm.ContainerSpec{
					Image:           "nginx:1.27@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
					Env:             []string{"A=1", "B=two words"},
					Labels:          map[string]string{"com.docker.stack.namespace": "shop"},
					StopGracePeriod: &grace,
					Mounts: []mount.Mount{
						{Type: mount.TypeVolume, Source: "shop_data", Target: "/data",
							VolumeOptions: &mount.VolumeOptions{Labels: stack}},
						{Type: mount.TypeBind, Source: "/srv/logs", Target: "/logs", ReadOnly: true},
					},
					Configs: []*swarm.ConfigReference{{ConfigID: "cfg1", ConfigName: "shop_nginx",
						File: &swarm.ConfigReferenceFileTarget{Name: "/etc/nginx/conf.d/default.conf", UID: "0", GID: "0", Mode: 0o444}}},
					Secrets: []*swarm.SecretReference{{SecretID: "sec1", SecretName: "shop_dbpw",
						File: &swarm.SecretReferenceFileTarget{Name: "dbpw", UID: "0", GID: "0", Mode: 0o444}}},
					Isolation: "default",
				},
				Networks:  []swarm.NetworkAttachmentConfig{{Target: "net1", Aliases: []string{"web"}}},
				Placement: &swarm.Placement{Constraints: []string{"node.role == manager"}, Platforms: []swarm.Platform{{Architecture: "amd64", OS: "linux"}}},
				Runtime:   swarm.RuntimeContainer,
			},
			Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
			EndpointSpec: &swarm.EndpointSpec{Mode: swarm.ResolutionModeVIP, Ports: []swarm.PortConfig{{Protocol: "tcp", TargetPort: 80, PublishedPort: 8080, PublishMode: "ingress"}}},
		}}},
	}
}

func TestExportRoundTrip(t *testing.T) {
	fd := stackService()
	res, err := Export(context.Background(), fd, Options{Cluster: "prod", Bootstrap: true, SecretStore: "fake", SecretStoreProvider: ProviderFake})
	if err != nil {
		t.Fatal(err)
	}
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
	for _, want := range []string{
		"clusters/prod/flux-system/gotk-sync.yaml",
		"clusters/prod/apps/shop/deployment-web.yaml",
		"clusters/prod/apps/shop/configmap-nginx.yaml",
		"clusters/prod/apps/shop/externalsecret-dbpw.yaml",
		"clusters/prod/apps/shop/persistentvolumeclaim-data.yaml",
		"clusters/prod/apps/shop/httproute-web.yaml",
		"clusters/prod/apps/traefik/gateway.yaml",
	} {
		if _, ok := res.Files[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
	dep := string(res.Files["clusters/prod/apps/shop/deployment-web.yaml"])
	if strings.Contains(dep, "sha256:") {
		t.Errorf("digest should be stripped without --adopt:\n%s", dep)
	}

	// Build the export like the controller does and compare.
	set, err := build.Build(dir, build.Options{Spec: specFor("./clusters/prod")})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := (&secrets.Resolver{}).Resolve(context.Background(), set.ExternalSecrets, set.SecretStores)
	if err != nil {
		t.Fatal(err)
	}
	out, err := render.Render(set, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Desired.Services) != 1 {
		t.Fatalf("got %d services", len(out.Desired.Services))
	}
	got := out.Desired.Services[0]
	if got.Name != "shop_web" {
		t.Fatalf("name %s", got.Name)
	}
	want, _ := cleanService(fd.services[0].Spec, map[string]string{"net1": "shop_default"},
		map[string]swarm.Config{"cfg1": fd.configs[0]}, map[string]swarm.Secret{"sec1": fd.secrets[0]}, false)
	gen := got.Spec
	normalize(&want, &gen)
	if p, _ := translate.CreatePatch(gen, want); p != nil {
		t.Errorf("round trip differs: %s", p)
	}
	// Configs and secrets are materialised under the ConfigMap/Secret names.
	if len(out.Desired.Configs) != 1 || out.Desired.Configs[0].Name != "shop_nginx_default.conf" {
		b, _ := json.Marshal(out.Desired.Configs)
		t.Errorf("configs: %s", b)
	}
	if len(out.Desired.Secrets) != 1 || out.Desired.Secrets[0].Name != "shop_dbpw_dbpw" || string(out.Desired.Secrets[0].Data) != "REPLACE_ME" {
		t.Errorf("secrets: %+v", out.Desired.Secrets)
	}
}

// A service made with `docker service create` (no stack label) is imported
// into --default-namespace, and reconciling that namespace gives the same
// unprefixed service back.
func TestExportPlainServiceDefaultNamespace(t *testing.T) {
	prev := api.SwarmNamespace
	t.Cleanup(func() { api.SwarmNamespace = prev })
	if err := api.SetSwarmNamespace("team-a"); err != nil {
		t.Fatal(err)
	}
	replicas := uint64(1)
	fd := &fakeDocker{services: []swarm.Service{{ID: "svc1", Spec: swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "web"},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image: "nginx:1.27@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
	}}}}
	res, err := Export(context.Background(), fd, Options{Cluster: "prod", SecretStore: "fake", SecretStoreProvider: ProviderFake})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"clusters/prod/apps/team-a/deployment-web.yaml", "clusters/prod/apps/team-a/namespace.yaml"} {
		if _, ok := res.Files[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
	if dep := string(res.Files["clusters/prod/apps/team-a/deployment-web.yaml"]); !strings.Contains(dep, "namespace: team-a") {
		t.Errorf("deployment not in team-a:\n%s", dep)
	}
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
	set, err := build.Build(dir, build.Options{Spec: specFor("./clusters/prod")})
	if err != nil {
		t.Fatal(err)
	}
	out, err := render.Render(set, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Desired.Services) != 1 {
		t.Fatalf("got %d services", len(out.Desired.Services))
	}
	got := out.Desired.Services[0]
	if got.Name != "web" {
		t.Fatalf("name %s, want web", got.Name)
	}
	if l := got.Spec.Labels[api.LabelStackNamespace]; l != "" {
		t.Errorf("unexpected stack label %q", l)
	}
	want, _ := cleanService(fd.services[0].Spec, nil, nil, nil, false)
	gen := got.Spec
	normalize(&want, &gen)
	if p, _ := translate.CreatePatch(gen, want); p != nil {
		t.Errorf("round trip differs: %s", p)
	}
}
