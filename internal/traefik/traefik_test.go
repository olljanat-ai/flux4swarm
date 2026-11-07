// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package traefik

import (
	"testing"
)

func TestParseRuleRoundTrip(t *testing.T) {
	cases := map[string]string{
		"Host(`a.example.com`)":                                      "Host(`a.example.com`)",
		"Host(`a.example.com`) && PathPrefix(`/api`)":                "Host(`a.example.com`) && PathPrefix(`/api`)",
		"(Host(`a.com`) || Host(`b.com`)) && Path(`/x`)":             "(Host(`a.com`) || Host(`b.com`)) && Path(`/x`)",
		"Host(`a.com`,`b.com`)":                                      "(Host(`a.com`) || Host(`b.com`))",
		"PathPrefix(`/`)":                                            "PathPrefix(`/`)",
		"Host(`a.com`) && Method(`POST`) && Header(`X-Env`, `prod`)": "Host(`a.com`) && Method(`POST`) && Header(`X-Env`, `prod`)",
		"Host(`a.com`) && (PathPrefix(`/a`) || PathPrefix(`/b`))":    "Host(`a.com`) && (PathPrefix(`/a`) || PathPrefix(`/b`))",
	}
	for in, want := range cases {
		hosts, matches, err := ParseRule(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		var hs []string
		for _, h := range hosts {
			hs = append(hs, "Host(`"+h+"`)")
		}
		if got := buildRule(hs, matches); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestParseRuleUnsupported(t *testing.T) {
	for _, in := range []string{"ClientIP(`10.0.0.0/8`)", "!Host(`a`)", "Host(`a`) || PathPrefix(`/b`)", "Host(`a`"} {
		if _, _, err := ParseRule(in); err == nil {
			t.Errorf("%s: expected an error", in)
		}
	}
}

func TestMiddlewareLabelsRoundTrip(t *testing.T) {
	def := map[string]string{
		"ratelimit.average": "100",
		"ratelimit.burst":   "50",
	}
	spec := unflatten(def)
	rl, ok := spec["rateLimit"].(map[string]any)
	if !ok || rl["average"] != int64(100) {
		t.Fatalf("unexpected spec %#v", spec)
	}
	labels, err := MiddlewareLabels("limit", spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range def {
		if labels["traefik.http.middlewares.limit."+k] != v {
			t.Errorf("label %s = %q, want %q (all: %v)", k, labels["traefik.http.middlewares.limit."+k], v, labels)
		}
	}
}

func TestImportRouter(t *testing.T) {
	p := Parse(map[string]string{
		"traefik.enable":                                       "true",
		"traefik.http.routers.app.rule":                        "Host(`app.example.com`) && PathPrefix(`/api`)",
		"traefik.http.routers.app.entrypoints":                 "websecure",
		"traefik.http.routers.app.tls.certresolver":            "le",
		"traefik.http.routers.app.middlewares":                 "strip@swarm,https",
		"traefik.http.middlewares.strip.stripprefix.prefixes":  "/api",
		"traefik.http.middlewares.https.redirectscheme.scheme": "https",
		"traefik.http.services.app.loadbalancer.server.port":   "8080",
	})
	res := Import(ImportInput{Namespace: "shop", K8sService: "app", Parsed: p, AllMiddlewares: p.Middlewares})
	if len(res.Warnings) > 0 {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	if len(res.Routes) != 1 || len(res.Middlewares) != 0 {
		t.Fatalf("routes %d middlewares %d", len(res.Routes), len(res.Middlewares))
	}
	r := res.Routes[0]
	if string(r.Spec.Hostnames[0]) != "app.example.com" || *r.Spec.ParentRefs[0].SectionName != "websecure" {
		t.Errorf("unexpected route %+v", r.Spec)
	}
	f := r.Spec.Rules[0].Filters
	if len(f) != 2 || f[0].URLRewrite == nil || f[1].RequestRedirect == nil {
		t.Errorf("unexpected filters %+v", f)
	}
	if int(*r.Spec.Rules[0].BackendRefs[0].Port) != 8080 || res.Ports[0] != 8080 {
		t.Errorf("unexpected backend")
	}
}
