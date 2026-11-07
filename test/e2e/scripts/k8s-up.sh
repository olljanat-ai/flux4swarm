#!/usr/bin/env bash
# Start the Kubernetes side of the e2e tests:
#   - pinned kind, kubectl, flux and helm CLIs (PATH if the version matches,
#     else downloaded into $E2E_BIN_DIR with checksum verification);
#   - a kind cluster ($E2E_KIND_CLUSTER) with every image the tests need
#     preloaded (pulled by the host daemon, then `kind load`), so the cluster
#     never needs registry access;
#   - stock Flux2 (`flux install`), External Secrets Operator (helm chart) and
#     the Gateway API CRDs (standard channel) and the Traefik CRDs;
#   - the OCI registry from up.sh joined to the `kind` network;
#   - a smart-HTTP git server container (fetch and push, no auth) on the
#     `kind` network,
#     serving the bare repos in $E2E_GIT_ROOT at
#       http://$E2E_GIT_NAME:8080/<repo>   (from pods)
#       http://127.0.0.1:$E2E_GIT_PORT/<repo>  (from the host)
set -euo pipefail
. "$(dirname "$0")/env.sh"
mkdir -p "$E2E_STATE_DIR" "$E2E_BIN_DIR" "$E2E_GIT_ROOT"
ARCH="$(e2e_arch)"
export PATH="$E2E_BIN_DIR:$PATH"

# ---------------------------------------------------------------------------
# tools
# ---------------------------------------------------------------------------

# has_version TOOL VERSION CMD...: TOOL is on PATH and CMD's output mentions VERSION.
has_version() {
  local tool=$1 want=$2; shift 2
  command -v "$tool" >/dev/null 2>&1 && "$@" 2>/dev/null | grep -qF -- "$want"
}

# fetch URL DEST
fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }

# sha256_check FILE SUM
sha256_check() {
  echo "$2  $1" | sha256sum -c --status || { echo "!! checksum mismatch for $1" >&2; rm -f "$1"; return 1; }
}

install_kind() {
  has_version kind "kind $KIND_VERSION " kind version && return
  echo ">> installing kind $KIND_VERSION"
  local tmp; tmp="$(mktemp -d)"
  local base="https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-linux-$ARCH"
  if fetch "$base" "$tmp/kind" && fetch "$base.sha256sum" "$tmp/sum"; then
    sha256_check "$tmp/kind" "$(awk '{print $1}' "$tmp/sum")"
    install -m 0755 "$tmp/kind" "$E2E_BIN_DIR/kind"
  else
    echo ">> GitHub download failed; building kind with go install (verified by the Go checksum DB)"
    GOBIN="$E2E_BIN_DIR" go install "sigs.k8s.io/kind@$KIND_VERSION"
  fi
  rm -rf "$tmp"
}

install_kubectl() {
  has_version kubectl "\"gitVersion\": \"$KUBECTL_VERSION\"" kubectl version --client -o json && return
  echo ">> installing kubectl $KUBECTL_VERSION"
  local tmp; tmp="$(mktemp -d)"
  local base="https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$ARCH/kubectl"
  fetch "$base" "$tmp/kubectl"
  fetch "$base.sha256" "$tmp/sum"
  sha256_check "$tmp/kubectl" "$(cat "$tmp/sum")"
  install -m 0755 "$tmp/kubectl" "$E2E_BIN_DIR/kubectl"
  rm -rf "$tmp"
}

install_flux() {
  has_version flux "flux: $FLUX_VERSION" flux version --client && return
  echo ">> installing flux $FLUX_VERSION"
  local tmp v="${FLUX_VERSION#v}"; tmp="$(mktemp -d)"
  local base="https://github.com/fluxcd/flux2/releases/download/$FLUX_VERSION"
  local tgz="flux_${v}_linux_${ARCH}.tar.gz"
  fetch "$base/$tgz" "$tmp/$tgz"
  fetch "$base/flux_${v}_checksums.txt" "$tmp/sums"
  sha256_check "$tmp/$tgz" "$(awk -v f="$tgz" '$2==f {print $1}' "$tmp/sums")"
  tar -xzf "$tmp/$tgz" -C "$tmp" flux
  install -m 0755 "$tmp/flux" "$E2E_BIN_DIR/flux"
  rm -rf "$tmp"
}

