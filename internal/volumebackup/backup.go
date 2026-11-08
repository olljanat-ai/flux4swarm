// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package volumebackup copies the data of a Swarm volume to a restic
// repository in the layout VolSync's restic mover restores on Kubernetes.
//
// The data is read through Docker: a one-off replicated-job service mounts
// the same named volume, with the same driver and options as the service
// that uses it, read-only at /data. Docker therefore handles every volume
// driver (local, NFS/CIFS options, plugins, cluster volumes), and the job
// runs on the node that holds a local volume. The job runs
// `restic backup --host volsync .` in /data, which is exactly what VolSync
// itself does, so a VolSync ReplicationDestination restores it as its own.
package volumebackup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/importer"
)

// DefaultImage is the restic image the backup job runs.
const DefaultImage = "restic/restic:0.18.1"

// envFile is the Docker secret target with the repository settings.
const envFile = "restic.env"

// Client is the Docker API subset Backup uses.
type Client interface {
	ServiceList(ctx context.Context, options swarm.ServiceListOptions) ([]swarm.Service, error)
	ServiceCreate(ctx context.Context, service swarm.ServiceSpec, options swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error)
	ServiceRemove(ctx context.Context, serviceID string) error
	ServiceLogs(ctx context.Context, serviceID string, options container.LogsOptions) (io.ReadCloser, error)
	TaskList(ctx context.Context, options swarm.TaskListOptions) ([]swarm.Task, error)
	NodeList(ctx context.Context, options swarm.NodeListOptions) ([]swarm.Node, error)
	SecretCreate(ctx context.Context, secret swarm.SecretSpec) (swarm.SecretCreateResponse, error)
	SecretRemove(ctx context.Context, id string) error
}

// Options configures a backup.
type Options struct {
	// Repository is the base restic repository of the Swarm; each volume
	// gets <Repository>/<namespace>/<claim>, as the exported
	// ReplicationDestination expects.
	Repository string
	// Env holds RESTIC_PASSWORD and the other restic settings (credentials)
	// passed to the job. RESTIC_REPOSITORY is set from Repository.
	Env map[string]string
	// Image is the restic image (default DefaultImage).
	Image string
	// Node pins the job to this node ID or hostname instead of the node
	// that runs the service.
	Node string
	// Networks are attached to the job, e.g. to reach a repository server
	// that runs on the Swarm.
	Networks []string
	// Every, when set, starts a long-running sync service instead of a
	// one-off job: it backs up the volume, sleeps Every and repeats until it
	// is stopped (StopSync, or the next one-off Backup of the volume).
	Every time.Duration
	// Restore runs the job the other way round, for a rollback: it
	// restores the newest snapshot of the repository into the volume
	// (deleting files that are not in it). The services using the volume
	// must be stopped.
	Restore bool
	// Keep is how many snapshots a sync service keeps (default 24); every
	// Keep rounds the older ones are forgotten and pruned.
	Keep int
	// Timeout bounds the wait for the job (default 1h).
	Timeout time.Duration
	// Poll is the task polling interval (default 2s).
	Poll time.Duration
	// Logf prints progress; nil discards it.
	Logf func(format string, args ...any)
}

// Result describes a finished backup.
type Result struct {
	Volume     string
	Namespace  string
	Claim      string
	Repository string
	Node       string
	// Service is the sync service, when Options.Every is set.
	Service string
	// Running lists the services that used the volume while it was copied;
	// a copy of a running writer may be inconsistent.
	Running []string
}

// Target is what Plan finds out about a volume.
type Target struct {
	Volume    string
	Namespace string
	Claim     string
	// Mount is the service's mount of the volume, used as is at /data.
	Mount mount.Mount
	// Node is the node ID the job is pinned to ("" for any node).
	Node string
	// Services that mount the volume, Running those with running tasks.
	Services []string
	Running  []string
}

