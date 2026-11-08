// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package volumebackup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

// SyncStatus is the line a sync service logs after every round.
type SyncStatus struct {
	Round  int    `json:"round"`
	Result string `json:"result"`
	// OK and Failed count the rounds since the task started.
	OK     int   `json:"ok"`
	Failed int   `json:"failed"`
	Start  int64 `json:"start"`
	End    int64 `json:"end"`
	// Summary is restic's backup summary; nil when the round failed.
	Summary *struct {
		FilesNew            int64   `json:"files_new"`
		FilesChanged        int64   `json:"files_changed"`
		FilesUnmodified     int64   `json:"files_unmodified"`
		DataAdded           int64   `json:"data_added"`
		DataAddedPacked     int64   `json:"data_added_packed"`
		TotalFilesProcessed int64   `json:"total_files_processed"`
		TotalBytesProcessed int64   `json:"total_bytes_processed"`
		TotalDuration       float64 `json:"total_duration"`
		SnapshotID          string  `json:"snapshot_id"`
	} `json:"summary"`
}

// ParseStatus returns the newest round and the newest successful round in
// the output of a sync service.
func ParseStatus(out []byte) (last, lastOK *SyncStatus) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		_, js, ok := strings.Cut(sc.Text(), StatusPrefix)
		if !ok {
			continue
		}
		st := &SyncStatus{}
		if json.Unmarshal([]byte(js), st) != nil {
			continue
		}
		if last == nil || st.End >= last.End {
			last = st
		}
		if st.Result == "ok" && (lastOK == nil || st.End >= lastOK.End) {
			lastOK = st
		}
	}
	return last, lastOK
}

var (
	syncLabels   = []string{"volume", "namespace", "claim"}
	syncInfoDesc = prometheus.NewDesc("flux4swarm_volume_sync_info",
		"Always 1, one per sync service.", append(syncLabels, "service", "node"), nil)
	syncUpDesc = prometheus.NewDesc("flux4swarm_volume_sync_up",
		"1 when the sync service has a running task.", syncLabels, nil)
	syncIntervalDesc = prometheus.NewDesc("flux4swarm_volume_sync_interval_seconds",
		"Backup interval of the sync service (volume-backup --every).", syncLabels, nil)
	syncRoundsDesc = prometheus.NewDesc("flux4swarm_volume_sync_rounds_total",
		"Backup rounds since the sync task started, by result (ok, error).", append(syncLabels, "result"), nil)
	syncLastRunDesc = prometheus.NewDesc("flux4swarm_volume_sync_last_run_timestamp_seconds",
		"Unix time the last backup round ended.", syncLabels, nil)
	syncLastSuccessDesc = prometheus.NewDesc("flux4swarm_volume_sync_last_success_timestamp_seconds",
		"Unix time the last successful backup round ended.", syncLabels, nil)
	syncLastResultDesc = prometheus.NewDesc("flux4swarm_volume_sync_last_round_success",
		"1 when the last backup round succeeded.", syncLabels, nil)
	syncDurationDesc = prometheus.NewDesc("flux4swarm_volume_sync_last_duration_seconds",
		"Duration of the last successful backup round.", syncLabels, nil)
	syncAddedDesc = prometheus.NewDesc("flux4swarm_volume_sync_last_added_bytes",
		"Data the last successful round added to the repository, before compression (the increment).", syncLabels, nil)
	syncAddedPackedDesc = prometheus.NewDesc("flux4swarm_volume_sync_last_added_packed_bytes",
		"Data the last successful round added to the repository, as stored.", syncLabels, nil)
	syncSizeDesc = prometheus.NewDesc("flux4swarm_volume_sync_volume_bytes",
		"Bytes in the volume at the last successful round.", syncLabels, nil)
	syncFilesDesc = prometheus.NewDesc("flux4swarm_volume_sync_last_files",
		"Files of the last successful round, by state (new, changed, unmodified).", append(syncLabels, "state"), nil)
)

// Collector exports the state of the sync services on the Swarm, read from
// Docker at scrape time: their tasks and the status line each round logs.
type Collector struct {
	Client Client
	// Timeout bounds one scrape (default 10s).
	Timeout time.Duration
}

// Describe implements prometheus.Collector.
func (Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{syncInfoDesc, syncUpDesc, syncIntervalDesc, syncRoundsDesc, syncLastRunDesc,
		syncLastSuccessDesc, syncLastResultDesc, syncDurationDesc, syncAddedDesc, syncAddedPackedDesc, syncSizeDesc, syncFilesDesc} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c Collector) Collect(ch chan<- prometheus.Metric) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	services, err := c.Client.ServiceList(ctx, swarm.ServiceListOptions{Filters: filters.NewArgs(filters.Arg("label", labelSync))})
	if err != nil {
		ch <- prometheus.NewInvalidMetric(syncUpDesc, err)
		return
	}
	for _, s := range services {
		l := s.Spec.Labels
		if l[labelSync] == "" {
			continue
		}
		lv := []string{l[api.LabelVolumeBackup], l[labelNamespace], l[labelClaim]}
		g := func(d *prometheus.Desc, v float64, extra ...string) {
			ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, append(append([]string(nil), lv...), extra...)...)
		}
		node, up := "", 0.0
		if tasks, err := c.Client.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", s.ID))}); err == nil {
			for _, t := range tasks {
				if t.Status.State == swarm.TaskStateRunning {
					up, node = 1, t.NodeID
				}
			}
		}
		if node == "" {
			node = pinnedNode(s.Spec)
		}
		g(syncInfoDesc, 1, s.Spec.Name, node)
		g(syncUpDesc, up)
		if secs, err := strconv.ParseFloat(l[labelEvery], 64); err == nil {
			g(syncIntervalDesc, secs)
		}
		last, ok := c.status(ctx, s.ID)
		if last == nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(syncRoundsDesc, prometheus.CounterValue, float64(last.OK), append(append([]string(nil), lv...), "ok")...)
		ch <- prometheus.MustNewConstMetric(syncRoundsDesc, prometheus.CounterValue, float64(last.Failed), append(append([]string(nil), lv...), "error")...)
		g(syncLastRunDesc, float64(last.End))
		success := 0.0
		if last.Result == "ok" {
			success = 1
		}
		g(syncLastResultDesc, success)
		if ok == nil {
			continue
		}
		g(syncLastSuccessDesc, float64(ok.End))
		d := float64(ok.End - ok.Start)
		if ok.Summary != nil && ok.Summary.TotalDuration > 0 {
			d = ok.Summary.TotalDuration
		}
		g(syncDurationDesc, d)
		if sm := ok.Summary; sm != nil {
			g(syncAddedDesc, float64(sm.DataAdded))
			g(syncAddedPackedDesc, float64(sm.DataAddedPacked))
			g(syncSizeDesc, float64(sm.TotalBytesProcessed))
			g(syncFilesDesc, float64(sm.FilesNew), "new")
			g(syncFilesDesc, float64(sm.FilesChanged), "changed")
			g(syncFilesDesc, float64(sm.FilesUnmodified), "unmodified")
		}
	}
}

// status reads the status lines of a sync service from its logs.
func (c Collector) status(ctx context.Context, id string) (last, ok *SyncStatus) {
	r, err := c.Client.ServiceLogs(ctx, id, container.LogsOptions{ShowStdout: true, Tail: "200"})
	if err != nil {
		return nil, nil
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return nil, nil
	}
	// The stream is multiplexed unless the service has a TTY.
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, bytes.NewReader(raw)); err != nil {
		return ParseStatus(raw)
	}
	return ParseStatus(out.Bytes())
}
