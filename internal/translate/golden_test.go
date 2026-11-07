// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package translate_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/swarm"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/render"
	"github.com/olljanat-ai/flux4swarm/internal/translate"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// golden is a readable view of translate.Output: config and secret data
// are strings rather than base64.
type golden struct {
	Warnings []string        `json:"warnings,omitempty"`
	Networks []goldenNetwork `json:"networks,omitempty"`
	Configs  []goldenObject  `json:"configs,omitempty"`
	Secrets  []goldenObject  `json:"secrets,omitempty"`
	Services []goldenService `json:"services,omitempty"`
}

type goldenNetwork struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

type goldenObject struct {
	Name   string            `json:"name"`
	Data   string            `json:"data"`
	Labels map[string]string `json:"labels,omitempty"`
	Source string            `json:"source,omitempty"`
}

type goldenService struct {
	Name         string                         `json:"name"`
	Spec         swarm.ServiceSpec              `json:"spec"`
	RegistryAuth map[string]registry.AuthConfig `json:"registryAuth,omitempty"`
}

func view(out *translate.Output) golden {
	g := golden{Warnings: out.Warnings}
	for _, n := range out.Desired.Networks {
		g.Networks = append(g.Networks, goldenNetwork{Name: n.Name, Labels: n.Labels})
	}
	for _, c := range out.Desired.Configs {
		g.Configs = append(g.Configs, goldenObject{Name: c.Name, Data: string(c.Data), Labels: c.Labels})
	}
	for _, s := range out.Desired.Secrets {
		g.Secrets = append(g.Secrets, goldenObject{Name: s.Name, Data: string(s.Data), Labels: s.Labels, Source: s.Source})
	}
	for _, s := range out.Desired.Services {
		auth := s.RegistryAuth
		if len(auth) == 0 {
			auth = nil
		}
		g.Services = append(g.Services, goldenService{Name: s.Name, Spec: s.Spec, RegistryAuth: auth})
	}
	return g
}

func marshal(t *testing.T, out *translate.Output) []byte {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(view(out)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func decode(t *testing.T, file string) *api.Set {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	set := &api.Set{}
	if err := set.Decode(filepath.Base(file), data, api.SwarmNamespace); err != nil {
		t.Fatal(err)
	}
	return set
}

// TestGolden renders every testdata/*.yaml (Kubernetes manifests in) and
// compares the result with testdata/*.golden.json (Swarm desired state
// out). Run `go test ./internal/translate -update` to rewrite the golden
// files after an intended change, and review the diff.
func TestGolden(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join("testdata", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) == 0 {
		t.Fatal("no test inputs")
	}
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".yaml")
		t.Run(name, func(t *testing.T) {
			set := decode(t, in)
			out, err := render.Render(set, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := marshal(t, out)
			file := filepath.Join("testdata", name+".golden.json")
			if *update {
				if err := os.WriteFile(file, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("output differs from %s (run with -update to accept):\n%s", file, lineDiff(string(want), string(got)))
			}
			// Translation must be deterministic.
			again, err := render.Render(decode(t, in), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(marshal(t, again), got) {
				t.Error("second render differs from the first")
			}
		})
	}
}

// lineDiff shows the first differing lines with some context.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	i := 0
	for i < len(w) && i < len(g) && w[i] == g[i] {
		i++
	}
	from := max(0, i-3)
	var b strings.Builder
	for j := from; j < i; j++ {
		b.WriteString("  " + w[j] + "\n")
	}
	for j := i; j < min(len(w), i+5); j++ {
		b.WriteString("- " + w[j] + "\n")
	}
	for j := i; j < min(len(g), i+5); j++ {
		b.WriteString("+ " + g[j] + "\n")
	}
	return b.String()
}
