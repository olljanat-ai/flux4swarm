// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// SwarmNamespace is the Kubernetes namespace that maps to Swarm objects
// without a stack: services made with `docker service create` are imported
// into it, and objects in it are reconciled without a "<namespace>_" prefix
// or com.docker.stack.namespace label. Manifests without metadata.namespace
// also land in it. It defaults to "default" and is set once at startup with
// --default-namespace, so a dedicated Swarm can move to its own namespace in
// a shared Kubernetes cluster.
var SwarmNamespace = "default"

// EnvSwarmNamespace is the environment variable for --default-namespace.
const EnvSwarmNamespace = "FLUX4SWARM_DEFAULT_NAMESPACE"

// SetSwarmNamespace validates and sets SwarmNamespace.
func SetSwarmNamespace(ns string) error {
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		return fmt.Errorf("invalid default namespace %q: %s", ns, strings.Join(errs, "; "))
	}
	SwarmNamespace = ns
	return nil
}
