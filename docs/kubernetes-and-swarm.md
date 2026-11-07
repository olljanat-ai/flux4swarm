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
