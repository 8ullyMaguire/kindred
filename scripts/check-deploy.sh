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
#
# Any HTTP status counts as reached: only curl's 000 (nothing answered at all) is
# UNREACHABLE. `curl -f` treats a 503 as silence, and /healthz answers 503 while
# the process is over its RSS budget -- measured 2026-10-16, the body and the
# headers are both there on that 503, and -f turned true checks into
# "UNREACHABLE" about a route that had answered.
check () {
  local name="$1" url="$2" pattern="$3"
  local body code
  body=$(curl -sS --max-time 20 -w $'\n__CODE__%{http_code}' "$url" 2>/dev/null) || true
  code=${body##*__CODE__}
  body=${body%$'\n__CODE__'*}
  if [ -z "$code" ] || [ "$code" = "000" ]; then
    unreachable=$((unreachable+1))
    echo "  UNREACHABLE  $name ($url)"
    return 0
  fi
  if grep -qE -- "$pattern" <<<"$body"; then ok "$name"; else bad "$name — expected /$pattern/ at $url"; fi
}

# check_not NAME URL PATTERN
#   asserts PATTERN does NOT appear in the body of URL. Same reachability rule.
#
#   An absence assertion must name its own subject, or it passes vacuously: a page
#   that failed to load has no forbidden string in it either. Every caller pairs this
#   with a check() of the same URL that proves the part of the body being denied
#   exists at all.
check_not () {
  local name="$1" url="$2" pattern="$3"
  local body code
  body=$(curl -sS --max-time 20 -w $'\n__CODE__%{http_code}' "$url" 2>/dev/null) || true
  code=${body##*__CODE__}
  body=${body%$'\n__CODE__'*}
  if [ -z "$code" ] || [ "$code" = "000" ]; then
    unreachable=$((unreachable+1))
    echo "  UNREACHABLE  $name ($url)"
    return 0
  fi
  if grep -qE -- "$pattern" <<<"$body"; then
    bad "$name — found /$pattern/ at $url, which must not be there"
  else
    ok "$name"
  fi
}

# header NAME URL PATTERN
#   asserts a response header matching PATTERN. Same rule as check(): a non-200
#   is an answer, and SPEC §3.2.2 wants the staleness headers on EVERY response,
#   so the 503 is exactly the response that must carry them.
header () {
  local name="$1" url="$2" pattern="$3"
  local h code
  h=$(curl -sS --max-time 20 -o /dev/null -D- -w $'\n__CODE__%{http_code}' "$url" 2>/dev/null) || true
  code=${h##*__CODE__}
  if [ -z "$code" ] || [ "$code" = "000" ]; then
    unreachable=$((unreachable+1))
    echo "  UNREACHABLE  $name ($url)"
    return 0
  fi
  if grep -qiE -- "$pattern" <<<"$h"; then ok "$name"; else bad "$name — no header matching /$pattern/ at $url"; fi
}

# NOT swept in this pass: the section-level fetches below still use `curl -f`.
# Several of them are the `x=$(curl -f … || true)` shape, where a non-200
# currently reads as absent -- flipping those to "any status is an answer" would
# invert them rather than fix them, so each needs its own reading. They are only
# reached for page routes, which answer 200 on this deployment.

echo "deploy-drift gate: $TARGET"

# --- is it answering, and what does it say about itself -------------------
#
# Unnumbered deliberately: this runs FIRST, and the numbered sections below keep
# their numbers. It used to sit at "--- 1. is it up at all" some 200 lines down,
# i.e. after the tag discovery it then threw away -- the cheapest precondition in
# the script was its last check. Measured 2026-10-16: the gate spent ten minutes
# choosing a tag to test and then exited 2 without running a single check.
#
# And a 503 is an ANSWER. `curl -f` reads it as silence, so the gate announced
# "/healthz did not answer" about a service that had answered -- on this host
# {"status":"over_budget","peak_rss_kib":265984,...} under the old 220 MiB cap
# (removed 2026-10-22 -- a serving process is healthy; this branch is now
# defensive), i.e. the
# process peaked above the `full` mode's 220 MB cap while the collab index was
# loaded (measured: 200 twenty-one seconds after a restart, 503 once warm).
# Reachability, health and drift are three different findings, and this file's
# own header already says two of them are not the same one.
HEALTH=$(curl -sS --max-time 10 -o - -w ' HTTP:%{http_code}' "$TARGET/healthz" 2>/dev/null) || true
case "$HEALTH" in
*HTTP:200*)
  echo
  echo "it answers, and reports itself healthy (HTTP 200)."
  ;;
*HTTP:000*)
  echo
  echo "UNREACHABLE: $TARGET/healthz did not answer at all (no HTTP status)."
  echo "That is a service problem, not a drift problem, and the two are not"
  echo "the same finding. Exit 2."
  exit 2
  ;;
*HTTP:*)
  echo
  echo "UNHEALTHY: the service answered, and this is its own verdict:"
  printf '  %s\n' "${HEALTH}" | head -c 400
  echo
  echo "That is a service problem, not a drift problem, and the two are not"
  echo "the same finding. No drift verdict is given: a service that cannot"
  echo "report itself healthy cannot be certified against this tree. Exit 2."
  exit 2
  ;;
