// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package migrate moves Swarm services to Kubernetes one at a time, from
// `import --watch --migrate`.
//
// A service asks for it with the Swarm label flux4swarm.io/migrate=kubernetes
// (docker service update --label-add, or deploy.labels in the stack file).
// The cutover then runs on its own:
//
//  1. the service's manifests are exported as they are now (real replica
//     count; ReplicationDestinations restoring once more, manual trigger
//     "final"),
//  2. the service is scaled to 0 on Swarm and its tasks are waited for,
//  3. the final increment of each of its volumes is backed up (this also
//     stops the volume's sync service),
//  4. the manifests are written to <cluster>/migrated/<namespace>/<name>/,
//     with two Flux Kustomizations: "restore" (the claims and their final
//     restore; Ready once the restore has run) and "app" (the workload,
//     dependsOn restore), and the service leaves apps/.
//
// The migrated/ directory in Git is the record of what has moved: a
// migrated service is kept at 0 replicas on Swarm, even when a stack deploy
// scales it up again. Deleting its directory from Git rolls it back.
package migrate

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"sigs.k8s.io/yaml"

	"github.com/olljanat-ai/flux4swarm/internal/api"
	"github.com/olljanat-ai/flux4swarm/internal/importer"
	"github.com/olljanat-ai/flux4swarm/internal/volumebackup"
)

// Dir is the directory below the cluster directory that holds the
// migrated services.
const Dir = "migrated"

// StatusFile in each migrated service's directory records the cutover.
const StatusFile = "MIGRATED.md"

// labelReplicas keeps the replica count of a service while it is scaled to
// 0 for its cutover, so a retried cutover still knows it.
const labelReplicas = "flux4swarm.io/migrate-replicas"

// Client is the Docker API subset the cutover needs.
type Client interface {
	importer.Client
	volumebackup.Client
	ServiceUpdate(ctx context.Context, serviceID string, version swarm.Version, service swarm.ServiceSpec, options swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error)
}

// Options configures Run.
type Options struct {
	// Export are the import options; Path must be the cluster directory.
	Export importer.Options
	// Backup holds the restic settings of the final backups.
	Backup volumebackup.Options
	// StopTimeout bounds the wait for the tasks of a stopped service
	// (default 5m).
	StopTimeout time.Duration
	// Poll is the task polling interval (default 2s).
	Poll time.Duration
	// Log receives a line for every step.
	Log *slog.Logger
	// Now returns the time written to MIGRATED.md (default time.Now).
	Now func() time.Time
	// Metrics, when set, records the cutovers for Prometheus.
	Metrics *Metrics
	// Pending are cutovers done by an earlier Run whose files are not in
	// the checkout yet (the push failed): they are written again instead of
	// cutting the service over a second time.
	Pending map[string]Cutover
}

// Cutover is one service handed over to Kubernetes.
type Cutover struct {
	// Dir is the service's directory below migrated/.
	Dir string
	// Files are the files of Dir.
	Files map[string][]byte
	// Namespace and Workload name it on Kubernetes.
	Namespace, Workload string
	// Stopped is when the service was scaled to 0 on Swarm, HandedOver
	// when its files were ready for Git.
	Stopped, HandedOver time.Time
}

// Result is what Run did.
type Result struct {
	// Migrated are the Swarm services that have moved, old and new.
	Migrated map[string]bool
	// New are the services moved by this run (or still Pending), not yet
	// in the checkout.
	New map[string]Cutover
	// Files to add to the export. Everything is written once, except
	// Owned, which the export rewrites every time.
	Files map[string][]byte
	Owned []string
}

