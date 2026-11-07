# flux4swarm end-to-end tests

These tests run the real `flux4swarm` binary against a real single-node
Docker Swarm, with a local bare Git repo (`file://` URL) as the GitOps source.

- **Swarm**: by default a privileged `docker:29-dind` container named
  `flux4swarm-e2e-dind` running `docker swarm init`. Its API is published on
  `tcp://127.0.0.1:23750`, so your own Docker daemon is never put into swarm
  mode. The test images are pulled by your daemon and loaded into DinD
  (`docker save | docker load`), so DinD needs no registry or proxy access.
- **Git**: each test creates its own bare repo in a temp dir, seeded with one
  commit on `main`.
- **Controller**: runs on the host in one-shot mode and reaches Swarm through
  `DOCKER_HOST`.

The tests are a separate Go module (`test/e2e/go.mod`, stdlib only, build tag
`e2e`). They call the `docker` and `git` CLIs through `os/exec`.

## Running

```sh
make e2e                      # build (if cmd/flux4swarm exists) -> up -> test -> down
make e2e E2E_KEEP=1           # leave the swarm running afterwards
make e2e E2E_RUN=TestReconcile
make e2e FLUX4SWARM_BIN=/path/to/flux4swarm
E2E_MODE=host make e2e        # use the current daemon directly (no DinD)

# Step by step:
make e2e-up
export DOCKER_HOST=tcp://127.0.0.1:23750
cd test/e2e && go test -tags e2e -v -count=1 ./...
make e2e-down
```

`make e2e` always runs `e2e-down`, even when the tests fail, unless you set
`E2E_KEEP=1`. It also keeps the failing exit code.

| Variable | Default | Meaning |
|---|---|---|
| `FLUX4SWARM_BIN` | `<repo>/bin/flux4swarm` | Controller binary. If it is missing, the controller tests are **skipped**. |
| `E2E_MODE` | `dind` | `dind`, or `host` to use the current daemon. In `host` mode the scripts run `swarm init` if needed and `swarm leave` in down only if up ran the init. |
| `E2E_KEEP` | unset | Set to anything to skip teardown. |
| `E2E_RUN` | unset | `go test -run` regex. |
| `E2E_DIND_PORT` | `23750` | Host port for the DinD API. |
| `E2E_DIND_IMAGE` | `docker:29-dind` | DinD image. |
| `E2E_IMAGES` | `nginx:alpine nginx:stable-alpine` | Images loaded into the swarm. |
| `E2E_GO_TEST_FLAGS` | `-v -count=1 -timeout 20m` | Extra `go test` flags. |

## Tests

| Test | Needs controller | What it checks |
|---|---|---|
| `TestSwarmSmoke` | no | The swarm is active and a manager. It can create, run and remove a service. |
| `TestImport` | yes | Creates a service (label, env, 1 replica), a config and a secret with the docker CLI, then runs `import --adopt --bootstrap`. Checks that the files exist and that the service file contains the image, the env var and `Replicas: 1`. Secret contents are not checked. |
| `TestRoundTripNoop` | yes | Runs import and then reconcile. The service's `Version.Index` and `UpdatedAt` must not change. |
| `TestReconcileService` | yes | Steps: create (image, 1 replica, managed-by label), then update (new tag, 2 replicas), then prune (remove the file and reconcile with `--prune`). The service must be gone, and an unmanaged service must survive. |
| `TestReconcileConfigAndSecret` | yes | A `Config` and an `ExternalSecret` that uses the `fake` SecretStore. Exactly one Swarm config and one secret must have the label `flux4swarm.io/name=<name>`. |

## Controller contract assumed by the tests

All of the CLI calls are in `helpers_test.go` (`importToGit`, `reconcile`,
`flux4swarmBin`). The manifest templates are in the same file (`serviceYAML`,
`configYAML`, `secretStoreYAML`, `externalSecretYAML`). If the contract
changes, adjust it there.

- Build: `go build -o bin/flux4swarm ./cmd/flux4swarm` (the root `make build`).
- Import (Swarm to Git):
  `flux4swarm import --cluster e2e --git-url file://… --branch main --path clusters/e2e [--adopt] [--bootstrap] --secret-store fake --secret-store-provider fake`.
  The binary clones, commits and pushes.
- Reconcile (Git to Swarm, one-shot):
  `flux4swarm reconcile --url file://… --branch main --path ./clusters/e2e --wait --timeout 3m [--prune]`.
  It exits non-zero if the result is not Ready.
- Layout: `clusters/<cluster>/flux-system/gotk-sync.yaml` (with `--bootstrap`)
  and `clusters/<cluster>/swarm/{networks,configs,secrets,services}/<name>.yaml`.
  The SecretStore is in `clusters/<cluster>/swarm/secrets/secret-store.yaml`.
- Kinds (`apiVersion: swarm.flux4swarm.io/v1alpha1`):
  - `Service`: `spec` is a Docker Engine ServiceSpec in PascalCase. The tests
    also set `spec.Name` equal to `metadata.name`.
  - `Config`: has `spec.data`.
  - Secrets are an `external-secrets.io/v1` `ExternalSecret`
    (`secretStoreRef`, `target.name`, `data[].remoteRef.key`) plus a
    `SecretStore` with `spec.provider.fake.data: [{key, value}]`.
- Managed Swarm objects have the labels `app.kubernetes.io/managed-by=flux4swarm`
  and `kustomize.toolkit.fluxcd.io/name`. Configs and secrets are named
  `<name>-<10 char hash>` with the label `flux4swarm.io/name=<name>`, so the
  tests look them up by that label.
- Prune removes only objects that flux4swarm manages. Unlabelled objects are
  left alone.
- The tests add `io.flux4swarm.e2e=true` to the objects they create. With
  `E2E_MODE=host`, `down.sh` removes only objects that have this label.

The tests run one after another, because `import` exports every object in
the swarm.
