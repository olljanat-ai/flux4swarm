// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package translate turns Kubernetes workloads (Deployment, StatefulSet,
// DaemonSet, Job) and their Services, ConfigMaps, Secrets and
// PersistentVolumeClaims into Docker Swarm services, configs, secrets and
// networks. A Kubernetes namespace becomes a Docker stack.
package translate

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/swarm"
	jsonpatch "github.com/evanphx/json-patch/v5"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	swarmapply "github.com/olljanat-ai/flux4swarm/internal/swarm"
)

// SwarmName returns the Swarm object name for a namespaced Kubernetes name:
// "<namespace>_<name>", or just "<name>" in api.SwarmNamespace (the
// --default-namespace, "default" unless set). This is the naming
// `docker stack deploy` uses.
func SwarmName(namespace, name string) string {
	if namespace == "" || namespace == api.SwarmNamespace {
		return name
	}
	return namespace + "_" + name
}

// NetworkName is the default overlay network of a namespace.
func NetworkName(namespace string) string {
	if namespace == "" {
		namespace = api.SwarmNamespace
	}
	return namespace + "_default"
}

// Workload is a pod-template based object.
type Workload struct {
	Kind        string
	Namespace   string
	Name        string
	Labels      map[string]string
	Annotations map[string]string
	Template    corev1.PodTemplateSpec
	// Replicas for Deployment/StatefulSet; nil means 1.
	Replicas *int32
	// Strategy of a Deployment.
	Strategy *appsv1.DeploymentStrategy
	// ClaimTemplates of a StatefulSet.
	ClaimTemplates []corev1.PersistentVolumeClaim
	// Job settings.
	Completions  *int32
	Parallelism  *int32
	BackoffLimit *int32
	// Suspend is a Job's spec.suspend: the job must not start.
	Suspend bool

	// Unsupported lists workload fields that have no Swarm equivalent.
	Unsupported []string
	// Canary is the Flagger Canary that targets this workload.
	Canary string
	// Rollback makes a failed Swarm update roll back automatically, which
	// is what Flagger does on Kubernetes.
	Rollback bool
	// Monitor is how long Swarm watches each updated task for failure.
	Monitor time.Duration
	// Scaler is the autoscaler that targets this workload; ScaledReplicas
	// is true when Replicas comes from its minReplicas.
	Scaler         *api.Scaler
	ScaledReplicas bool
}

// MainContainer returns the container of the pod that runs on Swarm: the
// one named by the swarm.flux4swarm.io/container annotation, else the first.
// ignored lists the names of the other containers.
func (w *Workload) MainContainer() (main corev1.Container, ignored []string, err error) {
	cs := w.Template.Spec.Containers
	if len(cs) == 0 {
		return main, nil, fmt.Errorf("pod has no containers")
	}
	idx := 0
	if n := w.Annotations[api.AnnotationContainer]; n != "" {
		idx = -1
		for i := range cs {
			if cs[i].Name == n {
				idx = i
			}
		}
		if idx < 0 {
			return main, nil, fmt.Errorf("annotation %s: container %q not found", api.AnnotationContainer, n)
		}
	}
	for i := range cs {
		if i != idx {
			ignored = append(ignored, cs[i].Name)
		}
	}
	return cs[idx], ignored, nil
}

// Key is "namespace/name" of the workload's Swarm service.
func (w *Workload) ServiceName() string {
	if n := w.Annotations[api.AnnotationName]; n != "" {
		return n
	}
	return SwarmName(w.Namespace, w.Name)
}

