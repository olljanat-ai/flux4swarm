# Running one repository on Kubernetes and Swarm

flux4swarm is meant for a migration where the same Git repository is
applied by Flux to Kubernetes and by flux4swarm to Swarm for a while. This
page describes what happens to Kubernetes features that Swarm does not have,
and how to lay out the repository so that new Kubernetes-only work does not
get in the way of the Swarm environments that are still live.

## Policy: Kubernetes-only features never fail a build

A Kubernetes feature with no Swarm counterpart never fails a Kustomization.
It is dropped, or mapped to the closest Swarm behaviour, and reported:

* **Objects** of kinds Swarm has no use for are listed under `skipped` in
  `/status` and in `reconcile` and `diff` output, with the reason.
* **Fields** that are dropped are listed under `warnings` in `/status`,
  logged, and printed by `reconcile` and `diff`.

Only real errors fail a Kustomization: a manifest that does not parse, a
missing ConfigMap or Secret that a Swarm workload needs, a `volumeMount`
without a volume, two objects that map to the same Swarm service, a
`swarm.flux4swarm.io/*` annotation that is wrong.

Run with `--strict` to turn the warnings into a failed build instead. That
is useful in CI, to see what a change would drop on Swarm before it merges.

| Kubernetes | On Swarm |
|------------|----------|
| Kinds with no Swarm meaning: HelmRelease, Ingress, NetworkPolicy, RBAC, CRDs, CronJob, PodDisruptionBudget, custom resources, ... | skipped |
| SOPS encrypted Secret | skipped (a Swarm workload that needs it fails with "secret not found"; use an ExternalSecret) |
| Pod with several containers (sidecars) | the container named by `swarm.flux4swarm.io/container`, else the first, runs; the others are ignored with a warning |
| Init containers, including native sidecars | ignored, warning |
| Volume types other than configMap, secret, emptyDir, hostPath, persistentVolumeClaim (projected, downwardAPI, csi, nfs, ephemeral, ...) | the mount is skipped, warning |
| httpGet, tcpSocket and grpc probes | ignored, warning (exec probes become the Swarm healthcheck) |
| affinity, tolerations, topologySpreadConstraints, serviceAccountName, priorityClassName, lifecycle, hostPort, hostNetwork, pod and container securityContext fields other than user, group, readOnlyRootFilesystem, capabilities | ignored, warning |
| resources other than cpu and memory (GPUs, ephemeral-storage) | ignored, warning |
| PVC `storageClassName` without `swarm.flux4swarm.io/volume-driver` | local volume, warning |
| Deployment `minReadySeconds`, `progressDeadlineSeconds`, `paused`; StatefulSet `podManagementPolicy: Parallel`, `OnDelete`; Job `activeDeadlineSeconds`, `ttlSecondsAfterFinished`, indexed completion | ignored, warning |
| `spec.healthChecks` entry for a Flux kind Swarm does not run (HelmRelease) | skipped, warning |
| Job `suspend: true` | not started; a job service that already ran is kept as it is (not updated, not pruned) until `suspend` is removed |
| Deployment with the default rolling update (surge) that writes to a PersistentVolumeClaim without `ReadWriteMany` | start-first update, warning: old and new task share the volume for a moment (use `Recreate` or `maxSurge: 0` for databases) |

## Autoscaling

Swarm has no autoscaler. A HorizontalPodAutoscaler or KEDA ScaledObject is
skipped, but it is not forgotten: its target runs `minReplicas`
(`minReplicaCount` for KEDA, at least 1) when the workload has no
`replicas` of its own, as is usual with an autoscaler. With `replicas` set,
that fixed number runs. Either way there is a warning.

## Canary and blue/green deployments

Swarm has no traffic shifting, but it can roll back a failed update.

* **Flagger** keeps the normal Deployment in Git and creates the primary
  copy and the traffic weights in the cluster, so it works well side by
  side. On Swarm the `Canary` is skipped and its target is updated with
  `FailureAction: rollback`, with `spec.analysis.interval` as the update
  monitor period. This is the recommended tool for mixed setups.