// Plan finds the service that mounts the named Swarm volume, the claim
// import exports for it, and the node that holds its data.
func Plan(ctx context.Context, c Client, volume, node string) (*Target, error) {
	services, err := c.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil {
		return nil, err
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Spec.Name < services[j].Spec.Name })
	t := &Target{Volume: volume}
	var users []swarm.Service
	for _, s := range services {
		if _, ok := s.Spec.Labels[api.LabelVolumeBackup]; ok {
			continue
		}
		cs := s.Spec.TaskTemplate.ContainerSpec
		if cs == nil {
			continue
		}
		for _, m := range cs.Mounts {
			if (m.Type == mount.TypeVolume || m.Type == mount.TypeCluster) && m.Source == volume {
				if len(users) == 0 {
					t.Mount = m
					ns, claim, ok := importer.VolumeClaim(s, volume)
					if !ok {
						return nil, fmt.Errorf("volume %s: import keeps no PersistentVolumeClaim for it (templated or external volume), so there is nothing to restore into", volume)
					}
					t.Namespace, t.Claim = ns, claim
				}
				users = append(users, s)
				t.Services = append(t.Services, s.Spec.Name)
				break
			}
		}
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("volume %s: no service mounts it", volume)
	}
	nodes := map[string]bool{}
	// last is the node of the newest task, for a stopped service: Swarm
	// keeps the recent tasks (task history), so this is where the data was
	// last written.
	var last swarm.Task
	for _, s := range users {
		tasks, err := c.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", s.ID))})
		if err != nil {
			return nil, err
		}
		running := false
		for _, tk := range tasks {
			if tk.NodeID == "" {
				continue
			}
			if tk.Status.State == swarm.TaskStateRunning {
				nodes[tk.NodeID] = true
				running = true
			}
			if tk.Status.Timestamp.After(last.Status.Timestamp) {
				last = tk
			}
		}
		if running {
			t.Running = append(t.Running, s.Spec.Name)
		}
	}
	if node != "" {
		t.Node = node
		return t, nil
	}
	if len(nodes) == 0 && last.NodeID != "" {
		nodes[last.NodeID] = true
	}
	if len(nodes) == 0 {
		// A service scaled to 0 has no tasks left: use its node
		// constraint, or the only node of the Swarm.
		if n := pinnedNode(users[0].Spec); n != "" {
			t.Node = n
			return t, nil
		}
		ns, err := c.NodeList(ctx, swarm.NodeListOptions{})
		if err != nil {
			return nil, err
		}
		if len(ns) == 1 {
			nodes[ns[0].ID] = true
		}
	}
	if !shared(t.Mount) {
		switch len(nodes) {
		case 0:
			return nil, fmt.Errorf("volume %s: %s has no tasks and no node constraint, so the node with the data is unknown; give --node", volume, strings.Join(t.Services, ", "))
		case 1:
		default:
			return nil, fmt.Errorf("volume %s: tasks run on %d nodes and each has its own copy of a local volume; give --node for the copy to keep", volume, len(nodes))
		}
	}
	for n := range nodes {
		// One node for local volumes; for shared ones any node that has
		// the driver, so stay where the service runs.
		if t.Node == "" || n < t.Node {
			t.Node = n
		}
	}
	return t, nil
}

// pinnedNode returns the node a node.id or node.hostname constraint pins
// the service to.
func pinnedNode(spec swarm.ServiceSpec) string {
	if spec.TaskTemplate.Placement == nil {
		return ""
	}
	for _, c := range spec.TaskTemplate.Placement.Constraints {
		c = strings.ReplaceAll(c, " ", "")
		for _, p := range []string{"node.id==", "node.hostname=="} {
			if strings.HasPrefix(c, p) {
				return strings.TrimPrefix(c, p)
			}
		}
	}
	return ""
}

// shared reports whether the volume's data is reachable from every node,
// as far as the mount tells: a plugin driver, local driver options (NFS,
// CIFS, ...) or a cluster volume.
func shared(m mount.Mount) bool {
	if m.Type == mount.TypeCluster {
		return true
	}
	if m.VolumeOptions == nil || m.VolumeOptions.DriverConfig == nil {
		return false
	}
	d := m.VolumeOptions.DriverConfig
	return (d.Name != "" && d.Name != "local") || d.Options["type"] != ""
}

// JobSpec returns the replicated-job service that backs up t once.
func JobSpec(t *Target, image, secretID, secretName string, networks []string) swarm.ServiceSpec {
	script := strings.Join([]string{
		"set -eu",
		"set -a; . /run/secrets/" + envFile + "; set +a",
		"restic cat config >/dev/null 2>&1 || restic init",
		"cd " + importer.VolSyncDataDir,
		backupCmd,
	}, "\n")
	one := uint64(1)
	spec := baseSpec(t, image, secretID, secretName, networks, jobName(t.Namespace, t.Claim), script)
	spec.Mode = swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{MaxConcurrent: &one, TotalCompletions: &one}}
	spec.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionNone}
	return spec
}

