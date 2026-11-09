// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"strings"
	"time"

	"github.com/docker/docker/api/types/swarm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

// DefaultVolSyncEnv are the restic environment variables, besides
// RESTIC_REPOSITORY and RESTIC_PASSWORD, that the VolSync repository Secret
// carries by default: the credentials of an S3 compatible repository.
var DefaultVolSyncEnv = []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}

// VolSync object conventions shared by import and volume-backup.
const (
	// VolSyncHost is the restic host name VolSync uses for its snapshots.
	VolSyncHost = "volsync"
	// VolSyncDataDir is the directory VolSync backs up from and restores
	// to. Its restore only picks snapshots whose path is this directory.
	VolSyncDataDir = "/data"
	// VolSyncRemoteKey is the remoteRef.key prefix (after --secret-key-prefix)
	// of the repository settings in the secret store.
	VolSyncRemoteKey = "volsync-"
)

// VolSyncFinal is the manual trigger of the final restore at cutover.
const VolSyncFinal = "final"

// holding reports whether the export holds workloads on Kubernetes until
// they are cut over: with VolSync, except for the cutover itself and for
// clusters that restore first.
func (e *exporter) holding() bool {
	return e.o.VolSync && !e.o.VolSyncCutover && !e.o.VolSyncRestoreFirst
}

// held reports whether a workload with this pod waits on Kubernetes for a
// VolSync restore: with VolSync, every workload that mounts a claim.
func (e *exporter) held(pod corev1.PodSpec) bool {
	if !e.holding() {
		return false
	}
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim != nil {
			return true
		}
	}
	return false
}

// VolSyncRepository returns the restic repository of claim ns/name below the
// base repository: one repository per claim, because VolSync restores the
// newest /data snapshot of a repository whatever its tags.
func VolSyncRepository(base, ns, name string) string {
	return strings.TrimSuffix(base, "/") + "/" + ns + "/" + name
}

// VolSyncSecretName is the name of the repository Secret of claim name.
func VolSyncSecretName(name string) string { return name + "-volsync-restic" }

