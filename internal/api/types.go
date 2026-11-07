// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package api defines the Flux and External Secrets Operator types
// flux4swarm reads, the annotations it understands on Kubernetes objects and
// the labels it writes on Swarm objects.
package api

import (
	"encoding/json"
	"fmt"
	"time"
)

// API groups and kinds.
const (
	SourceGroupVersion    = "source.toolkit.fluxcd.io/v1"
	KustomizeGroupVersion = "kustomize.toolkit.fluxcd.io/v1"
	ESOGroupVersion       = "external-secrets.io/v1"
	ESOGroupVersionBeta   = "external-secrets.io/v1beta1"

	KindGitRepository      = "GitRepository"
	KindKustomization      = "Kustomization"
	KindExternalSecret     = "ExternalSecret"
	KindSecretStore        = "SecretStore"
	KindClusterSecretStore = "ClusterSecretStore"
)

// Labels written on Swarm objects. The kustomize ones are the labels Flux's
// kustomize-controller uses for ownership.
const (
	LabelKustomizeName      = "kustomize.toolkit.fluxcd.io/name"
	LabelKustomizeNamespace = "kustomize.toolkit.fluxcd.io/namespace"
	LabelManagedBy          = "app.kubernetes.io/managed-by"
	ManagedByValue          = "flux4swarm"
	LabelName               = "flux4swarm.io/name"
	LabelSpecHash           = "flux4swarm.io/spec-hash"
	LabelExternalSecret     = "flux4swarm.io/external-secret"
	LabelStackNamespace     = "com.docker.stack.namespace"
)

// Annotations understood on Kubernetes objects. Stock Flux ignores them.
const (
	// AnnotationName sets the exact Swarm service name of a workload.
	AnnotationName = "swarm.flux4swarm.io/name"
	// AnnotationNetworks is a comma separated list of networks that
	// replaces the namespace default network.
	AnnotationNetworks = "swarm.flux4swarm.io/networks"
	// AnnotationServiceSpec is a JSON merge patch (RFC 7386) applied to the
	// generated Docker ServiceSpec.
	AnnotationServiceSpec = "swarm.flux4swarm.io/service-spec"
	// AnnotationIgnore skips an object on Swarm when "true".
	AnnotationIgnore = "swarm.flux4swarm.io/ignore"
	// AnnotationVolumeDriver and AnnotationVolumeOptions configure the
	// Docker volume behind a PersistentVolumeClaim.
	AnnotationVolumeDriver  = "swarm.flux4swarm.io/volume-driver"
	AnnotationVolumeOptions = "swarm.flux4swarm.io/volume-options"
	// AnnotationTraefikCertResolver on an HTTPRoute sets the router's
	// tls.certresolver.
	AnnotationTraefikCertResolver = "swarm.flux4swarm.io/traefik-certresolver"
	// AnnotationTraefikPriority on an HTTPRoute sets the router priority.
	AnnotationTraefikPriority = "swarm.flux4swarm.io/traefik-priority"
	// AnnotationReconcile is Flux's own annotation; "disabled" skips the object.
	AnnotationReconcile = "kustomize.toolkit.fluxcd.io/reconcile"
)

// DefaultNamespace is used when a manifest has no metadata.namespace.
const DefaultNamespace = "flux-system"

// TypeMeta is apiVersion and kind.
type TypeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// ObjectMeta is the subset of Kubernetes metadata flux4swarm uses.
type ObjectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Duration is a time.Duration that (un)marshals as a Go duration string
// ("1m", "30s"), like metav1.Duration.
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.Duration.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"1m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// LocalObjectReference refers to an object by name.
type LocalObjectReference struct {
	Name string `json:"name"`
}

// CrossNamespaceObjectReference refers to another object.
type CrossNamespaceObjectReference struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
}

// ---- source.toolkit.fluxcd.io/v1 ----

// GitRepository is a Flux Git source.
type GitRepository struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta        `json:"metadata"`
	Spec     GitRepositorySpec `json:"spec"`
}

