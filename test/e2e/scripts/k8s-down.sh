#!/usr/bin/env bash
# Tear down what k8s-up.sh created. Safe to run when nothing is up.
set -euo pipefail
. "$(dirname "$0")/env.sh"
export PATH="$E2E_BIN_DIR:$PATH"

docker rm -f "$E2E_GIT_NAME" >/dev/null 2>&1 && echo ">> removed $E2E_GIT_NAME" || true
if command -v kind >/dev/null 2>&1 && kind get clusters 2>/dev/null | grep -qx "$E2E_KIND_CLUSTER"; then
  kind delete cluster --name "$E2E_KIND_CLUSTER" --kubeconfig "$E2E_KUBECONFIG" >/dev/null && echo ">> deleted kind cluster $E2E_KIND_CLUSTER"
fi
rm -rf "$E2E_GIT_ROOT" "$E2E_KUBECONFIG" "$E2E_STATE_DIR/flux-install.yaml" "$E2E_STATE_DIR/kind-config.yaml" "$E2E_STATE_DIR/traefik-crds.yaml" "$E2E_STATE_DIR/gateway-api-standard-install.yaml" "$E2E_STATE_DIR"/external-secrets-*.tgz
