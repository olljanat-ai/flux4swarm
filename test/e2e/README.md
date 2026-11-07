# flux4swarm end-to-end tests

These tests run the real `flux4swarm` binary against a real single-node
Docker Swarm, and check that **the same Git repo is deployable by stock
Flux2** on a kind cluster. The repo format must stay 100% Flux2 compatible,
so one repo can drive both Swarm (flux4swarm) and Kubernetes (Flux2).

- **Swarm**: by default a privileged `docker:29-dind` container named
  `flux4swarm-e2e-dind` running `docker swarm init`. Its API is published on
  `tcp://127.0.0.1:23750`, so your own Docker daemon is never put into swarm
  mode. The test images are pulled by your daemon and loaded into DinD
  (`docker save | docker load`), so DinD needs no registry or proxy access.
- **Kubernetes** (`scripts/k8s-up.sh`): a kind cluster `flux4swarm-e2e` with
  stock Flux2 (`flux install`, source-controller, kustomize-controller and
  notification-controller),
  External Secrets Operator (Helm chart), the Gateway API CRDs (standard
  channel) and the Traefik CRDs (no gateway or Traefik controller runs). Every
  image is pulled by your daemon and loaded into the node (`kind load`), so the
  cluster needs no registry access, in CI and locally alike.
- **Git**: each test creates its own bare repo with one commit on `main`. When
  the K8s side is up, the repos live in `$E2E_GIT_ROOT` and are served by a
  small smart-HTTP git server (`gitserver/`, fetch and push, no auth) running
  in an `alpine/git` container on the `kind` docker network:
  `http://127.0.0.1:23780/<repo>` from the host and
  `http://flux4swarm-e2e-git:8080/<repo>` from pods. Flux's GitRepository
  only accepts http(s)/ssh URLs, so `git://` and `file://` cannot be used.
  flux4swarm is given the HTTP URL too; without the K8s side it gets
  `file://`.
- **Controller**: runs on the host in one-shot mode and reaches Swarm through
  `DOCKER_HOST`.

The tests are a separate Go module (`test/e2e/go.mod`, stdlib only, build tag
`e2e`). They call the `docker`, `git`, `kubectl` and `flux` CLIs through
`os/exec`.

## Running

```sh
make e2e                      # build (if cmd/flux4swarm exists) -> up (Swarm + kind) -> test -> down
make e2e E2E_K8S=0            # Swarm side only
make e2e E2E_KEEP=1           # leave everything running afterwards
make e2e E2E_RUN=TestSameRepo
make e2e FLUX4SWARM_BIN=/path/to/flux4swarm
E2E_MODE=host make e2e        # use the current daemon for Swarm (no DinD)

# Step by step:
make e2e-up e2e-k8s-up
make e2e-test                  # or: cd test/e2e && DOCKER_HOST=tcp://127.0.0.1:23750 \
                               #   PATH=$PWD/.bin:$PATH go test -tags e2e -v -count=1 ./...
make e2e-down e2e-k8s-down
```

`make e2e` always runs `e2e-down` and `e2e-k8s-down`, even when the tests
fail, unless you set `E2E_KEEP=1`. It keeps the failing exit code.

`k8s-up.sh` uses `kind`, `kubectl`, `flux` and `helm` from `PATH` if their
versions match the pins in `scripts/env.sh`; otherwise it downloads them into
`test/e2e/.bin` and checks the release checksums (kind/flux from GitHub
releases, kubectl from dl.k8s.io, helm from get.helm.sh; if GitHub or
get.helm.sh are unreachable, kind/helm fall back to `go install`, which the Go
checksum database verifies). The ESO chart, Gateway API and Traefik CRD files
are checked against pinned SHA-256 sums. It also adapts the kind config to
the host: kubelet `failCgroupV1: false` on cgroup v1 hosts, and containerd
`restrict_oom_score_adj` when the host cannot lower `oom_score_adj` (some
sandboxes). Neither applies on GitHub runners.

If your network cannot reach a registry (e.g. ghcr.io), pull or build the
images under the same names beforehand: `k8s-up.sh` only pulls images that
are not already present in your daemon.

