#!/usr/bin/env bash
# Deploy-drift gate.
#
# ## The failure this exists to prevent
#
# On 2026-10-04 the instance on thinkcentre had been serving a binary from
# 2026-09-29 while the repository carried fourteen commits on top of it. The
# live wire had no X-Kindred-Index-Age header (SPEC §3.2.2 requires one on
# EVERY response), no Server: kindred, no sort controls on /tag/{id}, and no
# dark mode in the served stylesheet.
#
# Nothing noticed, because every gate in this project runs against the
# repository. `go test` cannot see a deployed binary, and Playwright boots its
# own server from the working tree. A deploy is the one step where the code
# being tested and the code being run are different objects, and it had no
# gate at all.
#
# ## What it checks
#
# That a given URL serves the surfaces the current tree has. Not "is it up" --
# it was up for six days while serving the wrong build. It asserts the
# specific behaviours the recent commits added, so a binary that predates them
# fails loudly instead of quietly answering.
#
# ## Usage
#
#   scripts/check-deploy.sh http://127.0.0.1:8010
#   scripts/check-deploy.sh https://kindred.example --verbose
#
# Exits 0 if the deployment matches the tree, 1 if it has drifted, 2 if the
# target is unreachable (which is a different problem and is reported as
# such -- an unreachable service is not a drifted one).
set -uo pipefail

TARGET="${1:-}"
VERBOSE="${2:-}"

if [ -z "$TARGET" ]; then
  echo "usage: $0 <base-url> [--verbose]" >&2
  exit 2
fi

TARGET="${TARGET%/}"
pass=0
fail=0
unreachable=0

note () { printf '  %s\n' "$*"; }
ok   () { pass=$((pass+1));   [ -n "$VERBOSE" ] && note "PASS  $*"; return 0; }
bad  () { fail=$((fail+1));   echo "  FAIL  $*"; return 0; }

# check NAME URL PATTERN
#   asserts PATTERN appears in the body of URL.
check () {
  local name="$1" url="$2" pattern="$3"
  local body
  if ! body=$(curl -fsS --max-time 20 "$url" 2>/dev/null); then
    unreachable=$((unreachable+1))
    echo "  UNREACHABLE  $name ($url)"
    return 0
  fi
  if grep -qE -- "$pattern" <<<"$body"; then ok "$name"; else bad "$name — expected /$pattern/ at $url"; fi
}

# header NAME URL PATTERN
header () {
  local name="$1" url="$2" pattern="$3"
  local h
  if ! h=$(curl -fsS --max-time 20 -o /dev/null -D- "$url" 2>/dev/null); then
    unreachable=$((unreachable+1))
    echo "  UNREACHABLE  $name ($url)"
    return 0
  fi
  if grep -qiE -- "$pattern" <<<"$h"; then ok "$name"; else bad "$name — no header matching /$pattern/ at $url"; fi
}

echo "deploy-drift gate: $TARGET"

# --- 0. pick a tag that actually exists ---------------------------------
#
# Hardcoding /tag/1 is wrong for a real deployment: tag id 1 exists only in
# the test fixture, so every tag-page check came back UNREACHABLE against the
# live corpus and the gate refused to give a verdict. A gate that cannot run
# against production is a gate that never runs.
#
# Discovered from /search, which is the same thing a reader does. If that
# fails, fall back to /tag/1 so a fixture-backed instance still works.
echo
echo "discovering a tag to test against..."
TAG=""
for q in general dark angst; do
  TAG=$(curl -fsS --max-time 20 "$TARGET/search?q=$q" 2>/dev/null \
        | grep -oE '/tag/[0-9]+' | head -1 | grep -oE '[0-9]+' || true)
  [ -n "$TAG" ] && break
done
if [ -z "$TAG" ]; then
  TAG=1
  note "search found no tag; falling back to /tag/1 (fine for a fixture instance)"
fi
echo "  using /tag/$TAG"

# --- 1. is it up at all -------------------------------------------------
if ! curl -fsS --max-time 10 "$TARGET/healthz" >/dev/null 2>&1; then
  echo
  echo "UNREACHABLE: $TARGET/healthz did not answer."
  echo "That is a service problem, not a drift problem, and the two are not"
  echo "the same finding. Exit 2."
  exit 2
fi

# --- 2. SPEC §3.2.2: staleness headers on EVERY response ----------------
# Deliberately checked on three different routes, because the bug this gate
# was written for was middleware wired to the wrong layer: headers present on
# /api/ and absent on HTML. One route cannot see that; three can.
echo
echo "SPEC 3.2.2 — staleness headers on every response"
for route in /healthz /api/v1/stats / /tag/1 /search; do
  header "X-Kindred-Index-Age on $route" "$TARGET$route" '^X-Kindred-Index-Age:'
  header "X-Kindred-Index-Version on $route" "$TARGET$route" '^X-Kindred-Index-Version:'
done

