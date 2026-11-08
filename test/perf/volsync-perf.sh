#!/usr/bin/env bash
# Volume data sync performance test: Swarm volumes -> restic -> VolSync on
# Kubernetes. Needs the development server of docs/development-server.md
# (Swarm on the host daemon, K3s with VolSync, a restic rest-server).
#
#   test/perf/volsync-perf.sh NAME COUNT FILES FILE_MB [PARALLEL...]
#
# creates stack perf-NAME with COUNT services, each with its own volume of
# FILES files of FILE_MB random (incompressible) data, and measures:
#   backup-full      first volume-backup of every volume (services running)
#   backup-noop      again, nothing changed
#   backup-incr      again, after one file per volume was rewritten
#   cutover          stop every service + final volume-backup (the Swarm
#                    downtime of a cutover), after another file was rewritten
#   restore-full     VolSync ReplicationDestinations restore every claim on
#                    Kubernetes from empty
#   restore-incr     trigger again after the incremental changes
#   backup-add1m     with the services stopped, 1MB of new data per volume:
#                    incremental backup ...
#   restore-add1m    ... and incremental restore of just that
# Backups of several volumes run once per PARALLEL value (default 1), as that
# many concurrent volume-backup processes, each on its own repository set.
#
# Env: RESTIC_BASE (default rest:http://$HOST_IP:8000/, the root of a
#      rest-server whose data is in docker volume REST_DATA, default rest-data:
#      the perf-NAME repositories in it are wiped before each run),
#      RESTIC_PASSWORD,
#      FLUX4SWARM (default ./bin/flux4swarm), OUT (results directory),
#      KUBECONFIG, KEEP=1 (leave everything in place).
set -euo pipefail

NAME=$1 COUNT=$2 FILES=$3 FILE_MB=$4
shift 4
PARALLEL=("${@:-1}")
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
F4S="${FLUX4SWARM:-$ROOT/bin/flux4swarm}"
HOST_IP="${HOST_IP:-$(hostname -I | awk '{print $1}')}"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
RESTIC_BASE="${RESTIC_BASE:-rest:http://$HOST_IP:8000/}"
REST_DATA="${REST_DATA:-rest-data}"
export RESTIC_PASSWORD="${RESTIC_PASSWORD:-perf-only}"
OUT="${OUT:-$ROOT/test/perf/results}/$NAME-$RUN_ID"
STACK="perf-$NAME"
VOL_MB=$((FILES * FILE_MB))
TOTAL_MB=$((COUNT * VOL_MB))
mkdir -p "$OUT"
RESULTS="$OUT/results.tsv"
printf 'scenario\tphase\tparallel\tvolumes\tdata_mb\tseconds\tmb_per_s\n' >"$RESULTS"

log() { echo ">> $(date -u +%T) $*" >&2; }
now() { date +%s.%N; }
# record PHASE PARALLEL MB START
record() {
  local s; s=$(echo "$(now) - $4" | bc)
  local rate; rate=$(echo "scale=1; $3 / $s" | bc)
  printf '%s\t%s\t%s\t%s\t%s\t%.1f\t%.1f\n' "$NAME" "$1" "$2" "$COUNT" "$3" "$s" "$rate" | tee -a "$RESULTS" >&2
}
svc() { printf '%s_s%03d' "$STACK" "$1"; }
vols() { for i in $(seq 1 "$COUNT"); do svc "$i"; echo; done; }
# container of a service task on this node
ctr() { docker ps -q -f "label=com.docker.swarm.service.name=$1" | head -1; }

# ---------------------------------------------------------------------------
# Swarm: services that fill their volume once, then sleep.
# ---------------------------------------------------------------------------
log "deploying $STACK: $COUNT services x $FILES x ${FILE_MB}MB = ${TOTAL_MB}MB"
{
  echo "services:"
  for i in $(seq 1 "$COUNT"); do
    n=$(printf 's%03d' "$i")
    cat <<EOF
  $n:
    image: alpine:3.22
    command: ["sh", "-c", "if [ ! -f /data/.filled ]; then for f in \$\$(seq 1 $FILES); do dd if=/dev/urandom of=/data/f\$\$f bs=1M count=$FILE_MB 2>/dev/null; done; touch /data/.filled; fi; exec sleep infinity"]
    stop_grace_period: 1s
    volumes: [$n:/data]
EOF
  done
  echo "volumes:"
  for i in $(seq 1 "$COUNT"); do printf '  s%03d: {}\n' "$i"; done
} >"$OUT/stack.yaml"
docker stack deploy -d -c "$OUT/stack.yaml" "$STACK" >/dev/null
log "waiting for the data to be written"
t=$(now)
for i in $(seq 1 "$COUNT"); do
  until c=$(ctr "$(svc "$i")") && [ -n "$c" ] && docker exec "$c" test -f /data/.filled 2>/dev/null; do sleep 2; done
