// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/git"
)

// maxSubject is the longest subject line CommitMessage writes before it
// switches to a summary with the details in the body.
const maxSubject = 72

// CommitMessage describes an export commit by what changed in the objects,
// for example "shop: api image 1.4→1.5, removed tmp-debug", so the history
// of a watched Swarm reads as an audit log. When the list does not fit on
// one line the subject counts the changes and the body lists them. verb is
// "Import" or "Sync". It returns "" when no object changed (for example only
// a kustomization.yaml), so the caller can fall back to a fixed message.
func CommitMessage(verb string, changes []git.FileChange) string {
	oldObjs, newObjs := map[objKey]map[string]any{}, map[objKey]map[string]any{}
	for _, c := range changes {
		parseObjects(c.Old, oldObjs)
		parseObjects(c.New, newObjs)
	}
	keys := map[objKey]bool{}
	for k := range oldObjs {
		keys[k] = true
	}
	for k := range newObjs {
		keys[k] = true
	}
	sorted := make([]objKey, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].less(sorted[j]) })

	var groups []string
	items := map[string][]string{}
	n := 0
	for _, k := range sorted {
		item := describe(k, oldObjs[k], newObjs[k])
		if item == "" {
			continue
		}
		g := k.Namespace
		if g == "" {
			g = "cluster"
		}
		if items[g] == nil {
			groups = append(groups, g)
		}
		items[g] = append(items[g], item)
		n++
	}
	if n == 0 {
		return ""
	}
	parts := make([]string, len(groups))
	for i, g := range groups {
		parts[i] = g + ": " + strings.Join(items[g], ", ")
	}
	if subject := strings.Join(parts, "; "); len([]rune(subject)) <= maxSubject {
		return subject
	}
	where := strings.Join(groups, ", ")
	if len(groups) > 3 {
		where = fmt.Sprintf("%s and %d more", strings.Join(groups[:3], ", "), len(groups)-3)
	}
	noun := "changes"
	if n == 1 {
		noun = "change"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s Docker Swarm state: %d %s in %s\n\n", verb, n, noun, where)
	for _, g := range groups {
		for _, it := range items[g] {
			fmt.Fprintf(&b, "- %s: %s\n", g, it)
		}
	}
	return b.String()
}

type objKey struct{ Namespace, Kind, Name string }

func (k objKey) less(o objKey) bool {
	if k.Namespace != o.Namespace {
		return k.Namespace < o.Namespace
	}
	if k.Name != o.Name {
		return k.Name < o.Name
	}
	return k.Kind < o.Kind
}

// parseObjects adds the Kubernetes objects in a YAML file to objs. Files that
// do not parse, kustomization.yaml and Namespaces (which only follow the
// objects in them) are left out.
func parseObjects(b []byte, objs map[objKey]map[string]any) {
	for _, doc := range api.SplitYAML(b) {
		var m map[string]any
		if yaml.Unmarshal(doc, &m) != nil || m == nil {
			continue
		}
		kind, _ := m["kind"].(string)
		apiVersion, _ := m["apiVersion"].(string)
		meta, _ := m["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if kind == "" || name == "" || kind == "Namespace" || strings.HasPrefix(apiVersion, "kustomize.config.k8s.io/") {
			continue
		}
		ns, _ := meta["namespace"].(string)
		objs[objKey{Namespace: ns, Kind: kind, Name: name}] = m
	}
}

var workloadKinds = map[string]bool{"Deployment": true, "DaemonSet": true, "StatefulSet": true, "Job": true}

// describe says what happened to one object, or "" when nothing did.
func describe(k objKey, old, cur map[string]any) string {
	label := k.Name
	if !workloadKinds[k.Kind] {
		label = strings.ToLower(k.Kind) + " " + k.Name
	}
	switch {
	case old == nil && cur == nil:
		return ""
	case old == nil:
		return "added " + label
	case cur == nil:
		return "removed " + label
	case reflect.DeepEqual(old, cur):
		return ""
	}
	if !workloadKinds[k.Kind] {
		return "updated " + label
	}
	// Pull out the image and replica changes; whatever else differs is
	// summed up as "settings".
	old, cur = deepCopy(old), deepCopy(cur)
	var details []string
	oldImg, curImg := takeImages(old), takeImages(cur)
	names := make([]string, 0, len(curImg))
	for c := range curImg {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		from, ok := oldImg[c]
		if !ok || from == curImg[c] {
			continue
		}
		d := "image " + imageChange(from, curImg[c])
		if len(curImg) > 1 {
			d = c + " " + d
		}
		details = append(details, d)
	}
	if from, to := take(old, "spec", "replicas"), take(cur, "spec", "replicas"); !reflect.DeepEqual(from, to) && from != nil && to != nil {
		details = append(details, fmt.Sprintf("replicas %v→%v", from, to))
	}
	if len(details) == 0 {
		return "updated " + label
	}
	out := label + " " + strings.Join(details, ", ")
	if !reflect.DeepEqual(old, cur) {
		out += " and settings"
	}
	return out
}

// takeImages removes the container images from a workload and returns them
// by container name. Images of containers that only exist on one side stay
// in place, so they count as "settings".
func takeImages(obj map[string]any) map[string]string {
	out := map[string]string{}
	pod, _ := dig(obj, "spec", "template", "spec").(map[string]any)
	cs, _ := pod["containers"].([]any)
	for _, c := range cs {
		cm, _ := c.(map[string]any)
		name, _ := cm["name"].(string)
		img, ok := cm["image"].(string)
		if !ok {
			continue
		}
		out[name] = img
		delete(cm, "image")
	}
	return out
}

// imageChange writes from→to, leaving out the repository when both sides
// share it: "nginx:1.4", "nginx:1.5" gives "1.4→1.5".
func imageChange(from, to string) string {
	fr, ft := splitImage(from)
	tr, tt := splitImage(to)
	switch {
	case fr != tr || ft == "" || tt == "":
	case ft == tt:
		return ft + " (new digest)"
	default:
		return ft + "→" + tt
	}
	return from + "→" + to
}

// splitImage splits an image reference into repository and tag (or
// digest). A digest pinned next to a tag is dropped, since the tag says
// more to a reader: "nginx:1.5@sha256:..." gives "nginx", "1.5".
func splitImage(ref string) (repo, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		repo, tag = ref[:i], ref[i+1:]
		if j := strings.LastIndex(repo, ":"); j > strings.LastIndex(repo, "/") {
			return repo[:j], repo[j+1:]
		}
		if len(tag) > 19 { // sha256: plus 12 hex digits
			tag = tag[:19]
		}
		return repo, tag
	}
	if j := strings.LastIndex(ref, ":"); j > strings.LastIndex(ref, "/") {
		return ref[:j], ref[j+1:]
	}
	return ref, ""
}

func dig(m map[string]any, path ...string) any {
	var v any = m
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[p]
	}
	return v
}

// take removes and returns the value at path.
func take(m map[string]any, path ...string) any {
	parent, ok := dig(m, path[:len(path)-1]...).(map[string]any)
	if !ok {
		return nil
	}
	v := parent[path[len(path)-1]]
	delete(parent, path[len(path)-1])
	return v
}

func deepCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopy(t)
	case []any:
		s := make([]any, len(t))
		for i, e := range t {
			s[i] = deepCopyValue(e)
		}
		return s
	}
	return v
}