esac

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

# Discovery asks the CORPUS, not the search page.
#
# The search-based walk this replaced could not finish on the live mirror and
# did not fail — it HUNG, which is worse. Measured on 2026-10-23: /search returns
# 100 DISTINCT tag links per query, most of them freeform sentence tags
# ("a lot of coercion in general really") with one work each, so `count_works`
# made ~400 requests at 3.4s apiece, then picked a 1-work tag and asserted the
# filters against it. The gate had therefore been reporting on a tag no filter
# could discriminate on, and a deploy could not complete inside any reasonable
# window.
#
# One SQL query over the corpus answers the question the walk was trying to
# approximate: which tag has enough works, several ratings, and both completion
# states? On the live mirror that is tag 29 at 55,810 works (25,293 complete /
# 30,517 in progress, 5 distinct ratings). It is a FIXTURE CHOICE, not a
# behaviour assertion — every check below still goes over HTTP — so reading the
# corpus for it does not weaken the gate.
#
# Without the DB (a fixture instance, or a host that does not ship the corpus),
# the HTTP walk still runs, now capped: eight candidates, best-first, stopping
# at the first that discriminates. Bounded, and it says which source it used.
CORPUS_DB=${KINDRED_CORPUS_DB:-/home/alvaro/kindling-data/ao3_metadata.db}
if [ -z "$TAG" ] && [ -r "$CORPUS_DB" ] && command -v sqlite3 >/dev/null 2>&1; then
  TAG=$(sqlite3 -readonly "$CORPUS_DB" \
    "SELECT wt.tag_id FROM work_tags wt JOIN works w ON w.id = wt.work_id
     GROUP BY wt.tag_id
     HAVING COUNT(*) >= 50 AND COUNT(DISTINCT w.rating) >= 2
        AND SUM(w.complete = 1) > 5 AND SUM(w.complete = 0) > 5
     ORDER BY COUNT(*) DESC LIMIT 1;" 2>/dev/null || true)
  if [ -n "$TAG" ]; then
    TAG_WORKS=$(count_works "$TARGET/tag/$TAG?n=100")
    echo "  corpus names tag/$TAG ($TAG_WORKS works, several ratings, both completion states)"
  else
    echo "  corpus has no tag that discriminates; falling back to the search walk"
  fi
fi

if [ -z "$TAG" ]; then
  # Best-first over a bounded candidate set: the tag with the most works wins,
  # then the first that discriminates on both axes. The cap is what makes this
  # terminate; the message names it so a reader knows the walk was truncated.
  for q in general explicit angst relationship; do
    for cand in $(curl -fsS --max-time 20 "$TARGET/search?q=$q&n=100" 2>/dev/null \
                   | grep -oE '/tag/[0-9]+' | grep -oE '[0-9]+' | sort -un | head -8); do
      n=$(count_works "$TARGET/tag/$cand?n=100")
      if [ "${n:-0}" -gt "$TAG_WORKS" ]; then
        TAG=$cand
        TAG_WORKS=$n
      fi
    done
    if [ -n "$TAG" ] && discriminates "$TAG"; then
      echo "  tag/$TAG discriminates on completion and rating"
      break
    fi
  done
fi
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
# One generously-timed fetch, reused by every check below.
#
# The FIRST tag request after a restart pays for loading the 81 MiB tag-vector
# matrix and building the graph, and the page's default sort is the engine's
# taste blend (measured 3.67 s warm on tag 29, 0.54 s for sort=kudos). Against the
# 20 s cap this fetch used to have, a cold process read as an EMPTY body and the
# gate reported "complete control is MISSING from the tag page" — a working page
# reported broken because the gate timed out. 90 s is a cold-start allowance, not
# a latency assertion; latency is measured in its own section against a warm
# process.
# The STATUS is captured separately from the body, because "the page did not
# answer" and "the page answered without this control" are different findings and
# the gate reported them identically for two sessions: a cold start that answered
# 503 read as "complete control is MISSING from the tag page", which is a claim
# about the product made by a measurement of the network.
CTL_BODY=$(mktemp)
ctl_code=$(curl -sS --max-time 90 -o "$CTL_BODY" -w '%{http_code}' "$TARGET/tag/$TAG" 2>/dev/null || echo 000)
if [ "$ctl_code" != "200" ]; then
  bad "the tag page answered HTTP $ctl_code, so its filter controls cannot be checked at all"
else
  for name in complete rating lang; do
    if grep -qE "name=\"$name\"" "$CTL_BODY"; then
      ok "$name control is present"
    else
      bad "$name control is MISSING from the tag page (the page itself answered 200)"
    fi
  done
fi
rm -f "$CTL_BODY"

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

# --- SECTION RETIRED: "is it up at all" now runs at the top of the gate -----
# It read a 503 as silence ("did not answer") and it ran after the tag
# discovery, so the ten minutes that found a tag were discarded by the cheapest
# check in the file. Reachability and health are reported there now, before any
# work, and the exit contract is unchanged: 2 for a service problem.

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
# -f removed for the same reason as the helpers above, and one more: the exit code
# inside a PIPELINE is the last command's, so `curl -f` failing on a 503 did not
# stop this line at all -- it read the header off a response that the header()
# calls two lines up had just called UNREACHABLE, in the same run. Two verdicts
# about one response, disagreeing, and the wrong one was the loud one.
age=$(curl -sS --max-time 20 -o /dev/null -D- "$TARGET/healthz" 2>/dev/null \
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

# --- 4c. the CSV export agrees with the JSON it shadows
#
# ?format=csv exists for spreadsheets, so what matters is that it is the SAME
# list -- same filters, same order, same rows. A CSV that quietly ignored a
# filter would be worse than no CSV, because the file would look authoritative.
#
# Parsing here uses PYTHON'S csv MODULE, not cut. The first version used
# `cut -d, -f1`, which is correct for a CSV with no embedded newlines and wrong
# for this one: summaries in the mirror contain newlines, so a quoted field
# spans several lines and every field position after it is garbage. The gate
# reported the CSV as selecting a different list of works, including fragments of
# a Harry Potter summary, while the two formats in fact agreed perfectly. A gate
# that cannot parse its own input reports confident nonsense.
c_py=$(mktemp /tmp/csvchk-XXXXXX.py)
cat > "$c_py" <<'PYEOF'
import csv, json, sys, urllib.request
base = sys.argv[1]
qs = "?limit=50&sort=date"
def get(u):
    with urllib.request.urlopen(u, timeout=30) as r:
        return r.read().decode("utf-8", "replace")
raw = get(base + "/api/v1/ao3/works" + qs + "&format=csv")
rows = [r for r in csv.reader(l for l in raw.splitlines()
                              if not l.startswith("#"))]
header, data = rows[0], rows[1:]
js = json.loads(get(base + "/api/v1/ao3/works" + qs))
out = {
    "preamble": [l for l in raw.splitlines() if l.startswith("#")],
    "header_ok": header[:3] == ["id", "title", "url"],
    "ncols": len(header),
    "ragged": [i for i, r in enumerate(data) if len(r) != len(header)],
    "csv_ids": [r[0] for r in data],
    "json_ids": [str(w["id"]) for w in js["works"]],
    # Column positions come from the header, not from a list typed twice.
    "null_cols": sorted({header[i] for r in data for i, v in enumerate(r)
                         if v == "null"}),
    "empty_summary": sum(1 for r in data if r[header.index("summary")] == ""),
}
print(json.dumps(out))
PYEOF
got=$(python3 "$c_py" "$TARGET" 2>/dev/null)
rm -f "$c_py"

[ -n "$got" ] || bad 4c-0 "could not parse the CSV export (empty result)"
if [ -n "$got" ]; then
  printf '%s' "$got" | grep -q '"header_ok": true' \
    || bad 4c-2 "the CSV has no id,title,url header"
  printf '%s' "$got" | grep -q '"ragged": \[\]' \
    || bad 4c-3 "some CSV rows do not match the header's column count"
  printf '%s' "$got" | grep -q '# limit=50 offset=0' \
    || bad 4c-1 "the CSV does not record its limit in the preamble"
  # Same rows, same order -- compared as JSON, not as shell word lists.
  if [ "$(printf '%s' "$got" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["csv_ids"]==d["json_ids"])')" = "True" ]; then
    ok 4c-4 "CSV and JSON select the same works, in the same order"
  else
    bad 4c-4 "CSV and JSON disagree: $(printf '%s' "$got" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("csv",d["csv_ids"][:5],"json",d["json_ids"][:5])')"
  fi
  # The NULL convention, if this corpus has any NULLs to show. The live mirror
  # has ZERO -- every one of bookmarks/summary/language/complete/update_date is
  # populated for all 112,935 works -- so this asserts the header names the
  # columns and records how many empties there were, rather than demanding a
  # "null" that this corpus cannot produce. Claiming the mirror leaves 112,890
  # bookmarks NULL was measured on a DIFFERENT corpus and was wrong here.
  nc=$(printf '%s' "$got" | python3 -c 'import json,sys; print(",".join(json.load(sys.stdin)["null_cols"]))')
  if [ -n "$nc" ]; then
    ok 4c-6 "NULL columns rendered as \"null\": $nc"
  else
    ok 4c-6 "no NULLs in this corpus; empty and NULL stay distinguishable by construction"
  fi
fi

# --- 4b. /surprise picks a real, seedable work ---------------------------
echo
echo "surprise — a real seed, and a different one each call"
# A status-only assertion would pass on the empty state, so this checks the
# seed link AND the tag count, and then that two calls differ. The "random"
# query was deterministic once already -- a random offset with no ordering
# returned the same work every time -- and a 200 cannot see that.
check "surprise offers a seed"      "$TARGET/surprise" 'seed=ao3_work(%3a|:)[0-9]+'
check "surprise explains the pick"  "$TARGET/surprise" '[0-9]+ tags, so there is a neighbourhood'

surprise_picks=""
for _ in 1 2 3; do
  surprise_picks="$surprise_picks $(curl -fsS --max-time 20 "$TARGET/surprise" 2>/dev/null \
    | grep -oE 'seed=ao3_work(%3a|:)[0-9]+' | head -1)"
done
surprise_distinct=$(tr ' ' '\n' <<<"$surprise_picks" | grep -c . | tr -d ' ')
surprise_unique=$(tr ' ' '\n' <<<"$surprise_picks" | grep . | sort -u | grep -c . | tr -d ' ')
if [ "$surprise_distinct" -ge 2 ] && [ "$surprise_unique" -ge 2 ]; then
  ok "surprise varies between calls ($surprise_unique of $surprise_distinct distinct)"
else
  bad "surprise returns the same work every call ($surprise_unique of $surprise_distinct distinct) — the selection is not random"
fi

# The seed the page offers must be a seed /recommend accepts: a link to a
# ranking that 400s is worse than no link.
surprise_seed=$(curl -fsS --max-time 20 "$TARGET/surprise" 2>/dev/null \
  | grep -oE 'seed=ao3_work(%3a|:)[0-9]+' | head -1)
if [ -n "$surprise_seed" ]; then
  check "the surprise seed ranks" "$TARGET/recommend?$surprise_seed" 'data-testid="results"'
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

# --- 7. taste blend, author pages, search by address --------------------
echo
echo "taste blend, author pages, search by AO3 URL/id"
# The default view is the blend, it says which view it is, and silence
# (neither note) is the failure the Go test also pins down.
check "tag default names the taste blend" "$TARGET/tag/$TAG" 'Taste match'
check "blend explains itself (note or error)" "$TARGET/tag/$TAG" 'data-testid="taste-(note|error)"'
check "taste is one of the order choices"  "$TARGET/tag/$TAG" 'value="taste"'
if [ "${TAG_WORKS:-0}" -gt 2 ]; then
  check "truncated list links the raise"   "$TARGET/tag/$TAG?n=2" 'data-testid="raise-n"'
fi

# Author pages: a byline on a real card links to a page that lists that
# author's works. The href is taken FROM the page, so this cannot pass on
# a hand-constructed URL the server would answer differently.
author_href=$(curl -fsS --max-time 20 "$TARGET/tag/$TAG?sort=kudos&n=3" 2>/dev/null \
  | grep -oE '/author\?q=[^"]+' | head -1)
if [ -n "$author_href" ]; then
  ok "byline links to an author page ($author_href)"
  check "author page lists that author's works" "$TARGET$author_href" 'data-testid="author-count"'
else
  bad "no /author?q= link in any byline on tag/$TAG — bylines are plain text, authors are not pages"
fi

# Search by address. The id is one the mirror actually holds, taken from
# the same card row; the missing-id case must be said out loud.
wid=$(curl -fsS --max-time 20 "$TARGET/tag/$TAG?sort=kudos&n=1" 2>/dev/null \
  | grep -oE '/work/[0-9]+' | head -1 | grep -oE '[0-9]+')
if [ -n "$wid" ]; then
  check "search by AO3 work URL is an exact match" \
    "$TARGET/search?q=https%3A%2F%2Farchiveofourown.org%2Fworks%2F$wid" \
    'data-testid="exact-work"'
fi
check "search by a URL for a missing work says so" \
  "$TARGET/search?q=https%3A%2F%2Farchiveofourown.org%2Fworks%2F999999999" \
  'data-testid="exact-missing"'

# --- 8. the standings name their weighting ------------------------------
#
# A ranking that is partly the operator's taste, shown as the crowd's verdict,
# is a claim the reader cannot check. The pages name the weighting when an
# owner cohort is running and stay silent when it is not, and BOTH directions
# are checked because each is a different wrong answer: a note on a deployment
# with no cohort is a false claim about the numbers, and no note on a
# deployment with one is a hidden one.
#
# Which case applies is read from the SERVICE's env file, not from the gate's
# own environment. The gate runs without KINDRED_OWNER_SESSION by design (the
# key is a secret), so inferring "no owner here" from the gate's env would
# report drift on a correctly configured deployment -- the exact mistake this
# gate exists to catch, inverted.
echo
echo "standings name the owner-cohort weighting"
owner_env_file="${KINDRED_DEPLOY_ENV:-/etc/default/kindred-restrictions}"
owner_session=""
owner_known="no"
if [ -r "$owner_env_file" ]; then
  owner_known="yes"
  owner_session=$(sed -n 's/^KINDRED_OWNER_SESSION=//p' "$owner_env_file" | tail -1)
fi
if [ -z "$owner_session" ] && [ -n "${KINDRED_OWNER_SESSION:-}" ]; then
  owner_session="${KINDRED_OWNER_SESSION}"
  owner_known="yes"
fi

board=$(curl -fsS --max-time 20 "$TARGET/leaderboard" 2>/dev/null || true)
arena_page=$(curl -fsS --max-time 20 "$TARGET/arena" 2>/dev/null || true)
if [ -z "$board" ] || [ -z "$arena_page" ]; then
  unreachable=$((unreachable+1))
  echo "  UNREACHABLE  /leaderboard or /arena"
else
  # Count the subject first: a grep for an ABSENT note matches nothing on a
  # body that failed to render either, and would read as "correctly silent".
  if grep -qE 'class="ratings"|class="empty"|data-testid="cohort-note"' <<<"$board"; then
    ok "leaderboard rendered its own content"
  else
    bad "leaderboard rendered neither a standings table nor an empty state"
  fi
  case "$owner_known:$owner_session" in
  yes:)
    # Readable env file that names no owner: silence is the correct answer.
    if grep -q 'data-testid="cohort-note"' <<<"$board"; then
      bad "/leaderboard names a cohort weighting but $owner_env_file sets no KINDRED_OWNER_SESSION"
    else
      ok "leaderboard is silent about weighting with no owner session"
    fi
    ;;
  *:?*)
    if grep -q 'data-testid="cohort-note"' <<<"$board"; then
      ok "leaderboard names the cohort weighting"
    else
      bad "an owner session is configured but /leaderboard does not name the cohort weighting"
    fi
    if grep -q 'data-testid="cohort-note"' <<<"$arena_page"; then
      ok "arena carries the same caveat"
    else
      bad "/arena does not carry the cohort note /leaderboard carries"
    fi
    ;;
  *)
    note "skipped (no owner session in gate env)"
    ;;
  esac
