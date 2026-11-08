// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"context"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/swarm"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

func TestExportVolSync(t *testing.T) {
	res, err := Export(context.Background(), stackService(), Options{Cluster: "prod", VolSync: true,
		SecretStore: "fake", SecretStoreProvider: ProviderFake, SecretKeyPrefix: "prod-"})
	if err != nil {
		t.Fatal(err)
	}
	dir := "clusters/prod/apps/shop/"
	rd := string(res.Files[dir+"replicationdestination-data.yaml"])
	for _, want := range []string{
		"apiVersion: volsync.backube/v1alpha1", "kind: ReplicationDestination", "name: data",
		"swarm.flux4swarm.io/ignore: \"true\"", "destinationPVC: data", "repository: data-volsync-restic",
		"copyMethod: Direct", "enableFileDeletion: true", "manual: initial",
	} {
		if !strings.Contains(rd, want) {
			t.Errorf("ReplicationDestination misses %q:\n%s", want, rd)
		}
	}
	es := string(res.Files[dir+"externalsecret-data-volsync-restic.yaml"])
	for _, want := range []string{
		"name: data-volsync-restic", "swarm.flux4swarm.io/ignore: \"true\"", "engineVersion: v2",
		"key: prod-volsync-restic-repository", "key: prod-volsync-restic-password", "key: prod-volsync-aws-access-key-id",
		`RESTIC_REPOSITORY: '{{ .RESTIC_REPOSITORY | trimSuffix "/" }}/shop/data'`,
		"RESTIC_PASSWORD: '{{ .RESTIC_PASSWORD }}'",
	} {
		if !strings.Contains(es, want) {
			t.Errorf("ExternalSecret misses %q:\n%s", want, es)
		}
	}
	if st := string(res.Files[dir+"secretstore.yaml"]); !strings.Contains(st, "key: prod-volsync-restic-password") {
		t.Errorf("fake SecretStore misses the VolSync keys:\n%s", st)
	}
	if k := string(res.Files[dir+"kustomization.yaml"]); !strings.Contains(k, "replicationdestination-data.yaml") {
		t.Errorf("kustomization misses the ReplicationDestination:\n%s", k)
	}
	// Without --volsync nothing changes.
	res, err = Export(context.Background(), stackService(), Options{Cluster: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	for f := range res.Files {
		if strings.Contains(f, "volsync") || strings.Contains(f, "replicationdestination") {
			t.Errorf("unexpected %s without VolSync", f)
		}
	}
}

func TestExportSkipsVolumeBackupObjects(t *testing.T) {
	d := stackService()
	l := map[string]string{api.LabelVolumeBackup: "shop_data"}
	d.services = append(d.services, swarm.Service{ID: "job", Spec: swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: "flux4swarm-backup-shop-data", Labels: l},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "restic/restic"}},
	}})
	d.secrets = append(d.secrets, swarm.Secret{ID: "jobsec", Spec: swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: "flux4swarm-backup-shop-data-1234", Labels: l}}})
	res, err := Export(context.Background(), d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range res.Skipped {
		if s.Reason == SkippedBackup {
			n++
		}
	}
	if n != 2 {
		t.Errorf("skipped %v, want the backup job and its secret", res.Skipped)
	}
	for f := range res.Files {
		if strings.Contains(f, "backup") {
			t.Errorf("exported %s", f)
		}
	}
}

func TestVolumeClaim(t *testing.T) {
	stack := map[string]string{api.LabelStackNamespace: "shop"}
	svc := swarm.Service{Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "shop_db", Labels: stack}}}
	for _, c := range []struct {
		source, ns, claim string
		ok                bool
	}{
		{"shop_db", "shop", "db", true},
		{"shop_db_data", "", "", false}, // "_" is not valid in a claim name
		{"legacy-data", "", "", false},  // external volume of another stack
		{"shop_db-{{.Task.Slot}}", "", "", false},
	} {
		ns, claim, ok := VolumeClaim(svc, c.source)
		if !ok {
			ns = ""
		}
		if ok != c.ok || ns != c.ns || claim != c.claim {
			t.Errorf("VolumeClaim(%q) = %q, %q, %v; want %q, %q, %v", c.source, ns, claim, ok, c.ns, c.claim, c.ok)
		}
	}
}

func TestExportVolSyncSchedule(t *testing.T) {
	res, err := Export(context.Background(), stackService(), Options{Cluster: "prod", VolSync: true, VolSyncSchedule: "*/15 * * * *"})
	if err != nil {
		t.Fatal(err)
	}
	rd := string(res.Files["clusters/prod/apps/shop/replicationdestination-data.yaml"])
	if !strings.Contains(rd, "schedule: '*/15 * * * *'") || strings.Contains(rd, "manual:") {
		t.Errorf("ReplicationDestination:\n%s", rd)
	}
}

