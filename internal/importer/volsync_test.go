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
