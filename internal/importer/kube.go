// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

// KubeResource is one listable kind of a Kubernetes API server.
type KubeResource struct {
	Group, Version, Resource, Kind string
	Namespaced                     bool
}

// APIVersion returns group/version, or version for the core group.
func (r KubeResource) APIVersion() string {
	if r.Group == "" {
		return r.Version
	}
	return r.Group + "/" + r.Version
}

// KubeClient is what ExportKubernetes reads from a cluster.
type KubeClient interface {
	// Resources returns the preferred version of every kind that can be
	// listed.
	Resources(ctx context.Context) ([]KubeResource, error)
	// List returns the objects of r in namespace ("" for all namespaces or
	// a cluster-scoped kind).
	List(ctx context.Context, r KubeResource, namespace string) ([]map[string]any, error)
}

// KubeOptions selects what ExportKubernetes reads.
type KubeOptions struct {
	// Namespaces limits the export to these namespaces; empty means every
	// namespace except the SystemNamespaces.
	Namespaces []string
	// Exclude skips objects whose "namespace/Kind/name" (cluster-scoped:
	// "Kind/name") matches.
	Exclude *regexp.Regexp
	// IncludeFluxManaged also exports objects Flux applied (labels
	// kustomize.toolkit.fluxcd.io/name or helm.toolkit.fluxcd.io/name);
	// by default they are skipped because they are in Git already.
	IncludeFluxManaged bool
	// ClusterScoped also exports the ClusterScopedKinds, to <path>/apps/_cluster.
	ClusterScoped bool
	// StorageClasses renames storageClassName of the exported claims
	// (old -> new), for a target cluster with other storage.
	StorageClasses map[string]string
	// SecretValues fills Result.SecretValues with the value of every
	// exported secret key, by remote key, for loading into the store.
	SecretValues bool
}

// SystemNamespaces are skipped unless named in KubeOptions.Namespaces; so
// is every namespace starting with "kube-".
var SystemNamespaces = []string{"flux-system", "volsync-system"}

// ClusterScopedKinds are the cluster-scoped kinds KubeOptions.ClusterScoped
// exports; everything else cluster-scoped belongs to the cluster itself
// (nodes, volumes, API registrations, ...).
var ClusterScopedKinds = []string{
	"ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition", "StorageClass",
	"IngressClass", "PriorityClass", "GatewayClass", "ClusterIssuer", "ClusterSecretStore",
}

// ClusterDir is the directory, below apps/, of the cluster-scoped objects.
const ClusterDir = "_cluster"

// Reasons for Skipped in a Kubernetes export.
const (
	SkippedSystemNamespace = "system namespace (name it with --namespace to export it)"
	SkippedOwned           = "created by its owner"
	SkippedFlux            = "applied by Flux, so already in Git (--include-flux-managed exports it)"
	SkippedRuntime         = "runtime object the cluster creates itself"
	SkippedHelmRelease     = "Helm release record"
	SkippedOwnObject       = "object of flux4swarm kube-backup"
	SkippedCertManager     = "issued by cert-manager from its Certificate"
)

// skipKinds are runtime kinds, by Kind or group, never exported.
var skipKinds = map[string]bool{
	"Event": true, "Endpoints": true, "EndpointSlice": true, "Lease": true, "ControllerRevision": true,
	"PodMetrics": true, "NodeMetrics": true, "ComponentStatus": true, "Binding": true,
}

var skipGroups = map[string]bool{
	"events.k8s.io": true, "metrics.k8s.io": true, "coordination.k8s.io": true, "discovery.k8s.io": true,
	"authentication.k8s.io": true, "authorization.k8s.io": true,
}

