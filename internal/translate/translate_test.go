// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package translate_test

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/swarm"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/translate"
)

func decodeString(t *testing.T, yaml string) *api.Set {
	t.Helper()
	set := &api.Set{}
	if err := set.Decode("test", []byte(yaml), translate.DefaultNamespace); err != nil {
		t.Fatal(err)
	}
	return set
}

func deployment(name, ns, podSpec string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata: {name: ` + name + `, namespace: ` + ns + `}
spec:
  selector: {matchLabels: {app: ` + name + `}}
  template:
    metadata: {labels: {app: ` + name + `}}
    spec:
` + podSpec + "\n"
}

func TestTranslateErrors(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want string
	}{
		"two containers": {
			yaml: deployment("a", "default", "      containers: [{name: a, image: a}, {name: b, image: b}]"),
			want: "pods with 2 containers",
		},
		"missing envFrom configMap": {
			yaml: deployment("a", "default", "      containers: [{name: a, image: a, envFrom: [{configMapRef: {name: nope}}]}]"),
			want: `envFrom configMap "nope" not found`,
		},
		"missing secret key": {
			yaml: deployment("a", "default", "      containers: [{name: a, image: a, env: [{name: X, valueFrom: {secretKeyRef: {name: s, key: k}}}]}]"),
			want: "env X: secret key s/k not found",
		},
		"mount without volume": {
			yaml: deployment("a", "default", "      containers: [{name: a, image: a, volumeMounts: [{name: v, mountPath: /v}]}]"),
			want: `volumeMount "v" has no volume`,
		},
		"unsupported volume": {
			yaml: deployment("a", "default", `      containers: [{name: a, image: a, volumeMounts: [{name: v, mountPath: /v}]}]
      volumes: [{name: v, nfs: {server: s, path: /p}}]`),
			want: `volume "v": only configMap`,
		},
		"configMap item key missing": {
			yaml: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, namespace: default}\ndata: {a: b}\n---\n" +
				deployment("a", "default", `      containers: [{name: a, image: a, volumeMounts: [{name: v, mountPath: /v}]}]
      volumes: [{name: v, configMap: {name: c, items: [{key: x, path: x}]}}]`),
			want: `configMap c: key "x" not found`,
		},
		"subPath matches no key": {
			yaml: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c, namespace: default}\ndata: {a: b}\n---\n" +
				deployment("a", "default", `      containers: [{name: a, image: a, volumeMounts: [{name: v, mountPath: /v, subPath: z}]}]
      volumes: [{name: v, configMap: {name: c}}]`),
			want: `subPath "z" matches no key`,
		},
		"NodePort without nodePort": {
			yaml: deployment("a", "default", "      containers: [{name: a, image: a}]") +
				"---\napiVersion: v1\nkind: Service\nmetadata: {name: a, namespace: default}\nspec: {type: NodePort, selector: {app: a}, ports: [{port: 80}]}\n",
			want: "NodePort services need an explicit nodePort",
		},
		"named targetPort not found": {
			yaml: deployment("a", "default", "      containers: [{name: a, image: a}]") +
				"---\napiVersion: v1\nkind: Service\nmetadata: {name: a, namespace: default}\nspec: {type: LoadBalancer, selector: {app: a}, ports: [{port: 80, targetPort: http}]}\n",
			want: `named port "http" not found`,
		},
		"same Swarm service name": {
			yaml: deployment("a", "shop", "      containers: [{name: a, image: a}]") + "---\n" +
				`apiVersion: apps/v1
kind: Deployment
metadata:
  name: b
  namespace: shop
  annotations: {swarm.flux4swarm.io/name: shop_a}
spec:
  selector: {matchLabels: {app: b}}
  template: {metadata: {labels: {app: b}}, spec: {containers: [{name: b, image: b}]}}
`,
			want: `map to the same Swarm service "shop_a"`,
		},
		"service-spec patch with unknown field": {
			yaml: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: a
  namespace: default
  annotations: {swarm.flux4swarm.io/service-spec: '{"NoSuchField": 1}'}
spec:
  selector: {matchLabels: {app: a}}
  template: {metadata: {labels: {app: a}}, spec: {containers: [{name: a, image: a}]}}
`,
			want: "annotation swarm.flux4swarm.io/service-spec",
		},
		"imagePullSecret without dockerconfigjson": {
			yaml: "apiVersion: v1\nkind: Secret\nmetadata: {name: reg, namespace: default}\nstringData: {x: y}\n---\n" +
				deployment("a", "default", "      imagePullSecrets: [{name: reg}]\n      containers: [{name: a, image: a}]"),
			want: "imagePullSecret reg: secret has no .dockerconfigjson key",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			set := decodeString(t, tc.yaml)
			_, err := translate.Translate(translate.Input{Set: set, Secrets: set.Secrets})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got error %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestSwarmName(t *testing.T) {
	for _, tc := range []struct{ ns, name, want string }{
		{"", "web", "web"},
		{"default", "web", "web"},
		{"shop", "web", "shop_web"},
	} {
		if got := translate.SwarmName(tc.ns, tc.name); got != tc.want {
			t.Errorf("SwarmName(%q, %q) = %q, want %q", tc.ns, tc.name, got, tc.want)
		}
	}
	if got := translate.NetworkName(""); got != "default_default" {
		t.Errorf("NetworkName(\"\") = %q", got)
	}
}

func TestPatchRoundTrip(t *testing.T) {
	r := uint64(2)
	from := swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "a"}, Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &r}}}
	to := from
	to.Labels = map[string]string{"x": "y"}
	to.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "nginx"}

	p, err := translate.CreatePatch(from, to)
	if err != nil || p == nil {
		t.Fatalf("patch %s, err %v", p, err)
	}
	got, err := translate.ApplyPatch(from, p)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := translate.CreatePatch(got, to); again != nil {
		t.Errorf("patched spec differs from target: %s", again)
	}
	if same, err := translate.CreatePatch(from, from); err != nil || same != nil {
		t.Errorf("patch of equal specs = %s, %v", same, err)
	}
	// YAML patches are accepted.
	got, err = translate.ApplyPatch(from, []byte("TaskTemplate:\n  ContainerSpec:\n    Image: redis\n"))
	if err != nil || got.TaskTemplate.ContainerSpec.Image != "redis" {
		t.Errorf("yaml patch: %+v, %v", got.TaskTemplate.ContainerSpec, err)
	}
}

