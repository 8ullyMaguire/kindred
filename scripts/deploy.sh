#!/usr/bin/env bash
# Build, ship, install, restart, and verify — the whole deploy, in one script.
#
# ## Why this is a script and not a habit
#
# Because the previous session's deploy did not happen for six days while every
# gate in the project reported green. The cause was not that deploying is hard;
# it was that nothing forced it. `go test` cannot see a deployed binary,
# Playwright boots its own server from the working tree, and the behaviour gate
# verifies a product over a build it does not identify. So a stale deployment was
# indistinguishable from a good one, and nobody noticed.
#
# The step that closes it: after installing, this script runs BOTH gates and
# fails if either does. Behaviour AND provenance, because behaviour alone is the
# false clean sheet and provenance alone misses a change that was reverted.
#
# ## Order matters
#
#   build here (the deploy host has Go 1.22.2, the module needs 1.27.1)
#   -> scp
#   -> install to a temp path, then move into place
#   -> restart
#   -> wait for health, THEN gates
#
# Installing by `mv` rather than writing in place means the live binary is never
# a half-written file, and the unit is restarted only once the new file is
# complete. The mv itself needs sudo on the deploy host.
set -euo pipefail

REPO=${KINDRED_REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}
DEPLOY_HOST=${DEPLOY_HOST:-thinkcentre}
REMOTE_BIN=${REMOTE_BIN:-/usr/local/bin/kindred}
REMOTE_DIR=$(dirname "$REMOTE_BIN")
UNIT=${UNIT:-kindred}
PORT=${PORT:-8010}
LOCAL_PORT=${LOCAL_PORT:-8010}

cd "$REPO"

say() { printf '\n=== %s ===\n' "$*"; }

say "1. building for $DEPLOY_HOST"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
# -trimpath so the binary does not embed this checkout's path, and so a build
# from any directory is byte-identical to a build from this one. Without it the
# provenance gate fails for reasons that have nothing to do with the source.
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$TMP/kindred" ./cmd/kindred
go vet ./... >/dev/null
echo "  built $(sha256sum "$TMP/kindred" | cut -c1-16)… ($(stat -c %s "$TMP/kindred") bytes)"

say "2. shipping to $DEPLOY_HOST"
scp -q "$TMP/kindred" "$DEPLOY_HOST:/tmp/kindred.new"
ssh "$DEPLOY_HOST" "sha256sum /tmp/kindred.new | cut -d' ' -f1" > "$TMP/remote.sha"
LOCAL_SHA=$(sha256sum "$TMP/kindred" | cut -d' ' -f1)
REMOTE_SHA=$(cut -d' ' -f1 < "$TMP/remote.sha")
if [ "$LOCAL_SHA" != "$REMOTE_SHA" ]; then
  echo "  FAIL: the copy on $DEPLOY_HOST does not match what was built"
  echo "        local  $LOCAL_SHA"
  echo "        remote $REMOTE_SHA"
  exit 1
fi
echo "  copy verified: $REMOTE_SHA"

say "3. installing and restarting"
# `install` writes atomically via a temp file + rename, so the live path is
# never a partial file even without a separate mv.
ssh "$DEPLOY_HOST" "sudo install -m 0755 -o root -g root /tmp/kindred.new '$REMOTE_BIN' && rm -f /tmp/kindred.new"
INSTALLED=$(ssh "$DEPLOY_HOST" "sha256sum '$REMOTE_BIN' | cut -d' ' -f1")
if [ "$INSTALLED" != "$LOCAL_SHA" ]; then
  echo "  FAIL: the installed binary's sha differs from the one built"
  echo "        built    $LOCAL_SHA"
  echo "        installed $INSTALLED"
  exit 1
fi
echo "  installed and re-read: $INSTALLED"

ssh "$DEPLOY_HOST" "sudo systemctl restart $UNIT"
echo "  restarted $UNIT"

say "4. waiting for health"
# Readiness by observation, not by a fixed sleep: the gate must not run against a
# half-started process and report something about that instead.
READY=0
for i in $(seq 1 30); do
  if ssh -o BatchMode=yes -o ConnectTimeout=5 "$DEPLOY_HOST" \
       "curl -fsS --max-time 5 http://127.0.0.1:$LOCAL_PORT/healthz" >/dev/null 2>&1; then
    READY=1
    echo "  healthy after ${i}s"
    break
  fi
  sleep 1
done
if [ "$READY" -ne 1 ]; then
  echo "  FAIL: /healthz did not answer within 30s"
  ssh "$DEPLOY_HOST" "sudo journalctl -u $UNIT -n 30 --no-pager" || true
  exit 2
fi

say "5. provenance gate (is the deployed binary built from this tree?)"
bash scripts/check-provenance.sh

say "6. behaviour gate (does the product do the right thing?)"
scp -q scripts/check-deploy.sh "$DEPLOY_HOST:/tmp/check-deploy.sh"
ssh "$DEPLOY_HOST" "bash /tmp/check-deploy.sh http://127.0.0.1:$LOCAL_PORT"

say "done"
echo "deployed $LOCAL_SHA to $DEPLOY_HOST:$REMOTE_BIN and verified both gates."
echo
echo "The index age on /healthz is the age of the DATA, not of this build."
echo "A deploy does not rebuild an index; 'kindred ingest' does."