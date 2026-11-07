// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"
)

// TraefikMiddleware is a traefik.io/v1alpha1 Middleware. The spec is kept
// generic because it is flattened into Docker labels.
type TraefikMiddleware struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta     `json:"metadata"`
	Spec     map[string]any `json:"spec"`
}

// Set is the decoded result of a Kustomization build.
type Set struct {
	// Flux objects.
	GitRepositories []*GitRepository
	Kustomizations  []*Kustomization

	// Flux notification objects.
	Providers []*Provider
	Alerts    []*Alert
	Receivers []*Receiver

	// Kubernetes workloads and their dependencies.
	Deployments  []*appsv1.Deployment
	StatefulSets []*appsv1.StatefulSet
	DaemonSets   []*appsv1.DaemonSet
	Jobs         []*batchv1.Job
	Services     []*corev1.Service
	ConfigMaps   []*corev1.ConfigMap
	Secrets      []*corev1.Secret
	PVCs         []*corev1.PersistentVolumeClaim

	// External Secrets Operator.
	ExternalSecrets []*ExternalSecret
	SecretStores    []*SecretStore

	// Gateway API and Traefik.
	Gateways    []*gatewayv1.Gateway
	HTTPRoutes  []*gatewayv1.HTTPRoute
	Middlewares []*TraefikMiddleware

	// Ignored lists "apiVersion Kind namespace/name" of objects that have
	// no meaning on Swarm.
	Ignored []string
}

