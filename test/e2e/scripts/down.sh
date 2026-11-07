#!/usr/bin/env bash
# Tear down what up.sh created.
set -euo pipefail
. "$(dirname "$0")/env.sh"

docker rm -f -v "$E2E_REGISTRY_NAME" >/dev/null 2>&1 && echo ">> removed $E2E_REGISTRY_NAME" || true
if [ "$E2E_MODE" = "dind" ]; then
  docker rm -f -v "$E2E_DIND_NAME" >/dev/null 2>&1 && echo ">> removed $E2E_DIND_NAME" || true
else
  # Remove leftover objects labelled by the tests (never anything else), and leave the swarm only if up.sh created it.
  for kind in service config secret; do
    ids="$(docker $kind ls -q --filter label=io.flux4swarm.e2e=true 2>/dev/null || true)"
    # shellcheck disable=SC2086
    [ -n "${ids// /}" ] && docker $kind rm $ids >/dev/null 2>&1 || true
  done
  if [ -f "$E2E_STATE_DIR/host-swarm-initialized" ]; then
    docker swarm leave --force >/dev/null && echo ">> left swarm"
    rm -f "$E2E_STATE_DIR/host-swarm-initialized"
  fi
fi
