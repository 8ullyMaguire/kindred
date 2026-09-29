#!/usr/bin/env bash
# budget.sh — the memory budget gate from SPEC §6.
#
# Boots the real server against the real corpus, walks the routes that do
# real work, and fails if the kernel's high-water mark for the process
# exceeds the cap for the mode.
#
# VmHWM, not current RSS: a memory regression is a spike, and sampling
# current RSS can miss a spike that happened between two samples. The
# kernel's high-water mark cannot.
#
# Usage: budget.sh <binary> <corpus> <db> <port> <mode>

set -euo pipefail

BIN=${1:?binary}
CORPUS=${2:?corpus db}
DB=${3:?state db}
PORT=${4:-8010}
MODE=${5:-full}

case "$MODE" in
  lite) CAP_MIB=60 ;;
  full) CAP_MIB=220 ;;
  *) echo "budget: unknown mode $MODE (want lite or full)" >&2; exit 2 ;;
esac

TMP=$(mktemp -d)
LOG="$TMP/serve.log"
PID=""
cleanup() {
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "budget: mode=$MODE cap=${CAP_MIB} MiB"
echo "budget: corpus=$CORPUS"

# Refuse to run against a corpus that is not there. A gate that passes
# because it measured nothing is the worst kind of gate.
if [ ! -r "$CORPUS" ]; then
  echo "budget: FAIL corpus $CORPUS is not readable; measuring nothing is not a pass" >&2
  exit 1
fi

"$BIN" serve --corpus "$CORPUS" --db "$DB" --listen "127.0.0.1:$PORT" --mode "$MODE" >"$LOG" 2>&1 &
PID=$!

# Wait for readiness rather than sleeping a guessed interval. A fixed
# sleep is either too short on a cold page cache or wasted time on a warm
# one, and the first kind fails the gate for the wrong reason.
READY=0
for _ in $(seq 1 60); do
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "budget: FAIL the server exited during startup" >&2
    cat "$LOG" >&2
    exit 1
  fi
  if curl -fsS --max-time 3 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 1
done
if [ "$READY" -ne 1 ]; then
  echo "budget: FAIL the server never became ready" >&2
  cat "$LOG" >&2
  exit 1
fi

# Pick a real seed. Recommending from an id that is not in the corpus
# exercises the 404 path, which allocates almost nothing — the gate would
# pass while measuring the wrong thing.
SEED=$(sqlite3 "$CORPUS" 'SELECT id FROM works ORDER BY kudos DESC LIMIT 1;' 2>/dev/null || true)
if [ -z "$SEED" ]; then
  echo "budget: FAIL could not read a seed work id from the corpus" >&2
  exit 1
fi
echo "budget: seed=$SEED"

PEAK_SEEN=0
# step runs one request and reports the peak since the last one, so a
# failure names the request that caused it. "over cap" is not actionable;
# "over cap after recommend, +47 MiB" is.
step() {
  local before=$PEAK_SEEN
  local now
  now=0
  if [ -n "$PID" ] && [ -r "/proc/$PID/status" ]; then
    now=$(awk '/^VmHWM:/ {print $2}' "/proc/$PID/status" 2>/dev/null || echo 0)
  fi
  local delta=$(( now - before ))
  printf 'budget:   %-38s peak %4d MiB%s\n' "$1" "$(( now / 1024 ))" \
    "$( [ "$delta" -gt 2048 ] && printf ' (+%d MiB)' "$(( delta / 1024 ))" )"
  PEAK_SEEN=$now
}

step "healthz"
curl -fsS --max-time 10 "http://127.0.0.1:$PORT/healthz" >/dev/null

step "recommend (2 seeds, n=20)"
curl -fsS --max-time 60 "http://127.0.0.1:$PORT/api/v1/recommend?seed=ao3_work:$SEED&n=20" >/dev/null

# The wide-pool step is the worst case a request can create, so full mode
# walks it. Lite mode does not: it targets a Pi with 512 MB free, where a
# pool of 1000 candidates is not a request anyone makes, and charging it
# for one would report a cap failure for a workload it never serves.
if [ "$MODE" = "full" ]; then
  step "recommend (wide pool, n=50)"
  curl -fsS --max-time 120 "http://127.0.0.1:$PORT/api/v1/recommend?seed=ao3_work:$SEED&n=50&pool=1000" >/dev/null
else
  # No ?pool: the point is to measure the default this mode actually
  # serves. A number hardcoded here would drift from the server's default
  # and the gate would quietly stop testing the real path.
  step "recommend (lite default pool, n=20)"
  curl -fsS --max-time 60 "http://127.0.0.1:$PORT/api/v1/recommend?seed=ao3_work:$SEED&n=20" >/dev/null
fi

step "ao3 works list"
curl -fsS --max-time 20 "http://127.0.0.1:$PORT/api/v1/ao3/works?limit=50" >/dev/null

step "ao3 work by id"
curl -fsS --max-time 20 "http://127.0.0.1:$PORT/api/v1/ao3/works/$SEED" >/dev/null

step "ao3 tags"
curl -fsS --max-time 20 "http://127.0.0.1:$PORT/api/v1/ao3/tags?limit=50" >/dev/null

step "tag detail + tag works"
curl -fsS --max-time 20 "http://127.0.0.1:$PORT/api/v1/ao3/tags/1" >/dev/null
curl -fsS --max-time 20 "http://127.0.0.1:$PORT/api/v1/ao3/tags/1/works?limit=50" >/dev/null

step "tag similarity (the graph-backed path)"
# 503 is a correct answer when no index is loaded, so this step only fails
# on a connection error or a 5xx that is not that specific 503.
code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 "http://127.0.0.1:$PORT/api/v1/tags/1/similar" || echo 000)
case "$code" in
  200|503) : ;;
  *) echo "budget: FAIL tag similarity returned $code" >&2; exit 1 ;;
esac

step "stats"
curl -fsS --max-time 20 "http://127.0.0.1:$PORT/stats" >/dev/null

# The number that decides it.
PEAK_KIB=$(awk '/^VmHWM:/ {print $2}' "/proc/$PID/status" 2>/dev/null || echo 0)
PEAK_MIB=$(( PEAK_KIB / 1024 ))
echo
echo "budget: peak RSS ${PEAK_MIB} MiB against a ${CAP_MIB} MiB cap"

if [ "$PEAK_KIB" -eq 0 ]; then
  echo "budget: FAIL could not read VmHWM; the gate did not measure anything" >&2
  exit 1
fi

if [ "$PEAK_MIB" -gt "$CAP_MIB" ]; then
  echo "budget: FAIL over cap by $((PEAK_MIB - CAP_MIB)) MiB" >&2
  echo "budget: the per-stage trace names the stage; see Builder.Trace" >&2
  exit 1
fi

echo "budget: PASS"
