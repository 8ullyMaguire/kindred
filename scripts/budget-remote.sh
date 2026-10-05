#!/usr/bin/env bash
# budget-remote.sh — run the SPEC §6 memory budget gate where the corpus is.
#
# ## Why this exists
#
# `make budget` cannot run on the repo host: the 1.7 GB mirror lives on
# thinkcentre, and the gate correctly refuses to run without it rather than
# measuring nothing and calling it a pass. Until now the budget was measured by
# hand and written down, which is a wish rather than a gate.
#
# So: build here, ship the binary, run scripts/budget.sh there against the real
# corpus, and bring the verdict back. `make budget` calls this when the corpus is
# remote.
#
# ## The live state db must not be opened
#
# This is the load-bearing safety rule, and it is easy to get wrong: the script
# passes a `--db` to the server it boots, and the obvious value is the deployed
# service's own database. Booting a second process against a SQLite WAL
# database while the service holds it open risks the second writer checkpointing
# over the first's state. The pre-flight below COPIES the state db into a scratch
# directory and points the gate at the copy, and it verifies the copy is a
# separate inode before booting anything.
#
# The corpus is opened read-only by the server, so it is used in place — copying
# 1.7 GB to measure memory would be its own kind of waste.
#
# ## A stale binary measures the wrong thing
#
# The gate measures the process it boots, so it measures whatever binary was
# shipped. The sha is checked before and after, and the script refuses to report
# a number for a binary it cannot identify — the same lesson as
# check-provenance.sh, arrived at independently.
set -uo pipefail

REPO=${KINDRED_REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}
DEPLOY_HOST=${DEPLOY_HOST:-thinkcentre}
REMOTE_CORPUS=${REMOTE_CORPUS:-/home/alvaro/kindling-data/ao3_metadata.db}
REMOTE_DB=${REMOTE_DB:-/home/alvaro/.local/share/kindred/kindred.db}
MODE=${MODE:-full}
PORT=${PORT:-8199}   # deliberately not 8010: the live service owns that
BIN=/tmp/kindred-budget

cd "$REPO"
say() { printf '\n=== %s ===\n' "$*"; }

say "1. building"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$TMP/kindred" ./cmd/kindred || exit 1
LOCAL_SHA=$(sha256sum "$TMP/kindred" | cut -d' ' -f1)
echo "  built $LOCAL_SHA"

say "2. shipping to $DEPLOY_HOST"
# The GATE ships too. The deploy host has no checkout, so running the gate from
# "$REPO/scripts/budget.sh" there fails with 127 -- and the verdict was labelling
# that as "over the cap". A gate that cannot tell "the measurement failed" from
# "the measurement failed badly" will eventually report a memory regression when
# the real problem is a missing file.
scp -q "$TMP/kindred" "$DEPLOY_HOST:$BIN" || exit 1
scp -q "$REPO/scripts/budget.sh" "$DEPLOY_HOST:/tmp/budget-remote-inner.sh" || exit 1
echo "  binary at $BIN, gate at /tmp/budget-remote-inner.sh"

say "3. pre-flight on the deploy host"
# Everything below runs on the remote. Written to a file rather than inlined
# through ssh: zsh expands pipes inside double quotes, so a remote
# `grep -cE "(a|b)"` returns "no matches found" and the next echo reports 127
# for a command that never ran, with any real error sitting beside it looking
# like noise.
cat > "$TMP/preflight.sh" <<PREFLIGHT
set -uo pipefail
CORPUS='$REMOTE_CORPUS'
LIVE_DB='$REMOTE_DB'

if [ ! -r "\$CORPUS" ]; then
  echo "  FAIL corpus \$CORPUS is not readable"
  exit 1
fi
echo "  corpus: \$(stat -c %s "\$CORPUS") bytes"