// Workloads lists the workloads of a set.
func Workloads(set *api.Set) []*Workload {
	var out []*Workload
	for _, d := range set.Deployments {
		var un []string
		if d.Spec.MinReadySeconds > 0 {
			un = append(un, "minReadySeconds")
		}
		if d.Spec.ProgressDeadlineSeconds != nil {
			un = append(un, "progressDeadlineSeconds")
		}
		if d.Spec.Paused {
			un = append(un, "paused")
		}
		out = append(out, &Workload{Kind: "Deployment", Namespace: d.Namespace, Name: d.Name, Labels: d.Labels,
			Annotations: d.Annotations, Template: d.Spec.Template, Replicas: d.Spec.Replicas, Strategy: &d.Spec.Strategy,
			Unsupported: un})
	}
	for _, s := range set.StatefulSets {
		var un []string
		if s.Spec.PodManagementPolicy == appsv1.ParallelPodManagement {
			un = append(un, "podManagementPolicy: Parallel")
		}
		if s.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
			un = append(un, "updateStrategy: OnDelete")
		}
		if s.Spec.PersistentVolumeClaimRetentionPolicy != nil {
			un = append(un, "persistentVolumeClaimRetentionPolicy")
		}
		out = append(out, &Workload{Kind: "StatefulSet", Namespace: s.Namespace, Name: s.Name, Labels: s.Labels,
			Annotations: s.Annotations, Template: s.Spec.Template, Replicas: s.Spec.Replicas, ClaimTemplates: s.Spec.VolumeClaimTemplates,
			Unsupported: un})
	}
	for _, d := range set.DaemonSets {
		var un []string
		if d.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType {
			un = append(un, "updateStrategy: OnDelete")
		}
		out = append(out, &Workload{Kind: "DaemonSet", Namespace: d.Namespace, Name: d.Name, Labels: d.Labels,
			Annotations: d.Annotations, Template: d.Spec.Template, Unsupported: un})
	}
	for _, j := range set.Jobs {
		var un []string
		if j.Spec.ActiveDeadlineSeconds != nil {
			un = append(un, "activeDeadlineSeconds")
		}
		if j.Spec.TTLSecondsAfterFinished != nil {
			un = append(un, "ttlSecondsAfterFinished")
		}
		if j.Spec.CompletionMode != nil && *j.Spec.CompletionMode == batchv1.IndexedCompletion {
			un = append(un, "completionMode: Indexed")
		}
		if j.Spec.BackoffLimitPerIndex != nil {
			un = append(un, "backoffLimitPerIndex")
		}
		if j.Spec.PodFailurePolicy != nil {
			un = append(un, "podFailurePolicy")
		}
		out = append(out, &Workload{Kind: "Job", Namespace: j.Namespace, Name: j.Name, Labels: j.Labels,
			Annotations: j.Annotations, Template: j.Spec.Template, Completions: j.Spec.Completions,
			Parallelism: j.Spec.Parallelism, BackoffLimit: j.Spec.BackoffLimit, Unsupported: un,
			// A hold from import is for Kubernetes only.
			Suspend: j.Spec.Suspend != nil && *j.Spec.Suspend && j.Annotations[api.AnnotationHeldJob] != "true"})
	}
	canaries := map[string]api.Canary{}
	for _, c := range set.Canaries {
		canaries[api.TargetKey(c.Target.Kind, c.Namespace, c.Target.Name)] = c
	}
	scalers := map[string]api.Scaler{}
	for _, s := range set.Scalers {
		scalers[api.TargetKey(s.Target.Kind, s.Namespace, s.Target.Name)] = s
	}
	for _, w := range out {
		k := api.TargetKey(w.Kind, w.Namespace, w.Name)
		if c, ok := canaries[k]; ok {
			w.Canary, w.Rollback, w.Monitor = c.Object, true, c.Interval
		}
		if s, ok := scalers[k]; ok {
			w.Scaler = &s
			if w.Replicas == nil && w.Kind != "DaemonSet" && w.Kind != "Job" {
				m := s.MinReplicas
				w.Replicas, w.ScaledReplicas = &m, true
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServiceName() < out[j].ServiceName() })
	return out
}

// Input is everything the translation needs.
type Input struct {
	Set *api.Set
	// Secrets are the plain Secrets of the set plus those resolved from
	// ExternalSecrets.
	Secrets []*corev1.Secret
	// RouteLabels are extra labels per Swarm service name (Traefik).
	RouteLabels map[string]map[string]string
}

// Output is the Swarm desired state plus notes.
type Output struct {
	Desired  swarmapply.Desired
	Warnings []string
}

type translator struct {
	in         Input
	out        *Output
	configMaps map[string]*corev1.ConfigMap
	secrets    map[string]*corev1.Secret
	pvcs       map[string]*corev1.PersistentVolumeClaim
	configs    map[string]bool
	secretObjs map[string]bool
	networks   map[string]bool
}

// Translate converts the Kubernetes objects of in to Swarm objects.
func Translate(in Input) (*Output, error) {
	t := &translator{
		in:         in,
		out:        &Output{},
		configMaps: map[string]*corev1.ConfigMap{},
		secrets:    map[string]*corev1.Secret{},
		pvcs:       map[string]*corev1.PersistentVolumeClaim{},
		configs:    map[string]bool{},
		secretObjs: map[string]bool{},
		networks:   map[string]bool{},
	}
	for _, c := range in.Set.ConfigMaps {
		t.configMaps[c.Namespace+"/"+c.Name] = c
	}
	for _, s := range in.Secrets {
		t.secrets[s.Namespace+"/"+s.Name] = s
	}
	for _, p := range in.Set.PVCs {
		t.pvcs[p.Namespace+"/"+p.Name] = p
	}
	seen := map[string]string{}
	for _, p := range in.Set.PVCs {
		t.pvcWarnings("PersistentVolumeClaim "+p.Namespace+"/"+p.Name, p)
	}
	for _, w := range Workloads(in.Set) {
		svc, err := t.workload(w)
		if err != nil {
			return nil, fmt.Errorf("%s %s/%s: %w", w.Kind, w.Namespace, w.Name, err)
		}
		if prev, ok := seen[svc.Name]; ok {
			return nil, fmt.Errorf("%s %s/%s and %s map to the same Swarm service %q", w.Kind, w.Namespace, w.Name, prev, svc.Name)
		}
		seen[svc.Name] = w.Kind + " " + w.Namespace + "/" + w.Name
		t.out.Desired.Services = append(t.out.Desired.Services, svc)
	}
	var nets []string
	for n := range t.networks {
		nets = append(nets, n)
	}
	sort.Strings(nets)
	for _, n := range nets {
		ns, _, _ := strings.Cut(n, "_")
		l := map[string]string{}
		if strings.HasSuffix(n, "_default") && ns != "" {
			l[api.LabelStackNamespace] = ns
		}
		t.out.Desired.Networks = append(t.out.Desired.Networks, &swarmapply.Network{Name: n, Labels: l})
	}
	return t.out, nil
}