func TestBackend(t *testing.T) {
	set := decodeString(t, deployment("web", "shop", "      containers: [{name: web, image: web, ports: [{name: http, containerPort: 8080}]}]")+`---
apiVersion: v1
kind: Service
metadata: {name: web, namespace: shop}
spec: {selector: {app: web}, ports: [{port: 80, targetPort: http}, {port: 81}]}
---
apiVersion: v1
kind: Service
metadata: {name: one, namespace: shop}
spec: {selector: {app: web}, ports: [{port: 9000}]}
---
apiVersion: v1
kind: Service
metadata: {name: none, namespace: shop}
spec: {selector: {app: nothing}, ports: [{port: 80}]}
`)
	backend := translate.Backend(set)
	for _, tc := range []struct {
		svc     string
		port    int32
		wantSvc string
		want    uint32
		err     string
	}{
		{svc: "web", port: 80, wantSvc: "shop_web", want: 8080},
		{svc: "web", port: 81, wantSvc: "shop_web", want: 81},
		{svc: "web", port: 0, err: "has no port 0"},
		{svc: "one", port: 0, wantSvc: "shop_web", want: 9000},
		{svc: "none", port: 80, err: "selects 0 workloads"},
		{svc: "missing", port: 80, err: "not found"},
	} {
		name, port, err := backend("shop", tc.svc, tc.port)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s:%d: error %v, want %q", tc.svc, tc.port, err, tc.err)
			}
			continue
		}
		if err != nil || name != tc.wantSvc || port != tc.want {
			t.Errorf("%s:%d = %s, %d, %v; want %s, %d", tc.svc, tc.port, name, port, err, tc.wantSvc, tc.want)
		}
	}
}