fi

# --- 9. the surfaces added 2026-10-23 ------------------------------------
#
# Feeds (#61), the search descriptor (#62), the pair report (#78), the reading
# path (#40) and the new-since-visit list (#65). Each shipped with its own tests,
# and this is the only place they are checked on the DEPLOYED binary: a feature
# verified only by `go test` is verified against a working tree, not against what
# readers are served.
#
# status NAME URL EXPECTED is the one helper this section needs and the older
# sections did not: several of these checks are about a REFUSAL (404 for an
# unknown tag, 400 for a work paired with itself), and a refusal cannot be
# asserted by grepping a body for a string that should be there.
status () {
  local name="$1" url="$2" want="$3"
  local code
  code=$(curl -sS --max-time 25 -o /dev/null -w '%{http_code}' "$url" 2>/dev/null) || true
  if [ -z "$code" ] || [ "$code" = "000" ]; then
    unreachable=$((unreachable+1))
    echo "  UNREACHABLE  $name ($url)"
    return 0
  fi
  if [ "$code" = "$want" ]; then ok "$name"; else bad "$name — got $code, want $want at $url"; fi
}

# check_absent NAME URL PATTERN is the other shape: a promise the server must NOT
# make. The descriptor advertised a `format=rss` URL for a while, which /search
# does not answer.
check_absent () {
  local name="$1" url="$2" pattern="$3"
  local body code
  body=$(curl -sS --max-time 20 -w $'\n__CODE__%{http_code}' "$url" 2>/dev/null) || true
  code=${body##*__CODE__}
  body=${body%$'\n__CODE__'*}
  if [ -z "$code" ] || [ "$code" = "000" ]; then
    unreachable=$((unreachable+1))
    echo "  UNREACHABLE  $name ($url)"
    return 0
  fi
  if grep -qE -- "$pattern" <<<"$body"; then
    bad "$name — found /$pattern/ at $url, which this server cannot answer"
  else
    ok "$name"
  fi
}

echo
echo "feeds and the search descriptor"

header "tag feed says it is a feed" "$TARGET/feed/tag/$TAG" 'content-type: *application/rss\+xml'
check  "tag feed is RSS 2.0"       "$TARGET/feed/tag/$TAG" '<rss version="2.0"'
check  "tag feed carries items"    "$TARGET/feed/tag/$TAG" '<item>'
# An absolute guid is what makes a subscriber's client keep entry identity
# across polls. A relative one is invalid RSS and clients mint their own.
check  "tag feed guids are absolute" "$TARGET/feed/tag/$TAG" '<guid isPermaLink="true">https?://'
status "an unknown tag is 404, not an empty feed" "$TARGET/feed/tag/999999" 404

check  "opensearch names the site"  "$TARGET/opensearch.xml" '<ShortName>kindred</ShortName>'
check  "opensearch points at /search" "$TARGET/opensearch.xml" '/search\?q=\{searchTerms\}'
check_absent "opensearch advertises no url it cannot answer" "$TARGET/opensearch.xml" 'format=rss'

# Two real works, for the pair report and the reading path. From the corpus when
# it is readable, else from the tag page the gate already rendered; a fixture
# instance has neither, and the section then says it was skipped rather than
# failing over a fact about the environment.
WORK_A="" WORK_B=""
if [ -r "$CORPUS_DB" ] && command -v sqlite3 >/dev/null 2>&1; then
  ids=$(sqlite3 -readonly "$CORPUS_DB" \
    "SELECT w.id FROM work_tags wt JOIN works w ON w.id = wt.work_id
     WHERE wt.tag_id = $TAG ORDER BY w.kudos DESC, w.id LIMIT 2;" 2>/dev/null || true)
  WORK_A=$(sed -n '1p' <<<"$ids")
  WORK_B=$(sed -n '2p' <<<"$ids")
fi
if [ -z "${WORK_A:-}" ] || [ -z "${WORK_B:-}" ]; then
  ids=$(curl -fsS --max-time 20 "$TARGET/tag/$TAG?n=5" 2>/dev/null \
        | grep -oE '/work/[0-9]+' | grep -oE '[0-9]+' | head -2 || true)
  WORK_A=$(sed -n '1p' <<<"$ids")
  WORK_B=$(sed -n '2p' <<<"$ids")
fi

if [ -n "${WORK_A:-}" ] && [ -n "${WORK_B:-}" ]; then
  echo "  pair and path against /work/$WORK_A and /work/$WORK_B"
  check  "pair report scores"        "$TARGET/api/v1/works/$WORK_A/similarity/$WORK_B" '"score"'
  check  "pair report says why"      "$TARGET/api/v1/works/$WORK_A/similarity/$WORK_B" '"shared_tags"'
  status "a work is not a pair with itself" "$TARGET/api/v1/works/$WORK_A/similarity/$WORK_A" 400
  check  "reading path walks"        "$TARGET/path/$WORK_A" 'step 1'
  # The distinction the feature exists for: each step is measured from the work
  # BEFORE it, so the page must say so.
  check  "path measures from the previous step" "$TARGET/path/$WORK_A" 'from the step before'
else
  note "skipped the pair and path checks (no two work ids could be found)"
fi

echo
echo "per-kind algorithms, and the A/B they run in"
check  "the registry lists every kind"     "$TARGET/api/v1/algo/kinds" '"kind": "ao3_work"'
check  "and names a default per kind"      "$TARGET/api/v1/algo/kinds" '"default": "embed"'
# The disabled set is a claim the page makes; a registry that stopped publishing
# it would still look like a working picker.
check  "and publishes what cannot run"     "$TARGET/api/v1/algo/kinds" '"status": "disabled"'
check  "and says why it cannot run"        "$TARGET/api/v1/algo/kinds" 'training pipeline'
# Pinned with ?algo=, NOT left to the arm. An anonymous request is a NEW reader
# and the arm is a hash of the owner key, so asserting the default on an unpinned
# request is a coin flip -- measured at 5 default / 1 challenger across six
# consecutive curls. This check used to be unpinned and passed three deploys in a
# row on luck alone.
check  "the tag kind ranks with the default" "$TARGET/api/v1/related?kind=ao3_tag&id=$TAG&n=5&algo=embed" '"algo": "embed"' 
check  "and every related answer names its arm" "$TARGET/api/v1/related?kind=ao3_tag&id=$TAG&n=5" '"arm": "'
check  "the choice page renders"           "$TARGET/choose" 'Choose your algorithm'
check  "and lists a disabled algorithm with its reason" "$TARGET/choose" 'Why not:'
check  "the related page renders a list"   "$TARGET/related?kind=ao3_tag&id=$TAG" 'data-testid="related-items"'
# A disabled algorithm asked for by name must be REFUSED BY NAME, not silently
# honoured: the id must not appear as the algorithm that answered.
check  "a disabled algorithm is refused"   "$TARGET/api/v1/related?kind=ao3_tag&id=$TAG&algo=lightgcn" 'not available on this deployment'
check_absent "and is not the algorithm that answered" "$TARGET/api/v1/related?kind=ao3_tag&id=$TAG&algo=lightgcn" '"algo": "lightgcn"'

echo
echo "embedding drift"
# A page that cannot say "no comparison yet" would print 0% and look healthy.
check "the mirror page reports drift"     "$TARGET/mirror" 'Embedding drift'
check "and refuses to call absence zero"  "$TARGET/mirror" 'data-testid="drift-(none|verdict)"|no comparison at all'
check "the drift endpoint answers"        "$TARGET/api/v1/embed/drift" '"runs"'
check_absent "and does not claim a drift it never measured" "$TARGET/api/v1/embed/drift" '"centroid_cosine":0'
echo "  drift endpoint: $(curl -sS --max-time 20 "$TARGET/api/v1/embed/drift" | tr -d '\n' | head -c 160)"

echo
echo "works you keep skipping"
check "the skipping page renders"        "$TARGET/skipped" 'Works you keep skipping'
# The threshold is the page's honesty: it is a claim about the reader, and the
# page has to say how much evidence it needed.
check "and states its threshold"         "$TARGET/skipped" '3</strong> or more'
check "or says why it is empty"          "$TARGET/skipped" 'data-testid="skipped-(empty|items)"'
check "and offers the action that demotes" "$TARGET/skipped" 'Not interested|data-empty'

echo
echo "the learned factor space answers when asked (#89)"
# latent ships at weight 0 in the default tune, so the only way to check it is to ASK for
# the tune that weights it. A signal that is registered but never reachable is the
# registered-and-inert bug this gate exists for; a signal reachable but never scored is
# the same bug one layer down.
check "the opt-in tune is honoured"              "$TARGET/api/v1/recommend?seed=ao3_work:17249318&n=3&tune=als08" '"tune":"als08"'
check "and the latent signal scores a live list"  "$TARGET/api/v1/recommend?seed=ao3_work:17249318&n=3&tune=als08" '"signal":"latent"'
check "and it is not merely degraded"             "$TARGET/api/v1/recommend?seed=ao3_work:17249318&n=3&tune=als08" '"evidence"'

echo
echo "a tag's trend is measured against the corpus's own growth (#31)"
# The window must be anchored on the mirror's newest work. A live check that merely
# found rows would pass on a table of zero lifts; the note naming the anchor and the
# corpus growth is what makes the number checkable.
check "tag-trend answers for a real tag"        "$TARGET/api/v1/corpus-query/tag-trend?tag=angst&limit=3" '"mode":"tag-trend"'
check "and names the window it measured"        "$TARGET/api/v1/corpus-query/tag-trend?tag=angst&limit=3" 'the mirror'"'"'s newest work, not today'
check "and the corpus growth it cancels"        "$TARGET/api/v1/corpus-query/tag-trend?tag=angst&limit=3" 'growth'

echo
echo "a tag's gaps are what its genre carries and its own works do not (#34)"
# The checks name a REAL tag on this mirror (hurt/comfort, 12,018 works). A fixture-shaped
# check would pass on an empty corpus; the point of the gate is that the query runs against
# the archive the deployment serves.
check "tag-gap answers for a real tag"           "$TARGET/api/v1/corpus-query/tag-gap?tag=hurt%2Fcomfort&limit=3" '"mode":"tag-gap"'
check "and returns at least one gap"             "$TARGET/api/v1/corpus-query/tag-gap?tag=hurt%2Fcomfort&limit=3" '"rows":\[{"key"'
check "and names the neighbourhood it used"      "$TARGET/api/v1/corpus-query/tag-gap?tag=hurt%2Fcomfort&limit=3" 'neighbourhood of'
check "and states the formula it ranked by"      "$TARGET/api/v1/corpus-query/tag-gap?tag=hurt%2Fcomfort&limit=3" 'enrichment'
check "an unknown tag is named, not empty"       "$TARGET/api/v1/corpus-query/tag-gap?tag=zzz-no-such-tag&limit=3" 'no tag named'

echo
echo "the ordering survives a second model (#53)"
# BT is fitted from the judged history on every load, and the page must report the
# agreement WITH its counts: a correlation over a thin fit is a number about the prior,
# and the page has to be able to say so (data-bt-thin).
check "the leaderboard carries the BT cross-check"   "$TARGET/leaderboard" 'data-testid="bt-crosscheck"'
check "with a correlation and the items it covers"   "$TARGET/leaderboard" 'data-bt-rho='
check "and the comparison count it was fitted on"    "$TARGET/leaderboard" 'data-bt-comparisons='

echo
echo "a thin tag cannot brag (#39)"
# The tag page printed a MEAN over as few as three works, which reads as a quality
# claim. The shrunk number and its prior must both be on the page: a shrunk value with
# a hidden prior cannot be checked or argued with.
check "the tag shelf carries a shrunk kudos estimate"  "$TARGET/tag/1" 'data-testid="shrunk-kudos"'
check "and names the prior it was shrunk toward"      "$TARGET/tag/1" 'data-prior='
check "and the fictional works that prior is worth"   "$TARGET/tag/1" 'data-weight='

echo
echo "the walk reaches what one hop cannot (#74)"
# A registered signal at weight zero is this project's most repeated bug: it runs
# never, appears in no evidence, and names itself nowhere. The check is therefore
# not that the name exists in the tune, but that a live list carries a walk
# COMPONENT with a weight and a value.
check      "the API says which signals were skipped"    "$TARGET/api/v1/recommend?seed=ao3_work:17249318&n=5" '"degraded":\['
check      "a live list carries a walk component"      "$TARGET/api/v1/recommend?seed=ao3_work:17249318&n=5" '\"signal\":\"walk\"'
# The absence assertion names its own subject: the degraded array must EXIST (the
# check above), and walk must not be inside it. A bare grep for "walk", would pass
# on the very failure it is meant to catch, because a degraded walk is still the
# word "walk" in the same body.
check_not  "and the walk is not in that skipped list"  "$TARGET/api/v1/recommend?seed=ao3_work:17249318&n=5" '"degraded":\[[^]]*"walk"'

echo
echo "an interleaved difference is not a result (#84)"
# The standings print a rate. Whether that rate is EVIDENCE is a second question,
# and a page that prints the first without the second invites a reader to treat a
# 0.0004 difference as an ordering. The verdict column must be there, and on a young
# deployment it must say "no lead" rather than naming one.
check "the head-to-head table carries a verdict column" "$TARGET/eval" 'data-testid="interleaved-verdict"'
check "the dashboard says whether any pair has cleared the test" "$TARGET/eval" 'data-testid="eval-no-lead"'
check "and the endpoint carries the p-value"                          "$TARGET/api/v1/interleave/standings" '"p"'
# The vote table had the same hole: 17 wins against 11 looks like a lead until the
# test is applied. The COLUMN cannot be checked live -- this deployment has no votes
# yet, so the table renders its empty state and a data-testid inside it would never
# appear (a check for it would only pass on fabricated evidence). What IS checkable
# on an empty table is that the page states the standard the column applies.
check "the vote table states what counts as evidence"                 "$TARGET/eval" 'data-testid="eval-vote-standard"'
check "and the votes endpoint still answers"                          "$TARGET/api/v1/algo/standings" '"algorithms"'

echo
echo "length, measured rather than assumed (#41)"
# A length signal is judged by no other number: recall cannot see the shape of a
# list, so "the signal works" and "the signal does nothing" score the same on every
# other metric. The column must exist, and an unmeasured run must say so.
check "the eval table records how close lists came in LENGTH" "$TARGET/eval" 'data-testid="eval-length"'

echo
echo "what the order is worth (#42)"
# The block must be present even with no calibration recorded: a page that hides
# the block until data exists teaches a reader nothing about why it is missing.
check "the recommendation page says what the order is worth" "$TARGET/recommend" 'data-testid="calibration-sentence"'
# And the endpoint must answer the question "calibrated or not" explicitly, rather
# than returning an empty object a consumer would read as "zero".
check "the calibration endpoint reports whether it is calibrated" "$TARGET/api/v1/calibration" '"calibrated"'

echo
echo "diversity, recorded with each offline run (#79)"
# The columns must exist AND the page must say "not measured" for a run that did
# not measure them: rendering 0.000 there would claim a perfectly diverse list.
check "the eval table records intra-list overlap"  "$TARGET/eval" 'data-testid="eval-overlap"'
check "and the fandom count"                       "$TARGET/eval" 'data-testid="eval-fandoms"'
check "and explains what the two columns cost"     "$TARGET/eval" 'the price diversification charges against recall'

echo
echo "the pool cache and the MMR cost (measured 2026-10-23)"
# The cache is a BEHAVIOURAL claim, not a timing one: the same request twice must
# report a hit. Asserting wall-clock here would fail on a busy host for reasons
# that have nothing to do with the product.
h0=$(curl -fsS --max-time 20 "$TARGET/healthz" | tr ',' '\n' | grep '"hits"' | tr -dc '0-9')
curl -fsS -o /dev/null --max-time 60 "$TARGET/api/v1/recommend?seed=ao3_work:17249318&n=5" || true
curl -fsS -o /dev/null --max-time 60 "$TARGET/api/v1/recommend?seed=ao3_work:17249318&n=5" || true
h1=$(curl -fsS --max-time 20 "$TARGET/healthz" | tr ',' '\n' | grep '"hits"' | tr -dc '0-9')
if [ -n "$h0" ] && [ -n "$h1" ] && [ "$h1" -gt "$h0" ]; then
  ok "the second identical request was answered from the pool cache (hits $h0 -> $h1)"
else
  bad "the pool cache never reported a hit (hits $h0 -> $h1)"
fi
# 200 results took 10.4 s before the MMR fix (quadratic in n) and 0.36 s after. A
# 5 s ceiling is wide enough for a loaded host and still catches the quadratic
# shape, which is what a regression would look like.
t=$(curl -fsS -o /dev/null -w '%{time_total}' --max-time 60 \
  "$TARGET/api/v1/recommend?seed=ao3_work:17249318&n=200" || echo 999)
if awk "BEGIN{exit !($t < 5)}"; then
  ok "200 results still answer quickly (${t}s)"
else
  bad "200 results took ${t}s; the MMR pass is quadratic again"
fi

echo
echo "the tag shelf in numbers (#54)"
# Medians, not means: a mean on a skewed tag describes a work nobody wrote. The
# page must print both, so a reader can see the skew rather than a single number.
check "the tag page summarises its shelf"  "$TARGET/tag/$TAG" 'data-testid="tag-shelf"'
check "and prints a median length"         "$TARGET/tag/$TAG" 'data-testid="median-words"'
check "and a median kudos"                 "$TARGET/tag/$TAG" 'data-testid="median-kudos"'
check "and the trend is dated as updates"  "$TARGET/tag/$TAG" 'Counted by <strong>update</strong> year'
status "and a tag with no works is still a 404" "$TARGET/tag/999999" 404

echo
echo "the byline, and the shelf (#66/#50)"
if [ -n "${WORK_A:-}" ]; then
  check "the work page names its author"   "$TARGET/work/$WORK_A" 'data-testid="work-byline"'
  # A byline that renders but does not link is the defect the row names: the
  # author-similarity endpoint needs a way IN from a work.
  # Pattern has no regex metacharacter on purpose: `?` in an ERE makes the
  # preceding character optional, so a check written as /author?q= can never
  # match — it reported a working link as missing on the first deploy.
  check "and links it to the shelf"        "$TARGET/work/$WORK_A" 'href="/author'
  check "and names authors who write alike" "$TARGET/work/$WORK_A" 'data-testid="work-authors"|work-authors-error'
  AUTHOR=""
  if [ -r "$CORPUS_DB" ] && command -v sqlite3 >/dev/null 2>&1; then
    AUTHOR=$(sqlite3 -readonly "$CORPUS_DB" "SELECT authors FROM works WHERE id = $WORK_A;" 2>/dev/null \
             | cut -d, -f1 | sed 's/^ *//; s/ *$//' | sed 's/ /%20/g')
  fi
  if [ -n "$AUTHOR" ]; then
    echo "  author of /work/$WORK_A: $AUTHOR"
    check  "the shelf page lists the works" "$TARGET/author?q=$AUTHOR" 'data-testid="author-count"'
    # An anonymous reader must be promised nothing about their reading: the
    # progress line is a claim about a person and needs a session to be about
    # anyone.
    check_absent "and claims no reading progress for a stranger" \
      "$TARGET/author?q=$AUTHOR" 'data-testid="collection-progress"'
  else
    note "could not resolve a byline for /work/$WORK_A; shelf checks skipped"
  fi
fi

echo
echo "new since your last visit"
check "the list answers one way or the other" "$TARGET/new" 'first visit|Since your previous visit'
check "and refuses to invent a boundary"      "$TARGET/new" 'no .before. to compare against|began on'

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