func (t *translator) warn(format string, args ...any) {
	t.out.Warnings = append(t.out.Warnings, fmt.Sprintf(format, args...))
}

// matchingServices returns the Kubernetes Services selecting the pod labels.
func (t *translator) matchingServices(ns string, podLabels map[string]string) []*corev1.Service {
	var out []*corev1.Service
	for _, s := range t.in.Set.Services {
		if s.Namespace != ns || len(s.Spec.Selector) == 0 {
			continue
		}
		if labels.SelectorFromSet(s.Spec.Selector).Matches(labels.Set(podLabels)) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (t *translator) workload(w *Workload) (*swarmapply.Service, error) {
	pod := w.Template.Spec
	c, sidecars, err := w.MainContainer()
	if err != nil {
		return nil, err
	}
	if len(sidecars) > 0 {
		t.warn("%s %s/%s: a Swarm service runs one container; %q runs and %s %s ignored (choose with %s)",
			w.Kind, w.Namespace, w.Name, c.Name, quoteList(sidecars), plural(len(sidecars), "was", "were"), api.AnnotationContainer)
	}
	if len(pod.InitContainers) > 0 {
		t.warn("%s %s/%s: init containers are not supported on Swarm and were ignored", w.Kind, w.Namespace, w.Name)
	}
	t.audit(w, c)
	name := w.ServiceName()

	spec := swarm.ServiceSpec{Annotations: swarm.Annotations{Name: name, Labels: copyMap(w.Labels)}}
	if w.Namespace != api.SwarmNamespace {
		spec.Labels[api.LabelStackNamespace] = w.Namespace
	}
	cs := &swarm.ContainerSpec{
		Image:     c.Image,
		Command:   c.Command,
		Args:      c.Args,
		Dir:       c.WorkingDir,
		Hostname:  pod.Hostname,
		TTY:       c.TTY,
		OpenStdin: c.Stdin,
	}
	if w.Namespace != api.SwarmNamespace {
		cs.Labels = map[string]string{api.LabelStackNamespace: w.Namespace}
	}
	env, err := t.env(w.Namespace, c)
	if err != nil {
		return nil, err
	}
	cs.Env = env
	if pod.TerminationGracePeriodSeconds != nil {
		d := time.Duration(*pod.TerminationGracePeriodSeconds) * time.Second
		cs.StopGracePeriod = &d
	}
	user, group := int64(-1), int64(-1)
	if sc := pod.SecurityContext; sc != nil {
		if sc.RunAsUser != nil {
			user = *sc.RunAsUser
		}
		if sc.RunAsGroup != nil {
			group = *sc.RunAsGroup
		}
	}
	if sc := c.SecurityContext; sc != nil {
		if sc.RunAsUser != nil {
			user = *sc.RunAsUser
		}
		if sc.RunAsGroup != nil {
			group = *sc.RunAsGroup
		}
		if sc.ReadOnlyRootFilesystem != nil {
			cs.ReadOnly = *sc.ReadOnlyRootFilesystem
		}
		if sc.Capabilities != nil {
			for _, a := range sc.Capabilities.Add {
				cs.CapabilityAdd = append(cs.CapabilityAdd, capName(string(a)))
			}
			for _, d := range sc.Capabilities.Drop {
				cs.CapabilityDrop = append(cs.CapabilityDrop, capName(string(d)))
			}
		}
		if sc.Privileged != nil && *sc.Privileged {
			t.warn("%s %s/%s: privileged containers are not supported by Swarm services", w.Kind, w.Namespace, w.Name)
		}
	}
	if user >= 0 {
		cs.User = strconv.FormatInt(user, 10)
		if group >= 0 {
			cs.User += ":" + strconv.FormatInt(group, 10)
		}
	}
	cs.Healthcheck = healthcheck(c)
	if err := t.volumes(w, c, cs); err != nil {
		return nil, err
	}
	if pod.DNSConfig != nil {
		cs.DNSConfig = &swarm.DNSConfig{Nameservers: pod.DNSConfig.Nameservers, Search: pod.DNSConfig.Searches}
		for _, o := range pod.DNSConfig.Options {
			v := o.Name
			if o.Value != nil {
				v += ":" + *o.Value
			}
			cs.DNSConfig.Options = append(cs.DNSConfig.Options, v)
		}
	}
	for _, h := range pod.HostAliases {
		for _, hn := range h.Hostnames {
			cs.Hosts = append(cs.Hosts, h.IP+" "+hn)
		}
	}
	spec.TaskTemplate.ContainerSpec = cs

	// Resources.
	if res := resources(c.Resources); res != nil {
		spec.TaskTemplate.Resources = res
	}
	// Placement.
	if cons := constraints(pod.NodeSelector); len(cons) > 0 {
		spec.TaskTemplate.Placement = &swarm.Placement{Constraints: cons}
	}

	// Mode and restart policy.
	switch w.Kind {
	case "DaemonSet":
		spec.Mode = swarm.ServiceMode{Global: &swarm.GlobalService{}}
	case "Job":
		job := &swarm.ReplicatedJob{MaxConcurrent: u64(w.Parallelism, 1), TotalCompletions: u64(w.Completions, 1)}
		spec.Mode = swarm.ServiceMode{ReplicatedJob: job}
		cond := swarm.RestartPolicyConditionOnFailure
		if pod.RestartPolicy == corev1.RestartPolicyNever {
			cond = swarm.RestartPolicyConditionNone
		}
		rp := &swarm.RestartPolicy{Condition: cond}
		if w.BackoffLimit != nil && cond != swarm.RestartPolicyConditionNone {
			n := uint64(*w.BackoffLimit)
			rp.MaxAttempts = &n
		}
		spec.TaskTemplate.RestartPolicy = rp
	default:
		r := uint64(1)
		if w.Replicas != nil {
			r = uint64(*w.Replicas)
		}
		spec.Mode = swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &r}}
	}
	if w.Strategy != nil {
		switch w.Strategy.Type {
		case appsv1.RecreateDeploymentStrategyType:
			spec.UpdateConfig = &swarm.UpdateConfig{Parallelism: 0, Order: swarm.UpdateOrderStopFirst}
		default:
			surge := true
			if ru := w.Strategy.RollingUpdate; ru != nil && ru.MaxSurge != nil {
				surge = ru.MaxSurge.String() != "0" && ru.MaxSurge.String() != "0%"
			}
			// Without surge the Swarm default (one at a time, stop first) applies.
			if surge {
				spec.UpdateConfig = &swarm.UpdateConfig{Parallelism: 1, Order: swarm.UpdateOrderStartFirst}
			}
		}
	}
	// Flagger rolls back a failed release; Swarm can do
	// the same for a failed update.
	if w.Rollback && w.Kind != "Job" {
		if spec.UpdateConfig == nil {
			// The Swarm defaults, made explicit.
			spec.UpdateConfig = &swarm.UpdateConfig{Parallelism: 1, Order: swarm.UpdateOrderStopFirst}
		}
		spec.UpdateConfig.FailureAction = swarm.UpdateFailureActionRollback
		if w.Monitor > 0 {
			spec.UpdateConfig.Monitor = w.Monitor
		}
	}
	t.deliveryWarnings(w)

	// Networks, aliases and published ports from Kubernetes Services.
	svcs := t.matchingServices(w.Namespace, w.Template.Labels)
	var aliases []string
	for _, s := range svcs {
		aliases = append(aliases, s.Name)
	}
	nets := []string{NetworkName(w.Namespace)}
	if v, ok := w.Annotations[api.AnnotationNetworks]; ok {
		nets = nil
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" {
				nets = append(nets, n)
			}
		}
	}
	for _, n := range nets {
		spec.TaskTemplate.Networks = append(spec.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: n, Aliases: aliases})
		t.networks[n] = true
	}
	ports, err := publishedPorts(svcs, c)
	if err != nil {
		return nil, err
	}
	if len(ports) > 0 {
		spec.EndpointSpec = &swarm.EndpointSpec{Mode: swarm.ResolutionModeVIP, Ports: ports}
	}
	for _, s := range svcs {
		if s.Spec.ClusterIP == corev1.ClusterIPNone {
			spec.EndpointSpec = &swarm.EndpointSpec{Mode: swarm.ResolutionModeDNSRR, Ports: ports}
		}
	}

	// Traefik labels from HTTPRoutes.
	for k, v := range t.in.RouteLabels[name] {
		spec.Labels[k] = v
	}

	// Registry credentials.
	auths := map[string]registry.AuthConfig{}
	for _, ref := range pod.ImagePullSecrets {
		s := t.secrets[w.Namespace+"/"+ref.Name]
		if s == nil {
			t.warn("%s %s/%s: imagePullSecret %q not found", w.Kind, w.Namespace, w.Name, ref.Name)
			continue
		}
		a, err := dockerConfigAuths(s)
		if err != nil {
			return nil, fmt.Errorf("imagePullSecret %s: %w", ref.Name, err)
		}
		for k, v := range a {
			auths[k] = v
		}
	}

	// Swarm-only overrides.
	if patch := w.Annotations[api.AnnotationServiceSpec]; patch != "" {
		spec, err = ApplyPatch(spec, []byte(patch))
		if err != nil {
			return nil, fmt.Errorf("annotation %s: %w", api.AnnotationServiceSpec, err)
		}
		spec.Name = name
	}
	if len(spec.Labels) == 0 {
		spec.Labels = nil
	}
	if u := spec.UpdateConfig; u != nil && u.Order == swarm.UpdateOrderStartFirst && w.Kind != "Job" && t.writesSingleClaim(w, c) {
		t.warn("%s %s/%s: updates start the new task before stopping the old one, so both use the same volume for a moment; "+
			"use strategy Recreate or rollingUpdate.maxSurge: 0 for databases and other single-writer workloads",
			w.Kind, w.Namespace, w.Name)
	}
	return &swarmapply.Service{Name: name, Spec: spec, RegistryAuth: auths, Suspended: w.Suspend}, nil
}