// RestoreSpec returns the replicated-job service that restores the newest
// snapshot of the repository into t, for a rollback. Unlike the backups it
// mounts the volume read-write.
func RestoreSpec(t *Target, image, secretID, secretName string, networks []string) swarm.ServiceSpec {
	spec := JobSpec(t, image, secretID, secretName, networks)
	spec.Name = serviceName("flux4swarm-restore-", t.Namespace, t.Claim)
	spec.TaskTemplate.ContainerSpec.Mounts[0].ReadOnly = false
	// VolSync's backups and ours both store the volume's files at the top
	// of a /data snapshot.
	spec.TaskTemplate.ContainerSpec.Command[2] = strings.Join([]string{
		"set -eu",
		"set -a; . /run/secrets/" + envFile + "; set +a",
		"restic snapshots --latest 1 --path " + importer.VolSyncDataDir,
		"restic restore latest --path " + importer.VolSyncDataDir + " --target " + importer.VolSyncDataDir + " --delete",
	}, "\n")
	return spec
}

// SyncSpec returns the long-running service that backs up t every
// interval, keeping keep snapshots. Each round is incremental, and the
// restic cache stays in the container between rounds, so a round of an
// unchanged volume only reads file metadata.
func SyncSpec(t *Target, image, secretID, secretName string, networks []string, every time.Duration, keep int) swarm.ServiceSpec {
	secs := int(every.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	script := strings.Join([]string{
		"set -u",
		"set -a; . /run/secrets/" + envFile + "; set +a",
		"until restic cat config >/dev/null 2>&1 || restic init; do sleep 30; done",
		"cd " + importer.VolSyncDataDir,
		"n=0 ok=0 failed=0",
		"while :; do",
		"  n=$((n+1)) start=$(date +%s) r=ok",
		// --json --quiet prints only the summary (and errors on stderr).
		"  out=$(" + backupCmd + " --json --quiet) || { r=error; out=; }",
		"  [ $r = ok ] && ok=$((ok+1)) || failed=$((failed+1))",
		"  s=$(printf '%s\\n' \"$out\" | grep '\"message_type\":\"summary\"' | tail -n 1)",
		// One line per round, read by the metrics of import --watch.
		"  echo \"" + StatusPrefix + "{\\\"round\\\":$n,\\\"result\\\":\\\"$r\\\",\\\"ok\\\":$ok,\\\"failed\\\":$failed,\\\"start\\\":$start,\\\"end\\\":$(date +%s),\\\"summary\\\":${s:-null}}\"",
		// forget and prune lock the repository exclusively, which makes a
		// VolSync restore that runs at that moment fail and retry, so they
		// run only every keep rounds.
		fmt.Sprintf("  if [ $((n %% %d)) -eq 0 ]; then restic forget --host %s --keep-last %d --prune --retry-lock 10m || true; fi", keep, importer.VolSyncHost, keep),
		fmt.Sprintf("  sleep %d", secs),
		"done",
	}, "\n")
	one := uint64(1)
	spec := baseSpec(t, image, secretID, secretName, networks, syncName(t.Namespace, t.Claim), script)
	spec.Labels[labelSync] = "true"
	spec.Labels[labelNamespace] = t.Namespace
	spec.Labels[labelClaim] = t.Claim
	spec.Labels[labelEvery] = strconv.Itoa(secs)
	spec.Mode = swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}}
	delay := 30 * time.Second
	spec.TaskTemplate.RestartPolicy = &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny, Delay: &delay}
	return spec
}

// backupCmd is the backup VolSync itself runs, so its restore picks it up.
var backupCmd = "restic backup --host " + importer.VolSyncHost + " --exclude lost+found ."

// StatusPrefix starts the line a sync service logs after every round,
// followed by a JSON SyncStatus.
const StatusPrefix = "flux4swarm-sync "

// Labels of the sync services: labelSync marks them, the others are read
// by the metrics.
const (
	labelSync      = "flux4swarm.io/volume-sync"
	labelNamespace = "flux4swarm.io/volume-sync-namespace"
	labelClaim     = "flux4swarm.io/volume-sync-claim"
	labelEvery     = "flux4swarm.io/volume-sync-interval-seconds"
)

func baseSpec(t *Target, image, secretID, secretName string, networks []string, name, script string) swarm.ServiceSpec {
	m := t.Mount
	m.Target = importer.VolSyncDataDir
	m.ReadOnly = true
	if m.VolumeOptions != nil {
		vo := *m.VolumeOptions
		// Back up the whole volume, not the subpath one service uses.
		vo.Subpath = ""
		m.VolumeOptions = &vo
	}
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name:   name,
			Labels: map[string]string{api.LabelVolumeBackup: t.Volume, api.LabelManagedBy: api.ManagedByValue},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:   image,
				Command: []string{"/bin/sh", "-c", script},
				Mounts:  []mount.Mount{m},
				Secrets: []*swarm.SecretReference{{
					SecretID: secretID, SecretName: secretName,
					File: &swarm.SecretReferenceFileTarget{Name: envFile, UID: "0", GID: "0", Mode: 0o400},
				}},
			},
		},
	}
	for _, n := range networks {
		spec.TaskTemplate.Networks = append(spec.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: n})
	}
	if t.Node != "" {
		spec.TaskTemplate.Placement = &swarm.Placement{Constraints: []string{nodeConstraint(t.Node)}}
	}
	return spec
}