// VolSyncEnv returns the environment variable names of the repository Secret.
func VolSyncEnv(extra []string) []string {
	if extra == nil {
		extra = DefaultVolSyncEnv
	}
	out := []string{"RESTIC_REPOSITORY", "RESTIC_PASSWORD"}
	for _, e := range extra {
		if e = strings.TrimSpace(e); e != "" && !contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

// volsync writes a ReplicationDestination that restores the newest restic
// snapshot into PersistentVolumeClaim ns/name, and the ExternalSecret with
// its repository settings. Both are Kubernetes only.
func (e *exporter) volsync(ns, name string) {
	ignore := map[string]string{api.AnnotationIgnore: "true"}
	secret := VolSyncSecretName(name)
	es := &api.ExternalSecret{
		TypeMeta: api.TypeMeta{APIVersion: api.ESOGroupVersion, Kind: api.KindExternalSecret},
		Metadata: api.ObjectMeta{Name: secret, Namespace: ns, Annotations: ignore},
		Spec: api.ExternalSecretSpec{
			RefreshInterval: &api.Duration{Duration: time.Hour},
			SecretStoreRef:  api.SecretStoreRef{Name: e.o.SecretStore, Kind: e.o.SecretStoreKind},
			Target: api.ExternalSecretTarget{Name: secret, Template: &api.ExternalSecretTemplate{
				EngineVersion: "v2",
				Data:          map[string]string{},
			}},
		},
	}
	for _, env := range VolSyncEnv(e.o.VolSyncEnv) {
		rk := e.o.SecretKeyPrefix + VolSyncRemoteKey + strings.ReplaceAll(strings.ToLower(env), "_", "-")
		if e.o.SecretStoreProvider == ProviderAzureKV {
			rk = azureName(rk)
		}
		es.Spec.Data = append(es.Spec.Data, api.ExternalSecretData{SecretKey: env, RemoteRef: api.RemoteReference{Key: rk}})
		v := "{{ ." + env + " }}"
		if env == "RESTIC_REPOSITORY" {
			// The store holds the base repository of the Swarm; each claim
			// has its own repository below it, like volume-backup writes.
			v = `{{ .RESTIC_REPOSITORY | trimSuffix "/" }}/` + ns + "/" + name
		}
		es.Spec.Target.Template.Data[env] = v
		if !contains(e.volsyncKeys[ns], rk) {
			e.volsyncKeys[ns] = append(e.volsyncKeys[ns], rk)
		}
	}
	trigger := map[string]any{"manual": "initial"}
	switch {
	case e.o.VolSyncCutover:
		trigger = map[string]any{"manual": VolSyncFinal}
	case e.o.VolSyncRestoreFirst:
	case e.restoreSchedule[ns+"/"+name] != "":
		trigger = map[string]any{"schedule": e.restoreSchedule[ns+"/"+name]}
	case e.o.VolSyncSchedule != "":
		trigger = map[string]any{"schedule": e.o.VolSyncSchedule}
	}
	rd := map[string]any{
		"apiVersion": "volsync.backube/v1alpha1",
		"kind":       "ReplicationDestination",
		"metadata":   metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: ignore},
		"spec": map[string]any{
			// Patch it outside apps/ to restore again, for example to a new
			// manual value for the final copy at cutover.
			"trigger": trigger,
			"restic": map[string]any{
				"repository":         secret,
				"destinationPVC":     name,
				"copyMethod":         "Direct",
				"enableFileDeletion": true,
			},
		},
	}
	dir := e.nsDir(ns)
	e.add(dir+"/externalsecret-"+secret+".yaml", es)
	e.add(dir+"/replicationdestination-"+name+".yaml", rd)
	hold := " Workloads that mount a claim are held at 0 replicas on Kubernetes (annotation " + api.AnnotationHeld + " keeps the Swarm count), and Jobs are suspended (annotation " + api.AnnotationHeldJob + "), until they are cut over with `import --migrate`."
	if e.o.VolSyncRestoreFirst || e.o.VolSyncCutover {
		hold = ""
	}
	e.notes = appendOnce(e.notes, "Every PersistentVolumeClaim has a VolSync ReplicationDestination that restores the newest restic snapshot written by `flux4swarm volume-backup`; store the base repository, password and credentials under the "+e.o.SecretKeyPrefix+VolSyncRemoteKey+"* keys of the secret store."+hold)
}

// restoreSchedule is the restore cron the labels of a service ask for:
// api.LabelVolumeRestoreSchedule, else a cron api.LabelVolumeSyncSchedule
// (restores then run at the same times and get the previous round, unless
// the restore label shifts them after the backups).
func restoreSchedule(labels map[string]string) string {
	if v := strings.TrimSpace(labels[api.LabelVolumeRestoreSchedule]); v != "" {
		return v
	}
	v := strings.TrimSpace(labels[api.LabelVolumeSyncSchedule])
	if _, err := time.ParseDuration(v); err == nil || v == "" {
		return ""
	}
	return v
}

// WorkloadName returns the namespace and name of the Kubernetes workload
// that import writes for Swarm service s.
func WorkloadName(s swarm.Service) (ns, name string) {
	ns, short := splitNS(s.Spec.Name, s.Spec.Labels)
	return ns, dnsName(short)
}

// splitData moves, for VolSyncRestoreFirst, the claims, their restores and
// repository secrets to DataDir, with the namespaces and secret stores
// they need. Flux applies DataDir before apps/, so those move too.
func (e *exporter) splitData() {
	appsRoot := e.o.Path + "/" + AppsDir + "/"
	dataRoot := e.o.Path + "/" + DataDir + "/"
	claims := map[string]bool{} // namespaces with claims
	for f := range e.files {
		rel, ok := strings.CutPrefix(f, appsRoot)
		if !ok {
			continue
		}
		if ns, base, ok := strings.Cut(rel, "/"); ok && strings.HasPrefix(base, "persistentvolumeclaim-") {
			claims[ns] = true
		}
	}
	for f, objs := range e.files {
		rel, ok := strings.CutPrefix(f, appsRoot)
		if !ok {
			continue
		}
		ns, base, nested := strings.Cut(rel, "/")
		move := false
		switch {
		case !nested:
			move = base == "clustersecretstore.yaml" && len(claims) > 0
		case !claims[ns] || strings.Contains(base, "/"):
		case base == "namespace.yaml", base == "secretstore.yaml",
			strings.HasPrefix(base, "persistentvolumeclaim-"), strings.HasPrefix(base, "replicationdestination-"),
			strings.HasPrefix(base, "externalsecret-") && strings.HasSuffix(base, "-volsync-restic.yaml"):
			move = true
		}
		if move {
			delete(e.files, f)
			e.files[dataRoot+rel] = objs
		}
	}
	if len(claims) > 0 {
		e.notes = appendOnce(e.notes, "--volsync-restore-first: the claims and their restores are in "+dataRoot+"; "+e.o.Path+"/"+LayersFile+" makes Flux apply them first and start "+appsRoot+" only once every restore has run. Rebuild the cluster (or delete and re-create the ReplicationDestinations) to restore newer data.")
	}
}

// layers writes the cluster's kustomization.yaml and LayersFile for
// VolSyncRestoreFirst: Flux Kustomizations data (wait for the restores) and
// apps (dependsOn data). The root Kustomization then reads only those.
func (e *exporter) layers() error {
	src := api.CrossNamespaceObjectReference{Kind: api.KindGitRepository, Name: "flux-system"}
	timeout := api.Duration{Duration: 12 * time.Hour}
	appsTimeout := api.Duration{Duration: 10 * time.Minute}
	ignore := map[string]string{api.AnnotationIgnore: "true"}
	data := map[string]any{
		"apiVersion": api.KustomizeGroupVersion, "kind": api.KindKustomization,
		"metadata": metav1.ObjectMeta{Name: "data", Namespace: api.DefaultNamespace, Annotations: ignore},
		"spec": map[string]any{
			// Never prune: these are the claims with the data.
			"interval": "10m", "path": "./" + e.o.Path + "/" + DataDir, "prune": false, "wait": true, "timeout": timeout,
			"sourceRef": src,
			// Ready once every restore has run once.
			"healthCheckExprs": []map[string]any{{
				"apiVersion": "volsync.backube/v1alpha1", "kind": "ReplicationDestination",
				"current": "has(status.lastManualSync) && status.lastManualSync == spec.trigger.manual",
				"failed":  "has(status.latestMoverStatus) && status.latestMoverStatus.result == 'Failed' && has(status.lastManualSync) && status.lastManualSync == spec.trigger.manual",
			}},
		},
	}
	apps := &api.Kustomization{
		TypeMeta: api.TypeMeta{APIVersion: api.KustomizeGroupVersion, Kind: api.KindKustomization},
		Metadata: api.ObjectMeta{Name: "apps", Namespace: api.DefaultNamespace},
		Spec: api.KustomizationSpec{
			Interval: api.Duration{Duration: 10 * time.Minute}, Path: "./" + e.o.Path + "/" + AppsDir,
			Prune: true, Wait: true, Timeout: &appsTimeout, SourceRef: src,
			DependsOn: []api.DependencyReference{{Name: "data"}},
		},
	}
	// Both are for Kubernetes: data carries the ignore annotation like the
	// objects in it.
	if err := e.render(e.o.Path+"/"+LayersFile, data, apps); err != nil {
		return err
	}
	res := []string{LayersFile}
	if e.o.Bootstrap {
		res = append([]string{"flux-system"}, res...)
	}
	return e.render(e.o.Path+"/kustomization.yaml", kustomization(res))
}