// Metadata, annotations and labels the cluster writes itself.
var (
	dropAnnotations = []string{
		"kubectl.kubernetes.io/last-applied-configuration", "deployment.kubernetes.io/revision",
		"kubectl.kubernetes.io/restartedAt", "meta.helm.sh/release-name", "meta.helm.sh/release-namespace",
		"volume.beta.kubernetes.io/storage-provisioner", "volume.kubernetes.io/storage-provisioner",
		"volume.kubernetes.io/selected-node", "control-plane.alpha.kubernetes.io/leader",
		"batch.kubernetes.io/job-tracking", "autoscaling.alpha.kubernetes.io/conditions",
		"autoscaling.alpha.kubernetes.io/current-metrics", "kubernetes.io/service-account.uid",
		api.AnnotationStoppedReplicas,
	}
	dropAnnotationPrefixes = []string{"pv.kubernetes.io/", "kustomize.toolkit.fluxcd.io/", "helm.toolkit.fluxcd.io/"}
	dropLabels             = []string{
		"pod-template-hash", "controller-uid", "job-name", "batch.kubernetes.io/controller-uid",
		"batch.kubernetes.io/job-name", "controller-revision-hash", "statefulset.kubernetes.io/pod-name",
		"kubernetes.io/metadata.name",
	}
	dropLabelPrefixes = []string{"kustomize.toolkit.fluxcd.io/", "helm.toolkit.fluxcd.io/"}
)

// kubeWorkloadKinds have a pod template at spec.template (CronJob: below
// spec.jobTemplate).
var kubeWorkloadKinds = map[string]bool{
	"Deployment": true, "StatefulSet": true, "DaemonSet": true, "ReplicaSet": true,
	"ReplicationController": true, "Job": true, "CronJob": true,
}

// ExportKubernetes reads a running Kubernetes cluster and renders the
// objects that were not applied by Flux, cleaned of everything the cluster
// fills in, in the layout of Export: one directory per namespace below
// <path>/apps. Secrets become ExternalSecrets (one store key per value),
// so no value reaches Git. With VolSync every claim gets a
// ReplicationDestination that restores what `flux4swarm kube-backup` wrote.
func ExportKubernetes(ctx context.Context, c KubeClient, o Options, ko KubeOptions) (*Result, error) {
	e, err := newExporter(o)
	if err != nil {
		return nil, err
	}
	e.kube = true
	if err := e.runKube(ctx, c, ko); err != nil {
		return nil, err
	}
	return e.res, nil
}

type helmRelease struct{ ns, name, chart string }