func nodeConstraint(node string) string {
	// Node IDs are 25 lowercase alphanumerics; anything else is a hostname.
	if len(node) == 25 && strings.Trim(node, "abcdefghijklmnopqrstuvwxyz0123456789") == "" {
		return "node.id==" + node
	}
	return "node.hostname==" + node
}

func jobName(ns, claim string) string { return serviceName("flux4swarm-backup-", ns, claim) }

func syncName(ns, claim string) string { return serviceName("flux4swarm-sync-", ns, claim) }

func serviceName(prefix, ns, claim string) string {
	n := prefix + ns + "-" + claim
	if len(n) > 63 {
		n = strings.TrimRight(n[:63], "-")
	}
	return n
}

// EnvFile renders the repository settings as a POSIX shell file.
func EnvFile(env map[string]string) []byte {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s='%s'\n", k, strings.ReplaceAll(env[k], "'", `'\''`))
	}
	return []byte(b.String())
}

// Backup copies the named Swarm volume to its restic repository and waits
// for the job to finish. The job and its secret are removed afterwards.
func Backup(ctx context.Context, c Client, volume string, o Options) (*Result, error) {
	if o.Repository == "" {
		return nil, errors.New("no restic repository: give --repository or RESTIC_REPOSITORY")
	}
	if o.Env["RESTIC_PASSWORD"] == "" {
		return nil, errors.New("RESTIC_PASSWORD is not set")
	}
	if o.Image == "" {
		o.Image = DefaultImage
	}
	if o.Timeout == 0 {
		o.Timeout = time.Hour
	}
	if o.Poll == 0 {
		o.Poll = 2 * time.Second
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if o.Keep <= 0 {
		o.Keep = 24
	}
	t, err := Plan(ctx, c, volume, o.Node)
	if err != nil {
		return nil, err
	}
	// A new sync service replaces the old one; a one-off backup is the
	// final copy, so the sync service must not run after it.
	stopped, err := StopSync(ctx, c, volume, o.Poll)
	if err != nil {
		return nil, err
	}
	for _, n := range stopped {
		logf("stopped sync service %s", n)
	}
	repo := importer.VolSyncRepository(o.Repository, t.Namespace, t.Claim)
	res := &Result{Volume: volume, Namespace: t.Namespace, Claim: t.Claim, Repository: repo, Node: t.Node, Running: t.Running}
	if o.Restore {
		if o.Every > 0 {
			return nil, errors.New("a restore cannot run continuously")
		}
		if len(t.Running) > 0 {
			return nil, fmt.Errorf("%s still running; stop it before restoring into volume %s", strings.Join(t.Running, ", "), volume)
		}
	}
	if len(t.Running) > 0 && o.Every == 0 {
		logf("warning: %s still running; stop it first for a consistent final copy", strings.Join(t.Running, ", "))
	}

	env := map[string]string{}
	for k, v := range o.Env {
		env[k] = v
	}
	env["RESTIC_REPOSITORY"] = repo
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name := jobName(t.Namespace, t.Claim)
	if o.Every > 0 {
		name = syncName(t.Namespace, t.Claim)
	}
	secretName := name + "-" + hex.EncodeToString(suffix)
	sec, err := c.SecretCreate(ctx, swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: secretName, Labels: map[string]string{api.LabelVolumeBackup: volume, api.LabelManagedBy: api.ManagedByValue}},
		Data:        EnvFile(env),
	})
	if err != nil {
		return nil, fmt.Errorf("create secret: %w", err)
	}
	if o.Every > 0 {
		spec := SyncSpec(t, o.Image, sec.ID, secretName, o.Networks, o.Every, o.Keep)
		if _, err := c.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{}); err != nil {
			_ = c.SecretRemove(context.WithoutCancel(ctx), sec.ID)
			return nil, fmt.Errorf("create sync service %s: %w", spec.Name, err)
		}
		logf("sync service %s backs up volume %s (claim %s/%s) to %s every %s on node %s; follow it with: docker service logs -f %s",
			spec.Name, volume, t.Namespace, t.Claim, repo, o.Every, orAny(t.Node), spec.Name)
		res.Service = spec.Name
		return res, nil
	}
	// Clean up even when ctx is cancelled.
	cleanup := context.WithoutCancel(ctx)
	defer func() {
		for i := 0; i < 30; i++ {
			// The secret stays in use until the job's tasks are gone.
			if err := c.SecretRemove(cleanup, sec.ID); err == nil {
				return
			}
			time.Sleep(o.Poll)
		}
		logf("warning: could not remove secret %s", secretName)
	}()

	spec := JobSpec(t, o.Image, sec.ID, secretName, o.Networks)
	verb, done := "backing up volume %s (claim %s/%s) to %s on node %s", "volume %s backed up"
	if o.Restore {
		spec = RestoreSpec(t, o.Image, sec.ID, secretName, o.Networks)
		verb, done = "restoring volume %s (claim %s/%s) from %s on node %s", "volume %s restored"
	}
	logf(verb, volume, t.Namespace, t.Claim, repo, orAny(t.Node))
	svc, err := c.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create job %s: %w", spec.Name, err)
	}
	defer func() {
		if err := c.ServiceRemove(cleanup, svc.ID); err != nil {
			logf("warning: could not remove job %s: %v", spec.Name, err)
		}
	}()

	wctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	for {
		tasks, err := c.TaskList(wctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", svc.ID))})
		if err != nil {
			return nil, err
		}
		for _, tk := range tasks {
			switch tk.Status.State {
			case swarm.TaskStateComplete:
				logf(done, volume)
				return res, nil
			case swarm.TaskStateFailed, swarm.TaskStateRejected:
				return nil, fmt.Errorf("backup job %s %s: %s%s", spec.Name, tk.Status.State, tk.Status.Err, logs(cleanup, c, svc.ID))
			}
		}
		select {
		case <-wctx.Done():
			return nil, fmt.Errorf("backup job %s: %w", spec.Name, wctx.Err())
		case <-time.After(o.Poll):
		}
	}
}

func orAny(n string) string {
	if n == "" {
		return "any"
	}
	return n
}

// logs returns the tail of the job's output for an error message.
func logs(ctx context.Context, c Client, id string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := c.ServiceLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Tail: "20"})
	if err != nil {
		return ""
	}
	defer r.Close()
	b, _ := io.ReadAll(io.LimitReader(r, 64<<10))
	// The stream is multiplexed; keep the printable text.
	s := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (r >= ' ' && r != 0x7f) {
			return r
		}
		return -1
	}, string(b))
	if s = strings.TrimSpace(s); s == "" {
		return ""
	}
	return "\n" + s
}

// StopSync removes the sync services of the named volume and their secrets,
// and returns their names.
func StopSync(ctx context.Context, c Client, volume string, poll time.Duration) ([]string, error) {
	if poll == 0 {
		poll = 2 * time.Second
	}
	services, err := c.ServiceList(ctx, swarm.ServiceListOptions{Filters: filters.NewArgs(filters.Arg("label", labelSync))})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, s := range services {
		if s.Spec.Labels[api.LabelVolumeBackup] != volume || s.Spec.Labels[labelSync] == "" {
			continue
		}
		if err := c.ServiceRemove(ctx, s.ID); err != nil {
			return names, fmt.Errorf("remove sync service %s: %w", s.Spec.Name, err)
		}
		names = append(names, s.Spec.Name)
		if cs := s.Spec.TaskTemplate.ContainerSpec; cs != nil {
			for _, r := range cs.Secrets {
				// The secret stays in use until the tasks are gone.
				for i := 0; i < 30 && c.SecretRemove(ctx, r.SecretID) != nil; i++ {
					time.Sleep(poll)
				}
			}
		}
	}
	return names, nil
}

// EnsureSync starts the sync service of the volume (Backup with o.Every)
// unless one runs already. It returns nil when nothing was started.
func EnsureSync(ctx context.Context, c Client, volume string, o Options) (*Result, error) {
	if o.Every <= 0 {
		return nil, errors.New("EnsureSync needs Options.Every")
	}
	services, err := c.ServiceList(ctx, swarm.ServiceListOptions{Filters: filters.NewArgs(filters.Arg("label", labelSync))})
	if err != nil {
		return nil, err
	}
	for _, s := range services {
		if s.Spec.Labels[api.LabelVolumeBackup] == volume && s.Spec.Labels[labelSync] != "" {
			return nil, nil
		}
	}
	return Backup(ctx, c, volume, o)
}
