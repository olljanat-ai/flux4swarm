#!/usr/bin/env bash
# Rebuilds a test Swarm from Git with the newest volume data, in three steps:
#
#   1. reconcile CLUSTER_PATH/apps with every workload scaled to 0, so the
#      services, configs, secrets and networks exist but nothing writes to
#      the volumes,
#   2. restore every volume of those services from its restic repository
#      (flux4swarm volume-backup --restore: the newest snapshot that the sync
#      services on the source Swarm wrote, files not in it are deleted),
#   3. reconcile again at the real replica counts and wait until every
#      service has converged.
#
# Called by azure-pipelines.yml and github-workflow.yml next to this file;
# it runs the same anywhere flux4swarm and docker are on PATH.
#
# Environment:
#   DOCKER_HOST              manager of the test Swarm (unix:// or tcp://)
#   FLEET_URL                Git repository of the export
#   FLEET_BRANCH             branch (default main)
#   FLEET_COMMIT             commit to apply in both passes (default: branch head)
#   CLUSTER_PATH             cluster directory of the export (default clusters/nightly-swarm)
#   CREDENTIALS_DIR          flux4swarm --credentials-dir: flux-system/username and
#                            flux-system/password for Git, plus the secret store
#                            credentials (vault-token/token, azure-keyvault/clientId, ...)
#   RESTIC_REPOSITORY, RESTIC_PASSWORD and the AWS_* (or other --env) variables:
#                            the same restic settings as the sync services
#   RESTORE_NETWORKS         optional, comma-separated: networks for the restore jobs
#   CONTROLLER_SERVICE       optional: a flux4swarm controller service on the test
#                            Swarm (e.g. flux-system_controller) that is stopped
#                            during the rebuild and started again afterwards
#   TIMEOUT                  apply and health check timeout (default 30m)
set -euo pipefail

: "${DOCKER_HOST:?}" "${FLEET_URL:?}" "${CREDENTIALS_DIR:?}" "${RESTIC_REPOSITORY:?}" "${RESTIC_PASSWORD:?}"
FLEET_BRANCH="${FLEET_BRANCH:-main}"
CLUSTER_PATH="${CLUSTER_PATH:-clusters/nightly-swarm}"
TIMEOUT="${TIMEOUT:-30m}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# A Kustomization named flux-system, like the one in
# CLUSTER_PATH/flux-system/gotk-sync.yaml, so the services carry the same
# owner labels as when that controller applies the directory.
config() { # config FILE PATCHES
  cat >"$1" <<YAML
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: flux-system
  namespace: flux-system
spec:
  interval: 1m
  url: ${FLEET_URL}
  ref:
    branch: ${FLEET_BRANCH}
${FLEET_COMMIT:+    commit: ${FLEET_COMMIT}}
  secretRef:
    name: flux-system
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: flux-system
  namespace: flux-system
spec:
  interval: 10m
  path: ./${CLUSTER_PATH}/apps
  prune: true
  wait: true
  timeout: ${TIMEOUT}
  sourceRef:
    kind: GitRepository
    name: flux-system
$2
YAML
}

config "$work/hold.yaml" '  patches:
  - target: {kind: Deployment}
    patch: |-
      [{"op": "add", "path": "/spec/replicas", "value": 0}]
  - target: {kind: StatefulSet}
    patch: |-
      [{"op": "add", "path": "/spec/replicas", "value": 0}]
  - target: {kind: Job}
    patch: |-
      [{"op": "add", "path": "/spec/suspend", "value": true}]'
config "$work/run.yaml" ""

reconcile() {
  flux4swarm reconcile --config "$1" --credentials-dir "$CREDENTIALS_DIR" \
    --cache-dir "$work/cache" --timeout "$TIMEOUT"
}

if [ -n "${CONTROLLER_SERVICE:-}" ] && docker service inspect "$CONTROLLER_SERVICE" >/dev/null 2>&1; then
  echo ">> stopping $CONTROLLER_SERVICE during the rebuild"
  docker service scale --detach=false "$CONTROLLER_SERVICE=0"
fi

echo ">> 1/3 applying ${CLUSTER_PATH}/apps with every workload at 0 replicas"
reconcile "$work/hold.yaml"

echo ">> 2/3 restoring the volumes"
services="$(docker service ls -q --filter label=app.kubernetes.io/managed-by=flux4swarm)"
# Scaling to 0 returns before the tasks have stopped; a restore refuses
# to run while a service using the volume still runs.
for _ in $(seq 120); do
  running="$(echo "$services" | xargs -r -n1 docker service ps --format '{{.DesiredState}} {{.CurrentState}}' |
    grep -v '^Running' | grep -c ' Running' || true)"
  [ "$running" = 0 ] && break
  sleep 5
done
volumes="$(echo "$services" | xargs -r docker service inspect --format \
  '{{range .Spec.TaskTemplate.ContainerSpec.Mounts}}{{if and (eq .Type "volume") .Source}}{{.Source}}{{"\n"}}{{end}}{{end}}' |
  sort -u)"
failed=()
for v in $volumes; do
  if ! flux4swarm volume-backup --restore ${RESTORE_NETWORKS:+--network "$RESTORE_NETWORKS"} "$v"; then
    # A volume that was never backed up (no snapshot yet) starts empty.
    echo "!! restore of $v failed; it starts with the data it has" >&2
    failed+=("$v")
  fi
done

echo ">> 3/3 starting the workloads"
reconcile "$work/run.yaml"

if [ -n "${CONTROLLER_SERVICE:-}" ] && docker service inspect "$CONTROLLER_SERVICE" >/dev/null 2>&1; then
  docker service scale --detach=false "$CONTROLLER_SERVICE=1"
fi

if [ ${#failed[@]} -gt 0 ]; then
  echo "!! not restored: ${failed[*]}" >&2
  exit 1
fi
echo ">> test Swarm rebuilt from ${CLUSTER_PATH} with the newest data"