func TestExportVolSyncHoldsStatefulWorkloads(t *testing.T) {
	res, err := Export(context.Background(), stackService(), Options{Cluster: "prod", VolSync: true})
	if err != nil {
		t.Fatal(err)
	}
	dep := string(res.Files["clusters/prod/apps/shop/deployment-web.yaml"])
	if !strings.Contains(dep, "replicas: 0") || !strings.Contains(dep, `flux4swarm.io/held-replicas: "2"`) || !strings.Contains(dep, `Replicas":2`) {
		t.Errorf("Deployment not held at 0 with the Swarm replicas kept:\n%s", dep)
	}
	if pvc := string(res.Files["clusters/prod/apps/shop/persistentvolumeclaim-data.yaml"]); !strings.Contains(pvc, "kustomize.toolkit.fluxcd.io/prune: disabled") {
		t.Errorf("PVC can be pruned:\n%s", pvc)
	}
	if len(res.Volumes) != 1 || res.Volumes[0] != "shop_data" {
		t.Errorf("Volumes = %v", res.Volumes)
	}
	// The cutover export restores once more and does not hold.
	res, err = Export(context.Background(), stackService(), Options{Cluster: "prod", VolSync: true, VolSyncCutover: true})
	if err != nil {
		t.Fatal(err)
	}
	if dep := string(res.Files["clusters/prod/apps/shop/deployment-web.yaml"]); !strings.Contains(dep, "replicas: 2") {
		t.Errorf("cutover Deployment:\n%s", dep)
	}
	if rd := string(res.Files["clusters/prod/apps/shop/replicationdestination-data.yaml"]); !strings.Contains(rd, "manual: final") {
		t.Errorf("cutover ReplicationDestination:\n%s", rd)
	}
}

func TestExportSkipsMigrated(t *testing.T) {
	res, err := Export(context.Background(), stackService(), Options{Cluster: "prod", Migrated: map[string]bool{"shop_web": true}})
	if err != nil {
		t.Fatal(err)
	}
	for f := range res.Files {
		if strings.HasPrefix(f, "clusters/prod/apps/shop/") && !strings.HasSuffix(f, "/namespace.yaml") && !strings.HasSuffix(f, "/kustomization.yaml") && !strings.HasSuffix(f, "/secretstore.yaml") {
			t.Errorf("migrated service still exports %s", f)
		}
	}
	if _, ok := res.Files["clusters/prod/apps/shop/namespace.yaml"]; !ok {
		t.Error("namespace of a migrated service dropped")
	}
	found := false
	for _, s := range res.Skipped {
		found = found || (s.Name == "shop_web" && s.Reason == SkippedMigrated)
	}
	if !found {
		t.Errorf("skipped %v", res.Skipped)
	}
}

func TestExportVolSyncRestoreFirst(t *testing.T) {
	res, err := Export(context.Background(), stackService(), Options{Cluster: "nightly", VolSyncRestoreFirst: true, Bootstrap: true,
		SecretStore: "fake", SecretStoreProvider: ProviderFake})
	if err != nil {
		t.Fatal(err)
	}
	data, apps := "clusters/nightly/data/shop/", "clusters/nightly/apps/shop/"
	for _, f := range []string{"namespace.yaml", "secretstore.yaml", "persistentvolumeclaim-data.yaml", "replicationdestination-data.yaml", "externalsecret-data-volsync-restic.yaml", "kustomization.yaml"} {
		if _, ok := res.Files[data+f]; !ok {
			t.Errorf("no %s%s", data, f)
		}
		if _, ok := res.Files[apps+f]; ok && f != "kustomization.yaml" {
			t.Errorf("%s%s still in apps", apps, f)
		}
	}
	if rd := string(res.Files[data+"replicationdestination-data.yaml"]); !strings.Contains(rd, "manual: initial") {
		t.Errorf("not a one-off restore:\n%s", rd)
	}
	var dep string
	for f, b := range res.Files {
		if strings.HasPrefix(f, apps+"deployment-") || strings.HasPrefix(f, apps+"statefulset-") {
			dep = string(b)
		}
	}
	if dep == "" || strings.Contains(dep, "replicas: 0") || strings.Contains(dep, api.AnnotationHeld) {
		t.Errorf("workload held or missing:\n%s", dep)
	}
	if k := string(res.Files["clusters/nightly/data/kustomization.yaml"]); !strings.Contains(k, "- shop") {
		t.Errorf("data kustomization:\n%s", k)
	}
	if k := string(res.Files["clusters/nightly/apps/kustomization.yaml"]); strings.Contains(k, "data") {
		t.Errorf("apps kustomization lists data:\n%s", k)
	}
	l := string(res.Files["clusters/nightly/"+LayersFile])
	for _, want := range []string{"name: data", "path: ./clusters/nightly/data", "prune: false", "status.lastManualSync == spec.trigger.manual",
		"name: apps", "path: ./clusters/nightly/apps", "dependsOn:"} {
		if !strings.Contains(l, want) {
			t.Errorf("%s misses %q:\n%s", LayersFile, want, l)
		}
	}
	if k := string(res.Files["clusters/nightly/kustomization.yaml"]); !strings.Contains(k, "- flux-system") || !strings.Contains(k, "- "+LayersFile) {
		t.Errorf("root kustomization:\n%s", k)
	}
	if _, err := Export(context.Background(), stackService(), Options{VolSyncRestoreFirst: true, VolSyncSchedule: "* * * * *"}); err == nil {
		t.Error("no error with a schedule")
	}
	root := t.TempDir()
	if err := res.WriteTo(root, false); err != nil {
		t.Fatal(err)
	}
	if err := Validate(root, res.Root); err != nil {
		t.Fatal(err)
	}
}
