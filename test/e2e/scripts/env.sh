# Shared settings for up.sh / down.sh. Source, don't execute.
E2E_MODE="${E2E_MODE:-dind}"                       # dind | host
E2E_DIND_NAME="${E2E_DIND_NAME:-flux4swarm-e2e-dind}"
E2E_DIND_IMAGE="${E2E_DIND_IMAGE:-docker:29-dind}"
E2E_DIND_PORT="${E2E_DIND_PORT:-23750}"
# Images the tests use; pulled by the outer daemon and loaded into the swarm
# daemon so the tests never depend on registry access from inside DinD.
E2E_IMAGES="${E2E_IMAGES:-nginx:alpine nginx:stable-alpine}"
E2E_STATE_DIR="${E2E_STATE_DIR:-${TMPDIR:-/tmp}/flux4swarm-e2e}"
# OCI registry (distribution) for OCIRepository tests, on the outer daemon:
#   http://127.0.0.1:$E2E_REGISTRY_PORT       (from the host: flux push, flux4swarm)
#   http://$E2E_REGISTRY_NAME:5000            (from kind pods, once k8s-up.sh
#                                              joins it to the kind network)
E2E_REGISTRY_NAME="${E2E_REGISTRY_NAME:-flux4swarm-e2e-registry}"
E2E_REGISTRY_IMAGE="${E2E_REGISTRY_IMAGE:-registry:3}"
E2E_REGISTRY_PORT="${E2E_REGISTRY_PORT:-23781}"

# The docker daemon the swarm runs in.
swarm_docker() {
  if [ "$E2E_MODE" = "dind" ]; then
    DOCKER_HOST="tcp://127.0.0.1:${E2E_DIND_PORT}" docker "$@"
  else
    docker "$@"
  fi
}

# ---------------------------------------------------------------------------
# Kubernetes side (k8s-up.sh / k8s-down.sh): kind + stock Flux2 + ESO +
# Gateway API CRDs, and a smart-HTTP git server that both the kind pods and
# the host can reach. Versions are pinned; bump them together.
# ---------------------------------------------------------------------------
E2E_K8S="${E2E_K8S:-1}"                                   # 0 = skip the K8s side
E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
E2E_BIN_DIR="${E2E_BIN_DIR:-$E2E_DIR/.bin}"
E2E_KIND_CLUSTER="${E2E_KIND_CLUSTER:-flux4swarm-e2e}"
E2E_KUBECONFIG="${E2E_KUBECONFIG:-$E2E_STATE_DIR/kubeconfig}"
KIND_VERSION="${KIND_VERSION:-v0.33.0}"
# The default node image of kind v0.33.0.
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5}"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.37.2}"
FLUX_VERSION="${FLUX_VERSION:-v2.10.0}"
FLUX_COMPONENTS="${FLUX_COMPONENTS:-source-controller,kustomize-controller,notification-controller}"
HELM_VERSION="${HELM_VERSION:-v3.22.1}"
ESO_CHART_VERSION="${ESO_CHART_VERSION:-2.14.0}"
# The chart tarball from the ESO GitHub release (what charts.external-secrets.io
# serves), verified against ESO_CHART_SHA256.
ESO_CHART_URL="${ESO_CHART_URL:-https://github.com/external-secrets/external-secrets/releases/download/helm-chart-$ESO_CHART_VERSION/external-secrets-$ESO_CHART_VERSION.tgz}"
ESO_CHART_SHA256="${ESO_CHART_SHA256:-0a6d9e91b1ff50adfdbf24b50b582babfa7c5b7df42b21974ca4b3fb88ee5e34}"
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.7.0}"
# sha256 of standard-install.yaml (the release publishes no checksum file).
GATEWAY_API_SHA256="${GATEWAY_API_SHA256:-4e2602c4cbe1afbc394d86f4ac6522e1a5263b01133bba1e40a6d1a549e753a1}"
# Traefik CRDs only (traefik.io Middleware etc., which import may emit); no
# Traefik controller runs in kind.
TRAEFIK_VERSION="${TRAEFIK_VERSION:-v3.7.17}"
TRAEFIK_CRDS_SHA256="${TRAEFIK_CRDS_SHA256:-ecbe44ea9b011cda1b562716175b882d59cf6561fe6d3e5832df7ee072687d17}"
# Git server: bare repos live in E2E_GIT_ROOT on the host (the tests create
# them and push via file://); the container serves them over smart HTTP.
E2E_GIT_NAME="${E2E_GIT_NAME:-flux4swarm-e2e-git}"
E2E_GIT_IMAGE="${E2E_GIT_IMAGE:-alpine/git:v2.54.0}"
E2E_GIT_PORT="${E2E_GIT_PORT:-23780}"
E2E_GIT_ROOT="${E2E_GIT_ROOT:-$E2E_STATE_DIR/git}"

e2e_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) echo "unsupported arch $(uname -m)" >&2; return 1 ;;
  esac
}