// Run reads the migrated services from the checkout at root, moves the
// services labelled for migration, and keeps the migrated ones stopped on
// Swarm.
func Run(ctx context.Context, c Client, root string, o Options) (*Result, error) {
	if o.StopTimeout == 0 {
		o.StopTimeout = 5 * time.Minute
	}
	if o.Poll == 0 {
		o.Poll = 2 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Export.Path == "" {
		o.Export.Path = "clusters/" + o.Export.Cluster
	}
	o.Export.Path = strings.Trim(path.Clean(strings.TrimPrefix(o.Export.Path, "./")), "/")
	cluster := o.Export.Path
	res := &Result{Migrated: map[string]bool{}, New: map[string]Cutover{}, Files: map[string][]byte{}}
	dirs, err := Recorded(root, cluster)
	if err != nil {
		return nil, err
	}
	for svc := range dirs {
		res.Migrated[svc] = true
	}
	for svc, p := range o.Pending {
		if res.Migrated[svc] {
			continue
		}
		res.Migrated[svc] = true
		res.New[svc] = p
		dirs[svc] = p.Dir
		for f, b := range p.Files {
			res.Files[f] = b
		}
	}
	services, err := c.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil {
		return nil, err
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Spec.Name < services[j].Spec.Name })
	cut := map[string]bool{}
	for _, s := range services {
		name := s.Spec.Name
		if res.Migrated[name] || s.Spec.Labels[api.LabelMigrate] == "" {
			continue
		}
		if v := s.Spec.Labels[api.LabelMigrate]; v != api.MigrateKubernetes {
			o.Log.Warn("migration: unknown label value, ignored", "service", name, "label", api.LabelMigrate+"="+v, "want", api.MigrateKubernetes)
			continue
		}
		co, err := cutover(ctx, c, s, o)
		o.Metrics.cutover(name, co, err)
		if err != nil {
			// One failed cutover must not stop the export; it is retried
			// at the next one.
			o.Log.Error("migration failed; retried at the next export", "service", name, "error", err)
			continue
		}
		for f, b := range co.Files {
			res.Files[f] = b
		}
		res.Migrated[name] = true
		cut[name] = true
		res.New[name] = co
		dirs[name] = co.Dir
	}
	// Keep migrated services stopped on Swarm (those cut over just now are
	// stopped already; services holds their old spec).
	for _, s := range services {
		if !res.Migrated[s.Spec.Name] || cut[s.Spec.Name] {
			continue
		}
		if r := s.Spec.Mode.Replicated; r != nil && r.Replicas != nil && *r.Replicas > 0 {
			o.Log.Warn("migration: migrated service was started on Swarm again; scaling it back to 0",
				"service", s.Spec.Name, "replicas", *r.Replicas, "kubernetes", dirs[s.Spec.Name],
				"rollback", "delete "+dirs[s.Spec.Name]+" from Git first")
			o.Metrics.stoppedAgain(s.Spec.Name)
			if err := scale(ctx, c, s, 0, ""); err != nil {
				o.Log.Error("migration: could not stop migrated service", "service", s.Spec.Name, "error", err)
			}
		}
	}
	// migrated/kustomization.yaml lists every migrated service.
	f := cluster + "/" + Dir + "/kustomization.yaml"
	_, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(f)))
	// Rewritten while it exists, so a rolled back service leaves it too.
	if len(dirs) > 0 || statErr == nil {
		list := []string{}
		for _, d := range dirs {
			list = append(list, strings.TrimPrefix(d, cluster+"/"+Dir+"/"))
		}
		sort.Strings(list)
		k := kustomizationFile(list, "Written by flux4swarm import --migrate on every export: one entry per migrated service.")
		res.Files[f] = k
		res.Owned = append(res.Owned, f)
		// Flux reaches migrated/ by itself only when the cluster directory
		// has no kustomization.yaml of its own.
		if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(cluster), "kustomization.yaml")); err == nil && !bytes.Contains(b, []byte(Dir)) {
			o.Log.Warn("migration: "+cluster+"/kustomization.yaml does not list "+Dir+"; add it to resources or Flux never applies the migrated services", "file", cluster+"/kustomization.yaml")
		}
	}
	waiting := 0
	for _, s := range services {
		if !res.Migrated[s.Spec.Name] && s.Spec.Labels[api.LabelMigrate] == api.MigrateKubernetes {
			waiting++
		}
	}
	o.Metrics.services(len(res.Migrated), waiting)
	return res, nil
}

// NeedsRun reports whether Run has work: a service labelled for migration
// that has not moved yet, or a migrated one running on Swarm again.
func NeedsRun(ctx context.Context, c Client, migrated map[string]bool) (bool, error) {
	services, err := c.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil {
		return false, err
	}
	for _, s := range services {
		if migrated[s.Spec.Name] {
			if r := s.Spec.Mode.Replicated; r != nil && r.Replicas != nil && *r.Replicas > 0 {
				return true, nil
			}
			continue
		}
		if s.Spec.Labels[api.LabelMigrate] == api.MigrateKubernetes {
			return true, nil
		}
	}
	return false, nil
}

