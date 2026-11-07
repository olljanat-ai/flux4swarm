# flux4swarm

GitOps for Docker Swarm that reads the same Git repository as
[Flux v2](https://github.com/fluxcd/flux2) on Kubernetes.

The repository holds genuine Flux objects (`GitRepository`, `OCIRepository`,
`Kustomization`)
and plain Kubernetes manifests (`Deployment`, `Service`, `ConfigMap`,
`PersistentVolumeClaim`, External Secrets Operator `ExternalSecret`, Gateway
API `HTTPRoute`). Stock Flux applies them to a Kubernetes cluster;
flux4swarm builds them with the real kustomize library and translates them
into Docker Swarm services, configs, secrets, volumes and Traefik labels. That
makes it a migration helper: run existing Swarm environments and new
Kubernetes clusters from one repository, and move workloads over when you
are ready.

flux4swarm also has an **import** mode that dumps a running Swarm (including
`docker stack deploy` stacks and Traefik Swarm provider labels) into that
format and pushes it to GitHub or Azure DevOps.

See [docs/architecture.md](docs/architecture.md) for the design and the
complete mapping.

## Two modes

| Mode | Command | Use it to |
|------|---------|-----------|
| **Import** | `flux4swarm import` | Export services, configs, secrets (as ExternalSecrets), volumes and Traefik routes (as Gateway API HTTPRoutes) from a running Swarm to a directory or straight to a GitHub / Azure DevOps repository. |
| **Merge** | `flux4swarm merge` | Combine the imports of several Swarms (development, acceptance, production) into a shared Kustomize base plus a small overlay per environment that only patches what differs. |
| **Continuous import** | `flux4swarm import --watch` | Keep the Swarm as the source of truth (teams keep using `docker stack deploy`) and commit every change to Git, so Flux keeps a Kubernetes cluster in sync while the platform team adds Kubernetes-only features on top. |
| **GitOps** | `flux4swarm controller` (loop) / `flux4swarm reconcile` (one pass) | Pull the repository, build it like Flux's kustomize-controller, apply it to Swarm, prune what was removed and wait for services to converge. |

A typical migration: import once with `--bootstrap`, review the commit,
start the controller on the Swarm, and point Flux on Kubernetes at the same
repository (`flux bootstrap` adds `gotk-components.yaml`). With several
Swarms (development, acceptance, production), import each into its own
`clusters/<env>` and run `flux4swarm merge` before reviewing, so the shared
manifests live once in `apps/base` and each environment only keeps its
differences (see [Several Swarms](#several-swarms-shared-base-and-per-environment-overlays)). Kubernetes-only
features (sidecars, httpGet probes, autoscalers, canaries, HelmReleases, ...)
never fail the Swarm side: they are skipped with a warning, so new
Kubernetes work can start while Swarm is still live. See
[docs/kubernetes-and-swarm.md](docs/kubernetes-and-swarm.md) for the policy
and a repository layout for the migration.

## Install

Releases publish binaries for Linux, macOS and Windows (amd64, arm64) on the
[releases page](https://github.com/olljanat-ai/flux4swarm/releases) and
multi-arch images (linux/amd64, linux/arm64) to
`ghcr.io/olljanat-ai/flux4swarm` (tags `X.Y.Z`, `X.Y` and `latest`). Each
release has SPDX SBOMs and keyless [cosign](https://github.com/sigstore/cosign)
signatures for the checksums file and the images:

```sh
cosign verify ghcr.io/olljanat-ai/flux4swarm:latest \
  --certificate-identity-regexp 'https://github.com/olljanat-ai/flux4swarm/.github/workflows/release.yml@refs/tags/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Or build from source:

```sh
go install github.com/olljanat-ai/flux4swarm/cmd/flux4swarm@latest
# or
make build          # bin/flux4swarm
docker build -t flux4swarm .
```

`flux4swarm version` prints the version, commit and build date.
To cut a release, push a `vX.Y.Z` tag, or run the **release** workflow from the Actions tab with the version (e.g. `0.1.0`); it tags the head of the selected branch and runs GoReleaser.

flux4swarm talks to the Docker Engine API of a Swarm **manager** and honours
`DOCKER_HOST`, `DOCKER_TLS_VERIFY` and `DOCKER_CERT_PATH`.

## Import an existing Swarm

```sh
# To a local directory
flux4swarm import --cluster prod --output ./fleet --bootstrap --secret-store-provider vault

# Straight to GitHub
export GIT_USERNAME=git GIT_PASSWORD=<GitHub PAT with contents:write>
flux4swarm import --cluster prod --bootstrap --git-url https://github.com/acme/fleet --branch main

# Straight to Azure DevOps
export GIT_USERNAME=pat GIT_PASSWORD=<Azure DevOps PAT with Code read & write>
flux4swarm import --cluster prod --bootstrap --git-url https://dev.azure.com/acme/platform/_git/fleet
```

Result (one directory per Docker stack = Kubernetes namespace):

```
clusters/prod/
├── flux-system/
│   ├── gotk-sync.yaml              # GitRepository + Kustomization (Flux format)
│   └── kustomization.yaml
└── apps/
    ├── kustomization.yaml
    ├── shop/                       # stack "shop"
    │   ├── namespace.yaml
    │   ├── deployment-api.yaml     # service shop_api
    │   ├── service-api.yaml        # ClusterIP for the Traefik backend port
    │   ├── service-api-public.yaml # LoadBalancer for published ports
    │   ├── configmap-appconf.yaml
    │   ├── externalsecret-dbpw.yaml
    │   ├── secretstore.yaml
    │   ├── persistentvolumeclaim-apidata.yaml
    │   ├── httproute-api.yaml      # from traefik.http.routers.api.*
    │   ├── middleware-api-auth.yaml # Traefik Middleware CRD (no Gateway API equivalent)
    │   └── kustomization.yaml
    └── traefik/gateway.yaml        # GatewayClass + Gateway, one listener per entrypoint
```

Docker never returns secret values, so put each secret into the store under
the key in its ExternalSecret `remoteRef.key`. Env values, however, are
returned as they are, so a `DB_PASSWORD` from `docker stack deploy` would
end up in Git. Import therefore moves env vars whose name contains a
well-known secret word (case-insensitive: `PASSWORD`, `PASSWD`, `SECRET`,
`TOKEN`, `API_KEY`, `APIKEY`, `ACCESS_KEY`, `PRIVATE_KEY`, `CREDENTIAL`,
`CONNECTION_STRING`, `CONNECTIONSTRING`) into one ExternalSecret per service,
`externalsecret-<service>-env.yaml`, with the remote key
`<prefix><swarm service name>-<ENV_NAME>`, and the Deployment uses
`valueFrom.secretKeyRef`. A note lists each converted variable and key; put
the current value into the store. Names ending in `_FILE`, `_PATH` or `_DIR`
(`MYSQL_PASSWORD_FILE=/run/secrets/db`), empty values and Swarm templates stay
plain. Add your own name parts with `--secret-env-name LICENSE` (repeatable)
or turn the built-in list off with `--no-default-secret-env-names`. Only
names are matched; values are not inspected, so a secret under another name
is still exported as is. Everything that has no
Kubernetes equivalent (update policies, extra labels, placement preferences,
...) is kept in the `swarm.flux4swarm.io/service-spec` annotation, so
reconciling the export gives the same Swarm services back. `--adopt` keeps
the running image digests and, after the export is written, labels every
service the export reproduces exactly as owned by the `flux-system`
Kustomization (`--adopt-owner`), so the first reconcile changes nothing.
Only labels are changed, so no task restarts. Services that use secrets are
left for the first reconcile, because their new secret versions are created
from the secret store. Import prints every Swarm object it did not export
(`skipped: Service/tmp_debug (matches --exclude)`), so nothing is dropped
silently.

### Migration readiness report

Every export also writes `clusters/<cluster>/MIGRATION.md`: per service, a
checklist of what stands between it and Kubernetes. **Blockers** must change
first: bind mounts (hostPath), the Docker socket, ports published in host
mode, placement constraints with no nodeSelector equivalent, and environment
variables that look like plain text credentials (`*_PASSWORD`, `*_TOKEN`,
URLs with a password; values are never written to the report). **Checks**
work after the move but need a decision on the cluster: load balancers for
published ports, node labels, storage classes for volume drivers, log
drivers, attached networks, and Swarm-only settings that Kubernetes ignores.
The file has no timestamps, so with `--watch` it changes only when the Swarm
does, and the Git history shows the checklist shrinking. For a one-shot run
without writing anything:

```sh
flux4swarm migration-report --cluster prod            # Markdown to stdout
flux4swarm migration-report --json --fail-on-blockers # for CI; exit 2 while blockers remain
```

### Keep exporting (Swarm stays the source of truth)

`import --watch` keeps running: it exports again after every Swarm service,
config or secret event (after `--debounce`, default 5s, so one `docker stack
deploy` gives one commit) and every `--interval` (default 10m), and commits
only when the export differs. Swarm is never changed (`--adopt` is refused).
The export owns `clusters/<cluster>/apps/` and `MIGRATION.md` and rewrites them every time; files
anywhere else, such as `flux-system/` or the platform team's overlays and
patches, are created once when missing and then left alone. That is how
Kubernetes-only features are added on top, see
[docs/kubernetes-and-swarm.md](docs/kubernetes-and-swarm.md#keep-kubernetes-in-sync-with-a-live-swarm).
Before each commit the tree is kustomize-built like Flux does, including
those overlays; if an overlay no longer matches Swarm (a patched service was
renamed or removed) the export is not pushed and the failure is logged, so
Flux keeps the last export that worked.

Each commit message says what changed, grouped by namespace, so the Git
history reads as an audit log of the Swarm, for example
`shop: api image 1.4→1.5, removed tmp-debug`. When the list is too long for
one line the subject counts the changes (`Sync Docker Swarm state: 12
changes in shop, web`) and the body lists them. `--commit-message` sets a
fixed message instead.

```sh
flux4swarm import --watch --cluster prod --bootstrap --git-url https://github.com/acme/fleet
```

To run it as a Swarm service see
[deploy/flux4swarm-export-stack.yaml](deploy/flux4swarm-export-stack.yaml).
A failed export is retried, so watch mode never exits on an expired token or
a rejected push. It serves `/healthz`, `/status` and `/metrics` on
`--listen` (default `:9797`); `/healthz` fails once no export succeeded for
`--unhealthy-after` (default 3 x `--interval`). See
[docs/monitoring.md](docs/monitoring.md#import---watch) for the metrics and
alerts.

### Several Swarms: shared base and per-environment overlays

Import each Swarm into its own cluster directory, then let `merge` move
what they share into `apps/base` and rewrite each `clusters/<env>/apps` as
an overlay with only that environment's objects and patches (image tags,
replicas, env vars, config data, Traefik hosts, ...):

```sh
DOCKER_HOST=ssh://dev-manager  flux4swarm import --cluster development --bootstrap --output fleet
DOCKER_HOST=ssh://prod-manager flux4swarm import --cluster production  --bootstrap --output fleet
flux4swarm merge --env development --env production --output fleet
```

Every overlay is checked to build back to exactly its import. See
[docs/kubernetes-and-swarm.md](docs/kubernetes-and-swarm.md#several-environments-one-shared-base).

### Services outside a stack

Services, configs and secrets without a `com.docker.stack.namespace` label
(made with `docker service create` and friends) go to the namespace given
with `--default-namespace` (env `FLUX4SWARM_DEFAULT_NAMESPACE`, default
`default`). The controller treats that namespace the same way: objects in it
are deployed without a `<namespace>_` prefix or stack label, and manifests
without `metadata.namespace` land in it. To move several dedicated Swarms
into one shared Kubernetes cluster, give each its own namespace and use the
same value for `import` and `controller`:

```sh
flux4swarm import --default-namespace team-a --cluster prod --output ./fleet
flux4swarm controller --default-namespace team-a --config clusters/prod/flux-system/gotk-sync.yaml
```

Stacks keep mapping to the namespace of the same name. Don't pick a default
namespace that is also a stack name on that Swarm, because both would then
share one Kubernetes namespace.

## Run the GitOps controller

```sh
# From the Flux manifests written by import (or by flux bootstrap)
flux4swarm controller --config clusters/prod/flux-system/gotk-sync.yaml

# Or with flags
flux4swarm controller --url https://github.com/acme/fleet --branch main --path ./clusters/prod

# One pass for CI or debugging; exits non-zero if anything is not Ready
flux4swarm reconcile --path ./clusters/prod --dry-run      # local directory
flux4swarm reconcile --url https://dev.azure.com/acme/platform/_git/fleet --path ./clusters/prod --wait
```

Sources can be a `GitRepository` (branch, tag, commit or `ref.semver`, e.g.
`--semver '>=1.0.0 <2.0.0'`) or an `OCIRepository` with artifacts pushed by
`flux push artifact`:

```sh
flux push artifact oci://ghcr.io/acme/fleet:1.4.0 --path ./clusters/prod \
  --source "$(git config --get remote.origin.url)" --revision "1.4.0@sha1:$(git rev-parse HEAD)"
flux4swarm controller --url oci://ghcr.io/acme/fleet --semver '1.x'
```

or the same objects in a `--config` file (`kind: OCIRepository`,
`spec.ref.semver`, `spec.secretRef` with a `.dockerconfigjson`).

## See drift before applying

`flux4swarm diff` takes the same flags as `reconcile` and prints a unified
diff of every service and config that would change, like `flux diff
kustomization`. Docker defaults, IDs and pinned image digests are left out,
so only real differences show. Secret values are never shown. It exits 0
when Swarm matches Git, 1 when it differs and 2 on errors, so it works as a
CI gate.

```console
$ flux4swarm diff --path ./clusters/prod
Kustomization flux-system/flux-system:
► Service/shop_web drifted (spec changed)
--- live/Service/shop_web
+++ desired/Service/shop_web
@@ -5,7 +5,7 @@
 Mode:
   Replicated:
-    Replicas: 2
+    Replicas: 1
  skipped: policy/v1 PodDisruptionBudget shop/web (kind has no Swarm equivalent)
```

## Manage a running controller

`get`, `suspend` and `resume` talk to a running controller's status API, like
the flux CLI talks to Kubernetes (`--server`, default
`http://127.0.0.1:9797` or `FLUX4SWARM_SERVER`):

```sh
flux4swarm get all                     # or: get kustomizations, get sources git; -A, -o json
flux4swarm suspend kustomization apps
flux4swarm resume kustomization apps   # waits until it reconciled (--wait=false to skip)
flux4swarm suspend source git flux-system
```

A suspend through the API is kept in memory until `resume` or a controller
restart; for a lasting one set `spec.suspend: true` in Git (which `resume`
cannot override). Set `--api-token` (or `FLUX4SWARM_API_TOKEN`) on the
controller to require `Authorization: Bearer <token>` on the POST endpoints;
the CLI sends the same variable.

## Deploy

As a Swarm service see [deploy/flux4swarm-stack.yaml](deploy/flux4swarm-stack.yaml).
Credentials (the `secretRef` of a GitRepository or SecretStore) are files in
`--credentials-dir` (default `/run/secrets`): `<name>/<key>` or
`<name>_<key>`, so Docker secrets such as `flux-system_password` work as is.
Status is served on `:9797`: `/status` (like `flux get all`, including the
`skipped` objects of each Kustomization and why), `/metrics`
(Flux-compatible Prometheus metrics, see [docs/monitoring.md](docs/monitoring.md)),
`/healthz`, `/readyz`, `POST /reconcile`,
`POST /suspend/{kind}/{namespace}/{name}` and `POST /resume/...`.

## Notifications and webhook receivers

Put Flux's own notification objects in the repository; stock Flux and
flux4swarm both use them. Alerts send the Kustomization and GitRepository
events of flux4swarm to Providers, and Receivers trigger a reconcile when Git
(or a registry) calls a webhook.

```yaml
apiVersion: notification.toolkit.fluxcd.io/v1
kind: Provider
metadata:
  name: slack
  namespace: flux-system
spec:
  type: slack             # or msteams, generic, generic-hmac, github, azuredevops
  channel: deployments
  secretRef:
    name: slack-webhook   # key "address" = the webhook URL
---
apiVersion: notification.toolkit.fluxcd.io/v1
kind: Alert
metadata:
  name: slack
  namespace: flux-system
spec:
  providerRef:
    name: slack
  eventSeverity: info
  eventSources:
    - kind: Kustomization
      name: '*'
    - kind: GitRepository
      name: '*'
---
apiVersion: notification.toolkit.fluxcd.io/v1
kind: Receiver
metadata:
  name: github
  namespace: flux-system
spec:
  type: github            # or gitlab, bitbucket, generic, generic-hmac, harbor, ...
  events: ["ping", "push"]
  secretRef:
    name: webhook-token   # key "token"
  resources:
    - kind: GitRepository
      name: flux-system
```

As with Git credentials, the secrets are files in `--credentials-dir`, e.g.
Docker secrets `slack-webhook_address` and `webhook-token_token`. For commit
statuses, point a `github` Provider at `https://github.com/acme/fleet`
(`token` = PAT, or the GitHub App keys) and an `azuredevops` Provider at
`https://dev.azure.com/acme/platform/_git/fleet` (`token` = PAT, or
`tenantId`/`clientId`/`clientSecret`), and alert on Kustomizations.

Receivers are served on `--receiver-listen` (default `:9292`, the port of
Flux's webhook-receiver) at the path Flux computes,
`/hook/sha256(token+name+namespace)`; `/status` shows it as `webhookPath`.
Publish only that port to the Git server, not `:9797`. Supported: Provider
types `generic`, `generic-hmac`, `slack`, `msteams`, `github`,
`azuredevops`; Receiver types `generic`, `generic-hmac`, `github`, `gitlab`,
`bitbucket`, `harbor`, `dockerhub`, `quay`, `nexus`, `acr`. CEL expressions
(`commitStatusExpr`, `resourceFilter`) are not evaluated.

## How Kubernetes objects map to Swarm

| Kubernetes | Swarm |
|------------|-------|
| Namespace `shop` | Docker stack `shop`: names `shop_<name>`, label `com.docker.stack.namespace`, network `shop_default` (the `--default-namespace`, `default` unless set, has no prefix) |
| Deployment / StatefulSet / DaemonSet / Job | replicated / replicated (slot-templated volumes) / global / replicated-job service |
| Service `LoadBalancer` / `NodePort` | published ports (`externalTrafficPolicy: Local` = host mode) |
| Service (any) | network alias, so `http://<service>` resolves like in Kubernetes |
| ConfigMap / Secret volume, env, envFrom | Docker configs / secrets (one per key, versioned by content) or env values |
| ExternalSecret + SecretStore | value fetched from Vault, Azure Key Vault or the fake provider, then a Docker secret |
| PersistentVolumeClaim | named volume `<ns>_<claim>`; driver and options from annotations |
| emptyDir / hostPath | anonymous volume or tmpfs / bind mount |
| HTTPRoute (+ Gateway, Traefik Middleware) | Traefik Swarm provider labels on the backend service |
| imagePullSecrets | registry auth for the service |

Swarm-only settings are annotations on the workload, which Flux ignores:
`swarm.flux4swarm.io/name`, `swarm.flux4swarm.io/networks`,
`swarm.flux4swarm.io/service-spec` (JSON merge patch on the Docker
ServiceSpec), `swarm.flux4swarm.io/container` (the container of a
multi-container pod that runs) and `swarm.flux4swarm.io/ignore`. Flux
components (`app.kubernetes.io/part-of: flux`) and kinds with no Swarm
meaning (RBAC, Ingress, CRDs, HPA, ...) are skipped; each skipped object and
the reason is listed under `skipped` in `/status` and in `reconcile` and
`diff` output. Fields Swarm cannot run are dropped and listed under
`warnings`; `--strict` makes them fail the Kustomization instead. Flagger
Canary targets roll back a failed update and autoscaled workloads run
`minReplicas`
([details](docs/kubernetes-and-swarm.md)).

## Git authentication

| Host | How |
|------|-----|
| GitHub | PAT: `username` + `password`. GitHub App: `provider: github` with `githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`. |
| Azure DevOps | PAT: `username` + `password`. Service principal: `provider: azure` with `tenantId`, `clientId`, `clientSecret`. |
| Any | `bearerToken`, or SSH with `identity` + `known_hosts`. |

## Flux compatibility notes

The most Flux compatible option was chosen wherever Swarm differs:

* One process replaces source-controller, kustomize-controller and
  notification-controller. Status is
  served over HTTP because Swarm has no custom resources.
* Builds use kustomize like Flux, including the generated
  `kustomization.yaml`, `targetNamespace`, `namePrefix/Suffix`, `patches`,
  `images`, `components`, `commonMetadata` and `postBuild.substitute`.
  `postBuild.substituteFrom` reads directories in the credentials directory.
* SOPS decryption is not supported; SOPS encrypted Secrets are skipped, use
  ExternalSecrets.
* Docker configs and secrets are immutable, so they are versioned
  (`<name>-<hash>`) and services roll when content changes.
* `provider: azure` uses a service principal instead of workload identity.
* Pods with more than one container cannot be one Swarm service: only the
  main container runs (`swarm.flux4swarm.io/container`, else the first).

## License

Apache License 2.0, see [LICENSE](LICENSE). Files under
`internal/swarm/tfdocker/` are derived from
[kreuzwerker/terraform-provider-docker](https://github.com/kreuzwerker/terraform-provider-docker)
and stay under the Mozilla Public License 2.0; see [NOTICE](NOTICE) and
[LICENSES/MPL-2.0.txt](LICENSES/MPL-2.0.txt).
