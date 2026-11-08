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

// JobSpec returns the replicated-job service that backs up t.
func JobSpec(t *Target, image, secretID, secretName string, networks []string) swarm.ServiceSpec {
	m := t.Mount
	m.Target = importer.VolSyncDataDir
	m.ReadOnly = true
	if m.VolumeOptions != nil {
		vo := *m.VolumeOptions
		// Back up the whole volume, not the subpath one service uses.
		vo.Subpath = ""
		m.VolumeOptions = &vo
	}
	script := strings.Join([]string{
		"set -eu",
		"set -a; . /run/secrets/" + envFile + "; set +a",
		"restic cat config >/dev/null 2>&1 || restic init",
		"cd " + importer.VolSyncDataDir,
		"restic backup --host " + importer.VolSyncHost + " --exclude lost+found .",
	}, "\n")
	one := uint64(1)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name:   jobName(t.Namespace, t.Claim),
			Labels: map[string]string{api.LabelVolumeBackup: t.Volume, api.LabelManagedBy: api.ManagedByValue},
		},
		Mode: swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{MaxConcurrent: &one, TotalCompletions: &one}},
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
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionNone},
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

func jobName(ns, claim string) string {
	n := "flux4swarm-backup-" + ns + "-" + claim
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
	t, err := Plan(ctx, c, volume, o.Node)
	if err != nil {
		return nil, err
	}
	repo := importer.VolSyncRepository(o.Repository, t.Namespace, t.Claim)
	res := &Result{Volume: volume, Namespace: t.Namespace, Claim: t.Claim, Repository: repo, Node: t.Node, Running: t.Running}
	if len(t.Running) > 0 {
		logf("warning: %s still running; stop it first for a consistent final copy", strings.Join(t.Running, ", "))
	}

	env := map[string]string{}
	for k, v := range o.Env {
		env[k] = v
	}
	env["RESTIC_REPOSITORY"] = repo
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	secretName := jobName(t.Namespace, t.Claim) + "-" + hex.EncodeToString(suffix)
	sec, err := c.SecretCreate(ctx, swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: secretName, Labels: map[string]string{api.LabelVolumeBackup: volume, api.LabelManagedBy: api.ManagedByValue}},
		Data:        EnvFile(env),
	})
	if err != nil {
		return nil, fmt.Errorf("create secret: %w", err)
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
	logf("backing up volume %s (claim %s/%s) to %s on node %s", volume, t.Namespace, t.Claim, repo, orAny(t.Node))
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
				logf("volume %s backed up", volume)
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
