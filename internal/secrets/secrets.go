// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package secrets resolves External Secrets Operator ExternalSecrets into
// Docker secrets by reading the referenced SecretStore providers.
package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/creds"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Provider reads one value from a store.
type Provider interface {
	Get(ctx context.Context, ref api.RemoteReference) ([]byte, error)
}

// NewProvider builds the provider configured in store.
func NewProvider(store *api.SecretStore, c *creds.Store) (Provider, error) {
	p := store.Spec.Provider
	n := 0
	for _, set := range []bool{p.Vault != nil, p.AzureKV != nil, p.Fake != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return nil, fmt.Errorf("%s %s: exactly one supported provider (vault, azurekv, fake) must be set", store.Kind, store.Metadata.Name)
	}
	switch {
	case p.Vault != nil:
		return newVault(p.Vault, c)
	case p.AzureKV != nil:
		return newAzureKV(p.AzureKV, c)
	default:
		return &fake{data: p.Fake.Data}, nil
	}
}

// Resolver turns ExternalSecrets into the Kubernetes Secrets ESO would create.
type Resolver struct {
	Creds *creds.Store
	// ClusterStores are ClusterSecretStores known to the controller; they
	// are visible to every Kustomization.
	ClusterStores map[string]*api.SecretStore
}

// Resolve fetches the values for every ExternalSecret and returns one
// Secret per ExternalSecret, named after spec.target.name. stores are the
// SecretStores (and ClusterSecretStores) from the same build.
func (r *Resolver) Resolve(ctx context.Context, externals []*api.ExternalSecret, stores []*api.SecretStore) ([]*corev1.Secret, error) {
	local := map[string]*api.SecretStore{}
	cluster := map[string]*api.SecretStore{}
	for k, v := range r.ClusterStores {
		cluster[k] = v
	}
	for _, s := range stores {
		if s.Kind == api.KindClusterSecretStore {
			cluster[s.Metadata.Name] = s
		} else {
			local[s.Metadata.Namespace+"/"+s.Metadata.Name] = s
		}
	}
	providers := map[*api.SecretStore]Provider{}
	var out []*corev1.Secret
	for _, es := range externals {
		if len(es.Spec.DataFrom) > 0 {
			return nil, fmt.Errorf("ExternalSecret %s: dataFrom is not supported, use data", es.Metadata.NamespacedName())
		}
		if len(es.Spec.Data) == 0 {
			return nil, fmt.Errorf("ExternalSecret %s: spec.data is empty", es.Metadata.NamespacedName())
		}
		var store *api.SecretStore
		ref := es.Spec.SecretStoreRef
		if ref.Kind == api.KindClusterSecretStore {
			store = cluster[ref.Name]
		} else {
			store = local[es.Metadata.Namespace+"/"+ref.Name]
		}
		if store == nil {
			kind := ref.Kind
			if kind == "" {
				kind = api.KindSecretStore
			}
			return nil, fmt.Errorf("ExternalSecret %s: %s %q not found", es.Metadata.NamespacedName(), kind, ref.Name)
		}
		p, ok := providers[store]
		if !ok {
			var err error
			if p, err = NewProvider(store, r.Creds); err != nil {
				return nil, err
			}
			providers[store] = p
		}
		target := es.Spec.Target.Name
		if target == "" {
			target = es.Metadata.Name
		}
		var labels map[string]string
		if es.Spec.Target.Template != nil {
			labels = es.Spec.Target.Template.Metadata.Labels
		}
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: target, Namespace: es.Metadata.Namespace, Labels: labels},
			Data:       map[string][]byte{},
		}
		for _, d := range es.Spec.Data {
			v, err := p.Get(ctx, d.RemoteRef)
			if err != nil {
				return nil, fmt.Errorf("ExternalSecret %s: key %q: %w", es.Metadata.NamespacedName(), d.RemoteRef.Key, err)
			}
			sec.Data[d.SecretKey] = v
		}
		out = append(out, sec)
	}
	return out, nil
}

// property extracts a property from a JSON document, following ESO: a
// dotted path into nested objects. Strings are returned without quotes.
func property(doc []byte, prop string) ([]byte, error) {
	if prop == "" {
		return doc, nil
	}
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return nil, fmt.Errorf("value is not JSON, cannot read property %q", prop)
	}
	return pick(v, prop)
}

func pick(v any, prop string) ([]byte, error) {
	cur := v
	for _, part := range strings.Split(prop, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("property %q not found", prop)
		}
		if cur, ok = m[part]; !ok {
			// ESO also accepts keys that contain dots.
			if val, ok2 := m[prop]; ok2 {
				cur = val
				break
			}
			return nil, fmt.Errorf("property %q not found", prop)
		}
	}
	if s, ok := cur.(string); ok {
		return []byte(s), nil
	}
	return json.Marshal(cur)
}

type fake struct{ data []api.FakeProviderData }

func (f *fake) Get(_ context.Context, ref api.RemoteReference) ([]byte, error) {
	for _, d := range f.data {
		if d.Key == ref.Key && (ref.Version == "" || d.Version == ref.Version) {
			return property([]byte(d.Value), ref.Property)
		}
	}
	return nil, fmt.Errorf("not found in fake store")
}
