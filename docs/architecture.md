# flux4swarm architecture

flux4swarm lets one Git repository drive both stock Flux v2 on Kubernetes
and Docker Swarm. The repository contains only formats Flux understands:
Flux objects, plain Kubernetes manifests, External Secrets Operator (ESO)
objects, Gateway API routes and (optionally) Traefik Middleware CRDs.
flux4swarm reproduces what Flux's source-controller and kustomize-controller
do, and replaces "apply to the Kubernetes API" with "translate to Docker
Swarm and apply to the Docker Engine API".

```
 Git (GitHub, Azure DevOps, any)            flux4swarm (one process on a Swarm manager)
 ───────────────────────────────            ───────────────────────────────────────────
 clusters/prod/flux-system/      ──fetch──▶ source reconciler (GitRepository: go-git,
   gotk-sync.yaml                                │  OCIRepository: OCI distribution API)
                                                 │ artifact <branch>@sha1:<commit>, <tag>@sha256:<digest>
 clusters/prod/apps/<ns>/*.yaml                  ▼
   Deployment, Service, ConfigMap,          kustomization reconciler
   PVC, ExternalSecret, HTTPRoute ...         1. kustomize build (krusty), Flux overlays,
                                                 postBuild substitution
 Secret stores                                2. resolve ExternalSecrets ◀── Vault / Azure Key Vault / fake
                                              3. HTTPRoute → Traefik labels
                                              4. Kubernetes → Swarm translation
                                              5. apply, wait, prune ──────────▶ Docker Engine API
                                            status: /status /metrics /healthz /readyz
```

## Modes

| Mode | Command | Flux equivalent |
|------|---------|-----------------|
| GitOps | `flux4swarm controller`, `flux4swarm reconcile` (one pass) | source-controller + kustomize-controller + notification-controller |
| Import | `flux4swarm import` | `flux bootstrap` + exporting existing workloads |

## Packages

| Package | Role |
|---------|------|
| `internal/git` | go-git fetch (Flux revision format) and commit/push; auth for PAT, bearer, SSH, GitHub App, Azure Entra ID |
| `internal/oci` | OCIRepository fetch: Flux artifacts (`flux push artifact`) over the OCI distribution API, basic/bearer auth |
| `internal/source` | artifact type shared by the fetchers, semver tag selection |
| `internal/build` | Flux-compatible kustomize build |
| `internal/api` | Flux and ESO types, decoding of a build into typed Kubernetes objects |
| `internal/secrets` | ESO providers: `vault`, `azurekv`, `fake` |
| `internal/traefik` | HTTPRoute/Middleware ↔ Traefik Swarm provider labels, Traefik rule parser |
| `internal/translate` | Kubernetes workloads → Swarm service specs, configs, secrets, networks |
| `internal/render` | traefik + translate for one build |
| `internal/swarm` | applier: ownership labels, versioned configs/secrets, drift detection, prune, wait |
| `internal/swarm/tfdocker` | code derived from terraform-provider-docker (MPL-2.0) |
| `internal/controller` | reconcile loop, dependency ordering, status and metrics, events and Receiver webhooks |
| `internal/notify` | notification-controller: Alert matching, Providers (generic, generic-hmac, Slack, Microsoft Teams, GitHub and Azure DevOps commit status), Receiver validation |
| `internal/importer` | Swarm → Kubernetes manifests with lossless round trip |

## Build (kustomize-controller compatible)

1. The source checkout is copied to a temporary directory (symlinks that
   leave the source are dropped, like Flux's secure filesystem).
2. If `spec.path` has no `kustomization.yaml`, one is generated listing
   every manifest file and every sub-directory that has its own
   kustomization, as Flux does.
3. The Kustomization's `targetNamespace`, `namePrefix`, `nameSuffix`,
   `patches`, `images` and `components` are written into that
   kustomization, then `krusty` builds it (`LoadRestrictionsNone`).
4. `commonMetadata` is applied to every object, then `postBuild.substitute`
   (and `substituteFrom`) variables are replaced with drone/envsubst,
   honouring `kustomize.toolkit.fluxcd.io/substitute: disabled`.
5. Objects are decoded. Objects without a namespace go to `default`.
   Objects labelled `app.kubernetes.io/part-of: flux` (gotk-components),
   annotated `swarm.flux4swarm.io/ignore: "true"` or
   `kustomize.toolkit.fluxcd.io/reconcile: disabled`, and kinds with no
   Swarm meaning are recorded as ignored.

## Sources

* **GitRepository**: `spec.ref` `branch`, `tag`, `semver`, `name` and
  `commit` with Flux's precedence (commit, name, semver, tag, branch).
  `semver` lists the remote tags and picks the highest one that satisfies
  the range (Masterminds/semver syntax, the same as Flux), so the revision
  is `<tag>@sha1:<commit>`.
* **OCIRepository** (`source.toolkit.fluxcd.io/v1`): artifacts pushed with
  `flux push artifact`. `spec.ref` `digest`, `semver` (+ `semverFilter`)
  and `tag` (default `latest`); the revision is `<tag>@sha256:<digest>` (or
  `sha256:<digest>` for a digest ref), like Flux. The first layer, or the
  one matching `layerSelector.mediaType`, is extracted (`operation: extract`).
  Credentials come from `spec.secretRef` (a `.dockerconfigjson` or
  `username`/`password` in the credentials directory), else from the
  controller's `--registry-config`; `certSecretRef` (`ca.crt`, `tls.crt`,
  `tls.key`) and `insecure: true` (plain HTTP) are supported. Not supported:
  `verify` (cosign/notation), `layerSelector.operation: copy` and the cloud
  `provider`s (aws, azure, gcp); objects using them are not Ready.
  Tarballs are checked against their digest, and entries that leave the
  artifact root, links and special files are rejected.