install_helm() {
  has_version helm "$HELM_VERSION" helm version --short && return
  echo ">> installing helm $HELM_VERSION"
  local tmp; tmp="$(mktemp -d)"
  local tgz="helm-$HELM_VERSION-linux-$ARCH.tar.gz"
  if fetch "https://get.helm.sh/$tgz" "$tmp/$tgz" && fetch "https://get.helm.sh/$tgz.sha256sum" "$tmp/sum"; then
    sha256_check "$tmp/$tgz" "$(awk '{print $1}' "$tmp/sum")"
    tar -xzf "$tmp/$tgz" -C "$tmp" "linux-$ARCH/helm"
    install -m 0755 "$tmp/linux-$ARCH/helm" "$E2E_BIN_DIR/helm"
  else
    echo ">> get.helm.sh download failed; building helm with go install (verified by the Go checksum DB)"
    GOBIN="$E2E_BIN_DIR" go install -ldflags "-X helm.sh/helm/v3/internal/version.version=$HELM_VERSION" \
      "helm.sh/helm/v3/cmd/helm@$HELM_VERSION"
  fi
  rm -rf "$tmp"
}

install_kind
install_kubectl
install_flux
install_helm

# ---------------------------------------------------------------------------
# images: pull on the host, then load into the kind node
# ---------------------------------------------------------------------------

images_in() { grep -oE '^\s*(- )?image: *"?[^" ]+' | sed -E 's/.*image: *"?//' | sort -u; }

FLUX_MANIFESTS="$E2E_STATE_DIR/flux-install.yaml"
flux install --export --components "$FLUX_COMPONENTS" >"$FLUX_MANIFESTS"

ESO_CHART="$E2E_STATE_DIR/external-secrets-$ESO_CHART_VERSION.tgz"
fetch "$ESO_CHART_URL" "$ESO_CHART"
[ -z "$ESO_CHART_SHA256" ] || sha256_check "$ESO_CHART" "$ESO_CHART_SHA256"
ESO_VALUES=(--set installCRDs=true)
ESO_IMAGES="$(helm template external-secrets "$ESO_CHART" --namespace external-secrets "${ESO_VALUES[@]}" | images_in)"

PRELOAD="$(printf '%s\n' $E2E_IMAGES "$E2E_GIT_IMAGE" $ESO_IMAGES $(images_in <"$FLUX_MANIFESTS") | sort -u)"

load_image() {
  local img=$1 tar="$E2E_STATE_DIR/image.tar"
  echo ">> preloading $img"
  docker image inspect "$img" >/dev/null 2>&1 || docker pull -q --platform "linux/$ARCH" "$img" >/dev/null
  # With the containerd image store, plain `docker save` of a multi-arch image
  # exports an index whose other platforms are missing, which `kind load`
  # rejects; --platform (Docker >= 28) exports a single-platform image.
  docker save --platform "linux/$ARCH" -o "$tar" "$img" 2>/dev/null || docker save -o "$tar" "$img"
  kind load image-archive --name "$E2E_KIND_CLUSTER" "$tar" >/dev/null
  rm -f "$tar"
}

# ---------------------------------------------------------------------------
# cluster
# ---------------------------------------------------------------------------

if kind get clusters 2>/dev/null | grep -qx "$E2E_KIND_CLUSTER"; then
  echo ">> removing stale kind cluster $E2E_KIND_CLUSTER"
  kind delete cluster --name "$E2E_KIND_CLUSTER" >/dev/null
fi
echo ">> creating kind cluster $E2E_KIND_CLUSTER ($KIND_NODE_IMAGE)"
KIND_CONFIG="$E2E_STATE_DIR/kind-config.yaml"
{
  echo "kind: Cluster"
  echo "apiVersion: kind.x-k8s.io/v1alpha4"
  # Kubernetes >= 1.35 refuses cgroup v1 hosts unless told otherwise (CI
  # runners are cgroup v2; some dev/sandbox hosts are not).
  if [ "$(docker info --format '{{.CgroupVersion}}')" = "1" ]; then
    echo ">> host uses cgroup v1: setting kubelet failCgroupV1=false" >&2
    cat <<'YAML'
kubeadmConfigPatches:
  - |
    kind: KubeletConfiguration
    failCgroupV1: false
YAML
  fi
  # Sandboxed hosts without CAP_SYS_RESOURCE cannot lower oom_score_adj, which
  # runc needs for pod sandboxes; let containerd clamp it instead.
  if ! docker run --rm --privileged --entrypoint sh "$E2E_GIT_IMAGE" -c 'echo -999 >/proc/self/oom_score_adj' 2>/dev/null; then
    echo ">> host cannot lower oom_score_adj: setting containerd restrict_oom_score_adj" >&2
    cat <<'YAML'
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri"]
      restrict_oom_score_adj = true
YAML
  fi
} >"$KIND_CONFIG"
kind create cluster --name "$E2E_KIND_CLUSTER" --image "$KIND_NODE_IMAGE" \
  --config "$KIND_CONFIG" --kubeconfig "$E2E_KUBECONFIG" --wait 120s
export KUBECONFIG="$E2E_KUBECONFIG"

for img in $PRELOAD; do load_image "$img"; done

echo ">> installing Flux $FLUX_VERSION ($FLUX_COMPONENTS)"
flux install --components "$FLUX_COMPONENTS" --timeout 5m
flux check --components "$FLUX_COMPONENTS" >/dev/null 2>&1 || flux check --components "$FLUX_COMPONENTS"