SCRATCH=\$(mktemp -d /tmp/kindred-budget-XXXXXX)
# Copy the state db, never open the live one. See the header.
# The index is a SEPARATE FILE, and this is the bug this script had.
#
# state.db          216 MB, SQLite tables: edges, embeddings, arena_*
# state.db.graph     28 MB, the mmapped CSR co-occurrence index
#
# Copying only the .db gives the booted server a database with no index beside
# it. It starts, serves every route, and peaks at 24 MiB -- because the graph is
# never loaded. The live service, which has both files, peaks at 152 MiB. The
# gate was measuring a server six times lighter than the product, and PASSING.
#
# The log said so plainly and the gate ignored it:
#     no co-occurrence index; tag-similarity endpoints will answer 503
#     read index: open <scratch>/state.db.graph: no such file or directory
#
# which is the same shape as every other gate in this project that reported
# success while examining nothing. So the copy is now asserted, not attempted.
if [ -r "\$LIVE_DB" ]; then
  cp "\$LIVE_DB" "\$SCRATCH/state.db"
  echo "  state: copied \$LIVE_DB -> \$SCRATCH/state.db"
  for suffix in .graph .embeddings; do
    if [ -r "\$LIVE_DB\$suffix" ]; then
      cp "\$LIVE_DB\$suffix" "\$SCRATCH/state.db\$suffix"
      echo "  index: copied \$LIVE_DB\$suffix (\$(stat -c %s "\$LIVE_DB\$suffix") bytes)"
    else
      echo "  index: \$LIVE_DB\$suffix is ABSENT"
    fi
  done
  if [ ! -r "\$SCRATCH/state.db.graph" ]; then
    echo "  FAIL no index beside the live state db. A memory measurement without"
    echo "       the index describes a server the product does not run."
    exit 1
  fi
else
  : > "\$SCRATCH/state.db"
  echo "  state: no live db at \$LIVE_DB; starting empty"
fi
# Prove they are separate files, before anything boots.
LIVE_INO=\$(stat -c %i "\$LIVE_DB" 2>/dev/null || echo none)
COPY_INO=\$(stat -c %i "\$SCRATCH/state.db" 2>/dev/null || echo none)
if [ "\$LIVE_INO" != "none" ] && [ "\$LIVE_INO" = "\$COPY_INO" ]; then
  echo "  FAIL the scratch db and the live db are the same inode"
  exit 1
fi
echo "  inodes: live=\$LIVE_INO scratch=\$COPY_INO (distinct)"
echo "SCRATCH=\$SCRATCH"
PREFLIGHT

SCRATCH=$(ssh "$DEPLOY_HOST" "bash -s" < "$TMP/preflight.sh" | tee "$TMP/pre.out" | sed -n 's/^SCRATCH=//p')
PRE_RC=${PIPESTATUS[0]}
cat "$TMP/pre.out" | sed 's/^/  /'
if [ "$PRE_RC" -ne 0 ]; then
  echo "  pre-flight failed"
  exit 2
fi
if [ -z "$SCRATCH" ]; then
  echo "  pre-flight did not report a scratch dir; refusing to guess"
  exit 2
fi

say "4. running the gate on $DEPLOY_HOST (mode=$MODE, port=$PORT)"
ssh "$DEPLOY_HOST" "bash /tmp/budget-remote-inner.sh '$BIN' '$REMOTE_CORPUS' '$SCRATCH/state.db' '$PORT' '$MODE'" \
  2>&1 | tee "$TMP/budget.out" | sed 's/^/  /'
RC=${PIPESTATUS[0]}

say "5. verifying which binary was measured"
# The gate measured the process it booted, so it measured $BIN. Confirm that is
# what we built, or the number describes something else.
REMOTE_SHA=$(ssh "$DEPLOY_HOST" "sha256sum '$BIN' 2>/dev/null | cut -d' ' -f1")
if [ "$REMOTE_SHA" != "$LOCAL_SHA" ]; then
  echo "  FAIL the measured binary is $REMOTE_SHA, not the one built ($LOCAL_SHA)"
  echo "        A memory number for an unidentified binary is not a measurement."
  exit 2
fi
echo "  measured binary: $REMOTE_SHA (matches the build)"

ssh "$DEPLOY_HOST" "rm -rf '$SCRATCH'" >/dev/null 2>&1 || true
ssh "$DEPLOY_HOST" "rm -f '$BIN' /tmp/budget-remote-inner.sh" >/dev/null 2>&1 || true

say "verdict"
if [ "$RC" -eq 0 ]; then
  echo "PASS: peak RSS within the $MODE cap of $([ "$MODE" = lite ] && echo 60 || echo 220) MiB"
  echo "measured: $LOCAL_SHA"
  exit 0
fi
case "$RC" in
  2)
    echo "UNREACHABLE (exit 2): the gate could not measure. Not a pass."
    exit 2 ;;
  127)
    echo "UNREACHABLE (exit 127): the gate itself is missing on the host."
    echo "  This is a broken harness, not a memory regression. Do not read it as one."
    exit 2 ;;
esac
echo "FAIL (exit $RC): over the cap, or the gate failed."
grep -E 'FAIL|peak' "$TMP/budget.out" | tail -14 | sed 's/^/  /'
exit 1