GitRepository, OCIRepository and Kustomization objects found in a build are registered
with the controller, which gives the usual self-managing `flux-system`
setup. They are garbage collected when removed from Git if the parent has
`prune: true`.

## Translation

### Naming: namespace = stack

A Kubernetes namespace is a Docker stack, exactly like `docker stack
deploy`: object `web` in namespace `shop` becomes `shop_web` with label
`com.docker.stack.namespace=shop`; namespace `default` has no prefix. Every
workload joins the overlay network `<ns>_default` (created on demand) and
gets the names of the Kubernetes Services that select it as network aliases,
so in-cluster DNS names keep working.

### Workloads

| Kubernetes | Swarm ServiceSpec |
|------------|-------------------|
| Deployment `replicas` | `Mode.Replicated.Replicas` |
| DaemonSet | `Mode.Global` |
| StatefulSet | replicated; `volumeClaimTemplates` become volumes `<ns>_<tpl>-<sts>-{{.Task.Slot}}` |
| Job `parallelism`/`completions`/`backoffLimit`, `restartPolicy` | `Mode.ReplicatedJob`, `RestartPolicy` |
| Deployment strategy with surge (default) | `UpdateConfig{Parallelism: 1, Order: start-first}`; `maxSurge: 0` keeps the Swarm default |
| container `image`, `command`, `args`, `workingDir`, `tty`, `stdin` | `Image`, `Command`, `Args`, `Dir`, `TTY`, `OpenStdin` |
| `env` (`value`, `configMapKeyRef`, `secretKeyRef`, `fieldRef` name/node/namespace), `envFrom` | `Env` (values inlined) |
| `securityContext` runAsUser/Group, readOnlyRootFilesystem, capabilities | `User`, `ReadOnly`, `CapabilityAdd/Drop` |
| `resources.limits/requests` cpu, memory | `Resources.Limits/Reservations` |
| `nodeSelector` (hostname, os, arch, control-plane, others) | placement constraints |
| exec liveness/readiness/startup probe | `Healthcheck` |
| `terminationGracePeriodSeconds`, `hostname`, `dnsConfig`, `hostAliases` | `StopGracePeriod`, `Hostname`, `DNSConfig`, `Hosts` |
| `imagePullSecrets` (dockerconfigjson) | registry auth on create/update |
| workload `metadata.labels` | service labels |

One container per pod: Swarm services run one container, so pods with
sidecars are rejected with a clear error. Init containers are ignored with a
warning.

### Volumes

| Volume | Mount |
|--------|-------|
| `configMap` (items, defaultMode, subPath) | one Docker config per key, file at `mountPath/<path>` or `mountPath` with `subPath` |
| `secret` | one Docker secret per key, same rules |
| `persistentVolumeClaim` | named volume `<ns>_<claim>` (stack label added); `swarm.flux4swarm.io/volume-driver` and `swarm.flux4swarm.io/volume-options` (JSON) on the PVC set the driver |
| `emptyDir` / `emptyDir: {medium: Memory}` | anonymous volume / tmpfs (with `sizeLimit`) |
| `hostPath` | bind mount |

Volumes are never deleted by prune.

### Services and ports

`LoadBalancer` Services publish `port` (ingress mode, host mode with
`externalTrafficPolicy: Local`), `NodePort` Services publish `nodePort`.
Named `targetPort`s are resolved against the container ports. Headless
Services switch the endpoint mode to `dnsrr`. ClusterIP Services only add
aliases (Swarm VIPs cannot remap ports).

### Secrets