// SplitYAML splits a multi-document YAML stream.
func SplitYAML(data []byte) [][]byte {
	var docs [][]byte
	var cur bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	flush := func() {
		if len(bytes.TrimSpace(cur.Bytes())) > 0 {
			docs = append(docs, append([]byte(nil), cur.Bytes()...))
		}
		cur.Reset()
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimRight(line, " \t") == "---" || strings.HasPrefix(line, "--- ") {
			flush()
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	flush()
	return docs
}

type header struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
}

// Decode parses one or more YAML documents into s. source is used in errors.
// Objects without a namespace get defaultNamespace.
func (s *Set) Decode(source string, data []byte, defaultNamespace string) error {
	for i, doc := range SplitYAML(data) {
		if err := s.decodeOne(doc, defaultNamespace); err != nil {
			return fmt.Errorf("%s: document %d: %w", source, i+1, err)
		}
	}
	return nil
}

func (s *Set) decodeOne(doc []byte, defNS string) error {
	var h header
	if err := yaml.Unmarshal(doc, &h); err != nil {
		return err
	}
	if h.APIVersion == "" && h.Kind == "" {
		var anything any
		if err := yaml.Unmarshal(doc, &anything); err == nil && anything == nil {
			return nil
		}
		return fmt.Errorf("missing apiVersion and kind")
	}
	id := fmt.Sprintf("%s %s %s/%s", h.APIVersion, h.Kind, h.Metadata.Namespace, h.Metadata.Name)
	// Flux components (gotk-components.yaml) and explicitly ignored objects.
	if h.Metadata.Labels["app.kubernetes.io/part-of"] == "flux" ||
		h.Metadata.Annotations[AnnotationIgnore] == "true" ||
		h.Metadata.Annotations[AnnotationReconcile] == "disabled" {
		s.Ignored = append(s.Ignored, id)
		return nil
	}
	group, version := h.APIVersion, ""
	if i := strings.Index(group, "/"); i >= 0 {
		group, version = group[:i], group[i+1:]
	} else {
		group, version = "", group
	}
	ns := h.Metadata.Namespace
	if ns == "" {
		ns = defNS
	}
	dec := func(v any) error { return yaml.Unmarshal(doc, v) }
	switch {
	case group == "source.toolkit.fluxcd.io" && h.Kind == KindGitRepository:
		o := &GitRepository{}
		if err := dec(o); err != nil {
			return err
		}
		o.Metadata.Namespace = ns
		s.GitRepositories = append(s.GitRepositories, o)
	case group == "kustomize.toolkit.fluxcd.io" && h.Kind == KindKustomization:
		o := &Kustomization{}
		if err := dec(o); err != nil {
			return err
		}
		o.Metadata.Namespace = ns
		s.Kustomizations = append(s.Kustomizations, o)
	case group == NotificationGroup && h.Kind == KindProvider:
		o := &Provider{}
		if err := dec(o); err != nil {
			return err
		}
		o.Metadata.Namespace = ns
		s.Providers = append(s.Providers, o)
	case group == NotificationGroup && h.Kind == KindAlert:
		o := &Alert{}
		if err := dec(o); err != nil {
			return err
		}
		o.Metadata.Namespace = ns
		s.Alerts = append(s.Alerts, o)
	case group == NotificationGroup && h.Kind == KindReceiver:
		o := &Receiver{}
		if err := dec(o); err != nil {
			return err
		}
		o.Metadata.Namespace = ns
		s.Receivers = append(s.Receivers, o)
	case group == "apps" && version == "v1" && h.Kind == "Deployment":
		o := &appsv1.Deployment{}
		if err := dec(o); err != nil {
			return err
		}
		o.Namespace = ns
		s.Deployments = append(s.Deployments, o)
	case group == "apps" && version == "v1" && h.Kind == "StatefulSet":
		o := &appsv1.StatefulSet{}
		if err := dec(o); err != nil {
			return err
		}
		o.Namespace = ns
		s.StatefulSets = append(s.StatefulSets, o)
	case group == "apps" && version == "v1" && h.Kind == "DaemonSet":
		o := &appsv1.DaemonSet{}
		if err := dec(o); err != nil {
			return err
		}
		o.Namespace = ns
		s.DaemonSets = append(s.DaemonSets, o)
	case group == "batch" && version == "v1" && h.Kind == "Job":
		o := &batchv1.Job{}
		if err := dec(o); err != nil {
			return err
		}
		o.Namespace = ns
		s.Jobs = append(s.Jobs, o)
	case group == "" && h.Kind == "Service":
		o := &corev1.Service{}
		if err := dec(o); err != nil {
			return err
		}
		o.Namespace = ns
		s.Services = append(s.Services, o)
	case group == "" && h.Kind == "ConfigMap":
		o := &corev1.ConfigMap{}
		if err := dec(o); err != nil {
			return err
		}
		o.Namespace = ns
		s.ConfigMaps = append(s.ConfigMaps, o)
	case group == "" && h.Kind == "Secret":
		o := &corev1.Secret{}
		if err := dec(o); err != nil {
			return err
		}
		if _, sops := o.Data["sops"]; sops || strings.Contains(string(doc), "\nsops:") {
			return fmt.Errorf("secret %s/%s is SOPS encrypted; SOPS is not supported on Swarm, use an ExternalSecret", ns, o.Name)
		}
		o.Namespace = ns
		for k, v := range o.StringData {
			if o.Data == nil {
				o.Data = map[string][]byte{}
			}
			o.Data[k] = []byte(v)
		}
		s.Secrets = append(s.Secrets, o)
	case group == "" && h.Kind == "PersistentVolumeClaim":
		o := &corev1.PersistentVolumeClaim{}
		if err := dec(o); err != nil {
			return err
		}
		o.Namespace = ns
		s.PVCs = append(s.PVCs, o)
	case group == "external-secrets.io" && h.Kind == KindExternalSecret:
		o := &ExternalSecret{}
		if err := dec(o); err != nil {
			return err
		}
		o.Metadata.Namespace = ns
		s.ExternalSecrets = append(s.ExternalSecrets, o)
	case group == "external-secrets.io" && (h.Kind == KindSecretStore || h.Kind == KindClusterSecretStore):
		o := &SecretStore{}
		if err := dec(o); err != nil {
			return err
		}
		if h.Kind == KindSecretStore {
			o.Metadata.Namespace = ns
		}
		s.SecretStores = append(s.SecretStores, o)
	case group == "gateway.networking.k8s.io" && h.Kind == "HTTPRoute":
		o := &gatewayv1.HTTPRoute{}
		if err := dec(o); err != nil {
			return err
		}
		o.Namespace = ns
		s.HTTPRoutes = append(s.HTTPRoutes, o)
	case group == "gateway.networking.k8s.io" && h.Kind == "Gateway":
		o := &gatewayv1.Gateway{}
		if err := dec(o); err != nil {
			return err
		}
		o.Namespace = ns
		s.Gateways = append(s.Gateways, o)
	case (group == "traefik.io" || group == "traefik.containo.us") && h.Kind == "Middleware":
		o := &TraefikMiddleware{}
		if err := dec(o); err != nil {
			return err
		}
		o.Metadata.Namespace = ns
		s.Middlewares = append(s.Middlewares, o)
	default:
		s.Ignored = append(s.Ignored, id)
	}
	return nil
}

// NamespacedName returns "namespace/name".
func (m ObjectMeta) NamespacedName() string { return m.Namespace + "/" + m.Name }

// MarshalDocs renders objects as a multi-document YAML stream.
func MarshalDocs(objs ...any) ([]byte, error) {
	var buf bytes.Buffer
	for i, o := range objs {
		b, err := yaml.Marshal(o)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			buf.WriteString("---\n")
		}
		buf.Write(b)
	}
	return buf.Bytes(), nil
}
