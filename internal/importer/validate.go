// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"fmt"
	"path"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/build"
)

// ValidationError means the exported tree does not kustomize-build, so it
// was not committed: Flux on Kubernetes would fail on it. The usual cause
// is a platform-team overlay or patch outside apps/ that names a Swarm
// service which was renamed or removed.
type ValidationError struct {
	// Path is the Kustomization path that failed, relative to the
	// repository root.
	Path string
	Err  error
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("kustomize build of %s failed, not pushing: %v", e.Path, e.Err)
}

func (e *ValidationError) Unwrap() error { return e.Err }

// Validate builds the cluster directory below root the way Flux's
// kustomize-controller does, then every Flux Kustomization it contains
// that reads the same repository (sourceRef GitRepository flux-system,
// as flux bootstrap names it), with that Kustomization's patches,
// images, targetNamespace and the rest applied. The first failing build
// is returned as a *ValidationError.
func Validate(root, cluster string) error {
	seen := map[string]bool{}
	queue := []api.KustomizationSpec{{Path: cluster}}
	for len(queue) > 0 {
		spec := queue[0]
		queue = queue[1:]
		p := path.Clean(strings.TrimPrefix(spec.Path, "./"))
		key := p + "\x00" + specKey(spec)
		if seen[key] {
			continue
		}
		seen[key] = true
		spec.Path = p
		out, err := build.Render(root, build.Options{Spec: spec})
		if err != nil {
			return &ValidationError{Path: p, Err: err}
		}
		for _, doc := range api.SplitYAML(out) {
			var k api.Kustomization
			if err := yaml.Unmarshal(doc, &k); err != nil {
				continue
			}
			if k.Kind != api.KindKustomization || !strings.HasPrefix(k.APIVersion, "kustomize.toolkit.fluxcd.io/") {
				continue
			}
			ref := k.Spec.SourceRef
			if ref.Kind != api.KindGitRepository || ref.Name != "flux-system" ||
				(ref.Namespace != "" && ref.Namespace != api.DefaultNamespace) {
				continue
			}
			if k.Spec.Path == "" {
				k.Spec.Path = "."
			}
			queue = append(queue, k.Spec)
		}
	}
	return nil
}

// specKey distinguishes two Kustomizations of the same path with
// different overlays.
func specKey(s api.KustomizationSpec) string {
	s.Path = ""
	b, _ := yaml.Marshal(s)
	return string(b)
}
