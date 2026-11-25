// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package kubebackup

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/importer"
)

// fake is an in-memory cluster; ReplicationSources with a manual trigger
// complete on the next Get, like a fast VolSync.
type fake struct {
	mu      sync.Mutex
	objs    map[string]map[string]any // Kind/ns/name
	patches []string
	fail    bool
}

func key(r importer.KubeResource, ns, name string) string { return r.Kind + "/" + ns + "/" + name }

func (f *fake) List(_ context.Context, r importer.KubeResource, ns string) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for k, o := range f.objs {
		if strings.HasPrefix(k, r.Kind+"/") && (ns == "" || strings.HasPrefix(k, r.Kind+"/"+ns+"/")) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fake) Get(_ context.Context, r importer.KubeResource, ns, name string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.objs[key(r, ns, name)]
	if o != nil && r.Kind == ReplicationSource.Kind {
		spec := o["spec"].(map[string]any)
		if m, ok := spec["trigger"].(map[string]any)["manual"]; ok {
			result := "Successful"
			if f.fail {
				result = "Failed"
			}
			o["status"] = map[string]any{"lastManualSync": m, "lastSyncDuration": "1m2s",
				"latestMoverStatus": map[string]any{"result": result, "logs": "repository locked"}}
		}
	}
	return o, nil
}

func (f *fake) Apply(_ context.Context, r importer.KubeResource, obj map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := obj["metadata"].(map[string]any)
	f.objs[key(r, m["namespace"].(string), m["name"].(string))] = obj
	return nil
}

func (f *fake) MergePatch(_ context.Context, r importer.KubeResource, ns, name string, patch map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches = append(f.patches, key(r, ns, name))
	o := f.objs[key(r, ns, name)]
	o["spec"].(map[string]any)["replicas"] = int64(0)
	// The pods of the workload go away.
	for k := range f.objs {
		if strings.HasPrefix(k, "Pod/"+ns+"/"+name+"-") {
			delete(f.objs, k)
		}
	}
	return nil
}

func (f *fake) Delete(_ context.Context, r importer.KubeResource, ns, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objs, key(r, ns, name))
	return nil
}

func cluster() *fake {
	claim := func(ns, name string) map[string]any {
		return map[string]any{"metadata": map[string]any{"namespace": ns, "name": name}, "status": map[string]any{"phase": "Bound"}}
	}
	vol := func(claim string) []any {
		return []any{map[string]any{"name": "d", "persistentVolumeClaim": map[string]any{"claimName": claim}}}
	}
	return &fake{objs: map[string]map[string]any{
		"PersistentVolumeClaim/shop/data":         claim("shop", "data"),
		"PersistentVolumeClaim/shop/db-pg-0":      claim("shop", "db-pg-0"),
		"PersistentVolumeClaim/kube-system/etcd":  claim("kube-system", "etcd"),
		"PersistentVolumeClaim/shop/volsync-tmp":  {"metadata": map[string]any{"namespace": "shop", "name": "volsync-tmp", "labels": map[string]any{"app.kubernetes.io/created-by": "volsync"}}},
		"PersistentVolumeClaim/shop/pending-data": {"metadata": map[string]any{"namespace": "shop", "name": "pending-data"}, "status": map[string]any{"phase": "Pending"}},
		"Deployment/shop/api": {"metadata": map[string]any{"namespace": "shop", "name": "api"},
			"spec": map[string]any{"replicas": int64(2), "template": map[string]any{"spec": map[string]any{"volumes": vol("data")}}}},
		"Deployment/shop/web": {"metadata": map[string]any{"namespace": "shop", "name": "web"},
			"spec": map[string]any{"replicas": int64(2), "template": map[string]any{"spec": map[string]any{}}}},
		"StatefulSet/shop/pg": {"metadata": map[string]any{"namespace": "shop", "name": "pg"},
			"spec": map[string]any{"replicas": int64(1), "template": map[string]any{"spec": map[string]any{}},
				"volumeClaimTemplates": []any{map[string]any{"metadata": map[string]any{"name": "db"}}}}},
		"Pod/shop/api-abc": {"metadata": map[string]any{"namespace": "shop", "name": "api-abc"}, "spec": map[string]any{"volumes": vol("data")}},
	}}
}

