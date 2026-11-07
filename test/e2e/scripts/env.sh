# Shared settings for up.sh / down.sh. Source, don't execute.
E2E_MODE="${E2E_MODE:-dind}"                       # dind | host
E2E_DIND_NAME="${E2E_DIND_NAME:-flux4swarm-e2e-dind}"
E2E_DIND_IMAGE="${E2E_DIND_IMAGE:-docker:29-dind}"
E2E_DIND_PORT="${E2E_DIND_PORT:-23750}"
# Images the tests use; pulled by the outer daemon and loaded into the swarm
# daemon so the tests never depend on registry access from inside DinD.
E2E_IMAGES="${E2E_IMAGES:-nginx:alpine nginx:stable-alpine}"
E2E_STATE_DIR="${E2E_STATE_DIR:-${TMPDIR:-/tmp}/flux4swarm-e2e}"

# The docker daemon the swarm runs in.
swarm_docker() {
  if [ "$E2E_MODE" = "dind" ]; then
    DOCKER_HOST="tcp://127.0.0.1:${E2E_DIND_PORT}" docker "$@"
  else
    docker "$@"
  fi
}
