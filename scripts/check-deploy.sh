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

# REPO is the tree this gate compares against. Overridable so the gate can run
# from a copy, at the cost of comparing against the wrong tree if someone points
# it somewhere else -- which is why it defaults to the script's own location
# rather than to $PWD.
REPO=${KINDRED_REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}

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
# A tag with one work cannot prove a filter works: every filter would return
# either 0 or 1, and both are what a broken filter returns. So the discovery
# prefers a tag with enough works to discriminate, and says which it picked.
#
# /search?q=general&sort=kudos is used rather than the plain search, because
# the plain one's first hit on the real mirror is a tag with a single work.
# How many works a filtered tag page reports.
#
# NOT by counting the rendered cards: the page limit is 100, so every filter on
# a tag with 100+ works renders exactly 100 cards and all of them look inert.
# The first version of this gate counted cards, picked a 100-work tag, and
# reported "the filter is inert" on a filter that works perfectly.
#
# The heading's total is the FILTERED COUNT and is not limited by n -- which is
# exactly the property these checks want, since a count that ignores the filters
# is separately gated by TestTagTotalCountsTheFilteredSet.
#
# Two sources, in order: the explicit total, then the fallback of rendered
# cards. The fallback keeps the check working on a page too small to show a
# total.
count_works () {
  local page total
  page=$(curl -fsS --max-time 25 "$1" 2>/dev/null) || { echo 0; return; }
  total=$(printf '%s' "$page" \
    | tr '\n' ' ' \
    | grep -oE 'of [0-9,]+' \
    | head -1 \
    | tr -d ',' \
    | grep -oE '[0-9]+')
  if [ -n "${total:-}" ] && [ "$total" != "0" ]; then
    echo "$total"
    return
  fi
  # No works heading (empty result, or an error page): count what rendered.
  printf '%s' "$page" | grep -c '<div class="work">' || true
}
# Does this tag discriminate on both axes the gate tests?
#
# A tag whose rating distribution is single-valued cannot test a rating filter:
# on the real mirror, tag 26 is "explicit" and all 42,796 of its works are
# Explicit, so rating=E returns every one and rating=G returns none. Both are
# CORRECT, and both look exactly like a filter that does nothing. The first
# version of this gate picked the biggest tag, hit that, and reported two
# failures on working code.
discriminates () {
  local t=$1 base ct cf rg re
  base=$(count_works "$TARGET/tag/$t?n=100")
  [ "${base:-0}" -lt 10 ] && return 1
  ct=$(count_works "$TARGET/tag/$t?n=100&complete=true")
  cf=$(count_works "$TARGET/tag/$t?n=100&complete=false")
  # Both completion states present and different.
  [ "${ct:-0}" -ge 1 ] && [ "${cf:-0}" -ge 1 ] || return 1
  [ "$ct" = "$cf" ] && return 1
  # At least two distinct ratings present, none of which is everything.
  rg=$(count_works "$TARGET/tag/$t?n=100&rating=G")
  re=$(count_works "$TARGET/tag/$t?n=100&rating=E")
  [ "${rg:-0}" -ge 1 ] && [ "${re:-0}" -ge 1 ] || return 1
  [ "$re" = "$base" ] && return 1   # every work is E: cannot discriminate
  return 0
}

TAG="${KINDRED_GATE_TAG:-}"
TAG_WORKS=0
if [ -n "$TAG" ]; then
  TAG_WORKS=$(count_works "$TARGET/tag/$TAG?n=100")
  discriminates "$TAG" || note "KINDRED_GATE_TAG=$TAG cannot discriminate on every "\
"axis, so the filter checks below may report false failures"
fi
for q in general explicit angst relationship; do
  for cand in $(curl -fsS --max-time 20 "$TARGET/search?q=$q&n=100" 2>/dev/null \
                 | grep -oE '/tag/[0-9]+' | grep -oE '[0-9]+' | sort -un); do
    n=$(count_works "$TARGET/tag/$cand?n=100")
    if [ "${n:-0}" -gt "$TAG_WORKS" ]; then
      TAG=$cand
      TAG_WORKS=$n
    fi
  done
  # Only a tag that discriminates on both axes is usable. Keep the biggest
  # candidate seen so far, but stop as soon as one qualifies.
  if [ -n "$TAG" ] && discriminates "$TAG"; then
    echo "  tag/$TAG discriminates on completion and rating"
    break
  fi
done
if [ -z "$TAG" ]; then
  TAG=1
  TAG_WORKS=$(count_works "$TARGET/tag/1?n=100")
  note "found no tag with works; falling back to /tag/1 (fine for a fixture instance)"
fi
echo "  using /tag/$TAG ($TAG_WORKS works)"

