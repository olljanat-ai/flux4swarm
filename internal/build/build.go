// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package build runs a Flux-compatible kustomize build: like Flux's
// kustomize-controller it generates a kustomization.yaml when the path has
// none, applies the Kustomization's targetNamespace, namePrefix/Suffix,
// patches, images, components and commonMetadata, builds with the real
// kustomize library and runs postBuild variable substitution.
package build

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/drone/envsubst"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/yaml"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

var kustomizationFiles = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// Options for Build.
type Options struct {
	// Spec is the Flux Kustomization spec (path and overlay fields).
	Spec api.KustomizationSpec
	// Vars are extra variables for postBuild (from substituteFrom).
	Vars map[string]string
}

// Build builds root/opts.Spec.Path and decodes the result.
func Build(root string, opts Options) (*api.Set, error) {
	out, err := Render(root, opts)
	if err != nil {
		return nil, err
	}
	set := &api.Set{}
	if err := set.Decode(opts.Spec.Path, out, "default"); err != nil {
		return nil, err
	}
	return set, nil
}

// Render returns the multi-document YAML of the build.
func Render(root string, opts Options) ([]byte, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = r
	}
	rel := filepath.Clean(strings.TrimPrefix(filepath.FromSlash(opts.Spec.Path), "./"))
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path %q is outside the source", opts.Spec.Path)
	}
	if st, err := os.Stat(filepath.Join(absRoot, rel)); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("kustomization path not found: %s", opts.Spec.Path)
	}

	// Work on a copy so generated files never touch the source.
	tmp, err := os.MkdirTemp("", "flux4swarm-build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := copyTree(absRoot, tmp); err != nil {
		return nil, err
	}
	dir := filepath.Join(tmp, rel)
	if err := generate(dir); err != nil {
		return nil, err
	}
	if err := overlay(dir, opts.Spec); err != nil {
		return nil, err
	}

	ko := krusty.MakeDefaultOptions()
	ko.LoadRestrictions = types.LoadRestrictionsNone
	k := krusty.MakeKustomizer(ko)
	rm, err := k.Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return nil, fmt.Errorf("kustomize build failed: %s", strings.NewReplacer(tmp+"/", "", tmp, "").Replace(err.Error()))
	}
	if cm := opts.Spec.CommonMetadata; cm != nil {
		for _, r := range rm.Resources() {
			if len(cm.Labels) > 0 {
				l := r.GetLabels()
				if l == nil {
					l = map[string]string{}
				}
				for k, v := range cm.Labels {
					l[k] = v
				}
				if err := r.SetLabels(l); err != nil {
					return nil, err
				}
			}
			if len(cm.Annotations) > 0 {
				a := r.GetAnnotations()
				if a == nil {
					a = map[string]string{}
				}
				for k, v := range cm.Annotations {
					a[k] = v
				}
				if err := r.SetAnnotations(a); err != nil {
					return nil, err
				}
			}
		}
	}

	var out strings.Builder
	vars := map[string]string{}
	for k, v := range opts.Vars {
		vars[k] = v
	}
	substitute := opts.Spec.PostBuild != nil || len(opts.Vars) > 0
	if opts.Spec.PostBuild != nil {
		for k, v := range opts.Spec.PostBuild.Substitute {
			vars[k] = v
		}
	}
	for i, r := range rm.Resources() {
		b, err := r.AsYAML()
		if err != nil {
			return nil, err
		}
		if substitute && r.GetAnnotations()["kustomize.toolkit.fluxcd.io/substitute"] != "disabled" {
			b, err = substituteVars(b, vars)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: postBuild substitution: %w", r.GetKind(), r.GetName(), err)
			}
		}
		if i > 0 {
			out.WriteString("---\n")
		}
		out.Write(b)
	}
	return []byte(out.String()), nil
}

var varName = regexp.MustCompile(`^[_a-zA-Z][_a-zA-Z0-9]*$`)

// substituteVars mirrors Flux: ${var}, ${var:=default}, $${escaped}.
func substituteVars(b []byte, vars map[string]string) ([]byte, error) {
	for k := range vars {
		if !varName.MatchString(k) {
			return nil, fmt.Errorf("invalid variable name %q", k)
		}
	}
	s, err := envsubst.Eval(string(b), func(k string) string { return vars[k] })
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

// generate writes a kustomization.yaml listing every manifest and
// kustomization directory below dir, like Flux does, if dir has none.
func generate(dir string) error {
	if hasKustomization(dir) {
		return nil
	}
	var resources []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			if hasKustomization(p) {
				resources = append(resources, filepath.ToSlash(rel))
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		if ok, err := isManifest(p); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		} else if ok {
			resources = append(resources, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(resources)
	k := map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  resources,
	}
	b, err := yaml.Marshal(k)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "kustomization.yaml"), b, 0o644)
}

func isManifest(p string) (bool, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return false, err
	}
	for _, doc := range api.SplitYAML(b) {
		var h struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &h); err != nil {
			return false, nil
		}
		if h.APIVersion != "" && h.Kind != "" && !strings.HasPrefix(h.APIVersion, "kustomize.config.k8s.io") {
			return true, nil
		}
	}
	return false, nil
}

func hasKustomization(dir string) bool {
	for _, k := range kustomizationFiles {
		if _, err := os.Stat(filepath.Join(dir, k)); err == nil {
			return true
		}
	}
	return false
}

// overlay wraps dir's kustomization with the fields of the Flux spec.
func overlay(dir string, spec api.KustomizationSpec) error {
	if spec.TargetNamespace == "" && spec.NamePrefix == "" && spec.NameSuffix == "" &&
		len(spec.Patches) == 0 && len(spec.Images) == 0 && len(spec.Components) == 0 {
		return nil
	}
	var file string
	for _, k := range kustomizationFiles {
		if _, err := os.Stat(filepath.Join(dir, k)); err == nil {
			file = filepath.Join(dir, k)
			break
		}
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	k := map[string]any{}
	if err := yaml.Unmarshal(raw, &k); err != nil {
		return err
	}
	if spec.TargetNamespace != "" {
		k["namespace"] = spec.TargetNamespace
	}
	if spec.NamePrefix != "" {
		k["namePrefix"] = spec.NamePrefix
	}
	if spec.NameSuffix != "" {
		k["nameSuffix"] = spec.NameSuffix
	}
	appendRaw := func(key string, items []json.RawMessage) error {
		if len(items) == 0 {
			return nil
		}
		existing, _ := k[key].([]any)
		for _, it := range items {
			var v any
			if err := json.Unmarshal(it, &v); err != nil {
				return err
			}
			existing = append(existing, v)
		}
		k[key] = existing
		return nil
	}
	if err := appendRaw("patches", spec.Patches); err != nil {
		return err
	}
	if err := appendRaw("images", spec.Images); err != nil {
		return err
	}
	if len(spec.Components) > 0 {
		existing, _ := k["components"].([]any)
		for _, c := range spec.Components {
			existing = append(existing, c)
		}
		k["components"] = existing
	}
	b, err := yaml.Marshal(k)
	if err != nil {
		return err
	}
	return os.WriteFile(file, b, 0o644)
}

// copyTree copies src to dst, skipping .git and symlinks that leave src.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if d.Type()&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				return nil
			}
			if r, err := filepath.Rel(src, real); err != nil || strings.HasPrefix(r, "..") {
				return nil // points outside the source: ignore, like Flux's secure fs
			}
			st, err := os.Stat(real)
			if err != nil || st.IsDir() {
				return nil
			}
			return copyFile(real, target)
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
