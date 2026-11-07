// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package api

// notification.toolkit.fluxcd.io kinds. Provider and Alert are read in
// v1, v1beta3 and v1beta2 (same fields), Receiver in v1 and v1beta2.
const (
	NotificationGroup        = "notification.toolkit.fluxcd.io"
	NotificationGroupVersion = NotificationGroup + "/v1"

	KindProvider = "Provider"
	KindAlert    = "Alert"
	KindReceiver = "Receiver"
)

// Provider is a Flux notification Provider.
type Provider struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta   `json:"metadata"`
	Spec     ProviderSpec `json:"spec"`
}

// ProviderSpec mirrors Flux's ProviderSpec.
type ProviderSpec struct {
	Type             string                `json:"type"`
	Channel          string                `json:"channel,omitempty"`
	Username         string                `json:"username,omitempty"`
	Address          string                `json:"address,omitempty"`
	Timeout          *Duration             `json:"timeout,omitempty"`
	Proxy            string                `json:"proxy,omitempty"`
	ProxySecretRef   *LocalObjectReference `json:"proxySecretRef,omitempty"`
	SecretRef        *LocalObjectReference `json:"secretRef,omitempty"`
	CertSecretRef    *LocalObjectReference `json:"certSecretRef,omitempty"`
	Suspend          bool                  `json:"suspend,omitempty"`
	CommitStatusExpr string                `json:"commitStatusExpr,omitempty"`
	// Interval is deprecated in Flux and ignored.
	Interval *Duration `json:"interval,omitempty"`
}

// Alert is a Flux notification Alert.
type Alert struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta `json:"metadata"`
	Spec     AlertSpec  `json:"spec"`
}

// AlertSpec mirrors Flux's AlertSpec.
type AlertSpec struct {
	ProviderRef   LocalObjectReference    `json:"providerRef"`
	EventSeverity string                  `json:"eventSeverity,omitempty"`
	EventSources  []NotificationObjectRef `json:"eventSources"`
	InclusionList []string                `json:"inclusionList,omitempty"`
	ExclusionList []string                `json:"exclusionList,omitempty"`
	EventMetadata map[string]string       `json:"eventMetadata,omitempty"`
	Summary       string                  `json:"summary,omitempty"`
	Suspend       bool                    `json:"suspend,omitempty"`
}

// NotificationObjectRef is Flux's notification CrossNamespaceObjectReference
// (with matchLabels).
type NotificationObjectRef struct {
	APIVersion  string            `json:"apiVersion,omitempty"`
	Kind        string            `json:"kind"`
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
}

// Receiver is a Flux notification Receiver.
type Receiver struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta   `json:"metadata"`
	Spec     ReceiverSpec `json:"spec"`
}

// ReceiverSpec mirrors Flux's ReceiverSpec.
type ReceiverSpec struct {
	Type           string                `json:"type"`
	Interval       *Duration             `json:"interval,omitempty"`
	Events         []string              `json:"events,omitempty"`
	Resources      []ReceiverResource    `json:"resources"`
	ResourceFilter string                `json:"resourceFilter,omitempty"`
	SecretRef      *LocalObjectReference `json:"secretRef,omitempty"`
	Suspend        bool                  `json:"suspend,omitempty"`
}

// ReceiverResource is a resource a Receiver asks to reconcile.
type ReceiverResource struct {
	NotificationObjectRef `json:",inline"`
	Filter                string `json:"filter,omitempty"`
}
