// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

func TestDecodeRecordsIgnoredReasons(t *testing.T) {
	doc := `apiVersion: policy/v1
kind: PodDisruptionBudget
metadata: {name: web, namespace: shop}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: skip-me
  annotations: {swarm.flux4swarm.io/ignore: "true"}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: disabled-cm
  annotations: {kustomize.toolkit.fluxcd.io/reconcile: disabled}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: source-controller
  labels: {app.kubernetes.io/part-of: flux}
`
	s := &Set{}
	if err := s.Decode("test", []byte(doc), DefaultNamespace); err != nil {
		t.Fatal(err)
	}
	want := []IgnoredObject{
		{"policy/v1 PodDisruptionBudget shop/web", IgnoredUnsupported},
		{"v1 ConfigMap /skip-me", IgnoredAnnotation},
		{"v1 ConfigMap /disabled-cm", IgnoredReconcileOff},
		{"apps/v1 Deployment /source-controller", IgnoredFluxComponent},
	}
	if len(s.Ignored) != len(want) {
		t.Fatalf("ignored %v", s.Ignored)
	}
	for i := range want {
		if s.Ignored[i] != want[i] {
			t.Errorf("ignored[%d] = %v, want %v", i, s.Ignored[i], want[i])
		}
	}
	if len(s.ConfigMaps)+len(s.Deployments) != 0 {
		t.Errorf("ignored objects were decoded")
	}
}
