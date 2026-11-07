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

func TestDecodeDeliveryKinds(t *testing.T) {
	doc := `apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: web, namespace: shop}
spec: {scaleTargetRef: {kind: Deployment, name: web}, minReplicas: 2, maxReplicas: 5}
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: {name: worker, namespace: shop}
spec: {scaleTargetRef: {name: worker}, minReplicaCount: 0}
---
apiVersion: flagger.app/v1beta1
kind: Canary
metadata: {name: web, namespace: shop}
spec: {targetRef: {kind: Deployment, name: web}, analysis: {interval: 1m}}
---
apiVersion: v1
kind: Secret
metadata: {name: enc, namespace: shop}
data: {k: djE=}
sops: {version: 3.9.0}
`
	s := &Set{}
	if err := s.Decode("test", []byte(doc), DefaultNamespace); err != nil {
		t.Fatal(err)
	}
	if len(s.Scalers) != 2 || s.Scalers[0].MinReplicas != 2 || s.Scalers[1].MinReplicas != 1 || s.Scalers[1].Target.Kind != "Deployment" {
		t.Errorf("scalers %+v", s.Scalers)
	}
	if len(s.Canaries) != 1 || s.Canaries[0].Interval.String() != "1m0s" || s.Canaries[0].Target.Name != "web" {
		t.Errorf("canaries %+v", s.Canaries)
	}
	if len(s.Secrets) != 0 {
		t.Errorf("SOPS secret was decoded")
	}
	want := []IgnoredObject{
		{"autoscaling/v2 HorizontalPodAutoscaler shop/web", IgnoredAutoscaler},
		{"keda.sh/v1alpha1 ScaledObject shop/worker", IgnoredAutoscaler},
		{"flagger.app/v1beta1 Canary shop/web", IgnoredCanary},
		{"v1 Secret shop/enc", IgnoredSOPS},
	}
	if len(s.Ignored) != len(want) {
		t.Fatalf("ignored %v", s.Ignored)
	}
	for i := range want {
		if s.Ignored[i] != want[i] {
			t.Errorf("ignored[%d] = %v, want %v", i, s.Ignored[i], want[i])
		}
	}
}
