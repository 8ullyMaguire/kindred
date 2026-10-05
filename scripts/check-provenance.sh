#!/usr/bin/env bash
# Provenance gate: is the deployed binary built from THIS tree?
#
# ## The failure this exists to prevent
#
# `scripts/check-deploy.sh` makes 32 BEHAVIOUR checks. On 2026-10-04 it
# reported "all 32 checks pass" against a binary built on 2026-09-29, while the
# repository carried fourteen commits on top of it — including the commit that
# added the sort controls and dark mode the checks were verifying.
#
# Nothing was wrong with the checks. They asked "does the product do the right
# thing", the product did, and the product being asked about was not the one in
# the tree. Behaviour cannot distinguish "correct" from "correct, from six days
# ago", which is why this is a separate script rather than a thirty-third check.
#
# ## Why a separate script
#
# The two halves need different places. The behaviour gate needs `curl` against
# 127.0.0.1:8010, and the service binds localhost only, so it runs ON the deploy
# host. This needs `go` and .git, which are on the repo host. Trying to make one
# script do both meant a missing half was silently a pass, so they are two
# scripts and `deploy.sh` runs both.
#
# ## What it compares
#
# The live binary's sha256 against a fresh `-trimpath` build of ./cmd/kindred from
# this tree, with the same flags deploy.sh uses. Same source + same flags +
# same toolchain = same bytes, so equality is meaningful.
#
# It does NOT compare against a recorded "last deployed sha", because a recorded
# value is only as good as the moment it was written, and the whole problem here
# was a value that was written once and then went stale silently.
set -uo pipefail

REPO=${KINDRED_REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}
DEPLOY_HOST=${DEPLOY_HOST:-thinkcentre}
REMOTE_BIN=${REMOTE_BIN:-/usr/local/bin/kindred}

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  PASS  $*"; }
bad() { fail=$((fail+1)); echo "  FAIL  $*"; }

echo "Provenance — is $REMOTE_BIN on $DEPLOY_HOST built from this tree?"
echo "  repo    : $REPO"
echo "  host    : $DEPLOY_HOST"
echo "  head    : $(git -C "$REPO" log -1 --format='%h %s' 2>/dev/null)"
echo

LIVE_SHA=$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$DEPLOY_HOST" \
  "sha256sum $REMOTE_BIN 2>/dev/null | cut -d' ' -f1" 2>/dev/null)
if [ -z "$LIVE_SHA" ]; then
  bad "could not read $REMOTE_BIN's sha256 over ssh (host reachable? sudo?)"
  echo
  echo "-----------------------------------------------------------"
  echo "UNREACHABLE: no provenance verdict. That is not a pass."
  exit 2
fi
echo "  live    : $LIVE_SHA"

if ! command -v go >/dev/null 2>&1; then
  bad "go is not on PATH here, so there is nothing to compare against"
  exit 2
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
echo "  building ./cmd/kindred with the deploy flags..."
if ! CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
     -o "$TMP/kindred" "$REPO/cmd/kindred" 2>"$TMP/err"; then
  bad "build failed: $(head -3 "$TMP/err" | tr '\n' ' ')"
  exit 2
fi
LOCAL_SHA=$(sha256sum "$TMP/kindred" | cut -d' ' -f1)
LOCAL_SIZE=$(stat -c %s "$TMP/kindred")
echo "  local   : $LOCAL_SHA ($LOCAL_SIZE bytes)"

echo
if [ "$LIVE_SHA" = "$LOCAL_SHA" ]; then
  ok "deployed binary is byte-identical to a build of this tree"
  echo
  echo "-----------------------------------------------------------"
  echo "IN SYNC: the deployed binary matches this tree."
  exit 0
fi

LIVE_SIZE=$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$DEPLOY_HOST" \
  "stat -c %s $REMOTE_BIN 2>/dev/null" 2>/dev/null)
bad "deployed binary differs from a build of this tree"
echo "        live sha : $LIVE_SHA  (${LIVE_SIZE:-?} bytes)"
echo "        local sha: $LOCAL_SHA  ($LOCAL_SIZE bytes)"
if [ -n "$LIVE_SIZE" ] && [ "$LIVE_SIZE" != "$LOCAL_SIZE" ]; then
  echo "        The sizes differ too, so this is a different build and not a"
  echo "        rebuild of the same source."
else
  echo "        Same size, different bytes: source changed, or the build flags"
  echo "        differ from the ones deploy.sh uses."
fi
echo
echo "        Behaviour can still pass here, and that is the point: the product"
echo "        may be fine while the BUILD is not what anyone believes is deployed."
echo "        Redeploy:  bash scripts/deploy.sh"
echo
echo "-----------------------------------------------------------"
echo "DRIFTED: the deployed binary is not a build of this tree."
exit 1