echo ">> installing Gateway API CRDs $GATEWAY_API_VERSION (standard channel)"
gw="$E2E_STATE_DIR/gateway-api-standard-install.yaml"
fetch "https://github.com/kubernetes-sigs/gateway-api/releases/download/$GATEWAY_API_VERSION/standard-install.yaml" "$gw"
[ -z "$GATEWAY_API_SHA256" ] || sha256_check "$gw" "$GATEWAY_API_SHA256"
kubectl apply --server-side -f "$gw" >/dev/null
kubectl wait --for condition=Established --timeout 60s crd/httproutes.gateway.networking.k8s.io crd/gateways.gateway.networking.k8s.io >/dev/null

echo ">> installing Traefik $TRAEFIK_VERSION CRDs (no controller)"
tcrd="$E2E_STATE_DIR/traefik-crds.yaml"
fetch "https://raw.githubusercontent.com/traefik/traefik/$TRAEFIK_VERSION/docs/content/reference/dynamic-configuration/kubernetes-crd-definition-v1.yml" "$tcrd"
[ -z "$TRAEFIK_CRDS_SHA256" ] || sha256_check "$tcrd" "$TRAEFIK_CRDS_SHA256"
kubectl apply --server-side -f "$tcrd" >/dev/null
kubectl wait --for condition=Established --timeout 60s crd/middlewares.traefik.io >/dev/null

echo ">> installing External Secrets Operator chart $ESO_CHART_VERSION"
helm upgrade --install external-secrets "$ESO_CHART" \
  --namespace external-secrets --create-namespace "${ESO_VALUES[@]}" --wait --timeout 5m >/dev/null
kubectl wait --for condition=Established --timeout 60s \
  crd/externalsecrets.external-secrets.io crd/secretstores.external-secrets.io >/dev/null

# ---------------------------------------------------------------------------
# git server
# ---------------------------------------------------------------------------

echo ">> starting git server $E2E_GIT_NAME on the kind network and 127.0.0.1:$E2E_GIT_PORT"
(cd "$E2E_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -o "$E2E_BIN_DIR/gitserver-linux-$ARCH" ./gitserver)
docker rm -f "$E2E_GIT_NAME" >/dev/null 2>&1 || true
# Runs as the host user so pushes over HTTP leave host-owned files behind.
docker run -d --name "$E2E_GIT_NAME" --network kind \
  --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -p "127.0.0.1:$E2E_GIT_PORT:8080" \
  -v "$E2E_GIT_ROOT:/srv/git" \
  -v "$E2E_BIN_DIR/gitserver-linux-$ARCH:/usr/local/bin/gitserver:ro" \
  --entrypoint /usr/local/bin/gitserver "$E2E_GIT_IMAGE" -root /srv/git -listen :8080 >/dev/null

# Probe repo: proves push and fetch over HTTP from the host, and fetch from a pod.
probe="$E2E_GIT_ROOT/probe.git"
rm -rf "$probe"
git init -q --bare -b main "$probe"
for _ in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:$E2E_GIT_PORT/healthz" >/dev/null 2>&1 && break
  sleep 1
done
tmp="$(mktemp -d)"
git -C "$tmp" init -q -b main
git -C "$tmp" -c user.name=e2e -c user.email=e2e@example.invalid commit -q --allow-empty -m probe
git -C "$tmp" -c push.negotiate=false push -q "http://127.0.0.1:$E2E_GIT_PORT/probe.git" main
rm -rf "$tmp"
git ls-remote "http://127.0.0.1:$E2E_GIT_PORT/probe.git" | grep -q refs/heads/main
kubectl -n default delete pod git-probe --ignore-not-found >/dev/null
kubectl -n default run git-probe --restart=Never --image "$E2E_GIT_IMAGE" --image-pull-policy=Never \
  -- ls-remote "http://$E2E_GIT_NAME:8080/probe.git" >/dev/null
if ! kubectl -n default wait --for=jsonpath='{.status.phase}'=Succeeded pod/git-probe --timeout 120s >/dev/null; then
  kubectl -n default logs git-probe >&2 || true
  echo "!! the git server is not reachable from pods" >&2
  exit 1
fi
kubectl -n default delete pod git-probe >/dev/null

# OCI registry (started by up.sh): join it to the kind network so Flux's
# source-controller can pull from http://$E2E_REGISTRY_NAME:5000.
if docker inspect "$E2E_REGISTRY_NAME" >/dev/null 2>&1; then
  echo ">> connecting $E2E_REGISTRY_NAME to the kind network"
  docker network connect kind "$E2E_REGISTRY_NAME" 2>/dev/null || true
fi

echo ">> kind cluster ready: export KUBECONFIG=$E2E_KUBECONFIG PATH=$E2E_BIN_DIR:\$PATH"
