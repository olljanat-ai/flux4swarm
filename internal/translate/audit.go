// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package translate

import (
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

// The policy for Kubernetes features that Swarm does not have: they never
// fail a Kustomization. They are dropped with a warning, so that one
// repository can run on Kubernetes and Swarm side by side while Kubernetes
// only features are added. Only real errors (broken references, invalid
// manifests, name clashes) fail.

// audit warns about the pod and container fields that the translation
// drops, in one warning per workload.
func (t *translator) audit(w *Workload, c corev1.Container) {
	pod := w.Template.Spec
	f := append([]string(nil), w.Unsupported...)
	add := func(ok bool, name string) {
		if ok {
			f = append(f, name)
		}
	}
	// Pod.
	add(pod.Affinity != nil, "affinity")
	add(len(pod.Tolerations) > 0, "tolerations")
	add(len(pod.TopologySpreadConstraints) > 0, "topologySpreadConstraints")
	add(pod.ServiceAccountName != "" && pod.ServiceAccountName != "default", "serviceAccountName")
	add(pod.PriorityClassName != "", "priorityClassName")
	add(pod.RuntimeClassName != nil, "runtimeClassName")
	add(pod.SchedulerName != "" && pod.SchedulerName != corev1.DefaultSchedulerName, "schedulerName")
	add(pod.HostNetwork, "hostNetwork")
	add(pod.HostPID, "hostPID")
	add(pod.HostIPC, "hostIPC")
	add(pod.ShareProcessNamespace != nil && *pod.ShareProcessNamespace, "shareProcessNamespace")
	add(len(pod.ReadinessGates) > 0, "readinessGates")
	if sc := pod.SecurityContext; sc != nil {
		add(sc.FSGroup != nil, "securityContext.fsGroup")
		add(len(sc.SupplementalGroups) > 0, "securityContext.supplementalGroups")
		add(len(sc.Sysctls) > 0, "securityContext.sysctls")
		add(sc.SeccompProfile != nil, "securityContext.seccompProfile")
		add(sc.SELinuxOptions != nil, "securityContext.seLinuxOptions")
		add(sc.AppArmorProfile != nil, "securityContext.appArmorProfile")
		add(sc.RunAsNonRoot != nil, "securityContext.runAsNonRoot")
	}
	// Container.
	exec := false
	for _, p := range []struct {
		name  string
		probe *corev1.Probe
	}{{"livenessProbe", c.LivenessProbe}, {"readinessProbe", c.ReadinessProbe}, {"startupProbe", c.StartupProbe}} {
		if p.probe == nil {
			continue
		}
		switch {
		case p.probe.Exec != nil:
			// The first exec probe becomes the Swarm healthcheck.
			add(exec, p.name+" (a Swarm service has one healthcheck)")
			exec = true
		case p.probe.HTTPGet != nil:
			f = append(f, p.name+" (httpGet)")
		case p.probe.TCPSocket != nil:
			f = append(f, p.name+" (tcpSocket)")
		case p.probe.GRPC != nil:
			f = append(f, p.name+" (grpc)")
		}
	}
	add(c.Lifecycle != nil, "lifecycle")
	for _, p := range c.Ports {
		add(p.HostPort != 0, "ports.hostPort "+strconv.Itoa(int(p.HostPort))+" (use a NodePort or LoadBalancer Service)")
	}
	var res []string
	for _, l := range []corev1.ResourceList{c.Resources.Limits, c.Resources.Requests} {
		for k := range l {
			if k != corev1.ResourceCPU && k != corev1.ResourceMemory && !contains(res, string(k)) {
				res = append(res, string(k))
			}
		}
	}
	sort.Strings(res)
	for _, r := range res {
		f = append(f, "resources "+r)
	}
	add(len(c.Resources.Claims) > 0, "resources.claims")
	add(len(c.VolumeDevices) > 0, "volumeDevices")
	if sc := c.SecurityContext; sc != nil {
		add(sc.AllowPrivilegeEscalation != nil, "securityContext.allowPrivilegeEscalation")
		add(sc.SeccompProfile != nil, "securityContext.seccompProfile")
		add(sc.SELinuxOptions != nil, "securityContext.seLinuxOptions")
		add(sc.AppArmorProfile != nil, "securityContext.appArmorProfile")
		add(sc.RunAsNonRoot != nil, "securityContext.runAsNonRoot")
		add(sc.ProcMount != nil && *sc.ProcMount != corev1.DefaultProcMount, "securityContext.procMount")
	}
	if len(f) > 0 {
		t.warn("%s %s/%s: not supported on Swarm and ignored: %s", w.Kind, w.Namespace, w.Name, strings.Join(f, ", "))
	}
}

// deliveryWarnings explains how Flagger and autoscalers were mapped.
func (t *translator) deliveryWarnings(w *Workload) {
	id := w.Kind + " " + w.Namespace + "/" + w.Name
	if w.Canary != "" {
		t.warn("%s: target of Flagger %s; canary analysis is not supported on Swarm, a failed update rolls back", id, w.Canary)
	}
	if s := w.Scaler; s != nil {
		switch {
		case w.Kind == "DaemonSet" || w.Kind == "Job":
			t.warn("%s: autoscaled by %s, which has no effect on Swarm", id, s.Object)
		case w.ScaledReplicas:
			t.warn("%s: autoscaled by %s; autoscaling is not supported on Swarm, it runs minReplicas (%d)", id, s.Object, *w.Replicas)
		default:
			t.warn("%s: autoscaled by %s; autoscaling is not supported on Swarm, it runs spec.replicas (%d)", id, s.Object, *w.Replicas)
		}
	}
}

// pvcWarnings warns about PersistentVolumeClaim settings that need a Docker
// volume driver on Swarm.
func (t *translator) pvcWarnings(id string, p *corev1.PersistentVolumeClaim) {
	if p.Spec.StorageClassName != nil && *p.Spec.StorageClassName != "" && p.Annotations[api.AnnotationVolumeDriver] == "" {
		t.warn("%s: storageClassName %q is not supported on Swarm, a local volume is used (set %s)", id, *p.Spec.StorageClassName, api.AnnotationVolumeDriver)
	}
}

func volumeType(v corev1.Volume) string {
	switch {
	case v.Projected != nil:
		return "projected"
	case v.DownwardAPI != nil:
		return "downwardAPI"
	case v.CSI != nil:
		return "csi"
	case v.Ephemeral != nil:
		return "ephemeral"
	case v.NFS != nil:
		return "nfs"
	case v.Image != nil:
		return "image"
	}
	return "this"
}

func quoteList(l []string) string {
	q := make([]string, len(l))
	for i, s := range l {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}
