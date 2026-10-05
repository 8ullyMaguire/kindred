#!/usr/bin/env bash
# budget-pi.sh — the SPEC §6 memory budget, measured on the Pi.
#
# ## Why this is a separate script from budget.sh
#
# `budget.sh` measures a binary on a host that has the corpus. The Pi has the
# corpus too, but it has a different architecture and 512 MB of RAM, and a
# memory number from x86 is not a memory number from a Pi: the allocator, the
# page size and the kernel's accounting all differ.
#
# PLAN §7.3 is explicit about what counts as evidence here:
#
#   "A real arm64 run on real hardware — not a cross-compile, not a claim."
#
# So this script refuses to report a number it did not measure, and it refuses to
# run anywhere that is not arm64. A cross-compiled binary that starts on x86
# proves the build works; it says nothing about the memory, and a script that
# reported the x86 number under a Pi heading would be exactly the "claim" §7.3
# rules out.
#
# ## What it does check on any host
#
# The cross-build. That is not nothing: it is the part that can silently break
# (a cgo dependency creeps in, and GOOS/GOARCH stops mattering), and it is what
# PLAN §7.1 requires be true before a binary is copied to the Pi at all.
set -uo pipefail

REPO=${KINDRED_REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}
CAP_MIB=${BUDGET_MIB:-60}          # SPEC §6: the lite cap
MODE=${MODE:-lite}
PORT=${PORT:-8011}                 # not 8010: the live Pi service owns that
BIN=${1:-/tmp/kindred-pi}
CORPUS=${2:-}
DB=${3:-}
say() { printf '\n=== %s ===\n' "$*"; }

say "1. the arm64 cross-build (checkable anywhere)"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
if CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' \
     -o "$TMP/kindred" "$REPO/cmd/kindred"; then
  SIZE=$(stat -c %s "$TMP/kindred")
  echo "  built $SIZE bytes"
  # `file` confirms the architecture rather than trusting the env vars: a
  # CGO_ENABLED default that differs would silently produce a host binary that
  # still builds clean, which is the failure this check exists for.
  if command -v file >/dev/null; then
    file "$TMP/kindred" | sed 's/^/  /'
    if ! file "$TMP/kindred" | grep -qi 'aarch64\|arm64'; then
      echo "  FAIL the build is not aarch64 despite GOARCH=arm64"
      exit 1
    fi
  fi
  echo "  OK: static aarch64 binary, CGO_ENABLED=0 so the Pi needs no C toolchain"
else
  echo "  FAIL: the arm64 build does not compile"
  exit 1
fi

say "2. the measurement, which is NOT checkable here"
ARCH=$(uname -m)
echo "  this host is $ARCH"

if [ "$ARCH" != "aarch64" ] && [ "$ARCH" != "arm64" ]; then
  cat <<'EOF'
  SKIPPED: this is not a Pi.

  PLAN 7.3 requires "a real arm64 run on real hardware -- not a cross-compile,
  not a claim", so no memory figure is printed here. Printing the x86 number
  under a Pi heading is the exact failure that requirement exists to prevent.

  To measure, on the Pi:

      systemctl --user restart kindred          # or: sudo systemctl restart kindred-pi
      curl -fsS localhost:8010/v1/healthz       # {"status":"ok",...}

  and the number that matters is already reported by the running service:

      systemctl show kindred-pi -p MemoryCurrent

  which is the kernel's own accounting, not a sample taken by this script.
EOF
  exit 2
fi

# Past here we are on arm64, so the number is real.
if [ -z "$CORPUS" ] || [ -z "$DB" ]; then
  echo "  usage: budget-pi.sh [binary] <corpus> <state-db>" >&2
  exit 2
fi

say "3. measuring lite against a $CAP_MIB MiB cap"
exec "$REPO/scripts/budget.sh" "$BIN" "$CORPUS" "$DB" "$PORT" "$MODE"