func (e *exporter) runKube(ctx context.Context, c KubeClient, ko KubeOptions) error {
	resources, err := c.Resources(ctx)
	if err != nil {
		return err
	}
	// Deterministic: by group then kind, the core group first, so file
	// name clashes always resolve the same way.
	sort.Slice(resources, func(i, j int) bool {
		if resources[i].Group != resources[j].Group {
			return resources[i].Group < resources[j].Group
		}
		return resources[i].Kind < resources[j].Kind
	})
	wanted := map[string]bool{}
	for _, n := range ko.Namespaces {
		wanted[n] = true
	}
	clusterKinds := map[string]bool{}
	for _, k := range ClusterScopedKinds {
		clusterKinds[k] = true
	}
	if ko.SecretValues {
		e.res.SecretValues = map[string]string{}
	}
	nsOK := func(ns string) bool {
		if len(wanted) > 0 {
			return wanted[ns]
		}
		if strings.HasPrefix(ns, "kube-") {
			return false
		}
		for _, s := range SystemNamespaces {
			if s == ns {
				return false
			}
		}
		return true
	}
	// Namespaces first: only those with objects are written.
	namespaceObjs := map[string]map[string]any{}
	for _, r := range resources {
		if r.Group == "" && r.Kind == "Namespace" {
			list, err := c.List(ctx, r, "")
			if err != nil {
				return fmt.Errorf("list namespaces: %w", err)
			}
			for _, n := range list {
				namespaceObjs[str(n, "metadata", "name")] = n
			}
		}
	}
	var (
		helm     []helmRelease
		files    = map[string]string{} // file -> apiVersion/kind/ns/name, for clashes
		skippedN = map[string]bool{}
	)
	for _, r := range resources {
		if skipKinds[r.Kind] || skipGroups[r.Group] || (r.Group == "" && r.Kind == "Namespace") {
			continue
		}
		if !r.Namespaced && (!ko.ClusterScoped || !clusterKinds[r.Kind]) {
			continue
		}
		var list []map[string]any
		if r.Namespaced && len(wanted) > 0 {
			for _, ns := range ko.Namespaces {
				l, err := c.List(ctx, r, ns)
				if err != nil {
					return fmt.Errorf("list %s in %s: %w", r.Resource, ns, err)
				}
				list = append(list, l...)
			}
		} else {
			if list, err = c.List(ctx, r, ""); err != nil {
				return fmt.Errorf("list %s: %w", r.Resource, err)
			}
		}
		for _, obj := range list {
			ns, name := str(obj, "metadata", "namespace"), str(obj, "metadata", "name")
			id := r.Kind + "/" + name
			if r.Namespaced {
				id = ns + "/" + id
			}
			if r.Namespaced && !nsOK(ns) {
				if !skippedN[ns] {
					skippedN[ns] = true
					e.skipped("Namespace", ns, SkippedSystemNamespace)
				}
				continue
			}
			if ko.Exclude != nil && ko.Exclude.MatchString(id) {
				e.skipped(r.Kind, nsName(ns, name), SkippedExcluded)
				continue
			}
			if reason := e.kubeSkip(r, obj, ko); reason != "" {
				if reason == SkippedHelmRelease {
					helm = append(helm, helmReleaseOf(obj))
				}
				// Runtime objects are not worth a line each.
				if reason != SkippedOwned && reason != SkippedRuntime {
					e.skipped(r.Kind, nsName(ns, name), reason)
				}
				continue
			}
			obj["apiVersion"], obj["kind"] = r.APIVersion(), r.Kind
			if r.Kind == "Secret" && r.Group == "" {
				e.kubeSecret(obj)
				continue
			}
			e.cleanKube(obj, ko)
			dir := e.o.Path + "/" + AppsDir + "/" + ClusterDir
			if r.Namespaced {
				dir = e.nsDir(ns)
				if n, ok := namespaceObjs[ns]; ok {
					e.namespaces[ns] = cleanNamespace(n)
				}
			}
			base := strings.ToLower(r.Kind) + "-" + name
			file := dir + "/" + base + ".yaml"
			if prev, ok := files[file]; ok && prev != r.APIVersion() {
				file = dir + "/" + strings.ToLower(r.Kind) + "." + strings.ReplaceAll(r.Group, ".", "-") + "-" + name + ".yaml"
			}
			files[file] = r.APIVersion()
			e.add(file, obj)
			if r.Group == "" && r.Kind == "PersistentVolumeClaim" && e.o.VolSync {
				e.volsync(ns, name)
			}
		}
	}
	for _, h := range helm {
		e.note("Helm release %s/%s (%s) is exported as the plain manifests Helm rendered; to keep managing it as a chart, replace them with a Flux HelmRelease.", h.ns, h.name, h.chart)
	}
	if len(e.pvcsKube) > 0 {
		e.note("PersistentVolumeClaims are exported without their bound volume, so the target cluster provisions new ones; check storageClassName (--storage-class old=new renames it).")
	}
	return e.write()
}

