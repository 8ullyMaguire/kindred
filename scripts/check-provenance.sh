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
# ## ## What it compares, and why NOT the binary's sha256
#
# The obvious implementation -- hash the live binary, build this tree, compare
# the hashes -- is what I wrote first, and it is wrong twice over.
#
# 1. `go build` stamps the binary with the vcs revision. `go version -m` shows
#    `v0.1.1-0.2026...-1892cd1f9173` on a clean tree and the same string with
#    `+dirty` appended when anything is uncommitted. So committing changes the
#    bytes without changing a line of source, and a working tree that is one
#    commit behind produces a different binary from identical source.
# 2. CGO_ENABLED has to match exactly. With cgo the binary is ~45 KB larger and
#    can never equal the deployed one -- which made the gate report DRIFTED
#    against a binary that genuinely was built from this tree. A gate that is
#    always red gets ignored, which is worse than having no gate.
#
# So the comparison is between COMMITS, read out of the binary's own build
# metadata, and the dirty flag is reported rather than ignored:
#
#   live says +dirty   -> the binary was built from a tree with uncommitted
#                         changes, and this gate cannot tell you WHICH. That is
#                         the finding: the deployed binary is not reproducible
#                         from any commit.
#   live says a commit -> that commit must be HEAD. If it is an ancestor, the
#                         deployment is behind and the drift is named.
#
# A hash is still recorded, but as provenance evidence rather than as the
# comparison: it says which bytes were installed, and it survives the source
# moving on.
set -uo pipefail

REPO=${KINDRED_REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}
DEPLOY_HOST=${DEPLOY_HOST:-thinkcentre}
REMOTE_BIN=${REMOTE_BIN:-/usr/local/bin/kindred}

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "  PASS  $*"; }
bad() { fail=$((fail+1)); echo "  FAIL  $*"; }

REPO_HEAD=$(git -C "$REPO" log -1 --format=%H 2>/dev/null)
REPO_SHORT=$(git -C "$REPO" log -1 --format=%h 2>/dev/null)
DIRTY=$(git -C "$REPO" status --porcelain 2>/dev/null | grep -c . || true)

echo "Provenance — is $REMOTE_BIN on $DEPLOY_HOST built from this tree?"
echo "  repo    : $REPO"
echo "  host    : $DEPLOY_HOST"
echo "  head    : $REPO_SHORT ($REPO_HEAD)"
if [ "$DIRTY" -gt 0 ]; then
  echo "  tree    : $DIRTY uncommitted change(s)"
fi
echo

# Read the build metadata out of the LIVE binary on the deploy host. No copying
# a 15 MB binary over ssh just to read a string out of it.
LIVE_META=$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$DEPLOY_HOST" \
  "go version -m $REMOTE_BIN 2>/dev/null | grep -E '^\\s*(path|mod|build)' || strings $REMOTE_BIN 2>/dev/null | grep -m1 'v0.1.1-0' || true" \
  2>/dev/null)
LIVE_SHA=$(ssh -o BatchMode=yes -o ConnectTimeout=10 "$DEPLOY_HOST" \
  "sha256sum $REMOTE_BIN 2>/dev/null | cut -d' ' -f1" 2>/dev/null)

if [ -z "$LIVE_SHA" ]; then
  bad "could not read $REMOTE_BIN's sha256 over ssh (host reachable? sudo?)"
  echo
  echo "-----------------------------------------------------------"
  echo "UNREACHABLE: no provenance verdict. That is not a pass."
  exit 2
fi
echo "  live sha: $LIVE_SHA"

# The stamped revision looks like  v0.1.1-0.20261005003031-1892cd1f9173
#                    or, on a dirty tree, the same with +dirty appended.
# Keep the RAW stamp and the commit SEPARATE. An earlier version stripped the
# "+dirty" suffix off first and then asked whether the result contained "+dirty",
# so the evidence was deleted one line before it was read: a binary built from a
# dirty tree was reported as "built from <commit>, which is BEHIND HEAD" when it
# was in fact built from HEAD, just not reproducibly.
LIVE_STAMP=$(printf '%s' "$LIVE_META" | grep -oE 'v0\.[0-9.]+-0\.[0-9]+-[0-9a-f]{7,}(\+dirty)?' | tail -1)
LIVE_COMMIT=$(printf '%s' "$LIVE_STAMP" | sed 's/.*-//; s/+.*//')
LIVE_DIRTY=no
case "$LIVE_STAMP" in
  *+dirty) LIVE_DIRTY=yes ;;
esac
# `go version -m` also states it outright as a build flag, which is a second
# independent source. If either says dirty, treat it as dirty.
case "$LIVE_META" in
  *"vcs.modified=true"*) LIVE_DIRTY=yes ;;
esac
LIVE_REV=$LIVE_STAMP
echo "  live rev: ${LIVE_REV:-<not stamped>}"
echo "  dirty   : $LIVE_DIRTY"
echo

echo "Comparing:"
if [ -z "$LIVE_REV" ]; then
  bad "the deployed binary carries no readable build revision, so its origin"
  echo "        cannot be established."
  echo "-----------------------------------------------------------"
  echo "UNVERIFIABLE: the deployed binary's origin cannot be read."
  exit 2
fi

if [ "$LIVE_DIRTY" = "yes" ]; then
  bad "the deployed binary was built from a DIRTY working tree"
  echo "        +dirty means uncommitted source changes were compiled in. Those"
  echo "        changes are in no commit, so the deployment cannot be reproduced,"
  echo "        cannot be rolled back, and cannot be reviewed."
  echo "        Commit the tree, then: bash scripts/deploy.sh"
  echo "-----------------------------------------------------------"
  echo "DRIFTED: the deployed binary was built from uncommitted source."
  exit 1
fi

if [ "$LIVE_COMMIT" = "$REPO_SHORT" ]; then
  ok "the deployed binary was built from $LIVE_COMMIT, which is HEAD"
  echo "        sha $LIVE_SHA"
  echo
  echo "-----------------------------------------------------------"
  echo "IN SYNC: the deployed binary was built from this tree's HEAD."
  echo "sha $LIVE_SHA"
  exit 0
fi

if git -C "$REPO" merge-base --is-ancestor "$LIVE_COMMIT" "$REPO_HEAD" 2>/dev/null; then
  bad "the deployed binary was built from $LIVE_COMMIT, which is BEHIND HEAD"
  AHEAD=$(git -C "$REPO" rev-list --count "$LIVE_COMMIT..$REPO_HEAD" 2>/dev/null || echo '?')
  echo "        $AHEAD commit(s) behind. Redeploy: bash scripts/deploy.sh"
else
  bad "the deployed binary was built from $LIVE_COMMIT, which is not in this"
  echo "        repository's history at all -- a different tree, or a rewritten"
  echo "        history. Do not assume which; check before redeploying."
fi
echo
echo "-----------------------------------------------------------"
echo "DRIFTED: the deployed binary is not a build of HEAD."
exit 1