func opts() Options {
	return Options{Repository: "s3:https://s3.example.com/b/k8s/", Env: map[string]string{"RESTIC_PASSWORD": "pw", "AWS_ACCESS_KEY_ID": "id"},
		Poll: time.Millisecond, Now: func() time.Time { return time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC) }}
}

func TestBackupAllAndWait(t *testing.T) {
	f := cluster()
	o := opts()
	o.All, o.Wait = true, true
	res, err := Run(context.Background(), f, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Claim != "shop/data" || res[1].Claim != "shop/db-pg-0" {
		t.Fatalf("claims: %+v", res)
	}
	if res[0].Repository != "s3:https://s3.example.com/b/k8s/shop/data" || res[0].Duration != "1m2s" {
		t.Errorf("result: %+v", res[0])
	}
	sec := f.objs["Secret/shop/data-backup-restic"]
	sd := sec["stringData"].(map[string]any)
	if sd["RESTIC_REPOSITORY"] != res[0].Repository || sd["RESTIC_PASSWORD"] != "pw" || sd["AWS_ACCESS_KEY_ID"] != "id" {
		t.Errorf("secret: %v", sd)
	}
	rs := f.objs["ReplicationSource/shop/data-backup"]
	spec := rs["spec"].(map[string]any)
	if spec["sourcePVC"] != "data" || spec["trigger"].(map[string]any)["manual"] != "backup-20261125T010203Z" {
		t.Errorf("ReplicationSource: %v", spec)
	}
	if rs["metadata"].(map[string]any)["labels"].(map[string]any)[api.LabelManagedBy] != api.ManagedByValue {
		t.Error("not labelled, kube-import would export it")
	}
	if len(f.patches) != 0 {
		t.Errorf("workloads touched: %v", f.patches)
	}
}

func TestBackupSchedule(t *testing.T) {
	f := cluster()
	o := opts()
	o.Claims, o.Schedule, o.CopyMethod, o.Keep = []string{"shop/data"}, "*/15 * * * *", CopySnapshot, 7
	o.VolumeSnapshotClass = "csi"
	if _, err := Run(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	spec := f.objs["ReplicationSource/shop/data-backup"]["spec"].(map[string]any)
	restic := spec["restic"].(map[string]any)
	if spec["trigger"].(map[string]any)["schedule"] != "*/15 * * * *" || restic["copyMethod"] != CopySnapshot ||
		restic["volumeSnapshotClassName"] != "csi" || restic["retain"].(map[string]any)["last"] != int64(7) {
		t.Errorf("spec: %v", spec)
	}
	o.Delete = true
	if _, err := Run(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if f.objs["ReplicationSource/shop/data-backup"] != nil || f.objs["Secret/shop/data-backup-restic"] != nil {
		t.Error("not deleted")
	}
}

func TestBackupFinal(t *testing.T) {
	f := cluster()
	o := opts()
	o.Claims, o.Final = []string{"shop/data", "shop/db-pg-0"}, true
	res, err := Run(context.Background(), f, o)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.patches, ",")
	if !strings.Contains(got, "Deployment/shop/api") || !strings.Contains(got, "StatefulSet/shop/pg") || strings.Contains(got, "web") {
		t.Errorf("stopped: %s", got)
	}
	if res[0].Duration == "" || len(res[0].Stopped) != 1 {
		t.Errorf("result: %+v", res)
	}
	if _, err := Run(context.Background(), f, Options{Final: true, Schedule: "0 * * * *", Claims: []string{"shop/data"}}); err == nil {
		t.Error("final with schedule accepted")
	}
}

func TestBackupErrors(t *testing.T) {
	f := cluster()
	f.fail = true
	o := opts()
	o.Claims, o.Wait = []string{"shop/data"}, true
	if _, err := Run(context.Background(), f, o); err == nil || !strings.Contains(err.Error(), "repository locked") {
		t.Errorf("err = %v", err)
	}
	o.Claims = []string{"shop/missing"}
	if _, err := Run(context.Background(), f, o); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v", err)
	}
	o.Claims, o.Env = []string{"shop/data"}, nil
	if _, err := Run(context.Background(), f, o); err == nil || !strings.Contains(err.Error(), "RESTIC_PASSWORD") {
		t.Errorf("err = %v", err)
	}
}