// GitRepositorySpec mirrors Flux's GitRepositorySpec.
type GitRepositorySpec struct {
	URL       string                `json:"url"`
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`
	// Provider is generic (default), github or azure.
	Provider string            `json:"provider,omitempty"`
	Interval Duration          `json:"interval"`
	Timeout  *Duration         `json:"timeout,omitempty"`
	Ref      *GitRepositoryRef `json:"ref,omitempty"`
	Suspend  bool              `json:"suspend,omitempty"`
}

// GitRepositoryRef selects what to check out.
type GitRepositoryRef struct {
	Branch string `json:"branch,omitempty"`
	Tag    string `json:"tag,omitempty"`
	SemVer string `json:"semver,omitempty"`
	Name   string `json:"name,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// ---- kustomize.toolkit.fluxcd.io/v1 ----

// Kustomization is a Flux Kustomization.
type Kustomization struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta        `json:"metadata"`
	Spec     KustomizationSpec `json:"spec"`
}

// KustomizationSpec mirrors the subset of Flux's KustomizationSpec that
// applies to Swarm.
type KustomizationSpec struct {
	DependsOn     []DependencyReference         `json:"dependsOn,omitempty"`
	Interval      Duration                      `json:"interval"`
	RetryInterval *Duration                     `json:"retryInterval,omitempty"`
	Path          string                        `json:"path,omitempty"`
	Prune         bool                          `json:"prune"`
	SourceRef     CrossNamespaceObjectReference `json:"sourceRef"`
	Suspend       bool                          `json:"suspend,omitempty"`
	Timeout       *Duration                     `json:"timeout,omitempty"`
	Wait          bool                          `json:"wait,omitempty"`
	// HealthChecks lists workloads (Deployment, StatefulSet, DaemonSet,
	// Job), Kustomizations and GitRepositories that must be healthy for
	// this Kustomization to become Ready. They may belong to other
	// Kustomizations. Ignored when Wait is set, like in Flux.
	HealthChecks []CrossNamespaceObjectReference `json:"healthChecks,omitempty"`
	Force        bool                            `json:"force,omitempty"`

	TargetNamespace string            `json:"targetNamespace,omitempty"`
	NamePrefix      string            `json:"namePrefix,omitempty"`
	NameSuffix      string            `json:"nameSuffix,omitempty"`
	CommonMetadata  *CommonMetadata   `json:"commonMetadata,omitempty"`
	Patches         []json.RawMessage `json:"patches,omitempty"`
	Images          []json.RawMessage `json:"images,omitempty"`
	Components      []string          `json:"components,omitempty"`
	PostBuild       *PostBuild        `json:"postBuild,omitempty"`
	Decryption      *json.RawMessage  `json:"decryption,omitempty"`
}

// CommonMetadata is added to every object of the build.
type CommonMetadata struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PostBuild holds variable substitution settings.
type PostBuild struct {
	Substitute     map[string]string     `json:"substitute,omitempty"`
	SubstituteFrom []SubstituteReference `json:"substituteFrom,omitempty"`
}

// SubstituteReference points at a ConfigMap or Secret with variables.
type SubstituteReference struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Optional bool   `json:"optional,omitempty"`
}

// DependencyReference names another Kustomization.
type DependencyReference struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// ---- external-secrets.io/v1 ----

// ExternalSecret describes a secret whose value lives in an external store.
type ExternalSecret struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta         `json:"metadata"`
	Spec     ExternalSecretSpec `json:"spec"`
}

// ExternalSecretSpec is the ESO spec subset.
type ExternalSecretSpec struct {
	SecretStoreRef  SecretStoreRef       `json:"secretStoreRef"`
	Target          ExternalSecretTarget `json:"target,omitempty"`
	RefreshInterval *Duration            `json:"refreshInterval,omitempty"`
	Data            []ExternalSecretData `json:"data,omitempty"`
	DataFrom        []json.RawMessage    `json:"dataFrom,omitempty"`
}

