// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package merge

import (
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// pick computes the base value of a field from its values in the
// environments that have it (n is the number of environments). A value
// goes in the base when more than half of the environments agree on it;
// maps and name-keyed lists (containers, env, volumes...) are merged
// field by field, so the base keeps everything the majority shares and
// the overlays only patch what differs.
func pick(vals []any, n int) (any, bool) {
	if len(vals)*2 <= n {
		return nil, false
	}
	// A value shared by a majority.
	groups := map[string]int{}
	for _, v := range vals {
		groups[canonical(v)]++
	}
	for _, v := range vals {
		if groups[canonical(v)]*2 > n {
			return v, true
		}
	}
	if maps, ok := allMaps(vals); ok {
		out := map[string]any{}
		for _, k := range unionKeys(maps) {
			var sub []any
			for _, m := range maps {
				if v, ok := m[k]; ok {
					sub = append(sub, v)
				}
			}
			if v, ok := pick(sub, n); ok {
				out[k] = v
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	}
	if lists, ok := allNamedLists(vals); ok {
		var names []string
		elems := map[string][]any{}
		for _, l := range lists {
			for _, e := range l {
				name := e.(map[string]any)["name"].(string)
				if _, seen := elems[name]; !seen {
					names = append(names, name)
				}
				elems[name] = append(elems[name], e)
			}
		}
		var out []any
		for _, name := range names {
			if v, ok := pick(elems[name], n); ok {
				out = append(out, v)
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	}
	return nil, false
}

// smpDiff is a strategic merge patch from base to target: changed map
// keys, null for removed keys, name-keyed list elements with only their
// changes ($patch: delete for removed ones) and other values whole.
// Merge checks the result with kustomize and falls back to JSON6902.
func smpDiff(base, target any) (any, bool) {
	if reflect.DeepEqual(base, target) {
		return nil, false
	}
	bm, ok1 := base.(map[string]any)
	tm, ok2 := target.(map[string]any)
	if ok1 && ok2 {
		out := map[string]any{}
		for k, tv := range tm {
			bv, ok := bm[k]
			if !ok {
				out[k] = tv
				continue
			}
			if d, ok := smpDiff(bv, tv); ok {
				out[k] = d
			}
		}
		for k := range bm {
			if _, ok := tm[k]; !ok {
				out[k] = nil
			}
		}
		return out, true
	}
	if bl, ok := namedList(base); ok {
		if tl, ok := namedList(target); ok {
			return namedSMP(bl, tl), true
		}
	}
	return target, true
}

// namedSMP diffs name-keyed lists. A strategic merge puts the patch's
// elements first, in patch order, followed by the other base elements,
// so the patch lists the target's elements up to the last one that
// changes (unchanged ones by name only) and as many more as the order
// of the rest needs.
func namedSMP(bl, tl []map[string]any) []any {
	byName := map[string]map[string]any{}
	for _, e := range bl {
		byName[e["name"].(string)] = e
	}
	inTarget := map[string]bool{}
	last := -1
	for i, e := range tl {
		name := e["name"].(string)
		inTarget[name] = true
		if be, ok := byName[name]; !ok || !reflect.DeepEqual(be, e) {
			last = i
		}
	}
	k := last + 1
	for ; k < len(tl); k++ {
		patched := map[string]bool{}
		for _, e := range tl[:k] {
			patched[e["name"].(string)] = true
		}
		var rest []string
		for _, e := range bl {
			if n := e["name"].(string); inTarget[n] && !patched[n] {
				rest = append(rest, n)
			}
		}
		ok := len(rest) == len(tl)-k
		for i := 0; ok && i < len(rest); i++ {
			ok = rest[i] == tl[k+i]["name"].(string)
		}
		if ok {
			break
		}
	}
	var out []any
	for _, e := range tl[:k] {
		name := e["name"].(string)
		be, ok := byName[name]
		if !ok {
			out = append(out, e)
			continue
		}
		dm := map[string]any{}
		if d, ok := smpDiff(be, e); ok {
			dm = d.(map[string]any)
		}
		dm["name"] = name
		out = append(out, dm)
	}
	for _, e := range bl {
		if name := e["name"].(string); !inTarget[name] {
			out = append(out, map[string]any{"name": name, "$patch": "delete"})
		}
	}
	return out
}

// jsonOp is one RFC 6902 operation.
type jsonOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// MarshalJSON keeps an explicit null value for add/replace.
func (o jsonOp) MarshalJSON() ([]byte, error) {
	m := map[string]any{"op": o.Op, "path": o.Path}
	if o.Op != "remove" {
		m["value"] = o.Value
	}
	return json.Marshal(m)
}

// jsonDiff is an exact JSON6902 patch from base to target.
func jsonDiff(base, target any, p string, ops []jsonOp) []jsonOp {
	if reflect.DeepEqual(base, target) {
		return ops
	}
	bm, ok1 := base.(map[string]any)
	tm, ok2 := target.(map[string]any)
	if ok1 && ok2 {
		for _, k := range sortedKeys(bm) {
			if _, ok := tm[k]; !ok {
				ops = append(ops, jsonOp{Op: "remove", Path: p + "/" + escape(k)})
			}
		}
		for _, k := range sortedKeys(tm) {
			if bv, ok := bm[k]; ok {
				ops = jsonDiff(bv, tm[k], p+"/"+escape(k), ops)
			} else {
				ops = append(ops, jsonOp{Op: "add", Path: p + "/" + escape(k), Value: tm[k]})
			}
		}
		return ops
	}
	bl, ok1 := base.([]any)
	tl, ok2 := target.([]any)
	if ok1 && ok2 && len(bl) == len(tl) {
		for i := range bl {
			ops = jsonDiff(bl[i], tl[i], p+"/"+strconv.Itoa(i), ops)
		}
		return ops
	}
	return append(ops, jsonOp{Op: "replace", Path: p, Value: target})
}

func escape(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}

func canonical(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func allMaps(vals []any) ([]map[string]any, bool) {
	var out []map[string]any
	for _, v := range vals {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

func allNamedLists(vals []any) ([][]any, bool) {
	var out [][]any
	for _, v := range vals {
		if _, ok := namedList(v); !ok {
			return nil, false
		}
		out = append(out, v.([]any))
	}
	return out, true
}

// namedList reports whether v is a list of maps with unique string names.
func namedList(v any) ([]map[string]any, bool) {
	l, ok := v.([]any)
	if !ok || len(l) == 0 {
		return nil, false
	}
	seen := map[string]bool{}
	var out []map[string]any
	for _, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, false
		}
		name, ok := m["name"].(string)
		if !ok || seen[name] {
			return nil, false
		}
		seen[name] = true
		out = append(out, m)
	}
	return out, true
}

func unionKeys(maps []map[string]any) []string {
	seen := map[string]bool{}
	var keys []string
	for _, m := range maps {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
