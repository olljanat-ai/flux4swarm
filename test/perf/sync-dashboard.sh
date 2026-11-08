#!/usr/bin/env bash
# Continuous volume sync and cutover through `import --watch`, recorded by
# Prometheus and rendered from the Grafana dashboards of deploy/monitoring.
# Same data layout as volsync-perf.sh:
#
#   test/perf/sync-dashboard.sh NAME COUNT FILES FILE_MB [SYNC_INTERVAL]
#
#   1. stack perf-NAME: COUNT services with FILES x FILE_MB random data each
#   2. import --watch --volsync-sync SYNC_INTERVAL (default 1m) --migrate:
#      one sync service per volume; wait for every volume's first (full)
#      round and two more rounds (no change)
#   3. rewrite one file per volume; wait for the round that copies it
#   4. label every service flux4swarm.io/migrate=kubernetes; wait until all
#      are cut over
#   5. render the volume sync dashboard over the whole run
#
# Prometheus (scrape every 5s), Grafana and the Grafana image renderer run as
# plain containers on the host network (ports 9090, 3000, 8081); the watcher
# listens on 127.0.0.1:9797.
#
# Env: RESTIC_BASE (default rest:http://$HOST_IP:8000/), REST_DATA (rest-data),
#      RESTIC_PASSWORD, FLUX4SWARM, OUT, KEEP=1, KEEP_MONITORING=1.
set -euo pipefail

NAME=$1 COUNT=$2 FILES=$3 FILE_MB=$4 SYNC=${5:-1m}
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
F4S="${FLUX4SWARM:-$ROOT/bin/flux4swarm}"
HOST_IP="${HOST_IP:-$(hostname -I | awk '{print $1}')}"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
export RESTIC_REPOSITORY="${RESTIC_BASE:-rest:http://$HOST_IP:8000/}"
export RESTIC_PASSWORD="${RESTIC_PASSWORD:-perf-only}"
REST_DATA="${REST_DATA:-rest-data}"
OUT="${OUT:-$ROOT/test/perf/results}/$NAME-sync-$RUN_ID"
STACK="perf-$NAME"
VOL_MB=$((FILES * FILE_MB))
MON="$OUT/monitoring"
METRICS=http://127.0.0.1:9797/metrics
mkdir -p "$OUT" "$MON"
RESULTS="$OUT/results.tsv"
printf 'scenario\tphase\tvolumes\tdata_mb\tseconds\n' >"$RESULTS"

log() { echo ">> $(date -u +%T) $*" >&2; }
now() { date +%s.%N; }
record() {
  printf '%s\t%s\t%s\t%s\t%.1f\n' "$NAME" "$1" "$COUNT" "$2" "$(echo "$(now) - $3" | bc)" | tee -a "$RESULTS" >&2
}
svc() { printf '%s_s%03d' "$STACK" "$1"; }
ctr() { docker ps -q -f "label=com.docker.swarm.service.name=$1" | head -1; }
# metric NAME: the watcher's samples of NAME, "labels value" per line
metric() { curl -fsS "$METRICS" | grep -E "^$1(\{| )" || true; }
# count_ge NAME MIN: how many series of NAME are >= MIN
count_ge() { metric "$1" | awk -v m="$2" '$NF+0 >= m+0 {n++} END {print n+0}'; }