| Variable | Default | Meaning |
|---|---|---|
| `FLUX4SWARM_BIN` | `<repo>/bin/flux4swarm` | Controller binary. If it is missing, the controller tests are **skipped**. |
| `E2E_MODE` | `dind` | `dind`, or `host` to use the current daemon. In `host` mode the scripts run `swarm init` if needed and `swarm leave` in down only if up ran the init. |
| `E2E_K8S` | `1` in make | `0` skips the kind side (its tests skip). With `1`, a missing cluster fails the K8s tests; unset (plain `go test`), they skip if it is not up. |
| `E2E_KEEP` | unset | Set to anything to skip teardown. |
| `E2E_RUN` | unset | `go test -run` regex. |
| `E2E_DIND_PORT` | `23750` | Host port for the DinD API. |
| `E2E_GIT_PORT` | `23780` | Host port of the git server. |
| `E2E_STATE_DIR` | `$TMPDIR/flux4swarm-e2e` | kubeconfig (`kubeconfig`), git repos (`git/`), downloaded manifests. |
| `E2E_DIND_IMAGE` | `docker:29-dind` | DinD image. |
| `E2E_IMAGES` | `nginx:alpine nginx:stable-alpine` | Workload images loaded into the swarm and into kind. |
| `E2E_GO_TEST_FLAGS` | `-v -count=1 -timeout 40m` | Extra `go test` flags. |
| `KIND_VERSION`, `KIND_NODE_IMAGE`, `KUBECTL_VERSION`, `FLUX_VERSION`, `HELM_VERSION`, `ESO_CHART_VERSION`, `GATEWAY_API_VERSION`, `TRAEFIK_VERSION` | see `scripts/env.sh` | Pinned versions (kind v0.33.0 / Kubernetes v1.37.0, Flux v2.10.0, ESO chart 2.14.0, Gateway API v1.7.0, Traefik v3.7.17 CRDs). |

## Tests

| Test | Needs | What it checks |
|---|---|---|
| `TestSwarmSmoke` | Swarm | The swarm is active and a manager. It can create, run and remove a service. |
| `TestFlux2Smoke` | kind | Stock Flux2 fetches a test repo from the git server and applies a Deployment + ConfigMap in `clusters/e2e/apps/NS/`; the Deployment becomes available; removing it from Git prunes it. |
| `TestImport` | binary | Creates a service in stack `NS` (label, env, 1 replica) that mounts a config and a secret, runs `import --adopt --bootstrap`, and checks the Flux bootstrap layout: `flux-system/{gotk-sync,kustomization}.yaml`, `apps/NS/{namespace,kustomization,secretstore}.yaml`, `deployment-web.yaml` (image, env, `replicas: 1`), a `configmap-*.yaml` with the config data and an `externalsecret-*.yaml`. The secret value must not appear anywhere in Git. |
| `TestRoundTripNoop` | binary | A service without secrets: `import --adopt` must already relabel it as managed, and two reconciles must not change it (same version, task template and running tasks). |
| `TestReconcileService` | binary | Deployment `web` in namespace `NS`: create (service `NS_web`, image, 1 replica, labels `managed-by` and `com.docker.stack.namespace`), update (new tag, 2 replicas), prune (remove `apps/NS` and reconcile with `--prune`). An unmanaged service must survive. |
| `TestReconcileConfigAndSecret` | binary | A ConfigMap and an ESO `ExternalSecret` (fake `SecretStore`) mounted by a Deployment: exactly one Swarm config `NS_web-config_greeting-HASH10` and one secret `NS_web-secret_greeting-HASH10` (label `flux4swarm.io/name`), and the files in the running container have the right contents. |
| `TestSameRepoSwarmAndFlux2` | binary + kind | One repo with a real bootstrap `flux-system/` (pinned `flux install --export` + `flux create … --export`) and an app (Deployment + ConfigMap + SecretStore + ExternalSecret). `flux-system/` passes `kubectl kustomize`, a server-side dry-run against the Flux CRDs and `flux build kustomization --dry-run`. flux4swarm reconciles it to Swarm and Flux2 to kind; both must have the same image/replicas, the config and secret (Swarm objects by label and file contents in the container; the K8s Secret created by ESO). Then image + replicas change in Git and both converge; then the app is removed and both prune. |
| `TestImportIsFlux2Valid` | binary + kind | Imports the same fixture as `TestImport`, validates `flux-system/` as above and every `kustomization.yaml` with `kubectl kustomize`, then lets Flux2 apply `clusters/e2e`: the Kustomization must become Ready and the Deployment, ConfigMap, ExternalSecret and SecretStore must exist in kind. |
| `TestVolumeRoundTrip` | binary (+ kind) | A service with a named volume `NS_data` and a bind mount of `/tmp` is imported as `persistentvolumeclaim-data.yaml` (RWO, 1Gi, no storageClassName) and a Deployment with `claimName: data` + `hostPath /tmp`; reconcile back is a no-op as in `TestRoundTripNoop` and keeps the mounts; subtest `flux2` applies it with Flux2 and checks the PVC and volumes in kind. |
| `TestTraefikLabelsToGateway` | binary (+ kind) | A service with `traefik.http.routers.web.rule=Host(...) && PathPrefix(/)`, `entrypoints=web` and `loadbalancer.server.port=80` is imported as `httproute-web.yaml` (hostname, PathPrefix `/`, parentRef `traefik/traefik` section `web`, backend port 80), a `service-*.yaml` and `apps/traefik/gateway.yaml` (GatewayClass + Gateway, listener 80/HTTP). Reconcile back keeps equivalent Traefik labels. Subtest `flux2` applies it with Flux2 and checks the HTTPRoute, its backend Service, the Gateway and the GatewayClass in kind. |