// kubeSkip returns why obj is not exported, or "".
func (e *exporter) kubeSkip(r KubeResource, obj map[string]any, ko KubeOptions) string {
	meta, _ := obj["metadata"].(map[string]any)
	if refs, _ := meta["ownerReferences"].([]any); len(refs) > 0 {
		return SkippedOwned
	}
	labels := strMap(meta["labels"])
	annotations := strMap(meta["annotations"])
	name, _ := meta["name"].(string)
	if labels[api.LabelManagedBy] == api.ManagedByValue {
		return SkippedOwnObject
	}
	if !ko.IncludeFluxManaged && (labels[api.LabelKustomizeName] != "" || labels["helm.toolkit.fluxcd.io/name"] != "") {
		return SkippedFlux
	}
	if r.Group == "" {
		switch r.Kind {
		case "Pod":
			if annotations["kubernetes.io/config.mirror"] != "" {
				return SkippedRuntime
			}
		case "Service":
			if name == "kubernetes" && meta["namespace"] == "default" {
				return SkippedRuntime
			}
		case "ServiceAccount":
			if name == "default" {
				return SkippedRuntime
			}
		case "ConfigMap":
			if name == "kube-root-ca.crt" || name == "openshift-service-ca.crt" {
				return SkippedRuntime
			}
		case "Secret":
			switch t, _ := obj["type"].(string); t {
			case "helm.sh/release.v1":
				return SkippedHelmRelease
			case "kubernetes.io/service-account-token", "bootstrap.kubernetes.io/token":
				return SkippedRuntime
			}
			if annotations["cert-manager.io/certificate-name"] != "" {
				return SkippedCertManager
			}
		}
	}
	if r.Kind == "ClusterRole" || r.Kind == "ClusterRoleBinding" {
		if strings.HasPrefix(name, "system:") || labels["kubernetes.io/bootstrapping"] != "" {
			return SkippedRuntime
		}
	}
	if r.Kind == "PriorityClass" && strings.HasPrefix(name, "system-") {
		return SkippedRuntime
	}
	return ""
}

// cleanKube removes what the cluster fills in, so the manifest applies to
// any cluster and diffs only show real changes.
func (e *exporter) cleanKube(obj map[string]any, ko KubeOptions) {
	delete(obj, "status")
	kind, _ := obj["kind"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	// Replicas recorded by kube-backup --final, which stopped the workload
	// for the last backup.
	stopped := strMap(meta["annotations"])[api.AnnotationStoppedReplicas]
	cleanMeta(meta)
	ns, _ := meta["namespace"].(string)
	name, _ := meta["name"].(string)
	spec, _ := obj["spec"].(map[string]any)
	switch kind {
	case "Service":
		if spec != nil {
			if ip, _ := spec["clusterIP"].(string); ip != "None" {
				delete(spec, "clusterIP")
				delete(spec, "clusterIPs")
			}
			delete(spec, "healthCheckNodePort")
			if spec["ipFamilyPolicy"] == "SingleStack" {
				delete(spec, "ipFamilyPolicy")
				delete(spec, "ipFamilies")
			}
			if t, _ := spec["type"].(string); t != "NodePort" {
				// Allocated by the cluster for load balancers.
				for _, p := range list(spec["ports"]) {
					delete(p, "nodePort")
				}
			}
		}
	case "PersistentVolumeClaim":
		if spec != nil {
			delete(spec, "volumeName")
			if sc, ok := spec["storageClassName"].(string); ok {
				if to, ok := ko.StorageClasses[sc]; ok {
					spec["storageClassName"] = to
				}
			}
		}
		if e.o.VolSync {
			ann := ensureMap(meta, "annotations")
			ann["kustomize.toolkit.fluxcd.io/prune"] = "disabled"
		}
		e.pvcsKube = append(e.pvcsKube, ns+"/"+name)
	case "ServiceAccount":
		delete(obj, "secrets")
	case "Job":
		if spec != nil {
			if sel, ok := spec["selector"].(map[string]any); ok {
				ml := strMap(sel["matchLabels"])
				if ml["batch.kubernetes.io/controller-uid"] != "" || ml["controller-uid"] != "" {
					delete(spec, "selector")
					delete(spec, "manualSelector")
				}
			}
			// The pod template is immutable: let Flux recreate the Job
			// when it changes.
			ensureMap(meta, "annotations")[api.AnnotationForce] = "Enabled"
		}
	}
	tpl := podTemplate(obj)
	if tpl != nil {
		if tm, ok := tpl["metadata"].(map[string]any); ok {
			cleanMeta(tm)
			delete(tm, "creationTimestamp")
			if len(tm) == 0 {
				delete(tpl, "metadata")
			}
		}
	}
	if !kubeWorkloadKinds[kind] || spec == nil {
		return
	}
	if stopped != "" {
		if n, err := strconv.Atoi(stopped); err == nil && num(spec["replicas"]) == 0 {
			spec["replicas"] = int64(n)
		}
	}
	if !e.holding() || tpl == nil || !mountsClaim(tpl) {
		return
	}
	// Held on the target until the data is there, as for Swarm.
	switch kind {
	case "Job":
		spec["suspend"] = true
		ensureMap(meta, "annotations")[api.AnnotationHeldJob] = "true"
	case "CronJob":
		spec["suspend"] = true
		ensureMap(meta, "annotations")[api.AnnotationHeldJob] = "true"
	case "DaemonSet":
		// Cannot be scaled: wait on a node selector no node has.
		ps, _ := tpl["spec"].(map[string]any)
		ensureMap(ps, "nodeSelector")["flux4swarm.io/held"] = "true"
		ensureMap(meta, "annotations")[api.AnnotationHeld] = "daemonset"
	default:
		n := int64(1)
		if v, ok := spec["replicas"]; ok {
			n = num(v)
		}
		if n > 0 {
			spec["replicas"] = int64(0)
			ensureMap(meta, "annotations")[api.AnnotationHeld] = strconv.FormatInt(n, 10)
		}
	}
}

func cleanMeta(meta map[string]any) {
	if meta == nil {
		return
	}
	for _, k := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "deletionTimestamp",
		"deletionGracePeriodSeconds", "managedFields", "selfLink", "ownerReferences", "finalizers", "generateName"} {
		delete(meta, k)
	}
	if anns, ok := meta["annotations"].(map[string]any); ok {
		for k := range anns {
			if contains(dropAnnotations, k) || hasAnyPrefix(k, dropAnnotationPrefixes) {
				delete(anns, k)
			}
		}
		if len(anns) == 0 {
			delete(meta, "annotations")
		}
	}
	if labels, ok := meta["labels"].(map[string]any); ok {
		for k := range labels {
			if contains(dropLabels, k) || hasAnyPrefix(k, dropLabelPrefixes) {
				delete(labels, k)
			}
		}
		if len(labels) == 0 {
			delete(meta, "labels")
		}
	}
}