done
log "data written in $(echo "$(now) - $t" | bc | cut -d. -f1)s"

# rewrite file N of every volume
rewrite() {
  local i
  for i in $(seq 1 "$COUNT"); do
    docker exec "$(ctr "$(svc "$i")")" sh -c "dd if=/dev/urandom of=/data/f$1 bs=1M count=$FILE_MB 2>/dev/null" &
    if (( i % 20 == 0 )); then wait; fi
  done
  wait
}

# backup PHASE PARALLEL MB REPO: volume-backup every volume with PARALLEL
# concurrent processes into REPO.
backup() {
  local phase=$1 p=$2 mb=$3 repo=$4 t
  t=$(now)
  vols | xargs -P "$p" -n "$(( (COUNT + p - 1) / p ))" \
    env RESTIC_REPOSITORY="$repo" "$F4S" volume-backup --env '' --timeout 3h \
    >>"$OUT/backup.log" 2>&1
  record "$phase" "$p" "$mb" "$t"
}

# rest-server makes repositories at most two levels deep and volume-backup
# adds <namespace>/<claim>, so every run starts from empty repositories
# instead; the last PARALLEL value's are the ones Kubernetes restores from.
wipe_repos() {
  docker run --rm -v "$REST_DATA:/data" alpine:3.22 rm -rf "/data/$STACK"
}
for p in "${PARALLEL[@]}"; do
  repo="$RESTIC_BASE"
  wipe_repos
  log "backups with $p concurrent volume-backup processes to $repo"
  rewrite_mb=$((COUNT * FILE_MB))
  backup backup-full "$p" "$TOTAL_MB" "$repo"
  backup backup-noop "$p" 0 "$repo"
  rewrite 1
  backup backup-incr "$p" "$rewrite_mb" "$repo"
done
REPO="$RESTIC_BASE"

log "cutover: rewrite one more file, stop every service, final backup"
rewrite 2
t=$(now)
for i in $(seq 1 "$COUNT"); do docker service scale -d "$(svc "$i")=0" >/dev/null; done
for i in $(seq 1 "$COUNT"); do
  until [ -z "$(ctr "$(svc "$i")")" ]; do sleep 0.5; done
done
vols | xargs -P "${PARALLEL[-1]}" -n "$(( (COUNT + ${PARALLEL[-1]} - 1) / ${PARALLEL[-1]} ))" \
  env RESTIC_REPOSITORY="$REPO" "$F4S" volume-backup --env '' --timeout 3h >>"$OUT/backup.log" 2>&1
record cutover "${PARALLEL[-1]}" $((COUNT * FILE_MB)) "$t"

# ---------------------------------------------------------------------------
# Kubernetes: restore every claim with VolSync.
# ---------------------------------------------------------------------------
log "exporting with import --volsync"
"$F4S" import --volsync --volsync-env '' --cluster perf --output "$OUT/export" >/dev/null 2>&1
D="$OUT/export/clusters/perf/apps/$STACK"
kubectl delete namespace "$STACK" --ignore-not-found --wait=true >/dev/null
kubectl apply -f "$D/namespace.yaml" >/dev/null
size_gi=$(( (VOL_MB * 12 / 10 + 1023) / 1024 ))
for i in $(seq 1 "$COUNT"); do
  claim=$(printf 's%03d' "$i")
  sed "s/storage: 1Gi/storage: ${size_gi}Gi/" "$D/persistentvolumeclaim-$claim.yaml"
  echo "---"
  kubectl -n "$STACK" create secret generic "$claim-volsync-restic" --dry-run=client -o yaml \
    --from-literal=RESTIC_REPOSITORY="${REPO%/}/$STACK/$claim" --from-literal=RESTIC_PASSWORD="$RESTIC_PASSWORD"
  echo "---"
done >"$OUT/claims.yaml"
kubectl apply -f "$OUT/claims.yaml" >/dev/null

# wait_restores TRIGGER: until every ReplicationDestination synced TRIGGER.
wait_restores() {
  local done_n
  while :; do
    done_n=$(kubectl -n "$STACK" get replicationdestinations -o json | jq --arg t "$1" \
      '[.items[] | select(.status.lastManualSync == $t)] | length')
    [ "$done_n" -ge "$COUNT" ] && break
    if kubectl -n "$STACK" get replicationdestinations -o json | jq -e --arg t "$1" \
      '[.items[] | select(.status.latestMoverStatus.result == "Failed")] | length > 0' >/dev/null; then
      log "a restore failed"; kubectl -n "$STACK" get replicationdestinations -o yaml >"$OUT/rd-failed.yaml"; exit 1
    fi
    sleep 2
  done
}