// Recorded returns the migrated services recorded in the checkout: Swarm
// service name -> directory (relative to root).
func Recorded(root, cluster string) (map[string]string, error) {
	out := map[string]string{}
	matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(cluster), Dir, "*", "*", StatusFile))
	if err != nil {
		return nil, err
	}
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		svc := field(b, "Swarm service")
		if svc == "" {
			continue
		}
		rel, _ := filepath.Rel(root, filepath.Dir(m))
		out[svc] = filepath.ToSlash(rel)
	}
	return out, nil
}

// field reads "- **key:** value" (or "key: value") from a status file.
func field(b []byte, key string) string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		l = strings.TrimPrefix(l, "- ")
		l = strings.ReplaceAll(l, "**", "")
		if v, ok := strings.CutPrefix(l, key+":"); ok {
			return strings.Trim(strings.TrimSpace(v), "`")
		}
	}
	return ""
}

func cutover(ctx context.Context, c Client, s swarm.Service, o Options) (Cutover, error) {
	name := s.Spec.Name
	if s.Spec.Mode.Replicated == nil {
		return Cutover{}, fmt.Errorf("only replicated services can be migrated (global and job services cannot be stopped by scaling to 0)")
	}
	replicas := uint64(0)
	if r := s.Spec.Mode.Replicated.Replicas; r != nil {
		replicas = *r
	}
	if replicas == 0 {
		// A retried cutover: the service is already stopped.
		if n, err := strconv.ParseUint(s.Spec.Labels[labelReplicas], 10, 64); err == nil {
			replicas = n
		}
	}
	ns, wl := importer.WorkloadName(s)
	cluster := o.Export.Path
	dir := cluster + "/" + Dir + "/" + ns + "/" + wl
	o.Log.Info("migration 1/4: started", "service", name, "workload", ns+"/"+wl, "replicas", replicas, "directory", dir)
	started := o.Now().UTC()

	// 1. The manifests as they run now.
	eo := o.Export
	eo.VolSyncCutover = true
	eo.Migrated = nil
	eo.Bootstrap = false
	exp, err := importer.Export(ctx, &only{Client: c, svc: s, replicas: replicas}, eo)
	if err != nil {
		return Cutover{}, fmt.Errorf("export: %w", err)
	}
	if len(exp.Volumes) > 0 {
		// Checked before the service is stopped.
		switch {
		case !eo.VolSync:
			return Cutover{}, fmt.Errorf("it has volumes %s; run import with --volsync so their data moves too", strings.Join(exp.Volumes, ", "))
		case o.Backup.Repository == "":
			return Cutover{}, fmt.Errorf("it has volumes %s and RESTIC_REPOSITORY is not set", strings.Join(exp.Volumes, ", "))
		case o.Backup.Env["RESTIC_PASSWORD"] == "":
			return Cutover{}, fmt.Errorf("it has volumes %s and RESTIC_PASSWORD is not set", strings.Join(exp.Volumes, ", "))
		}
	}

	// 2. Stop it on Swarm. The downtime starts here.
	down := o.Now().UTC()
	if replicas > 0 || s.Spec.Mode.Replicated.Replicas == nil || *s.Spec.Mode.Replicated.Replicas > 0 {
		if err := scale(ctx, c, s, 0, strconv.FormatUint(replicas, 10)); err != nil {
			return Cutover{}, fmt.Errorf("scale to 0: %w", err)
		}
	}
	if err := waitStopped(ctx, c, s.ID, o.StopTimeout, o.Poll); err != nil {
		return Cutover{}, err
	}
	stopped := o.Now().UTC()
	o.Log.Info("migration 2/4: stopped on Swarm", "service", name, "downtime_started", down.Format(time.RFC3339))

	// 3. The final increment of every volume, with the writer stopped.
	type backup struct{ volume, repo, at string }
	var backups []backup
	for _, v := range exp.Volumes {
		bo := o.Backup
		bo.Every = 0
		r, err := volumebackup.Backup(ctx, c, v, bo)
		if err != nil {
			return Cutover{}, fmt.Errorf("final backup of volume %s: %w", v, err)
		}
		at := o.Now().UTC().Format(time.RFC3339)
		backups = append(backups, backup{v, r.Repository, at})
		o.Log.Info("migration 3/4: final backup done", "service", name, "volume", v, "repository", r.Repository)
	}
	if len(exp.Volumes) == 0 {
		o.Log.Info("migration 3/4: no volumes to back up", "service", name)
	}

	// 4. Hand it over in Git.
	files := map[string][]byte{}
	prefix := cluster + "/" + importer.AppsDir + "/" + ns + "/"
	var restore, app []string
	for f, b := range exp.Files {
		base, ok := strings.CutPrefix(f, prefix)
		if !ok || strings.Contains(base, "/") {
			continue
		}
		switch {
		case base == "namespace.yaml", base == "kustomization.yaml", base == "secretstore.yaml":
			// Stay in apps/, which keeps the namespace of migrated services.
			continue
		case strings.HasPrefix(base, "persistentvolumeclaim-"), strings.HasPrefix(base, "replicationdestination-"),
			strings.HasPrefix(base, "externalsecret-") && strings.HasSuffix(base, "-volsync-restic.yaml"):
			restore = append(restore, base)
			files[dir+"/restore/"+base] = b
		default:
			app = append(app, base)
			files[dir+"/app/"+base] = b
		}
	}
	sort.Strings(restore)
	sort.Strings(app)
	ks := fluxKustomizations(dir, ns, wl, restore)
	files[dir+"/kustomization.yaml"] = kustomizationFile([]string{"flux.yaml"}, "Written once by flux4swarm import --migrate; yours from now on.")
	files[dir+"/flux.yaml"] = ks
	if len(restore) > 0 {
		files[dir+"/restore/kustomization.yaml"] = kustomizationFile(restore, "")
	}
	files[dir+"/app/kustomization.yaml"] = kustomizationFile(app, "")

	var b strings.Builder
	fmt.Fprintf(&b, "# %s/%s migrated to Kubernetes\n\n", ns, wl)
	fmt.Fprintf(&b, "Written once by `flux4swarm import --migrate`. While this directory exists, the\n")
	fmt.Fprintf(&b, "Swarm service stays stopped and out of `apps/`.\n\n")
	fmt.Fprintf(&b, "- **Swarm service:** `%s`\n", name)
	fmt.Fprintf(&b, "- **Replicas:** %d\n", replicas)
	fmt.Fprintf(&b, "- **Started:** %s\n", started.Format(time.RFC3339))
	fmt.Fprintf(&b, "- **Stopped on Swarm:** %s\n", stopped.Format(time.RFC3339))
	for _, bk := range backups {
		fmt.Fprintf(&b, "- **Final backup:** volume `%s` to `%s` at %s\n", bk.volume, bk.repo, bk.at)
	}
	fmt.Fprintf(&b, "- **Handed over in Git:** %s\n\n", o.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "## Check it on Kubernetes\n\n```sh\n")
	if len(restore) > 0 {
		fmt.Fprintf(&b, "flux get kustomization -n flux-system migrate-%s-%s-restore   # Ready = final restore done\n", ns, wl)
		fmt.Fprintf(&b, "kubectl -n %s get replicationdestinations -o custom-columns=NAME:.metadata.name,LAST:.status.lastManualSync,RESULT:.status.latestMoverStatus.result\n", ns)
	}
	fmt.Fprintf(&b, "flux get kustomization -n flux-system migrate-%s-%s-app       # Ready = workload running\n", ns, wl)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "## Roll back to Swarm\n\n")
	fmt.Fprintf(&b, "See https://github.com/olljanat-ai/flux4swarm/blob/main/docs/kubernetes-and-swarm.md#roll-back-a-migrated-service for the details.\n\n")
	fmt.Fprintf(&b, "1. Remove the label (and `%s` from `deploy.labels` in its stack file):\n", api.LabelMigrate)
	fmt.Fprintf(&b, "   `docker service update --label-rm %s --label-rm %s %s`\n", api.LabelMigrate, labelReplicas, name)
	fmt.Fprintf(&b, "2. Stop it on Kubernetes: `flux suspend kustomization -n flux-system migrate-%s-%s-app` and\n", ns, wl)
	fmt.Fprintf(&b, "   `kubectl -n %s scale %s/%s --replicas 0`.\n", ns, workloadKind(app), wl)
	if len(restore) > 0 {
		fmt.Fprintf(&b, "3. To keep the data written on Kubernetes since the cutover, back the claims up to the same\n")
		fmt.Fprintf(&b, "   repositories with a VolSync ReplicationSource (manual trigger), then restore them into the\n")
		fmt.Fprintf(&b, "   Swarm volumes: `flux4swarm volume-backup --restore %s`.\n", strings.Join(exp.Volumes, " "))
	} else {
		fmt.Fprintf(&b, "3. No volumes: nothing to copy back.\n")
	}
	fmt.Fprintf(&b, "4. Delete this directory from Git. Flux removes the workload; the claims stay (prune is off).\n")
	fmt.Fprintf(&b, "   The next export brings the service back to `apps/` and stops keeping it at 0 on Swarm.\n")
	fmt.Fprintf(&b, "5. `docker service scale %s=%d`.\n", name, replicas)
	files[dir+"/"+StatusFile] = []byte(b.String())

	handed := o.Now().UTC()
	o.Log.Info("migration 4/4: handed over to Kubernetes in Git", "service", name, "directory", dir,
		"restore", len(restore) > 0, "swarm_downtime", handed.Sub(down).Round(time.Second).String(),
		"next", "Flux restores the data, then starts the workload")
	return Cutover{Dir: dir, Files: files, Namespace: ns, Workload: wl, Stopped: down, HandedOver: handed}, nil
}

