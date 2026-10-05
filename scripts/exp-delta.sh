#!/usr/bin/env bash
# Does the delta actually shrink, and does retention protect a live base?
# Runs against the real 1.7 GB mirror on thinkcentre.
set -uo pipefail

BIN=/tmp/kd-delta
CORPUS=/home/alvaro/kindling-data/ao3_metadata.db
WORK=/tmp/delta-exp

rm -rf "$WORK"; mkdir -p "$WORK/state" "$WORK/snaps"

echo "=== 0. a fresh state db, so StableSalt is genuinely stable ==="
: > "$WORK/state/kindred.db"

echo
echo "=== 1. dump v1 (stable salt, k=20) -- must be FULL ==="
$BIN dump --out "$WORK/snaps" --version 0 --stable-salt \
  --db "$WORK/state/kindred.db" --corpus "$CORPUS" 2>&1 | grep -viE '^$' | sed 's/^/    /'

echo
echo "=== 2. dump v2 (same stable salt, unchanged corpus) -- must be a DELTA ==="
$BIN dump --out "$WORK/snaps" --version 0 --stable-salt \
  --db "$WORK/state/kindred.db" --corpus "$CORPUS" 2>&1 | grep -viE '^$' | sed 's/^/    /'

echo
echo "=== 3. what is on disk ==="
for d in "$WORK"/snaps/v*; do
  v=$(basename "$d")
  shards=$(ls "$d"/shard-*.json 2>/dev/null | wc -l)
  bytes=$(du -sb "$d" | cut -f1)
  full=$(python3 -c "import json,sys;print(json.load(open('$d/manifest.canonical'))['full'])" 2>/dev/null)
  base=$(python3 -c "import json,sys;print(json.load(open('$d/manifest.canonical')).get('base_version','-'))" 2>/dev/null)
  snapv=$(python3 -c "import json,sys;print(json.load(open('$d/manifest.canonical'))['snapshot_version'])" 2>/dev/null)
  echo "    $v  snapshot_version=$snapv full=$full base=$base  shards=$shards  ${bytes}B"
done

echo
echo "=== 4. verify BOTH (a delta must verify like anything else) ==="
for d in "$WORK"/snaps/v*; do
  echo "    $(basename $d):"
  $BIN verify --dir "$d" 2>&1 | sed 's/^/      /'
done