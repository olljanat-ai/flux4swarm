// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"sigs.k8s.io/yaml"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

// UnifiedDiff returns a unified diff (3 lines of context) between a and b,
// or "" when they are equal. Names label the --- and +++ lines.
func UnifiedDiff(nameA, nameB, a, b string) string {
	if a == b {
		return ""
	}
	la, lb := splitLines(a), splitLines(b)
	ops := diffLines(la, lb)
	const ctx = 3
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", nameA, nameB)
	// Group the edit script into hunks with ctx lines of context.
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-ctx, 0)
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			// Run of equal lines: end the hunk if it is longer than 2*ctx
			// or reaches the end.
			j := end
			for j < len(ops) && ops[j].kind == ' ' {
				j++
			}
			if j == len(ops) || j-end > 2*ctx {
				end = min(end+ctx, len(ops))
				break
			}
			end = j
		}
		aStart, bStart, aLen, bLen := 0, 0, 0, 0
		for k := 0; k < start; k++ {
			if ops[k].kind != '+' {
				aStart++
			}
			if ops[k].kind != '-' {
				bStart++
			}
		}
		for k := start; k < end; k++ {
			if ops[k].kind != '+' {
				aLen++
			}
			if ops[k].kind != '-' {
				bLen++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(aStart, aLen), hunkRange(bStart, bLen))
		for k := start; k < end; k++ {
			out.WriteByte(ops[k].kind)
			out.WriteString(ops[k].line)
			out.WriteByte('\n')
		}
		i = end
	}
	return out.String()
}

func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if n == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

type diffOp struct {
	kind byte // ' ', '-' or '+'
	line string
}

// diffLines is a longest-common-subsequence line diff. Specs are small, so
// the quadratic table is fine.
func diffLines(a, b []string) []diffOp {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// serviceDiff is the unified diff between the live and the desired spec of
// a service, normalized so that only meaningful differences show: IDs,
// the spec-hash label, defaults Docker fills in and the image digest Docker
// pins are left out. live is nil for a service that will be created.
func (a *Applier) serviceDiff(ctx context.Context, name string, live *swarm.ServiceSpec, want swarm.ServiceSpec) string {
	var liveYAML string
	if live != nil {
		l, err := deepCopy(*live)
		if err != nil {
			return ""
		}
		if err := a.networkNames(ctx, &l); err != nil {
			a.log().Warn("diff: list networks", "error", err)
		}
		lc, wc := l.TaskTemplate.ContainerSpec, want.TaskTemplate.ContainerSpec
		if lc != nil && wc != nil && sameImage(lc.Image, wc.Image) {
			lc.Image = wc.Image
		}
		liveYAML = specYAML(l, &want)
	}
	return UnifiedDiff("live/Service/"+name, "desired/Service/"+name, liveYAML, specYAML(want, nil))
}

// networkNames replaces the network IDs Docker stores in a live spec with
// names, as in the desired spec.
func (a *Applier) networkNames(ctx context.Context, s *swarm.ServiceSpec) error {
	if len(s.TaskTemplate.Networks) == 0 {
		return nil
	}
	if a.netNames == nil {
		nets, err := a.Client.NetworkList(ctx, network.ListOptions{})
		if err != nil {
			return err
		}
		a.netNames = map[string]string{}
		for _, n := range nets {
			a.netNames[n.ID] = n.Name
		}
	}
	for i, n := range s.TaskTemplate.Networks {
		if name, ok := a.netNames[n.Target]; ok {
			s.TaskTemplate.Networks[i].Target = name
		}
	}
	return nil
}

// serverDefaults are values Docker fills in when the field is not set.
// They are dropped from the live spec when the desired spec leaves the
// field out.
var serverDefaults = map[string]any{
	"TaskTemplate.ContainerSpec.Isolation": "default",
	"TaskTemplate.Runtime":                 "container",
	"TaskTemplate.ForceUpdate":             float64(0),
	"TaskTemplate.Placement.Platforms":     nil, // any value
	"TaskTemplate.Resources":               nil,
	"EndpointSpec.Mode":                    "vip",
	"UpdateConfig":                         nil,
	"RollbackConfig":                       nil,
}

// specYAML renders a service spec as YAML without IDs and the spec-hash
// label. With ref, fields that ref does not set and that hold a Docker
// default are removed.
func specYAML(s swarm.ServiceSpec, ref *swarm.ServiceSpec) string {
	m := toMap(s)
	if ref != nil {
		r := toMap(*ref)
		for path, def := range serverDefaults {
			if _, set := lookup(r, path); set {
				continue
			}
			if v, ok := lookup(m, path); ok && (def == nil || v == def) {
				remove(m, path)
			}
		}
	}
	if labels, ok := m["Labels"].(map[string]any); ok {
		delete(labels, api.LabelSpecHash)
	}
	if cs, ok := lookupMap(m, "TaskTemplate.ContainerSpec"); ok {
		for _, key := range []string{"Configs", "Secrets"} {
			refs, _ := cs[key].([]any)
			for _, r := range refs {
				if rm, ok := r.(map[string]any); ok {
					delete(rm, "ConfigID")
					delete(rm, "SecretID")
				}
			}
		}
	}
	prune(m)
	b, err := yaml.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

func lookupMap(m map[string]any, path string) (map[string]any, bool) {
	v, ok := lookup(m, path)
	if !ok {
		return nil, false
	}
	mm, ok := v.(map[string]any)
	return mm, ok
}

func lookup(m map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = m
	for _, p := range parts {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func remove(m map[string]any, path string) {
	parts := strings.Split(path, ".")
	parent, ok := m, true
	if len(parts) > 1 {
		parent, ok = lookupMap(m, strings.Join(parts[:len(parts)-1], "."))
	}
	if ok {
		delete(parent, parts[len(parts)-1])
	}
}

// prune removes nulls and empty maps and lists, which encode "not set".
func prune(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case map[string]any:
		for k, x := range t {
			if prune(x) {
				delete(t, k)
			}
		}
		return len(t) == 0
	case []any:
		for _, x := range t {
			prune(x)
		}
		return len(t) == 0
	}
	return false
}

// configDiff is the unified diff between the data of the config version in
// use and the desired data.
func configDiff(name string, live *swarm.Config, data []byte) string {
	liveData := ""
	if live != nil {
		liveData = string(live.Spec.Data)
	}
	return UnifiedDiff("live/Config/"+name, "desired/Config/"+name, liveData, string(data))
}
