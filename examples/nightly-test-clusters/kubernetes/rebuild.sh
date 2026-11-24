#!/usr/bin/env bash
# Sets up a freshly created Kubernetes cluster as the nightly test cluster
# of `flux4swarm import --watch --cluster nightly --volsync-sync ... --volsync-restore-first`
# (see docs/kubernetes-and-swarm.md, "Nightly test clusters from the live Swarm"):
#
#   1. installs what the export needs: External Secrets Operator, VolSync
#      (with the VolumeSnapshot CRDs when the cluster has none) and the
#      Gateway API CRDs,
#   2. installs Flux, creates the Git secret and, in every namespace of the
#      export, the credentials of the secret store,
#   3. applies CLUSTER_PATH/flux-system/gotk-sync.yaml and waits until the
#      `data` Kustomization has restored every claim and `apps` is Ready.
#
# Flux only reads the repository: nothing is committed. Run it right after
# the cluster was created; kubectl must point at it.
#
# Called by azure-pipelines.yml and github-workflow.yml next to this file.
#
# Environment:
#   CLUSTER_PATH           cluster directory of the export (default clusters/nightly)
#   FLEET_DIR              checkout of the repository (default .)
#   GIT_USERNAME           Git user for Flux (default git)
#   GIT_PASSWORD           token Flux reads the repository with during the day
#   STORE_CREDENTIALS_DIR  optional: one directory per Secret the SecretStores
#                          reference, one file per key (vault-token/token,
#                          azure-keyvault/clientId and clientSecret, ...), the
#                          layout of flux4swarm --credentials-dir; created in
#                          every namespace of the export
#   RESTORE_TIMEOUT        how long the restores may take (default 3h)
#   APPS_TIMEOUT           how long the workloads may take to get Ready (default 30m)
#   ESO_CHART_VERSION, VOLSYNC_CHART_VERSION, GATEWAY_API_VERSION, SNAPSHOTTER_VERSION
set -euo pipefail

: "${GIT_PASSWORD:?}"
CLUSTER_PATH="${CLUSTER_PATH:-clusters/nightly}"
FLEET_DIR="${FLEET_DIR:-.}"
GIT_USERNAME="${GIT_USERNAME:-git}"
RESTORE_TIMEOUT="${RESTORE_TIMEOUT:-3h}"
APPS_TIMEOUT="${APPS_TIMEOUT:-30m}"
ESO_CHART_VERSION="${ESO_CHART_VERSION:-2.14.0}"
VOLSYNC_CHART_VERSION="${VOLSYNC_CHART_VERSION:-}"
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.7.0}"
SNAPSHOTTER_VERSION="${SNAPSHOTTER_VERSION:-v8.7.0}"
dir="$FLEET_DIR/$CLUSTER_PATH"
sync="$dir/flux-system/gotk-sync.yaml"
[ -f "$dir/layers.yaml" ] || { echo "$dir/layers.yaml missing: export with --volsync-restore-first" >&2; exit 1; }
[ -f "$sync" ] || { echo "$sync missing: export with --bootstrap" >&2; exit 1; }

echo ">> 1/3 add-ons"
helm repo add external-secrets https://charts.external-secrets.io >/dev/null
helm repo add backube https://backube.github.io/helm-charts/ >/dev/null
helm repo update >/dev/null
helm upgrade --install external-secrets external-secrets/external-secrets \
  -n external-secrets --create-namespace --version "$ESO_CHART_VERSION" --wait
if ! kubectl get crd volumesnapshots.snapshot.storage.k8s.io >/dev/null 2>&1; then
  # VolSync does not reconcile a ReplicationDestination without them.
  base="https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/$SNAPSHOTTER_VERSION/client/config/crd"
  for c in volumesnapshotclasses volumesnapshotcontents volumesnapshots; do
    kubectl apply -f "$base/snapshot.storage.k8s.io_$c.yaml"
  done
fi
helm upgrade --install volsync backube/volsync -n volsync-system --create-namespace \
  ${VOLSYNC_CHART_VERSION:+--version "$VOLSYNC_CHART_VERSION"} --wait
kubectl apply --server-side -f \
  "https://github.com/kubernetes-sigs/gateway-api/releases/download/$GATEWAY_API_VERSION/standard-install.yaml"
# Add your Gateway controller (Traefik, ...) here if the tests go through
# the HTTPRoutes of the export.

echo ">> 2/3 Flux and credentials"
flux install
url="$(awk '/^kind: GitRepository/{g=1} g && $1=="url:"{print $2; exit}' "$sync")"
flux create secret git flux-system --url "$url" --username "$GIT_USERNAME" --password "$GIT_PASSWORD"
# Every stack directory is a namespace; Flux applies the Namespace objects
# later and takes them over.
for ns in $(find "$dir/data" "$dir/apps" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' 2>/dev/null | sort -u); do
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f -
  if [ -n "${STORE_CREDENTIALS_DIR:-}" ]; then
    for s in "$STORE_CREDENTIALS_DIR"/*/; do
      kubectl -n "$ns" create secret generic "$(basename "$s")" --from-file="$s" \
        --dry-run=client -o yaml | kubectl apply -f -
    done
  fi
done
kubectl apply -f "$sync"

echo ">> 3/3 waiting for the restores (data) and the workloads (apps)"
wait_ks() { # wait_ks NAME TIMEOUT
  for _ in $(seq 120); do
    kubectl -n flux-system get kustomization "$1" >/dev/null 2>&1 && break
    sleep 5
  done
  kubectl -n flux-system wait kustomization "$1" --for=condition=Ready --timeout="$2"
}
status() {
  flux get kustomizations -A || true
  kubectl get replicationdestinations -A \
    -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,SYNCED:.status.lastManualSync,RESULT:.status.latestMoverStatus.result || true
}
trap 'status' EXIT
wait_ks data "$RESTORE_TIMEOUT"
wait_ks apps "$APPS_TIMEOUT"
echo ">> nightly test cluster ready from ${CLUSTER_PATH}"
