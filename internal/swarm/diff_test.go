// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package swarm

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/swarm"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

func TestUnifiedDiff(t *testing.T) {
	if d := UnifiedDiff("a", "b", "x\n", "x\n"); d != "" {
		t.Fatalf("equal inputs: %q", d)
	}
	a := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n"
	b := "1\n2\n3\n4\nfive\n6\n7\n8\n9\n10\n11\n12\n13\n"
	want := `--- a
+++ b
@@ -2,7 +2,7 @@
 2
 3
 4
-5
+five
 6
 7
 8
@@ -10,3 +10,4 @@
 10
 11
 12
+13
`
	if got := UnifiedDiff("a", "b", a, b); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	created := UnifiedDiff("a", "b", "", "x\ny\n")
	if !strings.Contains(created, "@@ -0,0 +1,2 @@\n+x\n+y\n") {
		t.Errorf("create diff:\n%s", created)
	}
}

func TestSpecYAMLNormalizes(t *testing.T) {
	replicas := uint64(1)
	want := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "web", Labels: map[string]string{api.LabelSpecHash: "new", "a": "1"}},
		Mode:        swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image:   "nginx:alpine",
			Configs: []*swarm.ConfigReference{{ConfigName: "c-1", ConfigID: "dry-run"}},
		}},
	}
	live := want
	live.Labels = map[string]string{api.LabelSpecHash: "old", "a": "1"}
	live.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{
		Image:     "nginx:alpine",
		Isolation: "default",
		Configs:   []*swarm.ConfigReference{{ConfigName: "c-1", ConfigID: "abc123"}},
	}
	live.TaskTemplate.Runtime = "container"
	live.TaskTemplate.Placement = &swarm.Placement{Platforms: []swarm.Platform{{Architecture: "amd64", OS: "linux"}}}
	live.EndpointSpec = &swarm.EndpointSpec{Mode: swarm.ResolutionModeVIP}
	if l, w := specYAML(live, &want), specYAML(want, nil); l != w {
		t.Errorf("defaults, IDs and spec-hash should not show:\n%s", UnifiedDiff("live", "want", l, w))
	}
	live.TaskTemplate.ContainerSpec.Env = []string{"A=1"}
	d := UnifiedDiff("live", "want", specYAML(live, &want), specYAML(want, nil))
	if !strings.Contains(d, "-    Env:\n-    - A=1\n") {
		t.Errorf("env drift not shown:\n%s", d)
	}
}