In kind, each test applies its own `GitRepository` + `Kustomization` (in
`flux-system`, named `e2e-sync-*`) over `./clusters/e2e`, the same path
flux4swarm reconciles. The repo's own `flux-system/` directory is excluded
with `spec.ignore`, so a test never replaces the cluster's Flux installation;
that directory is validated by the dry-runs above instead. Imported trees are
applied with `wait: false`, since `--adopt` may pin image digests that are not
preloaded into kind. Deleting the Kustomization at the end (`prune: true`)
removes what it applied.

## flux4swarm contract assumed by the tests

All of it is in the "flux4swarm contract" section of `helpers_test.go`
(`importToGit`, `reconcile`, `importFlags`, `swarmServiceName`,
`versionedNameRE`, the `imported*` paths and the label constants), plus the
manifest templates below it. If the contract changes, adjust it there.
Source: `/mnt/project-files/flux4swarm/e2e-contract.md` and the PR #3 author.

- Build: `make build` writes `bin/flux4swarm` (`./cmd/flux4swarm`).
- Import (Swarm to Git):
  `flux4swarm import --cluster e2e --git-url URL --branch main --path clusters/e2e --adopt --bootstrap --secret-store fake --secret-store-provider fake`.
  The binary clones, commits and pushes; URL is the smart-HTTP URL (no auth)
  or `file://`.
- Reconcile (Git to Swarm, one-shot):
  `flux4swarm reconcile --url URL --branch main --path ./clusters/e2e --wait --timeout 3m [--prune]`.
  It exits non-zero if the result is not Ready.
- Layout: standard Flux bootstrap layout.
  `clusters/e2e/flux-system/{gotk-components.yaml (optional), gotk-sync.yaml, kustomization.yaml}`;
  apps in `clusters/e2e/apps/NS/` with a `kustomization.yaml` per directory.
  flux4swarm builds the path like kustomize-controller (generates a
  kustomization.yaml when none exists) and skips `gotk-components` objects
  (label `app.kubernetes.io/part-of=flux`) and kinds with no Swarm meaning
  (Namespace, CRDs, RBAC, …).
- Import writes `apps/NS/{namespace,kustomization,secretstore}.yaml`,
  `deployment-X.yaml`, `service-X.yaml`, `configmap-X.yaml`,
  `externalsecret-X.yaml`, `persistentvolumeclaim-X.yaml`, `httproute-R.yaml`
  (and `middleware-M.yaml`), plus `apps/traefik/gateway.yaml`. The tests look
  up ConfigMaps/ExternalSecrets/Services by glob (`configmap-*.yaml`, …), and
  Deployments/PVCs/HTTPRoutes by exact name.
- Mapping: namespace = stack. Deployment `web` in namespace `NS` is Swarm
  service `NS_web` with label `com.docker.stack.namespace=NS` (namespace
  `default` gives `web`). image, env, replicas map 1:1. configMap/secret
  volumes become one Docker config/secret per key, mounted at
  `mountPath/key`, named `NS_OBJECT_KEY-HASH10` (no `NS_` in `default`) with
  label `flux4swarm.io/name`.
- Secrets: ESO `ExternalSecret` (`external-secrets.io/v1`) + `SecretStore`
  with the fake provider (`spec.provider.fake.data: [{key, value}]`) work on
  both sides. On Swarm the secret name uses the ExternalSecret's target name.
- Volumes: a Swarm volume `NS_data` is imported as PVC `data` (RWO, 1Gi, no
  storageClassName) and comes back as volume `NS_data`; a bind mount is a
  `hostPath` volume. Prune never deletes volumes.
- Traefik: router `R` becomes HTTPRoute `R` (parentRef `traefik/traefik`,
  sectionName = entrypoint, backendRef port = loadbalancer port) plus a
  ClusterIP Service; `apps/traefik/gateway.yaml` holds GatewayClass `traefik`
  (`traefik.io/gateway-controller`) and Gateway `traefik/traefik` (listener
  `web` 80/HTTP). Reconcile of a single-rule HTTPRoute writes back
  `traefik.enable=true`, `traefik.http.routers.R.{rule,entrypoints}` and
  `traefik.http.services.R.loadbalancer.server.port` (the tests compare the
  rule ignoring whitespace).
- Managed Swarm objects carry `app.kubernetes.io/managed-by=flux4swarm`,
  `kustomize.toolkit.fluxcd.io/{name,namespace}` and
  `com.docker.stack.namespace`. Prune removes only managed objects.
  Unlabelled objects are left alone.
- `import --adopt` keeps running image digests and relabels the live services
  as owned by Kustomization `flux-system/flux-system`, so the next reconcile
  is a no-op (services that use secrets are relabelled by the first
  reconcile instead; the no-op tests use services without secrets).
- The tests add `io.flux4swarm.e2e=true` to the objects they create. With
  `E2E_MODE=host`, `down.sh` removes only objects that have this label.

The tests run one after another, because `import` exports every object in
the swarm.