func cleanNamespace(n map[string]any) map[string]any {
	b, _ := json.Marshal(n)
	var c map[string]any
	_ = json.Unmarshal(b, &c)
	delete(c, "status")
	delete(c, "spec") // only finalizers: kubernetes
	c["apiVersion"], c["kind"] = "v1", "Namespace"
	cleanMeta(c["metadata"].(map[string]any))
	return c
}

// kubeSecret writes Secret obj as an ExternalSecret with one store key per
// value: <prefix><namespace>-<name>-<key>.
func (e *exporter) kubeSecret(obj map[string]any) {
	meta := obj["metadata"].(map[string]any)
	cleanMeta(meta)
	ns, name := str(obj, "metadata", "namespace"), str(obj, "metadata", "name")
	data := map[string][]byte{}
	for k, v := range strMap(obj["data"]) {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			b = []byte(v)
		}
		data[k] = b
	}
	for k, v := range strMap(obj["stringData"]) {
		data[k] = []byte(v)
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	es := &api.ExternalSecret{
		TypeMeta: api.TypeMeta{APIVersion: api.ESOGroupVersion, Kind: api.KindExternalSecret},
		Metadata: api.ObjectMeta{Name: name, Namespace: ns},
		Spec: api.ExternalSecretSpec{
			RefreshInterval: &api.Duration{Duration: time.Hour},
			SecretStoreRef:  api.SecretStoreRef{Name: e.o.SecretStore, Kind: e.o.SecretStoreKind},
			Target:          api.ExternalSecretTarget{Name: name},
		},
	}
	var remote []string
	for _, k := range keys {
		rk := e.o.SecretKeyPrefix + ns + "-" + name + "-" + strings.Trim(dnsInvalid.ReplaceAllString(strings.ToLower(k), "-"), "-")
		if e.o.SecretStoreProvider == ProviderAzureKV {
			rk = azureName(rk)
		}
		ref := api.RemoteReference{Key: rk}
		v := data[k]
		value := string(v)
		if !utf8.Valid(v) {
			ref.DecodingStrategy = "Base64"
			value = base64.StdEncoding.EncodeToString(v)
		}
		if e.res.SecretValues != nil {
			e.res.SecretValues[rk] = value
		}
		remote = append(remote, rk)
		es.Spec.Data = append(es.Spec.Data, api.ExternalSecretData{SecretKey: k, RemoteRef: ref})
	}
	t, _ := obj["type"].(string)
	labels := strMap(meta["labels"])
	annotations := strMap(meta["annotations"])
	if (t != "" && t != "Opaque") || len(labels) > 0 || len(annotations) > 0 {
		es.Spec.Target.Template = &api.ExternalSecretTemplate{}
		if t != "Opaque" {
			es.Spec.Target.Template.Type = t
		}
		if len(labels) > 0 {
			es.Spec.Target.Template.Metadata.Labels = labels
		}
		if len(annotations) > 0 {
			es.Spec.Target.Template.Metadata.Annotations = annotations
		}
	}
	if len(es.Spec.Data) == 0 {
		// ESO needs at least one value; an empty Secret stays a Secret.
		empty := map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": meta}
		if t != "" {
			empty["type"] = t
		}
		e.add(e.nsDir(ns)+"/secret-"+name+".yaml", empty)
		return
	}
	e.extSecrets[ns+"/"+name] = es
	e.add(e.nsDir(ns)+"/externalsecret-"+name+".yaml", es)
}