// writesSingleClaim reports whether container c writes to a
// PersistentVolumeClaim that is not shared (no ReadWriteMany access mode),
// so two tasks must not use it at the same time.
func (t *translator) writesSingleClaim(w *Workload, c corev1.Container) bool {
	claims := map[string]string{}
	for _, v := range w.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && !v.PersistentVolumeClaim.ReadOnly {
			claims[v.Name] = v.PersistentVolumeClaim.ClaimName
		}
	}
	for _, vm := range c.VolumeMounts {
		claim, ok := claims[vm.Name]
		if !ok || vm.ReadOnly {
			continue
		}
		shared := false
		if p := t.pvcs[w.Namespace+"/"+claim]; p != nil {
			for _, m := range p.Spec.AccessModes {
				if m == corev1.ReadWriteMany {
					shared = true
				}
			}
		}
		if !shared {
			return true
		}
	}
	return false
}

// ApplyPatch applies a JSON merge patch (YAML is accepted) to spec.
func ApplyPatch(spec swarm.ServiceSpec, patch []byte) (swarm.ServiceSpec, error) {
	pj, err := toJSON(patch)
	if err != nil {
		return spec, err
	}
	orig, err := json.Marshal(spec)
	if err != nil {
		return spec, err
	}
	merged, err := jsonpatch.MergePatch(orig, pj)
	if err != nil {
		return spec, err
	}
	var out swarm.ServiceSpec
	dec := json.NewDecoder(strings.NewReader(string(merged)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return spec, err
	}
	return out, nil
}

// CreatePatch returns the JSON merge patch that turns from into to, or nil
// if they are equal.
func CreatePatch(from, to swarm.ServiceSpec) ([]byte, error) {
	a, err := json.Marshal(from)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(to)
	if err != nil {
		return nil, err
	}
	p, err := jsonpatch.CreateMergePatch(a, b)
	if err != nil {
		return nil, err
	}
	if string(p) == "{}" {
		return nil, nil
	}
	return p, nil
}

func (t *translator) env(ns string, c corev1.Container) ([]string, error) {
	var out []string
	for _, ef := range c.EnvFrom {
		var data map[string]string
		switch {
		case ef.ConfigMapRef != nil:
			cm := t.configMaps[ns+"/"+ef.ConfigMapRef.Name]
			if cm == nil {
				if ef.ConfigMapRef.Optional != nil && *ef.ConfigMapRef.Optional {
					continue
				}
				return nil, fmt.Errorf("envFrom configMap %q not found", ef.ConfigMapRef.Name)
			}
			data = cm.Data
		case ef.SecretRef != nil:
			s := t.secrets[ns+"/"+ef.SecretRef.Name]
			if s == nil {
				if ef.SecretRef.Optional != nil && *ef.SecretRef.Optional {
					continue
				}
				return nil, fmt.Errorf("envFrom secret %q not found", ef.SecretRef.Name)
			}
			data = map[string]string{}
			for k, v := range s.Data {
				data[k] = string(v)
			}
		}
		keys := sortedKeys(data)
		for _, k := range keys {
			out = append(out, ef.Prefix+k+"="+data[k])
		}
	}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			out = append(out, e.Name+"="+e.Value)
			continue
		}
		vf := e.ValueFrom
		switch {
		case vf.ConfigMapKeyRef != nil:
			r := vf.ConfigMapKeyRef
			cm := t.configMaps[ns+"/"+r.Name]
			v, ok := "", false
			if cm != nil {
				v, ok = cm.Data[r.Key]
			}
			if !ok {
				if r.Optional != nil && *r.Optional {
					continue
				}
				return nil, fmt.Errorf("env %s: configMap key %s/%s not found", e.Name, r.Name, r.Key)
			}
			out = append(out, e.Name+"="+v)
		case vf.SecretKeyRef != nil:
			r := vf.SecretKeyRef
			s := t.secrets[ns+"/"+r.Name]
			var v []byte
			ok := false
			if s != nil {
				v, ok = s.Data[r.Key]
			}
			if !ok {
				if r.Optional != nil && *r.Optional {
					continue
				}
				return nil, fmt.Errorf("env %s: secret key %s/%s not found", e.Name, r.Name, r.Key)
			}
			out = append(out, e.Name+"="+string(v))
		case vf.FieldRef != nil:
			switch vf.FieldRef.FieldPath {
			case "metadata.name":
				out = append(out, e.Name+"={{.Task.Name}}")
			case "spec.nodeName":
				out = append(out, e.Name+"={{.Node.Hostname}}")
			case "metadata.namespace":
				out = append(out, e.Name+"="+ns)
			default:
				t.warn("env %s: fieldRef %s is not supported on Swarm", e.Name, vf.FieldRef.FieldPath)
			}
		default:
			t.warn("env %s: valueFrom is not supported on Swarm", e.Name)
		}
	}
	return out, nil
}