# --- filters: the controls must be there AND must work --------------------
#
# Presence is not behaviour. A control can render perfectly while the query
# ignores the parameter, which is exactly the defect these shipped with: the API
# honoured complete/rating/lang and the page ignored them, with no message.
# Each check therefore runs the filter and asserts the RESULT moved.
echo
echo "filters:"
ctl=$(curl -fsS --max-time 20 "$TARGET/tag/$TAG" || true)
for name in complete rating lang; do
  if printf '%s' "$ctl" | grep -qE "name=\"$name\""; then
    ok "$name control is present"
  else
    bad "$name control is MISSING from the tag page"
  fi
done

base_count=$TAG_WORKS
[ "${base_count:-0}" -eq 0 ] && base_count=$(count_works "$TARGET/tag/$TAG?n=100")
if [ "${base_count:-0}" -gt 0 ]; then
  ok "tag has $base_count works to filter"

  # complete: both states must be non-empty and different.
  ct=$(count_works "$TARGET/tag/$TAG?n=100&complete=true")
  cf=$(count_works "$TARGET/tag/$TAG?n=100&complete=false")
  if [ "${ct:-0}" -gt 0 ] && [ "${cf:-0}" -gt 0 ] && [ "$ct" != "$cf" ]; then
    ok "complete filter works ($ct complete, $cf in progress)"
  else
    bad "complete=true gave $ct, complete=false gave $cf (base $base_count). "\
"Both states must be non-empty and different, or the filter is inert."
  fi

  # The union must equal the base: the two states partition the tag.
  if [ $(( ct + cf )) -eq "$base_count" ]; then
    ok "the two completion states partition the tag ($ct + $cf = $base_count)"
  else
    bad "the completion states cover $(( ct + cf )) of $base_count works. "\
"A duplicated join row or a NULL complete makes these disagree."
  fi

  # rating: at least one letter must select a different, non-empty set.
  rG=$(count_works "$TARGET/tag/$TAG?n=100&rating=G")
  rE=$(count_works "$TARGET/tag/$TAG?n=100&rating=E")
  if [ "${rG:-0}" -gt 0 ] && [ "${rE:-0}" -gt 0 ] && [ "$rG" != "$rE" ]; then
    ok "rating filter works (G: $rG, E: $rE)"
  else
    bad "rating=G gave $rG and rating=E gave $rE; both must be non-empty and "\
"different. The mirror stores FULL rating names, so the letters must be "\
"translated."
  fi

  # A rating letter the mirror does not contain must match NOTHING, never
  # something nearby. Z is explicit in some storefronts; mapping it to Mature
  # would be confidently wrong.
  rZ=$(count_works "$TARGET/tag/$TAG?n=100&rating=Z")
  if [ "${rZ:-1}" -eq 0 ]; then
    ok "?rating=Z matches nothing, as it must"
  else
    bad "?rating=Z matched $rZ works. Only AO3's G/T/M/E may be translated; "\
"anything else has to match nothing."
  fi

  # lang: English vs a language the tag may not have. Only asserted when the
  # mirror has more than one language at all, or the check is theatre.
  lE=$(count_works "$TARGET/tag/$TAG?n=100&lang=English")
  lK=$(count_works "$TARGET/tag/$TAG?n=100&lang=Klingon")
  if [ "${lE:-0}" -gt 0 ] && [ "${lK:-1}" -eq 0 ]; then
    ok "language filter works (English: $lE, Klingon: 0)"
  elif [ "${lE:-0}" -le "$base_count" ] && [ "${lK:-1}" -eq 0 ]; then
    ok "language filter present (English: $lE)"
  else
    bad "language filter looks wrong: English $lE, Klingon $lK (must be 0)"
  fi

  # No work may be listed twice.
  dupes=$(curl -fsS --max-time 20 "$TARGET/tag/$TAG?n=100" \
    | grep -oE '<div class="work">\s*<h3><a href="/work/[0-9]+"' \
    | grep -oE '[0-9]+' | sort | uniq -d | wc -l)
  if [ "${dupes:-0}" -eq 0 ]; then
    ok "no work is listed twice (work_tags is keyed on work_id, tag_id, tag_type)"
  else
    bad "$dupes work(s) are listed twice; the list query needs SELECT DISTINCT"
  fi
else
  bad "the discovered tag has no works, so the filters cannot be checked"
fi

# The filter checks need a tag big enough to discriminate. Saying so is better
# than reporting a pass that means nothing.
if [ "${TAG_WORKS:-0}" -lt 10 ] && [ "${base_count:-0}" -lt 10 ]; then
  bad "tag/$TAG has only ${TAG_WORKS:-0} works. A filter over one work returns "\
"0 or 1 either way, so the checks above cannot distinguish a working filter "\
"from an inert one -- they are NOT a pass. Point KINDRED_GATE_TAG at a larger tag."
fi

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
echo "BEHAVIOUR IN SYNC: all $pass checks pass against $TARGET."
echo "This says the PRODUCT matches. It does NOT say the BUILD does."
echo "Run scripts/check-provenance.sh for that -- behaviour alone is the false"
echo "clean sheet that let a six-day-old binary survive."
exit 0
