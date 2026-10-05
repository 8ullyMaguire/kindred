#!/usr/bin/env bash
# Does retention protect a delta's base, and does the every-20 boundary force a
# full snapshot? Runs against the real mirror on thinkcentre.
set -uo pipefail

BIN=/tmp/kd-delta
CORPUS=/home/alvaro/kindling-data/ao3_metadata.db
W=/tmp/ret-exp
rm -rf "$W"; mkdir -p "$W/state" "$W/snaps"
: > "$W/state/kindred.db"

dump() { # $1 = extra flags
  $BIN dump --out "$W/snaps" --stable-salt --db "$W/state/kindred.db" \
    --corpus "$CORPUS" $1 2>&1
}

echo "=== 8 snapshots, retention 3 -- is the base of a retained delta protected? ==="
for i in 1 2 3 4 5 6 7 8; do
  line=$(dump "--version 0" | grep -E 'writing a (FULL|DELTA)')
  echo "  v$((i-1)) -> $line"
done

echo
echo "=== what survived retention ==="
for d in "$W"/snaps/v*; do
  v=$(basename "$d")
  full=$(python3 -c "import json;m=json.load(open('$d/manifest.canonical'));print(m['full'])" 2>/dev/null)
  base=$(python3 -c "import json;m=json.load(open('$d/manifest.canonical'));print(m['base_version'])" 2>/dev/null)
  snap=$(python3 -c "import json;m=json.load(open('$d/manifest.canonical'));print(m['snapshot_version'])" 2>/dev/null)
  sz=$(du -sb "$d" | cut -f1)
  echo "    $v  snapshot_version=$snap full=$full base=$base  ${sz}B"
done

echo
echo "=== the invariant: every retained DELTA's base is on disk ==="
python3 - <<'PY'
import json, os, glob
root = "/tmp/ret-exp/snaps"
live = set()
for d in glob.glob(root + "/v*"):
    m = json.load(open(d + "/manifest.canonical"))
    live.add(m["snapshot_version"])
bad = []
for d in sorted(glob.glob(root + "/v*")):
    m = json.load(open(d + "/manifest.canonical"))
    if not m["full"]:
        b = m["base_version"]
        if b not in live:
            bad.append((m["snapshot_version"], b))
print("    retained versions:", sorted(live))
print("    deltas with a MISSING base:", bad if bad else "none")
raise SystemExit(1 if bad else 0)
PY
echo "    invariant rc=$?"

echo
echo "=== --version 0 must now land past the newest, not on v0 ==="
dump "--version 0" | grep -E 'snapshot v' | sed 's/^/    /'

echo
echo "=== overwriting an existing version must be REFUSED ==="
dump "--version 0" | tail -1 | sed 's/^/    last: /'
$BIN dump --out "$W/snaps" --version 3 --stable-salt --db "$W/state/kindred.db" \
  --corpus "$CORPUS" 2>&1 | grep -viE '^$' | sed 's/^/    /'

echo
echo "=== --force-full must win over the delta decision ==="
dump "--version 0 --full" | grep -E 'writing a' | sed 's/^/    /'