// fluxKustomizations writes the restore and app Kustomizations.
func fluxKustomizations(dir, ns, wl string, restore []string) []byte {
	src := map[string]any{"kind": api.KindGitRepository, "name": "flux-system"}
	// Kubernetes only: a flux4swarm controller on the Swarm skips them.
	meta := func(n string) map[string]any {
		return map[string]any{"name": n, "namespace": api.DefaultNamespace, "annotations": map[string]string{api.AnnotationIgnore: "true"}}
	}
	app := map[string]any{
		"apiVersion": api.KustomizeGroupVersion, "kind": api.KindKustomization,
		"metadata": meta("migrate-" + ns + "-" + wl + "-app"),
		"spec": map[string]any{
			"interval": "10m", "path": "./" + dir + "/app", "prune": true, "wait": true, "timeout": "10m",
			"sourceRef": src,
		},
	}
	docs := []map[string]any{}
	if len(restore) > 0 {
		var checks []map[string]any
		for _, f := range restore {
			if n, ok := strings.CutPrefix(strings.TrimSuffix(f, ".yaml"), "replicationdestination-"); ok {
				checks = append(checks, map[string]any{"apiVersion": "volsync.backube/v1alpha1", "kind": "ReplicationDestination", "name": n, "namespace": ns})
			}
		}
		docs = append(docs, map[string]any{
			"apiVersion": api.KustomizeGroupVersion, "kind": api.KindKustomization,
			"metadata": meta("migrate-" + ns + "-" + wl + "-restore"),
			"spec": map[string]any{
				// Never prune: these are the claims with the data.
				"interval": "10m", "path": "./" + dir + "/restore", "prune": false, "timeout": "12h",
				"sourceRef":    src,
				"healthChecks": checks,
				// Ready once the final restore has run.
				"healthCheckExprs": []map[string]any{{
					"apiVersion": "volsync.backube/v1alpha1", "kind": "ReplicationDestination",
					"current": "has(status.lastManualSync) && status.lastManualSync == spec.trigger.manual",
					"failed":  "has(status.latestMoverStatus) && status.latestMoverStatus.result == 'Failed' && has(status.lastManualSync) && status.lastManualSync == spec.trigger.manual",
				}},
			},
		})
		app["spec"].(map[string]any)["dependsOn"] = []map[string]any{{"name": "migrate-" + ns + "-" + wl + "-restore"}}
	}
	docs = append(docs, app)
	var b bytes.Buffer
	b.WriteString("# Written once by flux4swarm import --migrate; yours from now on.\n")
	for i, d := range docs {
		if i > 0 {
			b.WriteString("---\n")
		}
		y, _ := yaml.Marshal(d)
		b.Write(y)
	}
	return b.Bytes()
}