# ---------------------------------------------------------------------------
# monitoring
# ---------------------------------------------------------------------------
start_monitoring() {
  docker rm -f perf-prometheus perf-grafana perf-renderer >/dev/null 2>&1 || true
  mkdir -p "$MON/datasources" "$MON/dashboards" "$MON/dash"
  cat >"$MON/prometheus.yml" <<EOF
global:
  scrape_interval: 5s
scrape_configs:
  - job_name: flux4swarm-export
    static_configs:
      - targets: ["127.0.0.1:9797"]
EOF
  sed 's|http://prometheus:9090|http://127.0.0.1:9090|' \
    "$ROOT/deploy/monitoring/grafana/provisioning/datasources/prometheus.yaml" >"$MON/datasources/prometheus.yaml"
  sed 's|/etc/grafana/dashboards|/var/lib/grafana/dash|' \
    "$ROOT/deploy/monitoring/grafana/provisioning/dashboards/flux.yaml" >"$MON/dashboards/flux.yaml"
  cp "$ROOT"/deploy/monitoring/grafana/*.json "$MON/dash/"
  chmod -R a+rX "$MON"
  docker run -d --name perf-prometheus --network host -v "$MON/prometheus.yml:/etc/prometheus/prometheus.yml:ro" \
    prom/prometheus:v3.7.0 --config.file=/etc/prometheus/prometheus.yml --storage.tsdb.retention.time=7d >/dev/null
  docker run -d --name perf-renderer --network host grafana/grafana-image-renderer:latest >/dev/null
  docker run -d --name perf-grafana --network host \
    -e GF_AUTH_ANONYMOUS_ENABLED=true -e GF_AUTH_ANONYMOUS_ORG_ROLE=Admin -e GF_AUTH_DISABLE_LOGIN_FORM=true \
    -e GF_RENDERING_SERVER_URL=http://127.0.0.1:8081/render -e GF_RENDERING_CALLBACK_URL=http://127.0.0.1:3000/ \
    -v "$MON/datasources:/etc/grafana/provisioning/datasources:ro" \
    -v "$MON/dashboards:/etc/grafana/provisioning/dashboards:ro" \
    -v "$MON/dash:/var/lib/grafana/dash:ro" \
    grafana/grafana:12.2.0 >/dev/null
  until curl -fsS http://127.0.0.1:3000/api/health >/dev/null 2>&1; do sleep 1; done
}

# render UID FILE HEIGHT FROM_MS TO_MS
render() {
  curl -fsS -o "$2" "http://127.0.0.1:3000/render/d/$1/x?orgId=1&from=$4&to=$5&width=1600&height=$3&tz=UTC&timeout=120&kiosk" ||
    log "rendering $1 failed"
}

log "starting Prometheus, Grafana and the image renderer"
start_monitoring

# ---------------------------------------------------------------------------
# Swarm stack and the watcher
# ---------------------------------------------------------------------------
log "deploying $STACK: $COUNT services x $FILES x ${FILE_MB}MB"
docker run --rm -v "$REST_DATA:/data" alpine:3.22 rm -rf "/data/$STACK"
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
for i in $(seq 1 "$COUNT"); do
  until c=$(ctr "$(svc "$i")") && [ -n "$c" ] && docker exec "$c" test -f /data/.filled 2>/dev/null; do sleep 2; done
done
log "data written"

git init -q --bare -b main "$OUT/fleet.git"
FROM_MS=$(( $(date +%s) * 1000 - 60000 ))
log "starting import --watch --volsync-sync $SYNC --migrate"
"$F4S" import --watch --git-url "file://$OUT/fleet.git" --cluster perf --volsync-env '' \
  --volsync-sync "$SYNC" --migrate --listen 127.0.0.1:9797 --interval 1m >"$OUT/watch.log" 2>&1 &
WATCH=$!
trap 'kill $WATCH 2>/dev/null || true' EXIT
until curl -fsS "$METRICS" >/dev/null 2>&1; do sleep 1; done

wait_for() { # DESCRIPTION CMD...: poll CMD until it succeeds
  local what=$1; shift
  until "$@"; do sleep 3; done
  log "$what"
}
all_volumes_ge() { [ "$(count_ge "$1" "$2")" -ge "$COUNT" ]; }

t=$(now)
wait_for "every volume has a sync service" all_volumes_ge flux4swarm_volume_sync_up 1
wait_for "first (full) round done for every volume" all_volumes_ge flux4swarm_volume_sync_last_success_timestamp_seconds 1
record sync-first-round $((COUNT * VOL_MB)) "$t"
metric flux4swarm_volume_sync_last_duration_seconds >"$OUT/first-round-durations.txt"

wait_for "three rounds done for every volume" bash -c "[ \$(curl -fsS $METRICS | grep -E '^flux4swarm_volume_sync_rounds_total\{' | grep 'result=\"ok\"' | awk '\$NF+0 >= 3 {n++} END {print n+0}') -ge $COUNT ]"
metric flux4swarm_volume_sync_last_duration_seconds >"$OUT/noop-round-durations.txt"

log "rewriting one ${FILE_MB}MB file per volume"
# (wait for these jobs only: a bare `wait` would wait for the watcher too)
pids=()
for i in $(seq 1 "$COUNT"); do
  docker exec "$(ctr "$(svc "$i")")" sh -c "dd if=/dev/urandom of=/data/f1 bs=1M count=$FILE_MB 2>/dev/null" &
  pids+=($!)
  if (( i % 20 == 0 )); then wait "${pids[@]}"; pids=(); fi
done
[ ${#pids[@]} -eq 0 ] || wait "${pids[@]}"
t=$(now)
since=$(date +%s)
wait_for "the change reached the repository of every volume" \
  all_volumes_ge flux4swarm_volume_sync_last_success_timestamp_seconds "$since"
record sync-change-visible $((COUNT * FILE_MB)) "$t"
metric flux4swarm_volume_sync_last_duration_seconds >"$OUT/incr-round-durations.txt"
metric flux4swarm_volume_sync_last_added_bytes >"$OUT/incr-round-added.txt"

log "a minute of steady state"
sleep 60

STEADY_MS=$(( $(date +%s) * 1000 ))
log "labelling every service for cutover"
t=$(now)
for i in $(seq 1 "$COUNT"); do
  docker service update -d --label-add flux4swarm.io/migrate=kubernetes "$(svc "$i")" >/dev/null
done
migrated() { metric flux4swarm_migration_services | grep 'state="migrated"' | awk -v n="$COUNT" '$NF+0 >= n+0 {ok=1} END {exit !ok}'; }
wait_for "every service cut over" migrated
record cutover-all "$((COUNT * VOL_MB))" "$t"
metric flux4swarm_migration_swarm_downtime_seconds >"$OUT/cutover-downtime.txt"
awk '{print $NF}' "$OUT/cutover-downtime.txt" | sort -n | awk '{a[NR]=$1; s+=$1} END {
  printf "downtime per service: min %.1fs, median %.1fs, max %.1fs, mean %.1fs\n", a[1], a[int((NR+1)/2)], a[NR], s/NR}' |
  tee "$OUT/cutover-summary.txt" >&2

sleep 20 # two more scrapes
TO_MS=$(( $(date +%s) * 1000 ))
log "rendering the dashboards"
H=1900
(( COUNT > 3 )) && H=$(( 1900 + (COUNT > 12 ? 12 : COUNT) * 36 ))
# the sync services as they ran (up to the cutover), then the whole run
render flux4swarm-volume-sync "$OUT/grafana-volume-sync.png" "$H" "$FROM_MS" "$STEADY_MS"
render flux4swarm-volume-sync "$OUT/grafana-cutover.png" "$H" "$FROM_MS" "$TO_MS"
curl -fsS "$METRICS" >"$OUT/metrics.txt"
grep -E 'migration (done|.*failed)|level=ERROR' "$OUT/watch.log" | tail -5 >&2 || true

kill $WATCH 2>/dev/null || true
wait $WATCH 2>/dev/null || true
if [ -z "${KEEP:-}" ]; then
  log "cleaning up"
  for s in $(docker service ls -q -f "name=flux4swarm-sync-$STACK"); do docker service rm "$s" >/dev/null; done
  docker stack rm "$STACK" >/dev/null
  until [ -z "$(docker ps -aq -f "label=com.docker.stack.namespace=$STACK")" ] &&
        [ -z "$(docker ps -aq -f "name=flux4swarm-sync-$STACK")" ]; do sleep 1; done
  sleep 2
  for i in $(seq 1 "$COUNT"); do docker volume rm "$(svc "$i")" >/dev/null || true; done
fi
[ -n "${KEEP_MONITORING:-}" ] || docker rm -f perf-prometheus perf-grafana perf-renderer >/dev/null
log "results in $OUT"
cat "$RESULTS"