log "full restore of $COUNT claims"
t=$(now)
for i in $(seq 1 "$COUNT"); do cat "$D/replicationdestination-$(printf 's%03d' "$i").yaml"; echo "---"; done |
  kubectl apply -f - >/dev/null
wait_restores initial
record restore-full - "$TOTAL_MB" "$t"
kubectl -n "$STACK" get replicationdestinations -o json | jq -r \
  '.items[] | [.metadata.name, .status.lastSyncDuration] | @tsv' >"$OUT/restore-full-durations.tsv"

log "incremental restore (two files per volume changed since the first backup)"
t=$(now)
for i in $(seq 1 "$COUNT"); do
  kubectl -n "$STACK" patch replicationdestination "$(printf 's%03d' "$i")" --type merge \
    -p '{"spec":{"trigger":{"manual":"incr"}}}' >/dev/null &
  if (( i % 20 == 0 )); then wait; fi
done
wait
wait_restores incr
record restore-incr - $((2 * COUNT * FILE_MB)) "$t"

# The everyday case: a little new data. With the services stopped, add 1MB
# to every volume, then an incremental backup and restore of just that.
log "adding a 1MB file to every volume"
for i in $(seq 1 "$COUNT"); do
  docker run --rm -v "$(svc "$i"):/data" alpine:3.22 dd if=/dev/urandom of=/data/fadd bs=1M count=1 2>/dev/null &
  if (( i % 20 == 0 )); then wait; fi
done
wait
backup backup-add1m "${PARALLEL[-1]}" "$COUNT" "$REPO"
t=$(now)
for i in $(seq 1 "$COUNT"); do
  kubectl -n "$STACK" patch replicationdestination "$(printf 's%03d' "$i")" --type merge \
    -p '{"spec":{"trigger":{"manual":"add1m"}}}' >/dev/null &
  if (( i % 20 == 0 )); then wait; fi
done
wait
wait_restores add1m
record restore-add1m - "$COUNT" "$t"

# Verify: the restored files match the Swarm volume (checksums of volume 1).
log "verifying checksums of $(svc 1) against claim s001"
docker run --rm -v "$(svc 1):/data:ro" alpine:3.22 sh -c 'cd /data && sha256sum f* | sort' >"$OUT/swarm.sha"
kubectl -n "$STACK" run verify --rm -i --restart=Never --image=alpine:3.22 --quiet --overrides='{"spec":{"volumes":[{"name":"d","persistentVolumeClaim":{"claimName":"s001"}}],"containers":[{"name":"verify","image":"alpine:3.22","command":["sh","-c","cd /d && sha256sum f* | sort"],"volumeMounts":[{"name":"d","mountPath":"/d"}]}]}}' \
  2>/dev/null | grep -E '^[0-9a-f]{64} ' >"$OUT/k8s.sha"
[ "$(wc -l <"$OUT/swarm.sha")" -eq $((FILES + 1)) ] || { log "volume 1 has $(wc -l <"$OUT/swarm.sha") files, want $((FILES + 1))"; exit 1; }
[ "$(docker run --rm -v "$(svc 1):/data:ro" alpine:3.22 du -sm /data | cut -f1)" -ge "$VOL_MB" ] || { log "volume 1 is smaller than ${VOL_MB}MB"; exit 1; }
if cmp -s "$OUT/swarm.sha" "$OUT/k8s.sha"; then log "checksums match"; echo ok >"$OUT/verified"; else log "CHECKSUM MISMATCH"; exit 1; fi

{
  echo "host: $(nproc) CPUs, $(free -g | awk '/Mem:/{print $2}')G RAM, $(uname -r)"
  echo "docker: $(docker version --format '{{.Server.Version}}'), k3s: $(kubectl version -o json | jq -r .serverVersion.gitVersion)"
  echo "volsync: $(helm -n volsync-system list -o json | jq -r '.[0].app_version'), restic image: restic/restic:0.18.1 (volume-backup default)"
  echo "flux4swarm: $(git -C "$ROOT" rev-parse --short HEAD)"
  echo "repository: $RESTIC_BASE ($(docker inspect -f '{{.Config.Image}}' rest-server 2>/dev/null || echo unknown))"
  echo "repository size: $(docker run --rm -v "$REST_DATA:/data" alpine:3.22 du -sm "/data/$STACK" | cut -f1) MB"
} >"$OUT/environment.txt"

if [ -z "${KEEP:-}" ]; then
  log "cleaning up"
  docker stack rm "$STACK" >/dev/null
  until [ -z "$(docker ps -aq -f "label=com.docker.stack.namespace=$STACK")" ]; do sleep 1; done
  sleep 2
  vols | xargs docker volume rm >/dev/null
  kubectl delete namespace "$STACK" --wait=false >/dev/null
fi
log "results in $RESULTS"
cat "$RESULTS"