func (t *translator) volumes(w *Workload, c corev1.Container, cs *swarm.ContainerSpec) error {
	vols := map[string]corev1.Volume{}
	for _, v := range w.Template.Spec.Volumes {
		vols[v.Name] = v
	}
	claimTpl := map[string]corev1.PersistentVolumeClaim{}
	for _, p := range w.ClaimTemplates {
		claimTpl[p.Name] = p
	}
	for _, vm := range c.VolumeMounts {
		v, ok := vols[vm.Name]
		if !ok {
			if p, ok := claimTpl[vm.Name]; ok {
				t.pvcWarnings(fmt.Sprintf("%s %s/%s volumeClaimTemplate %s", w.Kind, w.Namespace, w.Name, p.Name), &p)
				src := SwarmName(w.Namespace, p.Name+"-"+w.Name) + "-{{.Task.Slot}}"
				cs.Mounts = append(cs.Mounts, volumeMount(src, vm, &p, w.Namespace))
				continue
			}
			return fmt.Errorf("volumeMount %q has no volume", vm.Name)
		}
		switch {
		case v.ConfigMap != nil:
			cm := t.configMaps[w.Namespace+"/"+v.ConfigMap.Name]
			if cm == nil {
				if v.ConfigMap.Optional != nil && *v.ConfigMap.Optional {
					continue
				}
				return fmt.Errorf("configMap %q not found", v.ConfigMap.Name)
			}
			data := map[string][]byte{}
			for k, s := range cm.Data {
				data[k] = []byte(s)
			}
			for k, b := range cm.BinaryData {
				data[k] = b
			}
			files, err := projectFiles(data, v.ConfigMap.Items, v.ConfigMap.DefaultMode, 0o644, vm)
			if err != nil {
				return fmt.Errorf("configMap %s: %w", cm.Name, err)
			}
			for _, f := range files {
				logical := SwarmName(w.Namespace, cm.Name+"_"+f.key)
				t.addConfig(logical, f.data, w.Namespace)
				cs.Configs = append(cs.Configs, &swarm.ConfigReference{
					ConfigName: logical,
					File:       &swarm.ConfigReferenceFileTarget{Name: f.target, UID: "0", GID: "0", Mode: f.mode},
				})
			}
		case v.Secret != nil:
			s := t.secrets[w.Namespace+"/"+v.Secret.SecretName]
			if s == nil {
				if v.Secret.Optional != nil && *v.Secret.Optional {
					continue
				}
				return fmt.Errorf("secret %q not found", v.Secret.SecretName)
			}
			files, err := projectFiles(s.Data, v.Secret.Items, v.Secret.DefaultMode, 0o644, vm)
			if err != nil {
				return fmt.Errorf("secret %s: %w", s.Name, err)
			}
			for _, f := range files {
				logical := SwarmName(w.Namespace, s.Name+"_"+f.key)
				t.addSecret(logical, f.data, w.Namespace, s.Name)
				cs.Secrets = append(cs.Secrets, &swarm.SecretReference{
					SecretName: logical,
					File:       &swarm.SecretReferenceFileTarget{Name: f.target, UID: "0", GID: "0", Mode: f.mode},
				})
			}
		case v.EmptyDir != nil:
			if v.EmptyDir.Medium == corev1.StorageMediumMemory {
				m := mount.Mount{Type: mount.TypeTmpfs, Target: vm.MountPath}
				if v.EmptyDir.SizeLimit != nil {
					m.TmpfsOptions = &mount.TmpfsOptions{SizeBytes: v.EmptyDir.SizeLimit.Value()}
				}
				cs.Mounts = append(cs.Mounts, m)
			} else {
				cs.Mounts = append(cs.Mounts, mount.Mount{Type: mount.TypeVolume, Target: vm.MountPath})
			}
		case v.HostPath != nil:
			cs.Mounts = append(cs.Mounts, mount.Mount{Type: mount.TypeBind, Source: v.HostPath.Path, Target: vm.MountPath, ReadOnly: vm.ReadOnly})
		case v.PersistentVolumeClaim != nil:
			claim := v.PersistentVolumeClaim.ClaimName
			p := t.pvcs[w.Namespace+"/"+claim]
			if p == nil {
				t.warn("%s %s/%s: PersistentVolumeClaim %q is not in the source; using a local volume", w.Kind, w.Namespace, w.Name, claim)
			}
			m := volumeMount(SwarmName(w.Namespace, claim), vm, p, w.Namespace)
			if v.PersistentVolumeClaim.ReadOnly {
				m.ReadOnly = true
			}
			cs.Mounts = append(cs.Mounts, m)
		default:
			t.warn("%s %s/%s: %s volume %q is not supported on Swarm; its mount at %s was skipped (supported: configMap, secret, emptyDir, hostPath, persistentVolumeClaim)",
				w.Kind, w.Namespace, w.Name, volumeType(v), v.Name, vm.MountPath)
		}
	}
	return nil
}