ExternalSecrets are resolved into the Secret ESO would create
(`spec.target.name`, one key per `spec.data[].secretKey`; `dataFrom` is not
supported). Plain Kubernetes Secrets are used as is; SOPS encrypted Secrets
are rejected. Store credentials (`{name, key}` references) are files in the
credentials directory: `<dir>/<name>/<key>` or `<dir>/<name>_<key>`.

### Traefik and Gateway API

Each HTTPRoute rule becomes a Traefik router on the Swarm service behind its
first `backendRef` (found through the Kubernetes Service selector):

| Gateway API | Traefik label |
|-------------|---------------|
| route name (`<name>-<i>` for several rules) | router and service name |
| `hostnames`, `matches` (path prefix/exact/regex, method, headers, query) | `routers.<r>.rule` |
| `parentRefs[].sectionName` (listener) | `routers.<r>.entrypoints` |
| parentRef without `sectionName` | no `entrypoints` label (all entrypoints) |
| HTTPS listener on the Gateway (or `websecure`); without `sectionName` only when every listener is HTTPS | `routers.<r>.tls=true` |
| annotations `swarm.flux4swarm.io/traefik-certresolver`, `-priority` | `tls.certresolver`, `priority` |
| backendRef port → container port | `services.<r>.loadbalancer.server.port` |
| `RequestRedirect` scheme | `redirectscheme` middleware |
| `URLRewrite` ReplacePrefixMatch `/` / ReplaceFullPath | `stripprefix` / `replacepath` middleware |
| `RequestHeaderModifier` / `ResponseHeaderModifier` | `headers.customrequestheaders` / `customresponseheaders` |
| `ExtensionRef` to `traefik.io/v1alpha1` Middleware | the Middleware spec flattened into `middlewares.<name>.*`; `basicAuth.secret` users are read from the Secret |

Import does the reverse with a parser for the rule subset that HTTPRoute
can express. Routers it cannot express stay as labels (through the
service-spec annotation).

## Applying to Swarm

* **Ownership** uses kustomize-controller's labels
  `kustomize.toolkit.fluxcd.io/name` and `/namespace`, plus
  `app.kubernetes.io/managed-by: flux4swarm`. Prune only touches owned
  objects.
* **Configs and secrets** are immutable in Swarm, so each is created as
  `<logical name>-<hash>` with label `flux4swarm.io/name`; service
  references are rewritten to the current version. Config hashes are
  SHA-256 of the content; secret hashes are HMAC-SHA256 keyed with the
  Swarm cluster ID so names reveal nothing portable about values. Old
  versions are deleted once neither the service spec nor its previous spec
  (needed for rollback) uses them. On adoption an unmanaged config with
  identical content is reused, so taking over a Swarm does not roll services
  for nothing.
* **Services** carry `flux4swarm.io/spec-hash`, a hash of the desired spec.
  A service is updated when the hash differs, when it is not labelled yet
  (adoption) or when image, replicas or labels drifted. Updates send the
  registry auth and query the registry for the digest like `docker service
  update`. With `spec.force` a failing update recreates the service.
* **Wait** (`spec.wait`) uses the convergence checker from
  terraform-provider-docker: all replicas running in their slots on active
  nodes, update completed; rollbacks and paused updates fail the
  Kustomization. Jobs (replicated jobs) must complete their current
  iteration, like a Job's `Complete` condition in Flux; a job whose tasks
  failed with no retry left fails the Kustomization.
* **Health checks** (`spec.healthChecks`, ignored with `spec.wait` as in
  Flux) list Deployments, StatefulSets, DaemonSets and Jobs, also ones
  another Kustomization applied: the Swarm service name comes from the
  build that applied the workload (so `swarm.flux4swarm.io/name` works),
  else from the `NS_name` convention, and the service is checked like
  `spec.wait` does. Kustomization and GitRepository entries must be Ready.
  Kinds with no Swarm meaning (Service, ConfigMap, ...) count as healthy;
  other Flux kinds (HelmRelease, ...) are rejected.
* **dependsOn** waits until each dependency is Ready and, when it uses the
  same source, has applied the source's current revision, like
  kustomize-controller.
* **Prune** (`spec.prune`) removes owned services, then unused configs,
  secrets and networks that left the build.

## Notifications (notification-controller compatible)

`Provider`, `Alert` and `Receiver` objects
(`notification.toolkit.fluxcd.io/v1`, also read as `v1beta3`/`v1beta2`)
are discovered in builds like `GitRepository` and `Kustomization`, or come
from `--config`. They are registered as children of the Kustomization that
applied them and are removed with it when it prunes.

