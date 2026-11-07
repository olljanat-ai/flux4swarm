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
   `clusters/<name>/` and the controller reconciles it.
2. Point Flux on Kubernetes at the same repository. Flux ignores the
   `swarm.flux4swarm.io/*` annotations, so the imported manifests apply as
   they are (Kubernetes needs External Secrets Operator for the
   ExternalSecrets).
3. Move the shared manifests into a base and give each environment an
   overlay:

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
