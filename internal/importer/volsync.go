// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"strings"
	"time"

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
	if e.o.VolSyncSchedule != "" {
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
	e.notes = appendOnce(e.notes, "Every PersistentVolumeClaim has a VolSync ReplicationDestination that restores the newest restic snapshot written by `flux4swarm volume-backup`; store the base repository, password and credentials under the "+e.o.SecretKeyPrefix+VolSyncRemoteKey+"* keys of the secret store, and keep the workload at 0 replicas on Kubernetes until the restore has run.")
}
