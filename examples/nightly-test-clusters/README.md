# Nightly test clusters: CI examples

Pipelines that rebuild a test environment every night from the export of
the live Swarm, with volume data at most one sync interval old, so testers
work on almost current data while production stays on Swarm. See
[Nightly test clusters from the live Swarm](../../docs/kubernetes-and-swarm.md#nightly-test-clusters-from-the-live-swarm)
for the background.

| Target | Azure DevOps | GitHub Actions | Script |
|--------|--------------|----------------|--------|
| Docker Swarm | [swarm/azure-pipelines.yml](swarm/azure-pipelines.yml) | [swarm/github-workflow.yml](swarm/github-workflow.yml) | [swarm/rebuild.sh](swarm/rebuild.sh) |
| Kubernetes | [kubernetes/azure-pipelines.yml](kubernetes/azure-pipelines.yml) | [kubernetes/github-workflow.yml](kubernetes/github-workflow.yml) | [kubernetes/rebuild.sh](kubernetes/rebuild.sh) |

The pipelines are thin: they install the tools, connect to the target and
hand the credentials to the script, which does the same on both CI systems.
Copy the pipeline and its script into the repository the export writes to:

```
.github/workflows/nightly-test-swarm.yml     # or azure-pipelines-nightly-swarm.yml
.github/workflows/nightly-test-cluster.yml   # or azure-pipelines-nightly-cluster.yml
ci/nightly/rebuild-swarm.sh
ci/nightly/rebuild-kubernetes.sh
clusters/nightly/          # import --watch --cluster nightly --volsync-restore-first
clusters/nightly-swarm/    # import --watch --cluster nightly-swarm
```

Both run at 02:00 UTC on weekdays (`cron` in the pipeline) and can be started
by hand. The repository is only read; nothing is committed by the pipelines.

## On the production Swarm

The data comes from sync services that back up every volume to restic all
day. Start them, and the exports, with one or two watchers on the
production Swarm (see [deploy/flux4swarm-export-stack.yaml](../../deploy/flux4swarm-export-stack.yaml)):

```sh
# Kubernetes target: claims and one-off restores in clusters/nightly/data,
# workloads in clusters/nightly/apps once every restore has run.
flux4swarm import --watch --cluster nightly --bootstrap \
  --git-url https://github.com/acme/fleet \
  --volsync-sync 1h --volsync-restore-first

# Swarm target: a plain export, which reconciles to the same services.
flux4swarm import --watch --cluster nightly-swarm --bootstrap \
  --git-url https://github.com/acme/fleet
```

Without the Kubernetes watcher, start the sync services with
`flux4swarm volume-backup --every 1h <volume>...` instead. Both write to
`<RESTIC_REPOSITORY>/<namespace>/<claim>`, the repository each restore reads.

The Swarm target uses its own export because the layout of
`--volsync-restore-first` is for Kubernetes: its `data` Kustomization (with
the SecretStores of namespaces that have volumes) is skipped on Swarm, so
`apps` would wait for it forever.

## Docker Swarm target

`swarm/rebuild.sh` rebuilds the test Swarm from `clusters/nightly-swarm/apps`
at the commit that started the run:

1. `flux4swarm reconcile` with every Deployment and StatefulSet patched to 0
   replicas (and Jobs suspended), so the services, configs, secrets and
   networks are up to date but nothing writes to the volumes,
2. `flux4swarm volume-backup --restore` for every volume the services mount:
   the newest snapshot is restored and files that are not in it are deleted,
   so yesterday's test data is gone,
3. `flux4swarm reconcile` at the real replica counts, waiting until every
   service has converged.

The Kustomization is named `flux-system`, like the one in
`clusters/nightly-swarm/flux-system/gotk-sync.yaml`, so a controller that
keeps the test Swarm in sync during the day
([deploy/flux4swarm-stack.yaml](../../deploy/flux4swarm-stack.yaml) with that
file) owns the same services. `CONTROLLER_SERVICE` names it; the script
stops it during the rebuild and starts it again afterwards.

What the pipeline needs:

* SSH access to a manager of the test Swarm, by a user in the `docker` group.
  The pipeline forwards the manager's Docker socket to the agent
  (`ssh -L`) and sets `DOCKER_HOST=unix://...`. A `tcp://` endpoint with
  `DOCKER_TLS_VERIFY` and `DOCKER_CERT_PATH` works as well.
* The restic settings of the sync services (`RESTIC_REPOSITORY`,
  `RESTIC_PASSWORD`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`); read
  access is enough. Set `RESTORE_NETWORKS` when the repository is only
  reachable from an overlay network.
* The credentials of the secret store the ExternalSecrets read, as files in
  the `--credentials-dir` layout: `vault-token/token` for
  `--secret-store-provider vault` (GitHub example), `azure-keyvault/clientId`
  and `clientSecret` for `azurekv` (Azure DevOps example).

Local volumes live on one node, so a restore needs to know which: use a
single-node test Swarm, pin the services with a `kubernetes.io/hostname`
nodeSelector, or use a shared volume driver (NFS, ...). A volume that was
never backed up is reported and starts empty, and the run fails at the end
so it is noticed.

## Kubernetes target

The Kubernetes pipelines delete yesterday's AKS cluster and create a new
one; replace that step for another provider or an existing cluster you
reset. `kubernetes/rebuild.sh` then:

1. installs External Secrets Operator, VolSync (with the VolumeSnapshot CRDs
   when the cluster has none) and the Gateway API CRDs; add your Gateway
   controller if the tests go through the HTTPRoutes,
2. runs `flux install`, creates the `flux-system` Git secret and, in every
   namespace of the export, the Secrets the SecretStores reference
   (`STORE_CREDENTIALS_DIR`, same layout as above),
3. applies `clusters/nightly/flux-system/gotk-sync.yaml` and waits until the
   `data` Kustomization has restored every claim (`RESTORE_TIMEOUT`, default
   3h) and `apps` is Ready.

Flux keeps reconciling `clusters/nightly` during the day, so configuration
changes on Swarm reach the test cluster; the data is restored only once per
build. The Git token must stay valid all day: a PAT or deploy key, not the
job token. The restic settings come from the secret store through the
`*-volsync-restic` ExternalSecrets of the export.

`flux bootstrap git --path clusters/nightly` instead of `flux install` works
too, but it commits `gotk-components.yaml` and needs a token with write
access.
