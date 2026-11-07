// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package render turns a built set of Kubernetes objects into the Swarm
// desired state: Traefik labels from HTTPRoutes plus the workload
// translation.
package render

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/traefik"
	"github.com/olljanat-ai/flux4swarm/internal/translate"
)

// Render translates set. secrets are the Secrets ESO would have created
// for the set's ExternalSecrets; the set's own Secrets are added.
func Render(set *api.Set, secrets []*corev1.Secret) (*translate.Output, error) {
	all := append(append([]*corev1.Secret(nil), set.Secrets...), secrets...)
	routeLabels, warnings, err := traefik.RouteLabels(set, all, translate.Backend(set))
	if err != nil {
		return nil, err
	}
	out, err := translate.Translate(translate.Input{Set: set, Secrets: all, RouteLabels: routeLabels})
	if err != nil {
		return nil, err
	}
	out.Warnings = append(warnings, out.Warnings...)
	return out, nil
}
