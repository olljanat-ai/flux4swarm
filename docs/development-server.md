# Development server for full end-to-end testing

Some flux4swarm features can only be tested end to end with a real Swarm
*and* a real Kubernetes cluster that pull images from the internet:
`import --volsync` + `volume-backup` with a running VolSync operator, for
example. Cloud sandboxes (including the one Claude Code on the web uses)
usually lack Docker or block registries such as quay.io. A small Linux VM
with both a Docker Swarm and [K3s](https://k3s.io) has neither problem.

This guide sets one up so that you, or Claude through
[Remote Control](https://code.claude.com/docs/en/remote-control), can run:

* the existing e2e suite (`make e2e`: DinD Swarm + kind + stock Flux2), and
* the Swarm → K3s data migration (`volume-backup` → VolSync restore) by hand
  or from a script.

## How Claude reaches the server

Run Claude Code on the server itself, with Remote Control. Claude in a
project thread then starts a session on the server and runs commands
there, with the server's own Docker, kubeconfig and network. This needs no
inbound port, no Docker API exposed on the internet, and no credentials
copied into the cloud environment.

Driving the server from a cloud session over the network (a remote
`DOCKER_HOST`, a K3s API on port 6443) is not recommended. Cloud sessions
reach the internet only through an HTTPS proxy that re-terminates TLS, so
Docker and kubeconfig client-certificate authentication do not work
through it. Each host would also have to be added to the environment's
allowed domains.

Use a dedicated, disposable VM: whoever drives the Remote Control session
gets a root-equivalent shell (Docker group, K3s admin). Don't put
production credentials on it.

## 1. The VM

* Ubuntu 24.04 LTS (any recent systemd distribution works), x86_64.
* 4 vCPUs, 16 GB RAM, 60 GB disk. kind, K3s and DinD all run at once
  during a full run.
* Outbound internet access to Docker Hub, ghcr.io, quay.io, registry.k8s.io,
  github.com, get.k3s.io, get.helm.sh, dl.k8s.io and proxy.golang.org. No
  inbound ports are needed besides your own SSH.

## 2. Docker and a single-node Swarm

```sh
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"     # log out and in again
docker swarm init
```

`make e2e` runs its Swarm in a DinD container by default, so the tests
never touch this Swarm. The host Swarm is for manual testing such as the
volume migration below. `E2E_MODE=host make e2e` runs the suite on it
instead.

## 3. K3s

Install K3s without its Traefik and ServiceLB. Swarm's routing mesh and
klipper-lb would otherwise compete for host ports 80/443 and published
ports:

```sh
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--disable traefik --disable servicelb --write-kubeconfig-mode 0644" sh -
mkdir -p ~/.kube && cp /etc/rancher/k3s/k3s.yaml ~/.kube/config
kubectl get nodes
```

K3s brings its own containerd, so it does not use (or disturb) the Docker
daemon. Its flannel VXLAN uses UDP 8472, Swarm's overlay uses UDP 4789, so
they do not clash. The `local-path` StorageClass is the default: PVCs bind
when the first pod (here the VolSync mover) uses them.

## 4. Tools and cluster add-ons

```sh
# Go (version from go.mod), build tools
sudo apt-get install -y make git
curl -fsSL https://go.dev/dl/go1.27.0.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin' >> ~/.bashrc

# Helm and the Flux CLI
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
curl -s https://fluxcd.io/install.sh | sudo bash

# Flux, External Secrets Operator and VolSync on K3s
flux install
helm repo add external-secrets https://charts.external-secrets.io
helm repo add backube https://backube.github.io/helm-charts/
helm install external-secrets external-secrets/external-secrets -n external-secrets --create-namespace --wait
helm install volsync backube/volsync -n volsync-system --create-namespace --wait
```

`make e2e` downloads its own pinned kind, kubectl, flux and helm into
`test/e2e/.bin` and creates its own kind cluster, so these installs are only
for the K3s cluster.

## 5. A restic repository both sides can reach

[restic rest-server](https://github.com/restic/rest-server) as a plain
container (not a Swarm service) on the host. Swarm tasks and K3s pods both
reach it at the host's IP:

```sh
docker run -d --name rest-server --restart unless-stopped -p 8000:8000 \
  -e DISABLE_AUTHENTICATION=1 -v rest-data:/data restic/rest-server:0.14.0
export HOST_IP=$(hostname -I | awk '{print $1}')
```

rest-server creates repositories at most two directories deep, and
`volume-backup` adds `<namespace>/<claim>`. So the base repository must
be the server root: `rest:http://$HOST_IP:8000/`. An S3 service such as
Garage or SeaweedFS works too. Then use `s3:` URLs and keep the default
`--volsync-env` (AWS keys).

## 6. Claude Code with Remote Control

```sh
git clone https://github.com/olljanat-ai/flux4swarm.git ~/flux4swarm
curl -fsSL https://claude.ai/install.sh | bash
cd ~/flux4swarm
claude            # log in once, then exit
tmux new -s claude 'claude remote-control'
```

Keep `claude remote-control` running (tmux, or a systemd user service).
The server then shows up as a device in the Claude app. In a project
thread, ask Claude to "run it on the dev server" and approve the prompt.
Git pushes from the server use whatever credentials you set up there (for
example `gh auth login`, or a fine-grained token limited to this repository).

## 7. Running the tests

The full e2e suite:

```sh
cd ~/flux4swarm
make build e2e
```

The Swarm → K3s volume migration, start to finish:

```sh
cd ~/flux4swarm && make build && export PATH=$PWD/bin:$PATH
export RESTIC_REPOSITORY=rest:http://$HOST_IP:8000/ RESTIC_PASSWORD=dev-only

# A stateful service on Swarm with data in a named volume.
cat > /tmp/shop.yaml <<'EOF'
services:
  db:
    image: alpine:3.22
    command: ["sh", "-c", "echo hello > /var/lib/db/a.txt; mkdir -p /var/lib/db/sub; echo world > /var/lib/db/sub/b.txt; sleep infinity"]
    volumes: [db:/var/lib/db]
volumes:
  db: {}
EOF
docker stack deploy -c /tmp/shop.yaml shop
sleep 10

# Pre-seed while it runs, then a consistent final copy with it stopped.
flux4swarm volume-backup --env '' shop_db
docker service scale shop_db=0
flux4swarm volume-backup --env '' shop_db

# Export and apply the claim and its ReplicationDestination to K3s. The
# exported ExternalSecret needs a real secret store, so create its Secret
# directly instead.
flux4swarm import --volsync --volsync-env '' --cluster dev --output /tmp/out
D=/tmp/out/clusters/dev/apps/shop
kubectl apply -f $D/namespace.yaml -f $D/persistentvolumeclaim-db.yaml
kubectl -n shop create secret generic db-volsync-restic \
  --from-literal=RESTIC_REPOSITORY=rest:http://$HOST_IP:8000/shop/db \
  --from-literal=RESTIC_PASSWORD=$RESTIC_PASSWORD
kubectl apply -f $D/replicationdestination-db.yaml
kubectl -n shop wait replicationdestination/db \
  --for=jsonpath='{.status.lastManualSync}'=initial --timeout=5m

# The restored data.
kubectl -n shop run check --rm -i --restart=Never --image=alpine:3.22 \
  --overrides='{"spec":{"volumes":[{"name":"d","persistentVolumeClaim":{"claimName":"db"}}],"containers":[{"name":"check","image":"alpine:3.22","command":["sh","-c","find /d -type f | xargs head"],"volumeMounts":[{"name":"d","mountPath":"/d"}]}]}}'
```

To test a restore again after more backups, change the trigger:
`kubectl -n shop patch replicationdestination db --type merge -p '{"spec":{"trigger":{"manual":"again"}}}'`.

Clean up with `docker stack rm shop`, `docker volume rm shop_db` and
`kubectl delete namespace shop`.