* **Argo Rollouts** replaces the Deployment with a `Rollout`, which is a
  kind Swarm does not run: it is skipped, and a workload converted to a
  Rollout disappears from Swarm (prune removes its service). Keep a plain
  Deployment in the Swarm overlay, or use Flagger, while Swarm is live.
* A manual canary with several weighted `backendRefs` in an HTTPRoute uses
  only the first backend on Swarm (warning): Traefik's Docker provider has
  no weighted services.

## Database migrations

Run schema migrations as a Job in a Flux Kustomization of its own, with
`wait: true`, and let the application's Kustomization depend on it. This is
the usual Flux pattern, and flux4swarm runs it the same way on Swarm, so one
repository behaves the same on both platforms:

```
apps/shop/
  release/kustomization.yaml     # kind: Component, the one version to bump
  migrations/kustomization.yaml  # components: [../release]
  migrations/job-api-migrate.yaml
  app/kustomization.yaml         # components: [../release]
  app/deployment-api.yaml
clusters/prod/
  shop-migrations.yaml           # Flux Kustomization: wait, force, timeout
  shop-app.yaml                  # Flux Kustomization: dependsOn shop-migrations
```

```yaml
# apps/shop/release/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
images:
  - name: registry.example.com/shop/api
    newTag: "1.8.0"
---
# apps/shop/migrations/job-api-migrate.yaml
apiVersion: batch/v1
kind: Job
metadata: {name: api-migrate, namespace: shop}
spec:
  backoffLimit: 2
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: migrate
          image: registry.example.com/shop/api   # tag from ../release
          args: [migrate, up]
          env:
            - name: DATABASE_URL
              valueFrom: {secretKeyRef: {name: api-db, key: url}}
---
# clusters/prod/shop-migrations.yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: shop-migrations, namespace: flux-system}
spec:
  interval: 10m
  path: ./apps/shop/migrations
  sourceRef: {kind: GitRepository, name: flux-system}
  prune: true
  wait: true     # Ready only when the job's current iteration completed
  force: true    # Kubernetes: recreate the immutable Job when it changes
  timeout: 30m   # longer than the slowest migration
---
# clusters/prod/shop-app.yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: shop-app, namespace: flux-system}
spec:
  interval: 10m
  path: ./apps/shop/app
  sourceRef: {kind: GitRepository, name: flux-system}
  prune: true
  wait: true
  dependsOn:
    - name: shop-migrations
```

A release is one commit that changes `newTag`. On Swarm the Job is a
replicated-job service: flux4swarm updates it because its spec changed,
Swarm starts a new job iteration, and `shop-migrations` becomes Ready for
that revision only when the iteration completed. Until then `shop-app`
waits with `DependencyNotReady`. If the migration fails, `shop-migrations`
fails, notifications go out and the application stays on the previous
version. Keep the version in Git (a component or the image tags): on Swarm
`postBuild.substituteFrom` reads its variables from the credentials
directory, not from ConfigMaps in the repository.

Rules that keep this safe:

* **Backward compatible migrations (expand, then contract).** The old
  version keeps running during the migration and after a failed one, and a
  `git revert` of the application is safe. Drop columns a release later.
* **Idempotent, with a lock.** flux4swarm reruns the Job whenever its spec
  changes, including a rotated Secret (a new Secret version). Use a tool
  that records applied versions and locks, such as Flyway, Liquibase,
  golang-migrate, Alembic, EF Core migration bundles, Rails or Django, so
  a rerun does nothing. A plain resync or a controller restart does not
  rerun the Job.
* **No `ttlSecondsAfterFinished`.** On Kubernetes the finished Job is
  deleted and Flux creates it again on the next reconcile, which reruns the
  migration every interval.
* **No init containers.** They do not run on Swarm. Migrating in the
  entrypoint only works with a locking tool, since replicas and start-first
  updates start together.
* **Pause with `suspend: true`.** A suspended Job is not started on Swarm,
  and one that already ran is left alone until `suspend` is removed.