// helmReleaseOf reads the release name and chart from a Helm release
// Secret (data.release: base64 of gzipped JSON, inside the Secret's own
// base64).
func helmReleaseOf(obj map[string]any) helmRelease {
	meta, _ := obj["metadata"].(map[string]any)
	h := helmRelease{ns: str(obj, "metadata", "namespace"), name: strMap(meta["labels"])["name"], chart: "chart unknown"}
	if h.name == "" {
		h.name = str(obj, "metadata", "name")
	}
	raw := strMap(obj["data"])["release"]
	b, err := base64.StdEncoding.DecodeString(raw)
	if err == nil {
		b, err = base64.StdEncoding.DecodeString(string(b))
	}
	if err == nil {
		if zr, zerr := gzip.NewReader(bytes.NewReader(b)); zerr == nil {
			b, err = io.ReadAll(zr)
		}
	}
	var rel struct {
		Chart struct {
			Metadata struct{ Name, Version string } `json:"metadata"`
		} `json:"chart"`
	}
	if err == nil && json.Unmarshal(b, &rel) == nil && rel.Chart.Metadata.Name != "" {
		h.chart = "chart " + rel.Chart.Metadata.Name + " " + rel.Chart.Metadata.Version
	}
	return h
}

// podTemplate returns the pod template of a workload, or nil.
func podTemplate(obj map[string]any) map[string]any {
	spec, _ := obj["spec"].(map[string]any)
	if spec == nil {
		return nil
	}
	if jt, ok := spec["jobTemplate"].(map[string]any); ok {
		js, _ := jt["spec"].(map[string]any)
		if js == nil {
			return nil
		}
		t, _ := js["template"].(map[string]any)
		return t
	}
	t, _ := spec["template"].(map[string]any)
	return t
}

// mountsClaim reports whether a pod template mounts a PersistentVolumeClaim.
func mountsClaim(tpl map[string]any) bool {
	ps, _ := tpl["spec"].(map[string]any)
	for _, v := range list(ps["volumes"]) {
		if _, ok := v["persistentVolumeClaim"]; ok {
			return true
		}
	}
	return false
}

func nsName(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

func str(obj map[string]any, path ...string) string {
	var cur any = obj
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	s, _ := cur.(string)
	return s
}

func strMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}

func list(v any) []map[string]any {
	l, _ := v.([]any)
	var out []map[string]any
	for _, e := range l {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func ensureMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	v := map[string]any{}
	m[key] = v
	return v
}

func num(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case float64:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