// workloadKind is the kubectl resource of the workload in app/.
func workloadKind(app []string) string {
	for _, f := range app {
		for _, k := range []string{"deployment", "statefulset", "daemonset"} {
			if strings.HasPrefix(f, k+"-") {
				return k
			}
		}
	}
	return "deployment"
}

func kustomizationFile(resources []string, comment string) []byte {
	if resources == nil {
		resources = []string{}
	}
	y, _ := yaml.Marshal(map[string]any{"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": resources})
	if comment != "" {
		return append([]byte("# "+comment+"\n"), y...)
	}
	return y
}

// scale sets the replicas of s, recording keep (if set) in labelReplicas.
func scale(ctx context.Context, c Client, s swarm.Service, replicas uint64, keep string) error {
	spec := s.Spec
	mode := *spec.Mode.Replicated
	mode.Replicas = &replicas
	spec.Mode.Replicated = &mode
	if keep != "" {
		l := map[string]string{}
		for k, v := range spec.Labels {
			l[k] = v
		}
		l[labelReplicas] = keep
		spec.Labels = l
	}
	_, err := c.ServiceUpdate(ctx, s.ID, s.Version, spec, swarm.ServiceUpdateOptions{})
	return err
}

func waitStopped(ctx context.Context, c Client, id string, timeout, poll time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		tasks, err := c.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", id))})
		if err != nil {
			return err
		}
		running := 0
		for _, t := range tasks {
			switch t.Status.State {
			case swarm.TaskStateComplete, swarm.TaskStateShutdown, swarm.TaskStateFailed, swarm.TaskStateRejected,
				swarm.TaskStateRemove, swarm.TaskStateOrphaned:
			default:
				running++
			}
		}
		if running == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%d task(s) still running after %s", running, timeout)
		case <-time.After(poll):
		}
	}
}

