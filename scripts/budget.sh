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
# Status AND content.
#
# The status check was here first and it was not enough: the server
# answered 200 with `"returned": 0` for every tag, for as long as the
# graph loaded without its tag frequencies -- because a PMI of 0 is
# dropped, and a graph with no frequencies has a PMI of 0 everywhere. The
# gate called that a pass, which is the exact failure this gate exists to
# catch.
#
# So the assertion is a response that parses and carries a non-empty
# `similar` array. A route answering 200 with nothing in it is invisible to
# a status code, and it is the most expensive kind of bug there is: the
# service looks healthy and every answer it gives is wrong.
#
# Tag 363, not tag 1. PMI is
#
#     log( P(a and b) / (P(a) P(b)) )
#
# and tag 1 is "general audiences" with 8,377 of the corpus's 112,935
# works, so P(a) is enormous and the ratio is below 1 against every other
# tag: measured on the real mirror, its five strongest co-occurrences have
# PMI -1.78, -1.24, -2.40, -2.07, -1.99. An empty list is the
# mathematically CORRECT answer for the most common tag in the corpus.
#
# The first version of this assertion used tag 1 and reported a failure
# against a server that was working. A gate that fails on correct
# behaviour gets disabled, and a disabled gate catches nothing -- so the
# tag choice is part of the assertion, not a detail. Tag 363 is a Naruto
# character tag: 391 works, and a strongest neighbour at PMI 3.58.
body=$(curl -sS --max-time 20 "http://127.0.0.1:$PORT/api/v1/tags/363/similar?n=5" || echo '{}')
code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 20 "http://127.0.0.1:$PORT/api/v1/tags/363/similar" || echo 000)
case "$code" in
  # 503 means no index was loaded at all, which is a correct answer for a
  # host that has not been ingested -- there is nothing to check.
  503) : ;;
  # 200 means the graph IS loaded, so an empty list is now a wrong answer
  # rather than an absent one. This is the case the status check alone
  # let through.
  200)
    returned=$(printf '%s' "$body" | sed -n 's/.*"returned":\([0-9]*\).*/\1/p')
    if [ "${returned:-0}" -lt 1 ]; then
      echo "budget: FAIL tag similarity answered 200 with ${returned:-no} neighbours." >&2
      echo "budget:   the graph is loaded, so this is a wrong answer and not an" >&2
      echo "budget:   absent one. A graph with no tag frequencies has a PMI of 0" >&2
      echo "budget:   everywhere, every pair is dropped, and the empty list is" >&2
      echo "budget:   the CORRECT output for that graph. body: $body" >&2
      exit 1
    fi
    ;;
  *) echo "budget: FAIL tag similarity returned $code" >&2; exit 1 ;;
esac

step "stats"
curl -fsS --max-time 20 "http://127.0.0.1:$PORT/stats" >/dev/null

# The arena. Asserted on CONTENT, like the tag-similarity step above, and
# for the same reason: a 200 with an empty body is invisible to a status
# code. The arena page is the heaviest read in the UI -- it loads the tag
# list for BOTH works in a pair, which is more rows than any other page --
# so it is the step most likely to need the cap raised rather than the
# query trimming, and the only way to know is to walk it.
step "arena pair (both cards, with tags)"
arena=$(curl -sS --max-time 60 "http://127.0.0.1:$PORT/arena" || echo '')
code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 60 "http://127.0.0.1:$PORT/arena" || echo 000)
if [ "$code" != "200" ]; then
  echo "budget: FAIL /arena returned $code" >&2
  exit 1
fi
# Two cards is the whole point of a comparison; one is a broken pair, and
# zero is an empty pool that still has to render as a page.
cards=$(printf '%s' "$arena" | grep -c 'class="arena-card"' || true)
if [ "${cards:-0}" -lt 2 ] && [ "${cards:-0}" -ne 0 ]; then
  echo "budget: FAIL /arena rendered $cards card(s); a comparison is two." >&2
  exit 1
fi
if printf '%s' "$arena" | grep -q 'Something went wrong'; then
  echo "budget: FAIL /arena rendered the error page" >&2
  exit 1
fi

step "arena API (leaderboard + rank)"
curl -fsS --max-time 20 "http://127.0.0.1:$PORT/api/v1/arena/leaderboard?limit=5" >/dev/null
curl -fsS --max-time 20 "http://127.0.0.1:$PORT/api/v1/arena/rank/$SEED" >/dev/null

step "my-ranking (the session-keyed page)"
curl -fsS --max-time 20 "http://127.0.0.1:$PORT/my-ranking" >/dev/null

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
