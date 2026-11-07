# flux4swarm

GitOps for Docker Swarm, modelled on [Flux v2](https://github.com/fluxcd/flux2).

flux4swarm watches a Git repository (GitHub, Azure DevOps or any Git host)
and keeps a Docker Swarm in the state described there: services, configs,
networks and secrets. Secrets are never stored in Git; they are described as
[External Secrets Operator](https://external-secrets.io) `ExternalSecret`
manifests and fetched from a secret store (HashiCorp Vault, Azure Key Vault)
when they are applied.

It also has an **import** mode that dumps an existing Swarm into a Git
repository in the same format, so environments that were built by hand or
with `docker stack deploy` can be moved to GitOps.

See [docs/architecture.md](docs/architecture.md) for the design.

## Two modes

| Mode | Command | Use it to |
|------|---------|-----------|
| **Import** | `flux4swarm import` | Export services, configs, networks and secrets (as ExternalSecrets) from a running Swarm to a directory or straight to a GitHub / Azure DevOps repository. |
| **GitOps** | `flux4swarm controller` (loop) / `flux4swarm reconcile` (one pass) | Pull the repository and apply it to Swarm, prune what was removed and wait for services to converge. |

A typical migration uses both: import once with `--bootstrap --adopt`, review
the pull request, then start the controller pointing at the same repository.

## Install

```sh
go install github.com/olljanat-ai/flux4swarm/cmd/flux4swarm@latest
# or
make build   # writes bin/flux4swarm
```

flux4swarm talks to the Docker Engine API of a Swarm **manager** and honours
the usual `DOCKER_HOST`, `DOCKER_TLS_VERIFY` and `DOCKER_CERT_PATH` variables.

## Import an existing Swarm

To a local directory:

```sh
flux4swarm import --cluster prod --output ./fleet --bootstrap --adopt \
  --secret-store-provider vault
```

Straight to GitHub:

```sh
export GIT_USERNAME=git GIT_PASSWORD=<github PAT with repo scope>
flux4swarm import --cluster prod --bootstrap --adopt \
  --git-url https://github.com/acme/fleet --branch main
```

Straight to Azure DevOps:

```sh
export GIT_USERNAME=pat GIT_PASSWORD=<Azure DevOps PAT with Code (read & write)>
flux4swarm import --cluster prod --bootstrap --adopt \
  --git-url https://dev.azure.com/acme/platform/_git/fleet --branch main
```

The result looks like this:

```
clusters/prod/
├── flux-system/gotk-sync.yaml      # GitRepository + Kustomization for the controller
└── swarm/
    ├── networks/backend.yaml
    ├── configs/nginx-conf.yaml
    ├── secrets/
    │   ├── secret-store.yaml       # SecretStore to fill in
    │   └── db-password.yaml        # ExternalSecret
    └── services/web.yaml
```

Docker never returns secret values, so after an import put each secret into
the store under the key named in its ExternalSecret (`remoteRef.key`).

## Run the GitOps controller

Simplest form, with flags:

```sh
flux4swarm controller \
  --url https://github.com/acme/fleet --branch main \
  --path ./clusters/prod --interval 1m --prune
```

Or from Flux manifests (the `gotk-sync.yaml` written by import works as is):

```sh
flux4swarm controller --config clusters/prod/flux-system/gotk-sync.yaml
```

One pass, for CI or debugging (exits non-zero if anything is not Ready):

```sh
flux4swarm reconcile --path ./clusters/prod --dry-run   # local directory, show plan only
flux4swarm reconcile --url https://dev.azure.com/acme/platform/_git/fleet --path ./clusters/prod --wait
```

As a Swarm service (recommended), see [deploy/flux4swarm-stack.yaml](deploy/flux4swarm-stack.yaml).
Credentials are Docker secrets mounted under `/run/secrets`, and the
`secretRef` of a GitRepository or SecretStore points at them.

## Manifests

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: flux-system
  namespace: flux-system
spec:
  interval: 1m
  url: https://dev.azure.com/acme/platform/_git/fleet
  ref:
    branch: main
  secretRef:
    name: git-credentials        # /run/secrets/git-credentials/{username,password}
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: flux-system
  namespace: flux-system
spec:
  interval: 10m
  path: ./clusters/prod
  prune: true
  wait: true
  timeout: 5m
  sourceRef:
    kind: GitRepository
    name: flux-system
---
apiVersion: swarm.flux4swarm.io/v1alpha1
kind: Service
metadata:
  name: web
spec:                            # Docker Engine API ServiceSpec
  TaskTemplate:
    ContainerSpec:
      Image: nginx:1.27
      Configs:
        - ConfigName: nginx-conf
          File: {Name: /etc/nginx/conf.d/default.conf, UID: "0", GID: "0", Mode: 292}
      Secrets:
        - SecretName: db-password
          File: {Name: db-password, UID: "0", GID: "0", Mode: 256}
    Networks:
      - Target: backend
  Mode:
    Replicated: {Replicas: 2}
  EndpointSpec:
    Ports:
      - {Protocol: tcp, TargetPort: 80, PublishedPort: 8080}
---
apiVersion: swarm.flux4swarm.io/v1alpha1
kind: Config
metadata:
  name: nginx-conf
spec:
  data: |
    server { listen 80; location / { return 200 "ok"; } }
---
apiVersion: swarm.flux4swarm.io/v1alpha1
kind: Network
metadata:
  name: backend
spec:
  Driver: overlay
  Attachable: true
---
apiVersion: external-secrets.io/v1
kind: SecretStore
metadata:
  name: vault
spec:
  provider:
    vault:
      server: https://vault.example.com
      path: secret
      version: v2
      auth:
        tokenSecretRef: {name: vault-token, key: token}
---
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: db-password
spec:
  secretStoreRef: {name: vault, kind: SecretStore}
  target: {name: db-password}
  data:
    - secretKey: value
      remoteRef: {key: prod/db, property: password}
```

## Git authentication

| Host | How |
|------|-----|
| GitHub | PAT: `username` (anything) + `password`. GitHub App: `provider: github` with `githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`. |
| Azure DevOps | PAT: `username` (anything non-empty) + `password`. Service principal: `provider: azure` with `tenantId`, `clientId`, `clientSecret`. |
| Any | `bearerToken`, or SSH with `identity` + `known_hosts`. |

The keys are files in the credentials directory: `<dir>/<secretRef>/<key>`
or `<dir>/<secretRef>_<key>`.

## Flux compatibility notes

flux4swarm keeps Flux's API groups, field names, labels and revision format.
Where Swarm needs something different, the most Flux-like option was chosen
and is listed here:

* One process replaces source-controller and kustomize-controller, and
  status is served over HTTP (`/status`, `/metrics`) instead of written into
  custom resources.
* Kubernetes manifests are replaced by `swarm.flux4swarm.io/v1alpha1`
  `Service`, `Config` and `Network` kinds whose spec is the Docker Engine API
  object. Kubernetes secrets are replaced by ExternalSecrets resolved directly
  into Docker secrets.
* `kustomization.yaml` is read only for its `resources:` list.
* Docker configs and secrets are immutable, so they are stored as
  `<name>-<content hash>` and services are rolled when the content changes.
* `provider: azure` uses a service principal instead of workload identity.

## License

Apache License 2.0, see [LICENSE](LICENSE). Files under
`internal/swarm/tfdocker/` are derived from
[kreuzwerker/terraform-provider-docker](https://github.com/kreuzwerker/terraform-provider-docker)
and stay under the Mozilla Public License 2.0, see [NOTICE](NOTICE) and
[LICENSES/MPL-2.0.txt](LICENSES/MPL-2.0.txt).