// SecretStoreRef points at a SecretStore or ClusterSecretStore.
type SecretStoreRef struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

// ExternalSecretTarget names the resulting secret.
type ExternalSecretTarget struct {
	Name     string                  `json:"name,omitempty"`
	Template *ExternalSecretTemplate `json:"template,omitempty"`
}

// ExternalSecretTemplate carries metadata for the created secret.
type ExternalSecretTemplate struct {
	Metadata struct {
		Labels map[string]string `json:"labels,omitempty"`
	} `json:"metadata,omitempty"`
}

// ExternalSecretData maps one remote value to one key.
type ExternalSecretData struct {
	SecretKey string          `json:"secretKey"`
	RemoteRef RemoteReference `json:"remoteRef"`
}

// RemoteReference locates a value in the store.
type RemoteReference struct {
	Key      string `json:"key"`
	Property string `json:"property,omitempty"`
	Version  string `json:"version,omitempty"`
}

// SecretStore (or ClusterSecretStore) configures access to a secret backend.
type SecretStore struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta      `json:"metadata"`
	Spec     SecretStoreSpec `json:"spec"`
}

// SecretStoreSpec holds exactly one provider.
type SecretStoreSpec struct {
	Provider SecretStoreProvider `json:"provider"`
}

// SecretStoreProvider lists the supported providers.
type SecretStoreProvider struct {
	Vault   *VaultProvider   `json:"vault,omitempty"`
	AzureKV *AzureKVProvider `json:"azurekv,omitempty"`
	Fake    *FakeProvider    `json:"fake,omitempty"`
}

// SecretKeySelector references a key of a credentials secret. In Kubernetes
// that is a Secret; in flux4swarm it is a file in the credentials directory.
type SecretKeySelector struct {
	Name      string `json:"name"`
	Key       string `json:"key"`
	Namespace string `json:"namespace,omitempty"`
}

// VaultProvider is the ESO HashiCorp Vault provider subset.
type VaultProvider struct {
	Server    string    `json:"server"`
	Path      string    `json:"path,omitempty"`
	Version   string    `json:"version,omitempty"`
	Namespace string    `json:"namespace,omitempty"`
	CABundle  []byte    `json:"caBundle,omitempty"`
	Auth      VaultAuth `json:"auth"`
}

// VaultAuth supports token and AppRole auth.
type VaultAuth struct {
	TokenSecretRef *SecretKeySelector `json:"tokenSecretRef,omitempty"`
	AppRole        *VaultAppRole      `json:"appRole,omitempty"`
}

// VaultAppRole is AppRole auth.
type VaultAppRole struct {
	Path      string             `json:"path,omitempty"`
	RoleID    string             `json:"roleId,omitempty"`
	RoleRef   *SecretKeySelector `json:"roleRef,omitempty"`
	SecretRef SecretKeySelector  `json:"secretRef"`
}

// AzureKVProvider is the ESO Azure Key Vault provider subset.
type AzureKVProvider struct {
	VaultURL        string       `json:"vaultUrl"`
	TenantID        string       `json:"tenantId,omitempty"`
	AuthType        string       `json:"authType,omitempty"`
	EnvironmentType string       `json:"environmentType,omitempty"`
	AuthSecretRef   *AzureKVAuth `json:"authSecretRef,omitempty"`
}

// AzureKVAuth references service principal credentials.
type AzureKVAuth struct {
	ClientID     *SecretKeySelector `json:"clientId,omitempty"`
	ClientSecret *SecretKeySelector `json:"clientSecret,omitempty"`
}

// FakeProvider returns static values; useful for tests and demos.
type FakeProvider struct {
	Data []FakeProviderData `json:"data"`
}

// FakeProviderData is one static value.
type FakeProviderData struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Version string `json:"version,omitempty"`
}