* **Back up before destructive steps** with the database's own tool
  (`pg_dump`, `mysqldump`), for example at the start of the Job's script. A
  file copy of a running database volume is not a consistent backup.
* **Databases on Swarm update stop-first.** Use a StatefulSet, or a
  Deployment with `strategy: {type: Recreate}` or `maxSurge: 0`. The default
  rolling update becomes start-first on Swarm and briefly runs two
  database processes on one volume; flux4swarm warns about it.

## Repository layout for a migration

1. `flux4swarm import --bootstrap` writes the Swarm into
   `clusters/<name>/` and the controller reconciles it. With one Swarm per
   environment, import each into its own `clusters/<env>/` and run
   `flux4swarm merge` (see
   [Several environments](#several-environments-one-shared-base)): the
   shared manifests go to `apps/base/` and each `clusters/<env>/apps/`
   keeps only that environment's differences.
2. Point Flux on Kubernetes at the same repository. Flux ignores the
   `swarm.flux4swarm.io/*` annotations, so the imported manifests apply as
   they are (Kubernetes needs External Secrets Operator for the
   ExternalSecrets).
3. Move the shared manifests into a base and give Swarm and Kubernetes
   each an overlay (after `merge`, `apps/base/` already is that base):

   ```
   apps/
     base/shop/                 # shared Deployments, Services, ConfigMaps
     swarm/shop/kustomization.yaml       # resources: [../../base/shop]
     kubernetes/shop/kustomization.yaml  # base + Kubernetes-only objects
   clusters/
     swarm-prod/apps.yaml       # Kustomization path: ./apps/swarm
     k8s-prod/apps.yaml         # Kustomization path: ./apps/kubernetes
   ```

4. Add Kubernetes-only work (HelmReleases, NetworkPolicies, sidecars,
   autoscalers, Ingress) in the `kubernetes` overlay, or annotate a single
   object `swarm.flux4swarm.io/ignore: "true"` to keep it off Swarm.
   Changes to a shared workload, such as a new httpGet probe or a sidecar,
   can stay in the base: Swarm drops them with a warning.
5. Watch `warnings` and `skipped` in `/status` (or run
   `flux4swarm diff --strict` in CI) to see what Swarm does not run.
6. When a namespace has moved, delete it from the Swarm overlay; prune
   removes its services from Swarm.

## Keep Kubernetes in sync with a live Swarm

The layout above moves Swarm under GitOps. If the development teams should
keep deploying to Swarm the way they do today (`docker stack deploy`,
`docker service update`, their CI pipelines) while a platform team builds
the Kubernetes environment, turn it around: Swarm stays the source of
truth, and `flux4swarm import --watch` commits every change to Git, where
stock Flux applies it to Kubernetes.

```sh
flux4swarm import --watch --cluster prod --bootstrap \
  --git-url https://github.com/acme/fleet --branch main
```

Do not run the flux4swarm controller on the same path at the same time:
then both Git and Swarm would claim to be the source of truth.

### Who owns which file

```
clusters/prod/
  flux-system/            # created once by --bootstrap, then yours (flux bootstrap may rewrite it)
  apps/                   # owned by import --watch: regenerated on every Swarm change, do not edit
  MIGRATION.md            # owned by import --watch: what still blocks each service on Kubernetes
  migrated/               # with --migrate: one directory per service cut over, written once, then yours
  data/, layers.yaml      # with --volsync-restore-first: claims and restores (owned), Flux layers (written once)
  platform/               # owned by the platform team: Kubernetes-only objects
  kustomization.yaml      # owned by the platform team: ties it together and patches apps/
```

Every export empties `apps/` and writes it again, so a manual edit there is
gone with the next Swarm change (or the next `--interval` resync). Anything
outside `apps/` is never overwritten: the exporter only creates a missing
bootstrap file. This is plain kustomize, so it works the same in Flux and
in `kubectl kustomize`:

```yaml
# clusters/prod/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - flux-system
  - apps        # what runs on Swarm right now
  - platform    # HPAs, PodDisruptionBudgets, NetworkPolicies, HelmReleases, ...
patches:
  # Changes to exported objects. Select them with a target, so a patch
  # whose workload is later removed from Swarm matches nothing instead of
  # failing the build.
  - target:
      kind: Deployment
      name: api
      namespace: shop
    patch: |-
      - op: add
        path: /spec/template/spec/containers/0/readinessProbe
        value:
          httpGet: {path: /healthz, port: 8080}
  # An HPA owns the replica count on Kubernetes; drop the one Swarm runs.
  - target:
      kind: Deployment
      name: api
      namespace: shop
    patch: |-
      - op: remove
        path: /spec/replicas
```

Without a root `kustomization.yaml` Flux generates one from the
directories, so plain additions in `platform/` (with its own
`kustomization.yaml`) work too; the root file is only needed for patches.
If you prefer, the same `patches` can go into the `spec.patches` of the
Flux `Kustomization` in `flux-system/gotk-sync.yaml` instead.

Before every commit `import` (and `import --watch`) runs the same kustomize
build Flux would: of `clusters/<cluster>` and of every Flux `Kustomization`
in it that reads the `flux-system` GitRepository, with its `patches`,
`images`, `targetNamespace` and so on. When a build fails, for example
because a patch without a `target` names a service that was renamed or
removed on Swarm, nothing is committed or pushed: one-shot `import` exits
with the error, and `--watch` logs `export not pushed: kustomize build
failed` with the failing path and keeps retrying until the overlay is
fixed. Git then keeps the last export that Flux could apply. `--validate=false`
turns the check off.

### Moving a workload over

Start from `MIGRATION.md`: a service with no blockers can move as it is;
the others list what to change on Swarm first (see
[the README](../README.md#migration-readiness-report)).
When a workload should run only on Kubernetes, copy its manifests from
`apps/` into `platform/` (or another directory you own) and remove the
service from Swarm, or keep it on Swarm and `--exclude` it from the export.
The next export drops it from `apps/`, and Flux keeps the copy you own.
When everything has moved, stop `import --watch`; the repository is then an
ordinary Flux repository.

### Moving volume data (restic + VolSync)

Manifests move with Git; the data in Swarm volumes does not. flux4swarm
copies it with two standard tools instead of a mover of its own:
[restic](https://restic.net) on the Swarm side and
[VolSync](https://volsync.readthedocs.io)'s restic mover on Kubernetes,
with an S3 compatible bucket (or any restic repository) in between. The
two clusters never talk to each other.

1. Export with `--volsync`. Next to every PersistentVolumeClaim, import
   writes a VolSync `ReplicationDestination` (restic mover, `copyMethod:
   Direct` into the claim, `enableFileDeletion: true`) and an
   ExternalSecret `<claim>-volsync-restic` with the repository settings.
   Both carry `swarm.flux4swarm.io/ignore: "true"`, so Swarm ignores them.
   The cluster needs VolSync installed. In the secret store, put the base
   repository under `volsync-restic-repository` (for example
   `s3:https://s3.example.com/migration/prod`), the restic password under
   `volsync-restic-password` and the S3 credentials under
   `volsync-aws-access-key-id` / `volsync-aws-secret-access-key` (all after
   `--secret-key-prefix`; `--volsync-env` changes the credential variables).
   Each claim gets its own repository, `<base>/<namespace>/<claim>`.
2. The export keeps every workload that mounts a claim at 0 replicas on
   Kubernetes (annotation `flux4swarm.io/held-replicas` keeps the Swarm
   count) until it is cut over: the restore and the application must not
   use the claim at the same time.
3. Copy the data from Swarm with the same settings in the environment:

   ```sh
   export RESTIC_REPOSITORY=s3:https://s3.example.com/migration/prod
   export RESTIC_PASSWORD=... AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
   flux4swarm volume-backup shop_db shop_uploads
   ```

   For each volume it starts a one-off replicated-job service that mounts
   the volume exactly like the service that uses it (same driver and
   options, so NFS/CIFS options, old volume plugins and cluster volumes all
   work) read-only at `/data`, on the node where that service runs or last
   ran, and runs `restic backup --host volsync .` there, which is the
   layout VolSync's own backups have. The job and its temporary secret are
   removed afterwards, and import skips them. `--node` picks the node when
   it cannot be found (a service scaled to 0 on a multi-node Swarm), and
   `--network` attaches the job to a network, for example to reach a
   repository server on the Swarm.
4. The `initial` manual trigger restores the newest snapshot once. Backups
   are incremental, so run `volume-backup` while the service runs to
   pre-seed, then at cutover stop the service, run it again and change the
   trigger (patch `spec.trigger.manual` of the ReplicationDestination) for
   the final restore. Then scale the workload up on Kubernetes. With
   `import --watch`, let `--migrate` do this (see
   [Automated](#automated-sync-and-cutover-with-import---watch)).

#### Large volumes: sync continuously, cut over in minutes

For large volumes, keep both sides in sync for as long as the service runs
on Swarm (weeks, if needed), so the cutover only moves the last changes:

* `flux4swarm volume-backup --every 15m shop_db` starts a long-running
  sync service, `flux4swarm-sync-<namespace>-<claim>`, instead of a
  one-off job. It runs an incremental `restic backup` every interval. Its
  restic cache stays in the container between rounds, so a round only reads
  file metadata and uploads what changed. Every `--keep` rounds (default
  24) it forgets snapshots older than the last `--keep` and prunes the
  repository. Running it again replaces the service. `--stop` removes it.
  Follow it with `docker service logs -f flux4swarm-sync-shop-db`.
* `import --volsync --volsync-schedule '*/15 * * * *'` writes the
  ReplicationDestinations with that cron schedule instead of the manual
  trigger. Each restore downloads only what changed, skips files that are
  already right and, with `enableFileDeletion`, deletes files that are gone
  on Swarm. The claim then trails the Swarm volume by about one interval
  on each side. Keep the workload at 0 replicas on Kubernetes the whole
  time.

These copies come from a running service, so they are not consistent
snapshots. They are only there to make the final copy small. Cutover:

1. Take the workload over in Git (copy its manifests out of `apps/`, see
   above). In the copy you own, replace the ReplicationDestination's
   trigger with `manual: final`. That stops the schedule and starts one
   restore when you apply it, so don't apply it yet.
2. Stop the service on Swarm (`docker service scale shop_db=0`) and wait
   until its tasks are gone. Downtime starts here.
3. `flux4swarm volume-backup shop_db` (no `--every`): it removes the sync
   service and backs up the final increment with the writer stopped. It
   warns if a task still runs.
4. Commit the `manual: final` trigger and wait for the restore:
   `kubectl -n shop wait replicationdestination/db --for=jsonpath='{.status.lastManualSync}'=final`.
5. Scale the workload up on Kubernetes and switch traffic. Downtime ends
   here.

After the cutover, remove the ReplicationDestination, or leave it on the
manual trigger. A scheduled restore that runs after the cutover would
replace the data written on Kubernetes since then with the last Swarm copy.

Downtime is one incremental backup, one incremental restore and the start
of the application. Each of those depends on how much changed since the
last round and on how many files there are, not on the volume size. A
restore with VolSync also reads the files already in the claim to check
them, so measure the restore on the real volume before you plan the
window. Restores and backups can run at the same time. While a sync
service prunes, a restore that starts fails and VolSync retries it.

A file copy of a database is only consistent when the database is
stopped. For a short cutover window, use the database's own replication
or dump tools instead, and use `volume-backup` for file data.

### Automated sync and cutover with import --watch

`import --watch` can run all of the above on its own, so moving a Swarm
service by service is a label per service:

```sh
export RESTIC_REPOSITORY=s3:https://s3.example.com/migration/prod
export RESTIC_PASSWORD=... AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
flux4swarm import --watch --cluster prod --bootstrap \
  --git-url https://github.com/acme/fleet \
  --volsync-sync 15m --migrate
```

* `--volsync-sync 15m` (implies `--volsync`) keeps a sync service running
  for every exported volume, starting the missing ones after each export,
  and writes the ReplicationDestinations with a matching restore schedule
  (`*/15 * * * *` here; `--volsync-schedule` overrides it). The restic
  settings come from the environment of the watcher, the same values the
  secret store holds for Kubernetes. Run the watcher on a manager with
  access to the repository. A sync service whose interval, schedule or
  restic settings no longer match is replaced.
* Very large volumes can be copied outside business hours instead: give
  `--volsync-sync` a 5-field cron schedule in UTC, or put the label
  `flux4swarm.io/volume-sync-schedule` on the Swarm services that need it.
  The label takes a duration or a cron schedule and overrides the flag for
  that service's volumes. With only `--volsync`, sync services run just for
  the labelled services.

  ```sh
  # Weeknights at 01:00 UTC; restores on Kubernetes at 05:00 UTC.
  docker service update \
    --label-add 'flux4swarm.io/volume-sync-schedule=0 1 * * 1-5' \
    --label-add 'flux4swarm.io/volume-restore-schedule=0 5 * * 1-5' erp_db
  ```

  In cron mode the sync service runs busybox `crond` in the restic image.
  A round that is still running when the next one is due is skipped, not
  doubled. The restores follow the label `flux4swarm.io/volume-restore-schedule`.
  Without it they run on the same cron as the backups and so restore the
  previous round. Leave the backup enough time and schedule the restore
  after it. `flux4swarm_volume_sync_interval_seconds` is then the longest
  gap of the schedule (Friday to Monday above), so the "behind" alert
  allows for it. `volume-backup --schedule '0 1 * * *'` starts the same
  kind of service by hand.
* `--migrate` watches for the label `flux4swarm.io/migrate=kubernetes`
  on Swarm services. Put it on with
  `docker service update --label-add flux4swarm.io/migrate=kubernetes shop_db`
  (or in `deploy.labels` of the stack file). At the next export the watcher:

  1. exports the service as it runs, with a final restore (manual trigger
     `final`) for each of its claims,
  2. scales it to 0 on Swarm and waits until its tasks are gone,
  3. backs up the last increment of each volume (this also removes the
     sync service),
  4. moves it out of `apps/` into `migrated/<namespace>/<name>/`, with two
     Flux Kustomizations: `migrate-<namespace>-<name>-restore` (the claims
     and their final restore, Ready when the restore has run, never pruned)
     and `migrate-<namespace>-<name>-app` (the workload at its real replica
     count, `dependsOn` the restore), and commits.

  Flux then restores the last increment and starts the workload. Downtime
  is the time from step 2 until the workload is ready on Kubernetes.
  Services without volumes move the same way, without the restore.
  Global and job services can't be stopped by scaling, so they are refused.
  A failed cutover (for example a backup error) is logged and retried at
  the next export. A service with volumes is never stopped when the export
  has no `--volsync` or no restic settings.

`migrated/` is the record of what has moved: a migrated service stays out
of `apps/`, and the watcher scales it back to 0 if anything starts it on
Swarm again (a `docker stack deploy` of the old stack file, for example)
and logs a warning. Everything in `migrated/<namespace>/<name>/` is written
once and is yours from then on. `migrated/kustomization.yaml` is rewritten
on every export. Without a root `kustomization.yaml` Flux picks `migrated/`
up by itself. If you have one, add `migrated` to its resources; the watcher
warns until you do.

#### Checking that a cutover finished

The watcher logs every step with the service name:

| Log message | Meaning |
|-------------|---------|
| `migration 1/4: started` | the label was found; replica count and target directory |
| `migration 2/4: stopped on Swarm` | all tasks are gone; `downtime_started` |
| `migration 3/4: final backup done` | one line per volume, with its repository |
| `migration 4/4: handed over to Kubernetes in Git` | the manifests are ready; `swarm_downtime` so far |
| `migration done: handed over to Kubernetes` | committed and pushed; `commit` is the Git commit |
| `migration failed; retried at the next export` | with `error`; the service may be stopped on Swarm meanwhile |
| `migration not in Git yet` | the push failed; the service stays stopped and the hand-over is pushed again |
| `migration: migrated service was started on Swarm again; scaling it back to 0` | something started it on Swarm |
| `volume sync: sync service started` | `--volsync-sync` started a sync service for a volume |

The commit message starts with `Migrate <service> to Kubernetes`.
`migrated/<namespace>/<name>/MIGRATED.md` records the replica count and
the time of every step, and lists the commands to check Kubernetes:

```sh
flux get kustomization -n flux-system migrate-shop-db-restore   # Ready = final restore done
kubectl -n shop get replicationdestinations -o custom-columns=NAME:.metadata.name,LAST:.status.lastManualSync,RESULT:.status.latestMoverStatus.result
flux get kustomization -n flux-system migrate-shop-db-app       # Ready = workload running
```

The cutover is finished when the `-app` Kustomization is Ready. The
watcher's `/metrics` and the
[volume sync dashboard](monitoring.md#volume-sync-and-migration) show the
sync services and the cutovers too.

#### Roll back a migrated service

`MIGRATED.md` of each service has these steps with its names filled in:

1. Remove the label, so the service is not cut over again:
   `docker service update --label-rm flux4swarm.io/migrate --label-rm flux4swarm.io/migrate-replicas shop_db`,
   and drop it from `deploy.labels` in the stack file.
2. Stop it on Kubernetes:
   `flux suspend kustomization -n flux-system migrate-shop-db-app` and
   `kubectl -n shop scale deployment/db --replicas 0`.
3. Only if the data written on Kubernetes since the cutover must be kept:
   back the claim up to the same repository with a VolSync
   ReplicationSource, then restore it into the Swarm volume.

   ```yaml
   apiVersion: volsync.backube/v1alpha1
   kind: ReplicationSource
   metadata:
     name: db-rollback
     namespace: shop
   spec:
     sourcePVC: db
     trigger:
       manual: rollback
     restic:
       repository: db-volsync-restic   # the repository secret of the claim
       copyMethod: Direct
       retain:
         last: 10
   ```

   `kubectl -n shop wait replicationsource/db-rollback --for=jsonpath='{.status.lastManualSync}'=rollback`,
   then on Swarm `flux4swarm volume-backup --restore shop_db`. It runs a
   job like the backup, with the volume mounted read-write, restores the
   newest snapshot and deletes the files that are not in it. It refuses
   to run while a service using the volume runs.
4. Delete `migrated/shop/db/` from Git. Flux removes the workload and its
   Kustomizations. The claims stay because pruning is off for them. The
   next export brings the service back to `apps/` (held at 0 on
   Kubernetes again) and stops keeping it at 0 on Swarm.
5. `docker service scale shop_db=<replicas>`. With `--volsync-sync` the
   sync service starts again at the next export.

### Nightly test clusters from the live Swarm

A Kubernetes cluster that is rebuilt every night from the latest Swarm
configuration and data gives long test periods with almost current data,
while production stays on Swarm. Export it as a cluster of its own, with
the sync services running all the time:

```sh
flux4swarm import --watch --cluster nightly --bootstrap \
  --git-url https://github.com/acme/fleet \
  --volsync-sync 1h --volsync-restore-first
```

`--volsync-restore-first` (implies `--volsync`) is for a cluster built from
scratch from the export:

* the claims, their ReplicationDestinations (manual trigger `initial`: one
  restore when the cluster is built), their repository ExternalSecrets and
  the namespaces and secret stores they need go to `clusters/nightly/data/`,
* `clusters/nightly/layers.yaml` has two Flux Kustomizations: `data`
  (waits until every restore has run, never pruned) and `apps` (the
  workloads at their real replica counts, `dependsOn` data), and
  `clusters/nightly/kustomization.yaml` lists `flux-system` and
  `layers.yaml`, so the root reads only those two.

Every rebuild (`flux bootstrap` against `clusters/nightly`, with VolSync,
External Secrets and the secret store set up) then restores the newest
backups, at most one sync interval old, and starts the applications on top.
Configuration changes on Swarm keep flowing into `apps/` during the day.
The data is restored only once per build, so tests are not disturbed by
it. To refresh the data without a rebuild, delete the ReplicationDestinations
and let Flux create them again. It can't be combined with
`--volsync-schedule` or `--merge`. Several watchers can share the Swarm: a
volume has one sync service, which every cluster restores from.

## Several environments: one shared base

Most teams run the same stacks on several Swarms, for example development,
acceptance and production. `flux4swarm merge` turns their imports into one
Kustomize base with what they share and a small overlay per environment
with only what differs, so a change to the shared part is made once.

1. Import every Swarm into its own cluster directory of the same
   repository (point `DOCKER_HOST` or `docker context` at each Swarm):

   ```sh
   DOCKER_HOST=ssh://dev-manager  flux4swarm import --cluster development --bootstrap --output fleet
   DOCKER_HOST=ssh://acc-manager  flux4swarm import --cluster acceptance  --bootstrap --output fleet
   DOCKER_HOST=ssh://prod-manager flux4swarm import --cluster production  --bootstrap --output fleet
   ```

   (or `--git-url` instead of `--output`, one commit per Swarm.)

2. Merge them:

   ```sh
   flux4swarm merge --env development --env acceptance --env production --output fleet
   # or: flux4swarm merge --env ... --git-url https://github.com/acme/fleet --branch main
   ```

The result:

```
apps/base/                     # owned by merge: what the environments share
  shop/deployment-web.yaml
  shop/kustomization.yaml
  kustomization.yaml
clusters/
  development/
    flux-system/               # from import --bootstrap: Kustomization path ./clusters/development
    apps/                      # owned by merge: the development overlay
      kustomization.yaml       # resources: [../../../apps/base, own objects], patches: [...]
      shop/deployment-web-patch.yaml
      shop/configmap-debug.yaml  # an object only development has
  acceptance/...
  production/...
```

How the base is chosen: a field goes into the base when more than half of
the environments have the same value (with two environments, both must
agree). Maps and lists keyed by `name` (containers, env vars, volumes,
ports with names) are compared field by field, so with production on
`nginx:1.26` and three replicas, and development and acceptance on
`nginx:1.27` and one replica, the base holds `nginx:1.27` and `replicas: 1`
and only production's overlay has a patch:

```yaml
# clusters/production/apps/shop/deployment-web-patch.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
spec:
  replicas: 3
  template:
    spec:
      containers:
      - name: web
        image: nginx:1.26
```

Objects that exist in every environment go to the base; the others are
written whole into the overlays of the environments that have them. The
patches are standard kustomize strategic merge patches (ConfigMap data,
HTTPRoute hostnames from Traefik `Host()` rules, ExternalSecret keys and
the `swarm.flux4swarm.io/service-spec` annotation are replaced as a
whole); when list order cannot be expressed that way, merge writes a
JSON6902 patch with a `target` instead. Before writing anything, merge
builds every overlay with stock kustomize (root-only load restrictions)
and checks that it gives exactly the objects of that environment's import,
so reconciling an environment after the merge changes nothing on its
Swarm, and Flux on Kubernetes gets the same objects.

To pick up later changes, import the environment again and merge again:
import replaces `clusters/<env>/apps` with a full export, and merge reads
every environment (a plain export or an earlier overlay, built with
kustomize) and writes the base and all overlays again. Only `apps/base`
(`--base-path`) and `clusters/<env>/apps` are rewritten; `flux-system/`,
a root `kustomization.yaml` and directories such as `platform/` are left
alone, so the [ownership rules above](#who-owns-which-file) still apply
per environment. `--env NAME=DIR` reads an environment from a separate
local import (`DIR/clusters/NAME/apps`) instead of the target repository.

Merge is one-shot. Don't run `import --watch` on an environment that is
merged: every export would replace its overlay with a full copy again, and
the next merge would have to undo that. Use `import --watch` per
environment without merge, or import and merge on a schedule (for example
a CI job) instead.