// volumeMount maps a PersistentVolumeClaim to a named Docker volume. Like
// `docker stack deploy`, volumes of a stack carry the stack label.
func volumeMount(source string, vm corev1.VolumeMount, p *corev1.PersistentVolumeClaim, ns string) mount.Mount {
	m := mount.Mount{Type: mount.TypeVolume, Source: source, Target: vm.MountPath, ReadOnly: vm.ReadOnly}
	vo := &mount.VolumeOptions{Subpath: vm.SubPath}
	l := map[string]string{}
	if p != nil {
		for k, v := range p.Labels {
			l[k] = v
		}
		driver := p.Annotations[api.AnnotationVolumeDriver]
		var opts map[string]string
		if o := p.Annotations[api.AnnotationVolumeOptions]; o != "" {
			_ = json.Unmarshal([]byte(o), &opts)
		}
		if driver != "" || len(opts) > 0 {
			if driver == "" {
				driver = "local"
			}
			vo.DriverConfig = &mount.Driver{Name: driver, Options: opts}
		}
	}
	if ns != api.SwarmNamespace {
		l[api.LabelStackNamespace] = ns
	}
	if len(l) > 0 {
		vo.Labels = l
	}
	if vo.Subpath != "" || vo.DriverConfig != nil || vo.Labels != nil {
		m.VolumeOptions = vo
	}
	return m
}

