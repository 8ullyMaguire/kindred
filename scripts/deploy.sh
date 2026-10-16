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
# LOCAL_PORT is deliberately NOT defaulted here: it is read from the deployed
# unit's own --listen in step 4 (see the comment there). A default is the thing
# that broke, because the unit's port and this file's copy of it can disagree.
LOCAL_PORT=${LOCAL_PORT:-}

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
# The port comes from the UNIT, not from a default in this file.
#
# LOCAL_PORT used to default to 8010 here while the deployed unit's ExecStart
# says --listen 127.0.0.1:8019. The health wait therefore polled a port nothing
# served, failed after its 30 attempts, and exited 2 -- so steps 5 and 6, the
# provenance and behaviour gates, had not run for as long as that drift lasted.
# The failure mode is the one this whole script is written against: a deploy that
# reports nothing looks exactly like a deploy that passed. Measured on
# 2026-10-16: :8010 refuses, :8019 answers {"budget_ok":true,...}.
#
# A second hard-coded copy of the port would drift again, so the port is read
# from the running unit, and if it cannot be read this refuses to continue
# rather than guessing -- a guess here is a silent no-op gate.
if [ -z "${LOCAL_PORT:-}" ]; then
  LOCAL_PORT=$(ssh -o BatchMode=yes -o ConnectTimeout=5 "$DEPLOY_HOST" \
    "systemctl show -p ExecStart --value $UNIT" 2>/dev/null |
    grep -oE -- '-+listen[= ]*[^ ]*' | head -1 | sed -E 's/.*:([0-9]+)$/\1/') || true
fi
if ! printf '%s' "${LOCAL_PORT:-}" | grep -qE '^[0-9]+$'; then
  echo "  FAIL: could not read the listening port from $UNIT's ExecStart (got '${LOCAL_PORT:-}')"
  echo "        not guessing: a wrong port makes this wait poll nothing, and the"
  echo "        provenance and behaviour gates below would never run."
  exit 2
fi
echo "  $UNIT listens on 127.0.0.1:$LOCAL_PORT (read from the unit)"

# Readiness by observation, not by a fixed sleep: the gate must not run against a
# half-started process and report something about that instead.
#
# 90 attempts, not 30. The last measured cold start loaded 634,232 nodes and
# 7,774,334 edges in ~26s (2026-10-16), and the corpus grows; a window that
# expires a second before the service binds refuses a deploy that worked. The
# loop still leaves as soon as /healthz answers, so the bound costs nothing when
# the service is fast.
READY=0
for i in $(seq 1 90); do
  # Read the status AND the body: a 503 is an ANSWER. The service is up and
  # telling us something about itself, and reporting that as "did not answer" is
  # a false statement about the deploy -- the same conflation the behaviour gate
  # refuses to make ("that is a service problem, not a drift problem, and the two
  # are not the same finding").
  #
  # Measured on this host (2026-10-16): /healthz answers 503 with
  # {"status":"over_budget","peak_rss_kib":265984,"rss_cap_kib":225280} because
  # the process peaks above the `full` mode's 220 MB cap while the collab index is
  # loaded. That is a real condition of this deployment and it pre-dates the
  # round: the previous binary logged a 285,248 KiB peak against the same cap. It
  # is NOT this script's question -- the gates below ask about provenance and
  # behaviour -- so the wait records it and carries on, and the health line says
  # which of the two it is.
  HEALTH=$(ssh -o BatchMode=yes -o ConnectTimeout=5 "$DEPLOY_HOST" \
    "curl -sS --max-time 5 -o - -w ' HTTP:%{http_code}' http://127.0.0.1:$LOCAL_PORT/healthz" 2>/dev/null) || true
  case "$HEALTH" in
  *HTTP:200*)
    READY=1
    echo "  healthy after ${i}s"
    break
    ;;
  *HTTP:000*)
    # curl prints 000 when nothing answered at all -- connection refused, or the
    # ssh itself timed out. That is the "not started yet" case, and ONLY that
    # case: a 000 must not be read as "it answered".
    ;;
  *HTTP:*)
    # It answered. Any other status counts as started, and the status is reported
    # in the service's own words.
    READY=1
    echo "  answering after ${i}s, and NOT healthy: $HEALTH"
    echo "  ^ the service's own verdict. The gates below run anyway; the behaviour"
    echo "    gate is the one that refuses an unhealthy deployment, and says so."
    break
    ;;
  esac
  sleep 1
done
if [ "$READY" -ne 1 ]; then
  echo "  FAIL: nothing answered on 127.0.0.1:$LOCAL_PORT within 90s (no HTTP status at all)"
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