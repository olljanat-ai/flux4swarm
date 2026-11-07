// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Progressive delivery and autoscaling kinds. They have no Swarm
// equivalent, so they are listed as skipped, but they change how their
// target workload is translated.
const (
	KindRollout       = "Rollout"
	KindCanary        = "Canary"
	KindHPA           = "HorizontalPodAutoscaler"
	KindScaledObject  = "ScaledObject"
	IgnoredAutoscaler = "autoscaling has no Swarm equivalent; the target runs minReplicas"
	IgnoredCanary     = "progressive delivery has no Swarm equivalent; the target rolls back on a failed update"
	IgnoredSOPS       = "SOPS encrypted; SOPS is not supported on Swarm, use an ExternalSecret"
)

// Rollout is an Argo Rollouts argoproj.io/v1alpha1 Rollout. On Swarm it
// runs like a Deployment with its pod template.
type Rollout struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta  `json:"metadata"`
	Spec     RolloutSpec `json:"spec"`
}

// RolloutSpec is the subset of a Rollout spec flux4swarm uses.
type RolloutSpec struct {
	Replicas    *int32                 `json:"replicas,omitempty"`
	Template    corev1.PodTemplateSpec `json:"template"`
	WorkloadRef *WorkloadRef           `json:"workloadRef,omitempty"`
	Strategy    struct {
		Canary    *json.RawMessage `json:"canary,omitempty"`
		BlueGreen *json.RawMessage `json:"blueGreen,omitempty"`
	} `json:"strategy"`
}

// WorkloadRef points a Rollout or an autoscaler at a workload.
type WorkloadRef struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// Strategy is "canary", "blueGreen" or "".
func (r *Rollout) Strategy() string {
	switch {
	case r.Spec.Strategy.BlueGreen != nil:
		return "blueGreen"
	case r.Spec.Strategy.Canary != nil:
		return "canary"
	}
	return ""
}

// Scaler is an object that changes the replicas of a workload on
// Kubernetes: a HorizontalPodAutoscaler or a KEDA ScaledObject.
type Scaler struct {
	// Object is "apiVersion Kind namespace/name".
	Object      string
	Namespace   string
	Target      WorkloadRef
	MinReplicas int32
}

// Canary is a Flagger Canary: the target is updated with automatic
// rollback on Swarm.
type Canary struct {
	Object    string
	Namespace string
	Target    WorkloadRef
	// Interval is spec.analysis.interval (0 when unset).
	Interval time.Duration
}

type scalerDoc struct {
	Spec struct {
		// HPA.
		ScaleTargetRef WorkloadRef `json:"scaleTargetRef"`
		MinReplicas    *int32      `json:"minReplicas,omitempty"`
		// KEDA.
		MinReplicaCount *int32 `json:"minReplicaCount,omitempty"`
	} `json:"spec"`
}

func decodeScaler(id, ns, kind string, dec func(any) error) (Scaler, error) {
	var d scalerDoc
	if err := dec(&d); err != nil {
		return Scaler{}, err
	}
	s := Scaler{Object: id, Namespace: ns, Target: d.Spec.ScaleTargetRef, MinReplicas: 1}
	if s.Target.Kind == "" {
		// KEDA defaults the target kind to Deployment.
		s.Target.Kind = "Deployment"
	}
	min := d.Spec.MinReplicas
	if kind == KindScaledObject {
		min = d.Spec.MinReplicaCount
	}
	// Swarm cannot scale from zero on demand, so at least one replica runs.
	if min != nil && *min > 1 {
		s.MinReplicas = *min
	}
	return s, nil
}

type canaryDoc struct {
	Spec struct {
		TargetRef WorkloadRef `json:"targetRef"`
		Analysis  *struct {
			Interval string `json:"interval,omitempty"`
		} `json:"analysis,omitempty"`
	} `json:"spec"`
}

func decodeCanary(id, ns string, dec func(any) error) (Canary, error) {
	var d canaryDoc
	if err := dec(&d); err != nil {
		return Canary{}, err
	}
	c := Canary{Object: id, Namespace: ns, Target: d.Spec.TargetRef}
	if a := d.Spec.Analysis; a != nil && a.Interval != "" {
		v, err := time.ParseDuration(a.Interval)
		if err != nil {
			return Canary{}, fmt.Errorf("spec.analysis.interval: %w", err)
		}
		c.Interval = v
	}
	return c, nil
}

// TargetKey is "Kind/namespace/name" of a workload.
func TargetKey(kind, ns, name string) string {
	return strings.Join([]string{kind, ns, name}, "/")
}