type projected struct {
	key    string
	target string
	data   []byte
	mode   osFileMode
}

func projectFiles(data map[string][]byte, items []corev1.KeyToPath, defMode *int32, fallback int32, vm corev1.VolumeMount) ([]projected, error) {
	mode := fallback
	if defMode != nil {
		mode = *defMode
	}
	type kp struct {
		key, rel string
		mode     int32
	}
	var list []kp
	if len(items) > 0 {
		for _, it := range items {
			m := mode
			if it.Mode != nil {
				m = *it.Mode
			}
			list = append(list, kp{it.Key, it.Path, m})
		}
	} else {
		for _, k := range sortedKeysB(data) {
			list = append(list, kp{k, k, mode})
		}
	}
	var out []projected
	for _, e := range list {
		d, ok := data[e.key]
		if !ok {
			return nil, fmt.Errorf("key %q not found", e.key)
		}
		target := path.Join(vm.MountPath, e.rel)
		if vm.SubPath != "" {
			if e.rel != vm.SubPath {
				continue
			}
			target = vm.MountPath
		}
		out = append(out, projected{key: e.key, target: target, data: d, mode: osFileMode(e.mode)})
	}
	if vm.SubPath != "" && len(out) == 0 {
		return nil, fmt.Errorf("subPath %q matches no key", vm.SubPath)
	}
	return out, nil
}

func (t *translator) addConfig(name string, data []byte, ns string) {
	if t.configs[name] {
		return
	}
	t.configs[name] = true
	l := map[string]string{}
	if ns != api.SwarmNamespace {
		l[api.LabelStackNamespace] = ns
	}
	t.out.Desired.Configs = append(t.out.Desired.Configs, &swarmapply.Config{Name: name, Data: data, Labels: l})
}

func (t *translator) addSecret(name string, data []byte, ns, source string) {
	if t.secretObjs[name] {
		return
	}
	t.secretObjs[name] = true
	l := map[string]string{}
	if ns != api.SwarmNamespace {
		l[api.LabelStackNamespace] = ns
	}
	t.out.Desired.Secrets = append(t.out.Desired.Secrets, swarmapply.Secret{Name: name, Data: data, Labels: l, Source: ns + "/" + source})
}

func publishedPorts(svcs []*corev1.Service, c corev1.Container) ([]swarm.PortConfig, error) {
	var out []swarm.PortConfig
	seen := map[string]bool{}
	for _, s := range svcs {
		if s.Spec.Type != corev1.ServiceTypeLoadBalancer && s.Spec.Type != corev1.ServiceTypeNodePort {
			continue
		}
		mode := swarm.PortConfigPublishModeIngress
		if s.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal {
			mode = swarm.PortConfigPublishModeHost
		}
		for _, p := range s.Spec.Ports {
			target, err := targetPort(p.TargetPort, p.Port, c)
			if err != nil {
				return nil, fmt.Errorf("service %s port %d: %w", s.Name, p.Port, err)
			}
			published := uint32(p.Port)
			if s.Spec.Type == corev1.ServiceTypeNodePort {
				if p.NodePort == 0 {
					return nil, fmt.Errorf("service %s port %d: NodePort services need an explicit nodePort on Swarm", s.Name, p.Port)
				}
				published = uint32(p.NodePort)
			}
			proto := swarm.PortConfigProtocolTCP
			switch p.Protocol {
			case corev1.ProtocolUDP:
				proto = swarm.PortConfigProtocolUDP
			case corev1.ProtocolSCTP:
				proto = swarm.PortConfigProtocolSCTP
			}
			key := fmt.Sprintf("%s/%d", proto, published)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, swarm.PortConfig{Name: p.Name, Protocol: proto, TargetPort: target, PublishedPort: published, PublishMode: mode})
		}
	}
	return out, nil
}

// targetPort resolves a Service targetPort against the container ports.
func targetPort(tp intstr.IntOrString, port int32, c corev1.Container) (uint32, error) {
	if tp.Type == intstr.String && tp.StrVal != "" {
		for _, cp := range c.Ports {
			if cp.Name == tp.StrVal {
				return uint32(cp.ContainerPort), nil
			}
		}
		return 0, fmt.Errorf("named port %q not found in container", tp.StrVal)
	}
	if tp.IntVal != 0 {
		return uint32(tp.IntVal), nil
	}
	return uint32(port), nil
}

