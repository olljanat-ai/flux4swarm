// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package kubebackup backs up PersistentVolumeClaims of a Kubernetes
// cluster with VolSync ReplicationSources (restic mover) into the same
// repository layout `flux4swarm volume-backup` uses for Swarm volumes:
// one restic repository per claim, <base>/<namespace>/<claim>. The
// ReplicationDestinations of `kube-import --volsync` (or `import --volsync`)
// restore from there, on another Kubernetes cluster, and
// `volume-backup --restore` into a Swarm volume.
package kubebackup

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/importer"
)

// Kinds kube-backup reads and writes.
var (
	PVC               = importer.KubeResource{Version: "v1", Resource: "persistentvolumeclaims", Kind: "PersistentVolumeClaim", Namespaced: true}
	Secret            = importer.KubeResource{Version: "v1", Resource: "secrets", Kind: "Secret", Namespaced: true}
	Pod               = importer.KubeResource{Version: "v1", Resource: "pods", Kind: "Pod", Namespaced: true}
	Deployment        = importer.KubeResource{Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment", Namespaced: true}
	StatefulSet       = importer.KubeResource{Group: "apps", Version: "v1", Resource: "statefulsets", Kind: "StatefulSet", Namespaced: true}
	ReplicationSource = importer.KubeResource{Group: "volsync.backube", Version: "v1alpha1", Resource: "replicationsources", Kind: "ReplicationSource", Namespaced: true}
)

// Copy methods of the VolSync source.
const (
	CopyDirect   = "Direct"
	CopySnapshot = "Snapshot"
	CopyClone    = "Clone"
)

// Client is the Kubernetes API subset the backup needs.
type Client interface {
	List(ctx context.Context, r importer.KubeResource, ns string) ([]map[string]any, error)
	Get(ctx context.Context, r importer.KubeResource, ns, name string) (map[string]any, error)
	Apply(ctx context.Context, r importer.KubeResource, obj map[string]any) error
	MergePatch(ctx context.Context, r importer.KubeResource, ns, name string, patch map[string]any) error
	Delete(ctx context.Context, r importer.KubeResource, ns, name string) error
}

// Options configures Run.
type Options struct {
	// Claims are "namespace/name"; with All, every claim of Namespaces
	// (every namespace when empty).
	Claims     []string
	All        bool
	Namespaces []string
	// Repository is the base restic repository; each claim gets its own
	// below it.
	Repository string
	// Env are RESTIC_PASSWORD and the credentials (AWS_*, AZURE_*, ...).
	Env map[string]string
	// Schedule, a 5-field cron schedule, keeps backing up in the cluster;
	// without it one backup runs now (manual trigger).
	Schedule string
	// CopyMethod is Direct (default), Snapshot or Clone; Snapshot and
	// Clone need a CSI driver and give a point-in-time copy of a claim in
	// use.
	CopyMethod          string
	VolumeSnapshotClass string
	// Keep is the number of snapshots restic keeps (retain.last).
	Keep int
	// Final stops the Deployments and StatefulSets that mount the claims
	// first (their replicas are kept in annotation
	// api.AnnotationStoppedReplicas, which kube-import exports), so the
	// last backup holds every write. Implies a manual backup and Wait.
	Final bool
	// Wait blocks until the manual backups finished (default timeout 6h).
	Wait    bool
	Timeout time.Duration
	// Delete removes the ReplicationSources and Secrets kube-backup made.
	Delete bool
	Poll   time.Duration
	Log    *slog.Logger
	// Now names the manual trigger; for tests.
	Now func() time.Time
}

// Result is one claim's backup.
type Result struct {
	Claim      string
	Repository string
	Trigger    string
	Duration   string
	Stopped    []string
}

// SourceName is the ReplicationSource (and, with SecretSuffix, the
// repository Secret) of claim name.
func SourceName(name string) string { return name + "-backup" }

// SecretSuffix follows SourceName in the repository Secret's name; it
// differs from the ReplicationDestination's so both can live on one
// cluster.
const SecretSuffix = "-restic"

// Run backs up the claims.
func Run(ctx context.Context, c Client, o Options) ([]Result, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Poll <= 0 {
		o.Poll = 5 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 6 * time.Hour
	}
	if o.CopyMethod == "" {
		o.CopyMethod = CopyDirect
	}
	if o.Keep <= 0 {
		o.Keep = 24
	}
	switch o.CopyMethod {
	case CopyDirect, CopySnapshot, CopyClone:
	default:
		return nil, fmt.Errorf("copy method %q: use Direct, Snapshot or Clone", o.CopyMethod)
	}
	if o.Final {
		if o.Schedule != "" {
			return nil, fmt.Errorf("a final backup runs once; it cannot have a schedule")
		}
		o.Wait = true
	}
	claims, err := selectClaims(ctx, c, o)
	if err != nil {
		return nil, err
	}
	if len(claims) == 0 {
		return nil, fmt.Errorf("no PersistentVolumeClaims selected")
	}
	if o.Delete {
		var out []Result
		for _, cl := range claims {
			ns, name, _ := strings.Cut(cl, "/")
			if err := c.Delete(ctx, ReplicationSource, ns, SourceName(name)); err != nil {
				return out, fmt.Errorf("%s: %w", cl, err)
			}
			if err := c.Delete(ctx, Secret, ns, SourceName(name)+SecretSuffix); err != nil {
				return out, fmt.Errorf("%s: %w", cl, err)
			}
			out = append(out, Result{Claim: cl})
		}
		return out, nil
	}
	if o.Repository == "" || o.Env["RESTIC_PASSWORD"] == "" {
		return nil, fmt.Errorf("RESTIC_REPOSITORY and RESTIC_PASSWORD are needed")
	}
	var stopped map[string][]string
	if o.Final {
		if stopped, err = stopWorkloads(ctx, c, claims, o); err != nil {
			return nil, err
		}
	}
	trigger := "backup-" + o.Now().UTC().Format("20060102T150405Z")
	var out []Result
	for _, cl := range claims {
		ns, name, _ := strings.Cut(cl, "/")
		repo := importer.VolSyncRepository(o.Repository, ns, name)
		if err := apply(ctx, c, ns, name, repo, trigger, o); err != nil {
			return out, fmt.Errorf("%s: %w", cl, err)
		}
		r := Result{Claim: cl, Repository: repo, Trigger: trigger, Stopped: stopped[cl]}
		if o.Schedule != "" {
			r.Trigger = o.Schedule
		}
		out = append(out, r)
		o.Log.Info("backup configured", "claim", cl, "repository", repo, "trigger", r.Trigger, "copyMethod", o.CopyMethod)
	}
	if !o.Wait || o.Schedule != "" {
		return out, nil
	}
	for i := range out {
		ns, name, _ := strings.Cut(out[i].Claim, "/")
		d, err := wait(ctx, c, ns, SourceName(name), trigger, o)
		if err != nil {
			return out, fmt.Errorf("%s: %w", out[i].Claim, err)
		}
		out[i].Duration = d
		o.Log.Info("backup done", "claim", out[i].Claim, "duration", d)
	}
	return out, nil
}

func selectClaims(ctx context.Context, c Client, o Options) ([]string, error) {
	var out []string
	if !o.All {
		for _, cl := range o.Claims {
			ns, name, ok := strings.Cut(cl, "/")
			if !ok || ns == "" || name == "" {
				return nil, fmt.Errorf("claim %q: use namespace/name", cl)
			}
			if !o.Delete {
				p, err := c.Get(ctx, PVC, ns, name)
				if err != nil {
					return nil, err
				}
				if p == nil {
					return nil, fmt.Errorf("PersistentVolumeClaim %s not found", cl)
				}
			}
			out = append(out, cl)
		}
		return out, nil
	}
	nss := o.Namespaces
	if len(nss) == 0 {
		nss = []string{""}
	}
	for _, ns := range nss {
		l, err := c.List(ctx, PVC, ns)
		if err != nil {
			return nil, err
		}
		for _, p := range l {
			meta, _ := p["metadata"].(map[string]any)
			pns, _ := meta["namespace"].(string)
			name, _ := meta["name"].(string)
			if len(o.Namespaces) == 0 && (strings.HasPrefix(pns, "kube-") || contains(importer.SystemNamespaces, pns)) {
				continue
			}
			// Scratch claims of VolSync's own movers.
			if labels, _ := meta["labels"].(map[string]any); labels["app.kubernetes.io/created-by"] == "volsync" {
				continue
			}
			st, _ := p["status"].(map[string]any)
			if phase, _ := st["phase"].(string); phase != "" && phase != "Bound" {
				o.Log.Warn("claim not bound, skipped", "claim", pns+"/"+name, "phase", phase)
				continue
			}
			out = append(out, pns+"/"+name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func apply(ctx context.Context, c Client, ns, name, repo, trigger string, o Options) error {
	labels := map[string]any{api.LabelManagedBy: api.ManagedByValue, api.LabelVolumeBackup: name}
	sd := map[string]any{"RESTIC_REPOSITORY": repo}
	for k, v := range o.Env {
		sd[k] = v
	}
	secret := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": SourceName(name) + SecretSuffix, "namespace": ns, "labels": labels},
		"stringData": sd,
	}
	if err := c.Apply(ctx, Secret, secret); err != nil {
		return fmt.Errorf("secret: %w", err)
	}
	tr := map[string]any{"manual": trigger}
	if o.Schedule != "" {
		tr = map[string]any{"schedule": o.Schedule}
	}
	restic := map[string]any{
		"repository":        SourceName(name) + SecretSuffix,
		"copyMethod":        o.CopyMethod,
		"pruneIntervalDays": int64(7),
		"retain":            map[string]any{"last": int64(o.Keep)},
	}
	if o.VolumeSnapshotClass != "" {
		restic["volumeSnapshotClassName"] = o.VolumeSnapshotClass
	}
	rs := map[string]any{
		"apiVersion": ReplicationSource.APIVersion(), "kind": ReplicationSource.Kind,
		"metadata": map[string]any{"name": SourceName(name), "namespace": ns, "labels": labels},
		"spec":     map[string]any{"sourcePVC": name, "trigger": tr, "restic": restic},
	}
	if err := c.Apply(ctx, ReplicationSource, rs); err != nil {
		if strings.Contains(err.Error(), "the server could not find the requested resource") {
			return fmt.Errorf("ReplicationSource: is VolSync installed on this cluster? %w", err)
		}
		return fmt.Errorf("ReplicationSource: %w", err)
	}
	return nil
}

// wait polls the ReplicationSource until the manual trigger has run.
func wait(ctx context.Context, c Client, ns, name, trigger string, o Options) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	for {
		rs, err := c.Get(ctx, ReplicationSource, ns, name)
		if err != nil {
			return "", err
		}
		st, _ := rs["status"].(map[string]any)
		if st["lastManualSync"] == trigger {
			mover, _ := st["latestMoverStatus"].(map[string]any)
			if res, _ := mover["result"].(string); res == "Failed" {
				logs, _ := mover["logs"].(string)
				return "", fmt.Errorf("restic mover failed: %s", strings.TrimSpace(logs))
			}
			d, _ := st["lastSyncDuration"].(string)
			return d, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("backup did not finish in %s (kubectl -n %s describe replicationsource %s)", o.Timeout, ns, name)
		case <-time.After(o.Poll):
		}
	}
}

// stopWorkloads scales the Deployments and StatefulSets that mount the
// claims to 0, records their replicas, and waits until no pod mounts them.
func stopWorkloads(ctx context.Context, c Client, claims []string, o Options) (map[string][]string, error) {
	byNS := map[string]map[string]bool{}
	for _, cl := range claims {
		ns, name, _ := strings.Cut(cl, "/")
		if byNS[ns] == nil {
			byNS[ns] = map[string]bool{}
		}
		byNS[ns][name] = true
	}
	stopped := map[string][]string{}
	for ns, names := range byNS {
		for _, r := range []importer.KubeResource{Deployment, StatefulSet} {
			l, err := c.List(ctx, r, ns)
			if err != nil {
				return nil, err
			}
			for _, w := range l {
				meta, _ := w["metadata"].(map[string]any)
				wname, _ := meta["name"].(string)
				used := claimsOf(w, r.Kind)
				var hit []string
				for _, u := range used {
					if names[u] {
						hit = append(hit, ns+"/"+u)
					}
				}
				if len(hit) == 0 {
					continue
				}
				spec, _ := w["spec"].(map[string]any)
				n := int64(1)
				if v, ok := spec["replicas"]; ok {
					n = toInt(v)
				}
				if n == 0 {
					continue
				}
				patch := map[string]any{
					"metadata": map[string]any{"annotations": map[string]any{api.AnnotationStoppedReplicas: strconv.FormatInt(n, 10)}},
					"spec":     map[string]any{"replicas": 0},
				}
				if err := c.MergePatch(ctx, r, ns, wname, patch); err != nil {
					return nil, fmt.Errorf("stop %s %s/%s: %w", r.Kind, ns, wname, err)
				}
				o.Log.Info("stopped for the final backup", "kind", r.Kind, "workload", ns+"/"+wname, "replicas", n)
				for _, h := range hit {
					stopped[h] = append(stopped[h], r.Kind+"/"+wname)
				}
			}
		}
	}
	// Wait for the pods to go, so nothing writes during the backup.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	for {
		busy := ""
		for ns, names := range byNS {
			pods, err := c.List(ctx, Pod, ns)
			if err != nil {
				return nil, err
			}
			for _, p := range pods {
				for _, u := range podClaims(p) {
					if names[u] {
						meta, _ := p["metadata"].(map[string]any)
						if lbl, _ := meta["labels"].(map[string]any); lbl["app.kubernetes.io/created-by"] == "volsync" {
							continue
						}
						busy = fmt.Sprintf("%s/%v", ns, meta["name"])
					}
				}
			}
		}
		if busy == "" {
			return stopped, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("pod %s still mounts a claim; stop it before the final backup", busy)
		case <-time.After(o.Poll):
		}
	}
}

// claimsOf returns the claims a Deployment or StatefulSet mounts,
// including a StatefulSet's <template>-<name>-<ordinal> claims.
func claimsOf(w map[string]any, kind string) []string {
	spec, _ := w["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	out := podClaims(tpl)
	if kind == "StatefulSet" {
		meta, _ := w["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		n := int64(1)
		if v, ok := spec["replicas"]; ok {
			n = toInt(v)
		}
		vcts, _ := spec["volumeClaimTemplates"].([]any)
		for _, v := range vcts {
			vm, _ := v.(map[string]any)
			m, _ := vm["metadata"].(map[string]any)
			t, _ := m["name"].(string)
			for i := int64(0); i < n; i++ {
				out = append(out, fmt.Sprintf("%s-%s-%d", t, name, i))
			}
		}
	}
	return out
}

func podClaims(p map[string]any) []string {
	spec, _ := p["spec"].(map[string]any)
	vols, _ := spec["volumes"].([]any)
	var out []string
	for _, v := range vols {
		vm, _ := v.(map[string]any)
		if pvc, ok := vm["persistentVolumeClaim"].(map[string]any); ok {
			if n, _ := pvc["claimName"].(string); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

func toInt(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	}
	return 0
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}
