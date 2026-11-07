// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
)

func TestExportReport(t *testing.T) {
	one, yes := uint64(1), true
	d := stackService()
	cs := d.services[0].Spec.TaskTemplate.ContainerSpec
	cs.Env = append(cs.Env, "DB_PASSWORD=hunter2", "DB_PASSWORD_FILE=/run/secrets/dbpw", "DATABASE_URL=postgres://app:s3cret@db/app")
	cs.Mounts = append(cs.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"})
	d.services[0].Spec.TaskTemplate.Placement.Constraints = append(d.services[0].Spec.TaskTemplate.Placement.Constraints,
		"node.labels.zone == a", "node.id != abc")
	d.services[0].Spec.EndpointSpec.Ports = append(d.services[0].Spec.EndpointSpec.Ports,
		swarm.PortConfig{Protocol: "udp", TargetPort: 53, PublishedPort: 53, PublishMode: swarm.PortConfigPublishModeHost})
	d.services = append(d.services, swarm.Service{ID: "svc2", Spec: swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "tools"},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "busybox", Init: &yes},
			LogDriver: &swarm.Driver{Name: "gelf"}},
		UpdateConfig:   &swarm.UpdateConfig{Parallelism: 1, FailureAction: "pause", Monitor: 5e9, Order: "stop-first"},
		RollbackConfig: &swarm.UpdateConfig{Parallelism: 1, FailureAction: "pause", Monitor: 5e9, Order: "stop-first"},
		Mode:           swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}},
	}})

	res, err := Export(context.Background(), d, Options{Cluster: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	md := string(res.Files["clusters/prod/"+ReportFile])
	if os.Getenv("REPORT_PRINT") != "" {
		t.Log("\n" + md)
	}
	for _, want := range []string{
		"# Kubernetes migration readiness: clusters/prod",
		"**1 of 2 services have no blockers.**",
		"| Deployment default/tools | `tools` | 0 | 2 |",
		"**check:** logs with the Docker `gelf` log driver",
		"ignored on Kubernetes: `TaskTemplate.ContainerSpec.Init`\n",
		"## Deployment shop/web",
		"**blocker:** mounts the Docker socket `/var/run/docker.sock`",
		"**blocker:** bind mount `/srv/logs` → `/logs`",
		"**blocker:** port 53/udp is published in host mode",
		"**blocker:** placement constraint `node.id!=abc`",
		"**blocker:** environment `DB_PASSWORD`, `DATABASE_URL` look like plain text credentials",
		"**check:** port 8080/tcp is published on the routing mesh",
		"**check:** needs Kubernetes nodes labelled `zone=a`",
		"**check:** runs on managers only",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report misses %q:\n%s", want, md)
		}
	}
	for _, leak := range []string{"hunter2", "s3cret", "DB_PASSWORD_FILE"} {
		if strings.Contains(md, leak) {
			t.Errorf("report contains %q", leak)
		}
	}
	if res.Report == nil || len(res.Report.Services) != 2 {
		t.Fatalf("Report = %+v", res.Report)
	}

	// The report is owned by the export: watch mode rewrites it.
	dir := t.TempDir()
	p := filepath.Join(dir, "clusters", "prod", ReportFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := res.WriteTo(dir, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != md {
		t.Errorf("watch mode did not rewrite the report: %q", b)
	}

	// Deterministic, so an unchanged Swarm gives no commit.
	again, err := Export(context.Background(), d, Options{Cluster: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if again.Hash() != res.Hash() {
		t.Error("export with report is not deterministic")
	}
}