* **Events.** The reconcilers emit Flux-shaped events
  (`event.toolkit.fluxcd.io/v1` JSON): `source-controller` sends
  `NewArtifact` and `GitOperationFailed`; `kustomize-controller` sends the
  applied change set (reason `Progressing`), errors (the Ready reason, for
  example `BuildFailed`, `HealthCheckFailed`) and, after every successful
  reconciliation, `Reconciliation finished in ...` with
  `commit_status: update`. Metadata keys carry the object's API group, as in
  Flux, and are stripped before delivery.
* **Dispatch** follows notification-controller: event sources match by
  kind, namespace (default the Alert's), name or `*` with `matchLabels`,
  severity, `inclusionList`/`exclusionList`; metadata is merged from
  `event.toolkit.fluxcd.io/*` annotations, `eventMetadata` and the
  controller; identical events are suppressed for 5 minutes; commit status
  updates only go to Git and generic providers, and Git providers only get
  those and errors. Sends are asynchronous with retries; failures are logged
  and shown as a warning on the Alert's status. Nothing is sent with
  `--dry-run`.
* **Providers.** `generic` posts the event (with `gotk-component` header and
  the `headers` of the secret), `generic-hmac` adds `X-Signature:
  sha256=<hmac>`, `slack` and `msteams` use Flux's payloads (Teams uses the
  legacy MessageCard for `*.webhook.office.com` and Adaptive Cards
  otherwise), `github` and `azuredevops` set commit statuses on the commit
  in the revision with context `<kind>/<name>/<id>`, skipping duplicates.
  Secrets (`address`, `token`, `username`, `password`, `headers`, GitHub App
  or Entra ID service principal keys), `proxySecretRef` and
  `certSecretRef` are read from the credentials directory. Swarm has no
  object UIDs, so the commit status id is derived from the Provider's
  namespace and name. `commitStatusExpr` (CEL) falls back to the default.
* **Receivers** are served on `--receiver-listen` (default `:9292`, like
  notification-controller's webhook-receiver) at
  `/hook/sha256(token+name+namespace)`, the same path Flux computes, which
  is also shown as `webhookPath` in `/status`. Requests are validated per
  type (`generic`, `generic-hmac`, `github`, `gitlab`, `bitbucket`,
  `harbor`, `dockerhub`, `quay`, `nexus`, `acr`) and `spec.events`; the
  listed resources (by name, or `*` with `matchLabels`) are reconciled
  immediately. CEL filters (`resourceFilter`, `resources[].filter`) and the
  `generic-oidc`, `gcr` and `cdevents` types are reported as not supported.

## Import

`flux4swarm import` reads services, configs, secrets and networks and writes
the layout shown in the README. For each service it:

1. cleans runtime data (IDs, digests unless `--adopt`, daemon defaults such
   as platforms, isolation and runtime),
2. builds the Kubernetes workload, Services (ClusterIP for Traefik backend
   ports, LoadBalancer for published ports), ConfigMaps, ExternalSecrets,
   PVCs, HTTPRoutes and Middlewares,
3. renders those manifests back through the same translation used by the
   controller and stores the JSON merge patch between the result and the
   live spec in `swarm.flux4swarm.io/service-spec`. The import fails if the
   patch does not reproduce the live spec, so reconciling an import is
   lossless.
4. with `--adopt`, after the files are written, adds the owner and
   `flux4swarm.io/spec-hash` labels to each live service whose rendered spec
   matches, so the first reconcile is a no-op.

Secret values are not readable from Docker; the generated SecretStore
(`vault`, `azurekv` or `fake` template) and ExternalSecrets name the keys
to fill in. Output goes to `--output` or is committed and pushed to
`--git-url` (GitHub, Azure DevOps or any Git server; an empty repository is
initialised).

## Code from terraform-provider-docker

`internal/swarm/tfdocker` is adapted from
[kreuzwerker/terraform-provider-docker](https://github.com/kreuzwerker/terraform-provider-docker)
under MPL-2.0: the service convergence checker (task-by-slot tracking,
update/rollback state machine, numbered task states) and the registry auth
selection and Docker Hub hostname helpers. The Terraform SDK was removed;
the logic is unchanged. The files keep their MPL-2.0 headers (see `NOTICE`).

## Limitations

* Run one controller replica on a manager; there is no leader election.
* Helm and Bucket sources, OCI artifact verification, image automation and
  SOPS are not implemented. Notification Provider types other than the six listed above
  are reported as not supported.
* Status lives in memory, so after a restart every Kustomization sends its
  "Reconciliation finished" event again (Git providers skip unchanged
  commit statuses).
* Kubernetes features without a Swarm counterpart (multi-container pods,
  ClusterIP port remapping, HPA, RBAC, NetworkPolicy, Ingress) are rejected
  or ignored as described above.
