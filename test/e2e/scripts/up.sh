#!/usr/bin/env bash
# Start a single-node Docker Swarm for the e2e tests.
#   E2E_MODE=dind (default): privileged docker:dind container, swarm inside it,
#                            reachable at tcp://127.0.0.1:$E2E_DIND_PORT.
#   E2E_MODE=host:           use the current daemon; swarm init if needed.
set -euo pipefail
. "$(dirname "$0")/env.sh"
mkdir -p "$E2E_STATE_DIR"

if [ "$E2E_MODE" = "dind" ]; then
  if docker inspect "$E2E_DIND_NAME" >/dev/null 2>&1; then
    echo ">> removing stale $E2E_DIND_NAME"
    docker rm -f -v "$E2E_DIND_NAME" >/dev/null
  fi
  echo ">> starting $E2E_DIND_NAME ($E2E_DIND_IMAGE) on 127.0.0.1:$E2E_DIND_PORT"
  docker run -d --privileged --name "$E2E_DIND_NAME" \
    -e DOCKER_TLS_CERTDIR= \
    -p "127.0.0.1:${E2E_DIND_PORT}:2375" \
    "$E2E_DIND_IMAGE" --tls=false >/dev/null
fi

echo ">> waiting for docker daemon"
for _ in $(seq 1 60); do
  swarm_docker info >/dev/null 2>&1 && break
  sleep 1
done
swarm_docker info >/dev/null

state="$(swarm_docker info --format '{{.Swarm.LocalNodeState}}')"
if [ "$state" != "active" ]; then
  echo ">> docker swarm init"
  swarm_docker swarm init --advertise-addr 127.0.0.1 >/dev/null
  [ "$E2E_MODE" = "host" ] && touch "$E2E_STATE_DIR/host-swarm-initialized"
fi

for img in $E2E_IMAGES; do
  echo ">> preloading $img"
  docker image inspect "$img" >/dev/null 2>&1 || docker pull -q "$img" >/dev/null
  if [ "$E2E_MODE" = "dind" ]; then
    docker save "$img" | swarm_docker load -q >/dev/null
  fi
done

if [ "$E2E_MODE" = "dind" ]; then
  echo ">> swarm ready: export DOCKER_HOST=tcp://127.0.0.1:${E2E_DIND_PORT}"
else
  echo ">> swarm ready on the current docker daemon"
fi