# --- 3. SPEC §3.2.3: the response says what it is ------------------------
echo
echo "SPEC 3.2.3 — the response identifies itself"
header "Server: kindred" "$TARGET/" '^Server:\s*kindred'

# The index age must be a real duration or the literal "unknown". A bare "0s"
# on an index older than an hour is the bug fixed in ed84b86 and it must not
# come back.
age=$(curl -fsS --max-time 20 -o /dev/null -D- "$TARGET/healthz" 2>/dev/null \
      | grep -i '^X-Kindred-Index-Age:' | tr -d '\r' | sed 's/.*: *//')
if [ -z "$age" ]; then
  bad "index age is unparseable — header absent"
elif [ "$age" = "0s" ] || [ "$age" = "0m0s" ]; then
  bad "index age is \"$age\", which claims the mirror was rebuilt this second"
elif [ "$age" = "unknown" ]; then
  # Allowed: a mirror with no recorded stamp must say so rather than guess.
  [ -n "$VERBOSE" ] && note "index age is \"unknown\" (no recorded stamp; that is the honest answer)"
  ok "index age is honest about being unknown"
else
  ok "index age is a real duration ($age)"
fi

# --- 4. the tag page's sort and length controls -------------------------
echo
echo "tag page — sort and length controls"
check "sort control present"      "$TARGET/tag/$TAG" 'name="sort"'
check "length control present"    "$TARGET/tag/$TAG" 'name="words"'
check "heading names the order"   "$TARGET/tag/$TAG" 'data-testid="works-heading"'

# Sort must REORDER, not merely render a dropdown. The dropdown can render
# while the query ignores the parameter — that is exactly the defect the
# controls shipped with, and a presence check cannot see it.
k=$(curl -fsS --max-time 20 "$TARGET/tag/1?sort=kudos&n=10" 2>/dev/null | grep -o 'href="/work/[0-9]*"' | md5sum | cut -c1-8)
r=$(curl -fsS --max-time 20 "$TARGET/tag/1?sort=recent&n=10" 2>/dev/null | grep -o 'href="/work/[0-9]*"' | md5sum | cut -c1-8)
if [ -z "$k" ] || [ "$k" = "${k//[0-9a-f]/}" ]; then
  unreachable=$((unreachable+1))
  echo "  UNREACHABLE  cannot read the works list from $TARGET/tag/1"
elif [ "$k" = "$r" ]; then
  bad "?sort=kudos and ?sort=recent return the same list ($k) — the sort is inert"
else
  ok "?sort= changes the order ($k != $r)"
fi

# The length filter must EXCLUDE. Same reason: a control can render while the
# bound is ignored, which is indistinguishable from "no short works here".
all=$(curl -fsS --max-time 20 "$TARGET/tag/1?n=50" 2>/dev/null | grep -c '<div class="work">')
short=$(curl -fsS --max-time 20 "$TARGET/tag/1?words=under:1&n=50" 2>/dev/null | grep -c '<div class="work">')
if [ -z "$all" ]; then
  unreachable=$((unreachable+1))
  echo "  UNREACHABLE  cannot count works on $TARGET/tag/1"
elif [ "$short" -ge "$all" ]; then
  bad "?words=under:1 returned $short of $all works — a length bound below the minimum should return none"
else
  ok "?words= excludes rows ($short of $all)"
fi

# --- 5. the stylesheet --------------------------------------------------
echo
echo "stylesheet — dark mode, print, narrow screens, focus"
check "dark mode block"      "$TARGET/static/style.css" 'prefers-color-scheme:\s*dark'
check "print rules"          "$TARGET/static/style.css" '@media print'
check "narrow-screen block"  "$TARGET/static/style.css" '@media[^{]*max-width'
check "visible focus ring"   "$TARGET/static/style.css" ':focus-visible'

# --- 6. no JavaScript ---------------------------------------------------
# SPEC §1.1 forbids it, and the reason this project is a 15 MB binary that
# runs on a Pi. A tag that crept in is a regression in the product's claim,
# not a feature.
echo
echo "SPEC 1.1 — no JavaScript"
scripts=$(curl -fsS --max-time 20 "$TARGET/" 2>/dev/null | grep -c '<script' || true)
if [ "$scripts" -eq 0 ]; then ok "no <script> on the root page"; else bad "the root page loads $scripts script(s); SPEC 1.1 forbids client-side JS"; fi

# --- verdict ------------------------------------------------------------
echo
echo "-----------------------------------------------------------"
if [ "$unreachable" -gt 0 ]; then
  echo "INCOMPLETE: $unreachable check(s) could not reach $TARGET."
  echo "Not a verdict. Fix reachability and re-run."
  exit 2
fi
if [ "$fail" -gt 0 ]; then
  echo "DRIFTED: $fail of $((pass+fail)) checks failed."
  echo "The deployment at $TARGET does not match this tree."
  echo "Rebuild, redeploy, re-run this gate."
  exit 1
fi
echo "IN SYNC: all $pass checks pass against $TARGET."
exit 0