// only shows the export one service, with its replica count before the
// cutover and the configs and secrets it uses.
type only struct {
	Client
	svc      swarm.Service
	replicas uint64
}

func (o *only) ServiceList(context.Context, swarm.ServiceListOptions) ([]swarm.Service, error) {
	s := o.svc
	spec := s.Spec
	mode := *spec.Mode.Replicated
	r := o.replicas
	mode.Replicas = &r
	spec.Mode.Replicated = &mode
	l := map[string]string{}
	for k, v := range spec.Labels {
		if k != api.LabelMigrate && k != labelReplicas {
			l[k] = v
		}
	}
	spec.Labels = l
	s.Spec = spec
	return []swarm.Service{s}, nil
}

func (o *only) ConfigList(ctx context.Context, opts swarm.ConfigListOptions) ([]swarm.Config, error) {
	all, err := o.Client.ConfigList(ctx, opts)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	if cs := o.svc.Spec.TaskTemplate.ContainerSpec; cs != nil {
		for _, r := range cs.Configs {
			used[r.ConfigID] = true
		}
	}
	var out []swarm.Config
	for _, c := range all {
		if used[c.ID] {
			out = append(out, c)
		}
	}
	return out, nil
}

func (o *only) SecretList(ctx context.Context, opts swarm.SecretListOptions) ([]swarm.Secret, error) {
	all, err := o.Client.SecretList(ctx, opts)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	if cs := o.svc.Spec.TaskTemplate.ContainerSpec; cs != nil {
		for _, r := range cs.Secrets {
			used[r.SecretID] = true
		}
	}
	var out []swarm.Secret
	for _, s := range all {
		if used[s.ID] {
			out = append(out, s)
		}
	}
	return out, nil
}

func (o *only) NetworkList(ctx context.Context, opts network.ListOptions) ([]network.Summary, error) {
	return o.Client.NetworkList(ctx, opts)
}

// DirFor is the directory of a migrated service, for logs and docs.
func DirFor(cluster, ns, name string) string { return path.Join(cluster, Dir, ns, name) }