func healthcheck(c corev1.Container) *containerHealth {
	p := c.LivenessProbe
	if p == nil || p.Exec == nil {
		p = c.ReadinessProbe
	}
	if p == nil || p.Exec == nil {
		p = c.StartupProbe
	}
	if p == nil || p.Exec == nil {
		return nil
	}
	hc := &containerHealth{Test: append([]string{"CMD"}, p.Exec.Command...)}
	if p.PeriodSeconds > 0 {
		hc.Interval = time.Duration(p.PeriodSeconds) * time.Second
	}
	if p.TimeoutSeconds > 0 {
		hc.Timeout = time.Duration(p.TimeoutSeconds) * time.Second
	}
	if p.FailureThreshold > 0 {
		hc.Retries = int(p.FailureThreshold)
	}
	if p.InitialDelaySeconds > 0 {
		hc.StartPeriod = time.Duration(p.InitialDelaySeconds) * time.Second
	}
	return hc
}

func resources(r corev1.ResourceRequirements) *swarm.ResourceRequirements {
	lim := limits(r.Limits)
	res := limits(r.Requests)
	if lim == nil && res == nil {
		return nil
	}
	out := &swarm.ResourceRequirements{}
	if lim != nil {
		out.Limits = &swarm.Limit{NanoCPUs: lim.NanoCPUs, MemoryBytes: lim.MemoryBytes}
	}
	if res != nil {
		out.Reservations = res
	}
	return out
}

func limits(l corev1.ResourceList) *swarm.Resources {
	if len(l) == 0 {
		return nil
	}
	out := &swarm.Resources{}
	if q, ok := l[corev1.ResourceCPU]; ok {
		out.NanoCPUs = q.MilliValue() * 1_000_000
	}
	if q, ok := l[corev1.ResourceMemory]; ok {
		out.MemoryBytes = q.Value()
	}
	if out.NanoCPUs == 0 && out.MemoryBytes == 0 {
		return nil
	}
	return out
}

// constraints maps a nodeSelector to Swarm placement constraints.
func constraints(sel map[string]string) []string {
	var out []string
	for _, k := range sortedKeys(sel) {
		v := sel[k]
		switch k {
		case "kubernetes.io/hostname":
			out = append(out, "node.hostname=="+v)
		case "kubernetes.io/os", "beta.kubernetes.io/os":
			out = append(out, "node.platform.os=="+v)
		case "kubernetes.io/arch", "beta.kubernetes.io/arch":
			out = append(out, "node.platform.arch=="+archToSwarm(v))
		case "node-role.kubernetes.io/control-plane", "node-role.kubernetes.io/master":
			out = append(out, "node.role==manager")
		default:
			out = append(out, "node.labels."+k+"=="+v)
		}
	}
	return out
}

func archToSwarm(a string) string {
	switch a {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return a
}

func capName(c string) string {
	c = strings.ToUpper(c)
	if !strings.HasPrefix(c, "CAP_") && c != "ALL" {
		c = "CAP_" + c
	}
	return c
}

func dockerConfigAuths(s *corev1.Secret) (map[string]registry.AuthConfig, error) {
	raw, ok := s.Data[corev1.DockerConfigJsonKey]
	if !ok {
		return nil, fmt.Errorf("secret has no %s key", corev1.DockerConfigJsonKey)
	}
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	out := map[string]registry.AuthConfig{}
	for host, a := range cfg.Auths {
		ac := registry.AuthConfig{Username: a.Username, Password: a.Password, ServerAddress: host}
		if a.Auth != "" {
			user, pass, err := decodeAuth(a.Auth)
			if err != nil {
				return nil, err
			}
			ac.Username, ac.Password = user, pass
		}
		h := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/")
		if host == "https://index.docker.io/v1/" {
			h = host
		}
		out[h] = ac
	}
	return out, nil
}

func u64(p *int32, def uint64) *uint64 {
	v := def
	if p != nil {
		v = uint64(*p)
	}
	return &v
}

func copyMap(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysB(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Backend finds the Swarm service and container port behind a Kubernetes
// Service port. port 0 means the Service's only port.
func Backend(set *api.Set) func(ns, name string, port int32) (string, uint32, error) {
	return func(ns, name string, port int32) (string, uint32, error) {
		var svc *corev1.Service
		for _, s := range set.Services {
			if s.Namespace == ns && s.Name == name {
				svc = s
			}
		}
		if svc == nil {
			return "", 0, fmt.Errorf("Service %s/%s not found", ns, name)
		}
		var sp *corev1.ServicePort
		for i := range svc.Spec.Ports {
			if svc.Spec.Ports[i].Port == port || (port == 0 && len(svc.Spec.Ports) == 1) {
				sp = &svc.Spec.Ports[i]
			}
		}
		if sp == nil {
			return "", 0, fmt.Errorf("Service %s/%s has no port %d", ns, name, port)
		}
		var found []*Workload
		for _, w := range Workloads(set) {
			if w.Namespace == ns && len(svc.Spec.Selector) > 0 &&
				labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(w.Template.Labels)) {
				found = append(found, w)
			}
		}
		if len(found) != 1 {
			return "", 0, fmt.Errorf("Service %s/%s selects %d workloads; exactly one is needed on Swarm", ns, name, len(found))
		}
		w := found[0]
		c, _, err := w.MainContainer()
		if err != nil {
			return "", 0, fmt.Errorf("workload %s: %w", w.Name, err)
		}
		tp, err := targetPort(sp.TargetPort, sp.Port, c)
		if err != nil {
			return "", 0, err
		}
		return w.ServiceName(), tp, nil
	}
}
