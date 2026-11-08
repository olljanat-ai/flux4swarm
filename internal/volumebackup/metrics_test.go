// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package volumebackup

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const syncLog = `Fatal: unable to open config file
flux4swarm-sync {"round":1,"result":"ok","ok":1,"failed":0,"start":100,"end":110,"summary":{"message_type":"summary","files_new":5,"files_changed":0,"files_unmodified":0,"data_added":2048,"data_added_packed":1024,"total_files_processed":5,"total_bytes_processed":4096,"total_duration":9.5}}
=== something else
flux4swarm-sync {"round":2,"result":"error","ok":1,"failed":1,"start":170,"end":171,"summary":null}
`

func TestParseStatus(t *testing.T) {
	last, ok := ParseStatus([]byte(syncLog))
	if last == nil || last.Round != 2 || last.Result != "error" || ok == nil || ok.Round != 1 || ok.Summary.DataAdded != 2048 {
		t.Errorf("last %+v, ok %+v", last, ok)
	}
	if last, ok := ParseStatus([]byte("nothing\n")); last != nil || ok != nil {
		t.Error("parsed a status from nothing")
	}
}

type logFake struct{ fake }

func (f *logFake) ServiceLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	var b bytes.Buffer
	_, _ = stdcopy.NewStdWriter(&b, stdcopy.Stdout).Write([]byte(syncLog))
	return io.NopCloser(&b), nil
}

func TestCollector(t *testing.T) {
	tg := &Target{Volume: "shop_db", Namespace: "shop", Claim: "db", Mount: mount.Mount{Type: mount.TypeVolume, Source: "shop_db", Target: "/db"}, Node: "n1"}
	spec := SyncSpec(tg, DefaultImage, "sec", "secname", nil, time.Minute, "", 4)
	f := &logFake{fake{services: []swarm.Service{{ID: "sync", Spec: spec}}, tasks: map[string][]swarm.Task{"sync": {task("n1", swarm.TaskStateRunning, 0)}}}}
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(Collector{Client: f})
	want := `
# HELP flux4swarm_volume_sync_last_added_bytes Data the last successful round added to the repository, before compression (the increment).
# TYPE flux4swarm_volume_sync_last_added_bytes gauge
flux4swarm_volume_sync_last_added_bytes{claim="db",namespace="shop",volume="shop_db"} 2048
# HELP flux4swarm_volume_sync_last_round_success 1 when the last backup round succeeded.
# TYPE flux4swarm_volume_sync_last_round_success gauge
flux4swarm_volume_sync_last_round_success{claim="db",namespace="shop",volume="shop_db"} 0
# HELP flux4swarm_volume_sync_last_success_timestamp_seconds Unix time the last successful backup round ended.
# TYPE flux4swarm_volume_sync_last_success_timestamp_seconds gauge
flux4swarm_volume_sync_last_success_timestamp_seconds{claim="db",namespace="shop",volume="shop_db"} 110
# HELP flux4swarm_volume_sync_rounds_total Backup rounds since the sync task started, by result (ok, error).
# TYPE flux4swarm_volume_sync_rounds_total counter
flux4swarm_volume_sync_rounds_total{claim="db",namespace="shop",result="error",volume="shop_db"} 1
flux4swarm_volume_sync_rounds_total{claim="db",namespace="shop",result="ok",volume="shop_db"} 1
# HELP flux4swarm_volume_sync_up 1 when the sync service has a running task.
# TYPE flux4swarm_volume_sync_up gauge
flux4swarm_volume_sync_up{claim="db",namespace="shop",volume="shop_db"} 1
# HELP flux4swarm_volume_sync_interval_seconds Backup interval of the sync service (volume-backup --every).
# TYPE flux4swarm_volume_sync_interval_seconds gauge
flux4swarm_volume_sync_interval_seconds{claim="db",namespace="shop",volume="shop_db"} 60
# HELP flux4swarm_volume_sync_volume_bytes Bytes in the volume at the last successful round.
# TYPE flux4swarm_volume_sync_volume_bytes gauge
flux4swarm_volume_sync_volume_bytes{claim="db",namespace="shop",volume="shop_db"} 4096
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"flux4swarm_volume_sync_last_added_bytes", "flux4swarm_volume_sync_last_round_success", "flux4swarm_volume_sync_last_success_timestamp_seconds",
		"flux4swarm_volume_sync_rounds_total", "flux4swarm_volume_sync_up", "flux4swarm_volume_sync_interval_seconds", "flux4swarm_volume_sync_volume_bytes"); err != nil {
		t.Error(err)
	}
	if n, err := testutil.GatherAndCount(reg); err != nil || n < 12 {
		t.Errorf("count %d, %v", n, err)
	}
}
