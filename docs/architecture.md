# flux4swarm architecture

flux4swarm is a GitOps controller for Docker Swarm modelled on
[Flux v2](https://github.com/fluxcd/flux2). Flux is built from several
Kubernetes controllers that watch custom resources in the API server. Docker
Swarm has no custom resources and no API server we can extend, so flux4swarm
packs the relevant parts of Flux into one process that keeps its objects in
memory and talks to the Docker Engine API on a Swarm manager.

```
                 ┌──────────────────────────── flux4swarm controller ───────────────────────────┐
  Git (GitHub /  │  ┌──────────────────┐  artifact  ┌──────────────────────┐  apply  ┌────────┐ │
  Azure DevOps / │─▶│ source reconciler │──────────▶│ kustomization        │────────▶│ swarm  │─┼─▶ Docker Engine API
  any Git host)  │  │ (GitRepository)   │ (revision │ reconciler           │  prune  │ applier│ │   (Swarm manager)
                 │  └──────────────────┘  + files) │ (Kustomization)      │  wait   └────────┘ │
                 │                                  │   │ ExternalSecret      │                   │
                 │                                  │   ▼                     │                   │
                 │                                  │ secret store providers  │──▶ Vault / Azure Key Vault / fake
                 │                                  └──────────────────────┘                   │
                 │   status: /healthz /readyz /status (JSON) /metrics                           │
                 └───────────────────────────────────────────────────────────────────────────────┘
```

## Modes

The binary has two modes, chosen by sub-command.

| Mode | Command | Flux equivalent | What it does |
|------|---------|-----------------|--------------|
| GitOps | `flux4swarm controller` (and `flux4swarm reconcile` for a single pass) | source-controller + kustomize-controller | Pulls Git, applies the manifests to Swarm, prunes what was removed, waits for services to converge. |
| Import | `flux4swarm import` | `flux bootstrap` + `flux export` | Reads the services, configs, secrets and networks of an existing Swarm, writes them as manifests (secrets as `ExternalSecret`) and commits/pushes them to GitHub or Azure DevOps. Optionally writes the `flux-system` sync manifests so the controller can take over. |

## API objects

All objects are Kubernetes-style YAML (`apiVersion`, `kind`, `metadata`, `spec`),
several per file separated by `---`. `metadata.namespace` is accepted and kept
in labels for Flux compatibility, but Swarm itself has no namespaces.

### Flux objects (unchanged API groups)

* `source.toolkit.fluxcd.io/v1` **GitRepository**: `spec.url`, `spec.ref`
  (`branch`, `tag`, `semver` is not supported, `commit`), `spec.interval`,
  `spec.timeout`, `spec.secretRef`, `spec.provider` (`generic`, `github`,
  `azure`), `spec.suspend`.
* `kustomize.toolkit.fluxcd.io/v1` **Kustomization**: `spec.sourceRef`,
  `spec.path`, `spec.interval`, `spec.retryInterval`, `spec.timeout`,
  `spec.prune`, `spec.wait`, `spec.suspend`, `spec.dependsOn`.

GitRepository and Kustomization objects found inside a synced path are
registered with the controller, exactly as Flux's self-managing
`flux-system` Kustomization does. They are removed again when they disappear
from Git (if the parent Kustomization has `prune: true`).

### Swarm objects (`swarm.flux4swarm.io/v1alpha1`)

* **Service**: `spec` is a Docker Engine API
  [`ServiceSpec`](https://docs.docker.com/reference/api/engine/version/v1.47/#tag/Service/operation/ServiceCreate)
  verbatim (PascalCase keys such as `TaskTemplate.ContainerSpec.Image`).
  `metadata.name` is the service name. Configs and secrets are referenced by
  their logical names (`ConfigName`, `SecretName`); IDs are filled in by the
  controller. Networks are referenced by name in `TaskTemplate.Networks[].Target`.
* **Config**: `spec.data` (UTF-8 text) or `spec.binaryData` (base64),
  `spec.labels`, `spec.templating` (Docker template driver name).
* **Network**: `spec` is a Docker `NetworkCreate` request (`Driver`,
  `Attachable`, `Internal`, `Options`, `IPAM`, `Labels`). Networks are
  immutable; they are created once and only removed when pruned and unused.

### Secrets (`external-secrets.io/v1`)

Plain secrets never go to Git. A Docker secret is described with an
External Secrets Operator **ExternalSecret** that points at a
**SecretStore** or **ClusterSecretStore**. The controller fetches the value
from the store at apply time and creates the Docker secret.

* `ExternalSecret.spec.target.name` (default: `metadata.name`) is the Docker
  secret name. With one `spec.data` entry the secret holds that value. With
  several entries, one Docker secret per entry is created, named
  `<target>-<secretKey>`. `spec.dataFrom` is not supported.
* Supported store providers: `vault` (KV v1/v2, token or AppRole auth),
  `azurekv` (service principal client secret), `fake` (static values, for
  tests and demos). Other providers can be added behind the `Provider`
  interface in `internal/secrets`.
* ESO references Kubernetes secrets for store credentials
  (`{name: x, key: y}`). flux4swarm resolves those to files in the
  credentials directory (`--credentials-dir`, default `/run/secrets`):
  first `<dir>/<name>/<key>`, then `<dir>/<name>_<key>`. When the controller
  runs as a Swarm service, these are ordinary Docker secrets mounted into it.
  The same lookup is used for `GitRepository.spec.secretRef`.

## Reconciliation

### Source reconciler

Every `spec.interval` the GitRepository is fetched into a cache directory with
[go-git](https://github.com/go-git/go-git) (pure Go, no `git` binary needed).
The artifact revision uses the Flux format `<branch>@sha1:<commit>`.
Authentication follows Flux's secret keys:

| Source | `spec.provider` | Keys in `secretRef` |
|--------|-----------------|---------------------|
| Any HTTPS host (GitHub, Azure DevOps, GitLab, Gitea...) | `generic` | `username` + `password` (PAT), or `bearerToken` |
| Any SSH host | `generic` | `identity`, `known_hosts`, optional `password` |
| GitHub App | `github` | `githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`, optional `githubAppBaseURL` |
| Azure DevOps with Entra ID | `azure` | `tenantId`, `clientId`, `clientSecret` (service principal; Flux uses workload identity here, which Swarm does not have) |

For GitHub a PAT works with any username. For Azure DevOps a PAT works with
any non-empty username. go-git >= 5.11 speaks the `multi_ack` protocol
that Azure DevOps requires.

### Kustomization reconciler

1. Wait for the source artifact and for every `dependsOn` Kustomization to be Ready.
2. **Build**: read every `*.yaml`/`*.yml` below `spec.path`, recursively.
   If a directory has a `kustomization.yaml`, only the files and directories
   listed in its `resources:` are read (a subset of kustomize; patches,
   generators and overlays are not supported, see *Limitations*).
3. **Resolve secrets** for every `ExternalSecret` through its store.
4. **Apply** in dependency order: Networks, Configs, Secrets, Services, then
   Flux objects are registered.
5. **Prune** (if `spec.prune`): remove Services, then unreferenced
   Configs/Secrets, then unused Networks that carry this Kustomization's
   ownership labels but are no longer in the build.
6. **Wait** (if `spec.wait`): wait until every replicated service has all
   replicas running and every update is complete, up to `spec.timeout`. A
   rollback or paused update makes the Kustomization not Ready.
7. Record the status (Ready condition, last applied revision, inventory).

### Ownership labels

Every object the controller creates gets the Flux kustomize-controller labels:

```
kustomize.toolkit.fluxcd.io/name: <kustomization name>
kustomize.toolkit.fluxcd.io/namespace: <kustomization namespace>
```

plus `app.kubernetes.io/managed-by: flux4swarm` and, on services, a
`flux4swarm.io/spec-hash` label. Pruning only touches objects with these
labels, so objects created by hand or by `docker stack deploy` are never
deleted unless they are adopted (see Import below).

### Immutable configs and secrets

Docker configs and secrets cannot be changed once created. The controller
uses the approach Docker users know from `terraform-provider-docker` and
compose `name:` suffixes: the Swarm object is named
`<logical-name>-<first 10 hex chars of sha256(data)>` and labelled
`flux4swarm.io/name: <logical-name>`. Service references are rewritten to the
current version, so changing the content in Git rolls the service, and the
old version is pruned once no service uses it.

### Service drift

Docker fills defaults into a stored `ServiceSpec`, so comparing desired and
live specs field by field gives false positives. The controller stores a hash
of the desired spec in `flux4swarm.io/spec-hash` and updates the service when
the hash differs, when the label is missing (adoption) or when the live
image, replica count or labels were changed by hand.

### Code borrowed from terraform-provider-docker

`internal/swarm/tfdocker` contains code adapted from
[kreuzwerker/terraform-provider-docker](https://github.com/kreuzwerker/terraform-provider-docker)
under MPL-2.0: the service convergence checker (task-by-slot tracking,
update/rollback state machine) and the registry auth selection for images.
The Terraform SDK dependencies were removed; the logic is unchanged. Those
files keep the MPL-2.0 header (see `NOTICE`).

## Import mode

`flux4swarm import` connects to a Swarm manager and writes:

```
<repo>/
└── clusters/<cluster>/
    ├── flux-system/
    │   └── gotk-sync.yaml       # GitRepository + Kustomization (with --bootstrap)
    └── swarm/
        ├── networks/<name>.yaml
        ├── configs/<name>.yaml
        ├── secrets/
        │   ├── secret-store.yaml    # SecretStore template for --secret-store-provider
        │   └── <name>.yaml          # ExternalSecret, remoteRef.key = secret name
        └── services/<name>.yaml
```

* Runtime fields (IDs, versions, timestamps, `UpdateStatus`, endpoint VIPs)
  are dropped, network IDs are turned into names, and config/secret IDs are
  removed.
* Config data is exported. Secret values cannot be read from the Docker API,
  so the user has to put them in the secret store under the key written in
  the ExternalSecret (`--secret-key-prefix` controls the remote key).
* Services from `docker stack deploy` keep their `com.docker.stack.*`
  labels and names.
* `--adopt` adds the ownership labels to the exported manifests so that the
  first reconcile adopts the existing objects instead of leaving them alone.
* Output goes to a local directory (`--output`) or straight to Git
  (`--git-url`, `--branch`, credentials as for GitRepository). Both
  `https://github.com/<org>/<repo>` and
  `https://dev.azure.com/<org>/<project>/_git/<repo>` work.

## Status and operations

Flux writes status into the custom resources. flux4swarm serves it over HTTP
(`--listen`, default `:9797`): `/healthz`, `/readyz`, `/status` (JSON list of
objects with their Ready condition, revision and inventory, similar to
`flux get all`) and `/metrics` (Prometheus text format with
`gotk_reconcile_condition` and `gotk_reconcile_duration_seconds`).
`POST /reconcile` triggers an immediate reconcile, like
`flux reconcile --with-source`.

## Limitations and design choices

* Only one controller replica should run, on a manager node, with the Docker
  socket mounted. There is no leader election.
* Kustomize overlays, patches and generators, Helm, OCI and Bucket sources,
  notifications and image automation are not implemented.
* Where Flux behaviour could not be copied, the most Flux-like choice was
  taken: API groups and field names match Flux and ESO, the labels match
  kustomize-controller, and Kubernetes secret references map to Docker
  secrets.
