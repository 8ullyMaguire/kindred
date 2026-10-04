#!/usr/bin/env python3
"""
goal-check.py -- the completion predicate for kindred.

    cd ~/code-local/go/kindred && python3 docs/goal-check.py

Exit 0  -> the goal is COMPLETE.
Exit 1  -> work remains; stdout names the clause.
Exit 2  -> CANNOT CLAIM COMPLETE; a clause could not be evaluated.

## Why this file exists

Second project in the estate to get one (after polaris and threadlight), and the
pattern is the same each time. kindred has a **green** suite — 10 packages, measured
2026-10-05 — and no way to say so that a later change cannot quietly invalidate. "I
ran it once and it was fine" is not a gate; `go test ./...` in CI is.

## This project's actual gap, which the clause names

`internal/engine` and `internal/web` have **no test files**. kindred is a
recommendation engine, so `internal/engine` is the package the project exists for,
and it is the one with nothing. The suite is green partly *because* the part that
matters most is untested.

So this predicate carries a clause the other two do not: **the engine must have
tests.** It is a FAIL, not a WARNING, because "the recommendation logic is correct"
is currently unfalsifiable — there is no assertion anywhere that could fail.

## Design rules, each tied to a failure this estate has actually produced

  * **A check that can silently match nothing is worse than no check.** polaris's
    `scripts/audit_counters.py` printed "0 counter columns have a write path; 0 do
    not" with exit 0 while its Postgres container was dead — textually identical to a
    clean bill of health. Any clause here that greps asserts it FOUND something, and
    the untested-package clause works the same way in reverse: it enumerates the
    packages that have no test files and fails if that set is non-empty.

  * **A clause that cannot be evaluated reports UNKNOWN, never PASS.** A timeout
    produces no output and reads exactly like a clean run.

  * **Never trust a count written in a document. Recompute it.** `--update-baseline`
    rewrites the floor from the tree.

  * **Measure the floor with the same function that compares against it.** stash-box
    shipped a floor measured with an `ok`-only count and compared it against a count
    that also included `?`; a suite that lost 27 packages still reported PASS. One
    parser, used everywhere.
"""

import os
import re
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
MODULE = "git.polarisocial.xyz/kindred/kindred"

# Floor, not a target. Measured 2026-10-05 by `--update-baseline`: 14 packages
# report `ok`. I first wrote 10 here by copying polaris's number, which is precisely
# the copy-a-floor-without-measuring error this file's own rules warn about — caught
# because the clause reported "14 packages green (floor 10)" and a floor that is
# below reality guards nothing.
# `?  [no test files]` does NOT count — that is a package with nothing in it, which is
# the opposite of evidence.
BASELINE_PACKAGES = 14

# Packages that must have at least one test file. Measured as empty on 2026-10-05,
# so this clause is red on arrival, which is the honest state.
#
# `internal/engine` is here because it is the core: kindred is a recommendation
# engine and this is the recommendation logic. `internal/web` is here because it is
# the surface a reader actually touches.
REQUIRED_TESTED = ["internal/engine", "internal/web"]

results = []


def sh(cmd, timeout=1800, env=None):
    e = dict(os.environ)
    if env:
        e.update(env)
    try:
        p = subprocess.run(cmd, shell=True, cwd=REPO, capture_output=True,
                           text=True, timeout=timeout, env=e)
        return p.returncode, p.stdout.strip(), p.stderr.strip()
    except subprocess.TimeoutExpired:
        return 124, "", f"timeout after {timeout}s"


def add(clause, status, detail):
    results.append((clause, status, detail))
    mark = {"PASS": "ok  ", "FAIL": "FAIL", "UNKNOWN": "????"}[status]
    print(f"[{mark}] {clause}\n         {detail}")


def count_packages(out):
    """Packages reporting `ok`. `?` (no test files) is excluded BY CONSTRUCTION.

    Both halves of the floor must agree with the writer in --update-baseline. Two
    copies of one measurement is two chances to disagree, and they disagreed once in
    stash-box.
    """
    return len(re.findall(r"^ok\s+\S+", out, re.M))


# ---------------------------------------------------------------- clauses

def clause_build():
    rc, out, err = sh("go build ./...", timeout=900)
    add("builds", "PASS" if rc == 0 else "FAIL",
        "go build ./... clean" if rc == 0 else (err or out).splitlines()[-1][:160])


def clause_vet():
    rc, out, err = sh("go vet ./...", timeout=900)
    add("go vet clean", "PASS" if rc == 0 else "FAIL",
        "no findings" if rc == 0 else (err or out).splitlines()[-1][:160])


def clause_gofmt():
    _, out, _ = sh("gofmt -l . 2>/dev/null | head -20")
    files = [f for f in out.splitlines() if f.strip()]
    add("gofmt clean", "PASS" if not files else "FAIL",
        "all files formatted" if not files
        else f"{len(files)} unformatted: " + ", ".join(files[:5]))


def clause_suite():
    rc, out, err = sh("go test ./... -count=1", timeout=1800)
    if rc == 124:
        add("test suite green", "UNKNOWN", "go test timed out after 1800s")
        return
    if rc != 0:
        failed = re.findall(r"^--- FAIL: (\S+)", out, re.M)
        add("test suite green", "FAIL",
            f"{len(failed)} failing: " + ", ".join(failed[:5]) if failed
            else f"go test exited {rc}: {(err or out).splitlines()[-1][:160]}")
        return
    n = count_packages(out)
    if n < BASELINE_PACKAGES:
        add("test suite green", "FAIL",
            f"only {n} packages green, floor is {BASELINE_PACKAGES}. A suite that lost "
            f"packages looks identical to one that never ran them.")
        return
    add("test suite green", "PASS", f"{n} packages green (floor {BASELINE_PACKAGES})")


def clause_untested_packages():
    """Every package must contain at least one test function.

    This is the clause that makes "kindred is fine" falsifiable. Enumerated rather
    than counted, so the failure names WHICH packages are empty -- a count would say
    "2 packages untested" and leave the reader to go find them.

    A package counts as tested if any `_test.go` in it declares a `func Test...`, or a
    `TestMain`. A `_test.go` containing only helpers is not a test, and asserting
    otherwise is the silent-no-match failure in its purest form.
    """
    pkgs = sorted(p for p in (REPO / "internal").iterdir() if p.is_dir())
    if not pkgs:
        add("no untested packages", "UNKNOWN",
            f"no packages under internal/; if the layout moved, this clause is stale "
            f"and would pass on an empty directory")
        return
    untested = []
    for p in pkgs:
        blobs = "".join(f.read_text(errors="ignore") for f in p.glob("*_test.go"))
        if not re.search(r"^func (Test|TestMain|Example|Benchmark|Fuzz)", blobs, re.M):
            untested.append(p.name)
    required_missing = [r for r in REQUIRED_TESTED
                        if (REPO / r).is_dir() and r.split("/")[-1] in untested]
    if untested:
        add("no untested packages", "FAIL",
            f"{len(untested)} of {len(pkgs)} packages have no test function: "
            + ", ".join(untested)
            + (f"\n         REQUIRED (core, not optional): {', '.join(required_missing)}"
               if required_missing else ""))
        return
    add("no untested packages", "PASS", f"all {len(pkgs)} packages have tests")


def clause_page_coverage():
    """Every page must be reachable, registered, and rendered by a test.

    This clause exists because of a specific failure. Six pages were added for
    parity (/recommend with diversity controls, /fandoms, /underrated,
    /neighbours, /profiles, /profile), the build went green, `go vet` was clean,
    all 13 Playwright tests passed -- and `/neighbours?tag=dark` answered:

        500  wrong type for value; expected int64; got int

    Five of the six new pages had ZERO test coverage. html/template resolves
    field types at RENDER time, so nothing short of rendering the page can see
    the bug. It had been in the tree since the page was written.

    So the checks are per-page and mechanical, not "the suite is green":

      * each route in the routing table is reached by the browser suite;
      * each page template in the parse list is rendered by a Go test;
      * every nav link points at a route that exists.

    A page added without its test turns this red on the commit that adds it.
    """
    problems = []

    routes = set()
    handlers = (REPO / "internal/web/handlers.go")
    if handlers.is_file():
        for m in re.finditer(r'case p == "([^"]+)":', handlers.read_text()):
            routes.add(m.group(1))
        for m in re.finditer(r'HasPrefix\(p, "([^"]+)"\)', handlers.read_text()):
            routes.add(m.group(1))
    else:
        problems.append("internal/web/handlers.go does not exist")

    # (route, the file that must mention it). The browser suite drives real
    # HTTP, so a route the suite never visits is a route nothing checks.
    suite = ""
    for cand in sorted((REPO / "e2e/tests").glob("*.spec.js")):
        suite += cand.read_text(errors="ignore")
    if not suite:
        problems.append("e2e/tests has no *.spec.js; the browser suite is required")

    # /recommend and /neighbours take query parameters, so a bare visit is not
    # the interesting case. Both are named explicitly below.
    # Strip COMMENTS only, never string literals.
    #
    # The first version grepped the raw file, and the suite's own header comment
    # -- which lists every page it was written for -- satisfied the check for
    # all of them. Deleting the tests then left the clause green, which is the
    # precise trap the ao3-recommender parity clause was written to avoid.
    #
    # The second version also stripped string literals, which was worse: the
    # route IS a string literal, so stripping them deleted the very thing
    # being searched for and flagged all 14 routes as unvisited.
    #
    # So: comments out, literals in. What remains that can still fool the check
    # is a route named inside a non-comment string that is not a request --
    # e.g. `expect(body).toContain('/fandoms')`. Accepting that is deliberate:
    # requiring every mention to be a request makes the check unmaintainable,
    # and a human adding a page reads this failure and adds the test.
    suite_code = re.sub(r"/\*.*?\*/", " ", suite, flags=re.S)
    suite_code = re.sub(r"//[^\n]*", " ", suite_code)

    for route in sorted(routes):
        if route in ("/static/", ""):
            continue
        needle = route.rstrip("/")
        if not needle:
            continue
        # The route must be USED: inside goto()/request.*(), or as the `path:`
        # of the PAGES table the render sweep iterates. Both are actual
        # requests -- the sweep calls page.goto(p.path).
        used = re.search(
            r"(?:goto|request\s*\.\s*(?:get|post|fetch))\s*\(\s*[\"']"
            + re.escape(needle) + r"(?:\b|\?|'|\")"
            r"|\bpath\s*:\s*[\"']" + re.escape(needle) + r"[\"']",
            suite_code)
        if not used:
            problems.append(
                f"route {needle} is never requested by the browser suite "
                f"(e2e/tests/*.spec.js); add a test that GETs it")

    # Each page template must appear in the Go render sweep, which is what
    # catches a wrong-typed field.
    render_tests = (REPO / "internal/web/render_pages_test.go")
    tpl_list = ""
    render_go = (REPO / "internal/web/render.go")
    if render_go.is_file():
        m = re.search(r'pages = map\[string\]\*template\.Template\{\}\s*\n\s*\n?func init\(\) \{\s*\n\s*for _, page := range \[\]string\{(.*?)\}',
                      render_go.read_text(), re.S)
        if m:
            tpl_list = m.group(1)
        else:
            problems.append("could not find the page list in render.go")
    if not render_tests.is_file():
        problems.append(
            "internal/web/render_pages_test.go does not exist; the render sweep "
            "is what catches a template type error, which go build and go vet "
            "both miss")
    else:
        rt = render_tests.read_text(errors="ignore")
        if tpl_list:
            for tpl in re.findall(r'"([a-z]+\.html)"', tpl_list):
                if tpl == "layout.html":
                    continue
                if tpl not in rt:
                    problems.append(
                        f"template {tpl} is registered but not rendered by "
                        f"render_pages_test.go")

    # Every nav link must resolve to a real route, or the nav is a set of 404s.
    layout = (REPO / "internal/web/assets/layout.html")
    if layout.is_file():
        nav = layout.read_text(errors="ignore")
        nav_block = nav.split("<nav", 1)[-1].split("</nav>", 1)[0] if "<nav" in nav else ""
        for href in re.findall(r'href="(/[^"?]*)', nav_block):
            if href in ("/static/",):
                continue
            if href not in routes and not any(
                    r.startswith(href.rstrip("/")) or href.startswith(r.rstrip("/"))
                    for r in routes if r):
                problems.append(f"nav links to {href}, which is not a route")

    if problems:
        add("page coverage", "FAIL",
            f"{len(problems)} gap(s):\n         - " + "\n         - ".join(problems))
        return
    add("page coverage", "PASS",
        f"{len(routes)} routes, every page reached by the browser suite and "
        f"rendered by the Go sweep; nav links resolve")


def clause_parity():
    """The ao3-recommender parity surface must exist and be reachable.

    This clause exists because every parity feature in kindred was, at some
    point, a flag that was accepted and did nothing:

      * `--max-per-fandom` keyed on `tag_type='fandoms'`, a column that is
        'freeforms' for 3,890,504 of 3,891,300 rows in the real mirror, so the
        cap's group key set was empty and it excluded nothing while reporting
        success;
      * `crawl` reported "fetched 2" while writing zero rows, because Run parsed
        and counted and no Sink was ever constructed;
      * offline mode still issued an HTTP request for robots.txt;
      * `--profile-from-user` was captured by the profile branch, so the corpus
        query silently only wrote to the DB.

    A feature that exists, is wired, and does nothing is worse than a missing
    one: the missing one produces an error you can see. So each item below is
    checked for the WIRING, not for the existence of a symbol -- a stub named
    `runTune` would satisfy a grep for `runTune` and pass this clause.
    """
    # (label, file, pattern that proves the wiring, prose for the failure)
    checks = [
        ("max-per-fandom cap is wired",
         "internal/engine/engine.go", r'group_by:|case "fandom"',
         "engine must select the fandom group key; without it the cap is a no-op"),

        ("fandom detection by name shape",
         "internal/fandom/fandom.go", r"func LooksLikeFandom",
         "fandoms cannot be found by tag_type (it is nearly empty); name shape "
         "is the only thing that works on the real corpus"),

        ("offline mode makes no request",
         "internal/crawl/sink.go", r"func NewOffline",
         "offline must be structural (a client that cannot fetch), not a flag "
         "that skips one call site"),

        ("crawl writes to a sink",
         "cmd/kindred/crawl.go", r"Sink:\s+sink|crawl\.NewSQLSink",
         "a crawl that parses and counts but never writes reports success "
         "while growing the mirror by nothing"),

        ("resume state is atomic and locked",
         "internal/crawl/crawl.go", r"os\.Rename|mu sync\.Mutex",
         "resume must survive the crash that made you want it"),

        ("corpus-query modes exist",
         "internal/corpusquery/corpusquery.go",
         r"ModeFandomRanking|ModeUnderrated|ModeTagNeighbours",
         "the corpus-query family is a parity requirement"),

        ("fandom ranking gates on evidence",
         "internal/corpusquery/corpusquery.go", r"DefaultMinCoWorksToRank",
         "lift without an evidence gate is dominated by one-work fandoms, whose "
         "shrinkage pulls them ABOVE genuinely liked ones"),

        ("profiles persist and rate",
         "cmd/kindred/profile.go", r"profile\.Apply|openProfileStore",
         "profile/rate must write to the writable state DB, never the read-only "
         "mirror"),

        ("tune is implemented",
         "cmd/kindred/tune.go", r"func runTune",
         "tune was a stub returning 'not implemented yet'"),
    ]

    missing = []
    for label, rel, pattern, why in checks:
        path = REPO / rel
        if not path.is_file():
            missing.append(f"{label}: {rel} does not exist ({why})")
            continue
        if not re.search(pattern, path.read_text(errors="ignore")):
            missing.append(f"{label}: {rel} has no match for /{pattern}/ ({why})")

    # The E2E suite is a parity requirement of its own: it found two 500s that
    # every Go test passed, because no Go test called Recommend against a
    # running server.
    e2e = REPO / "e2e" / "tests"
    if not e2e.is_dir() or not list(e2e.glob("*.spec.js")):
        missing.append(
            "browser suite exists: e2e/tests/*.spec.js is required; it is what "
            "caught the closed-store 500 and the missing-work 500")

    if missing:
        add("ao3-recommender parity", "FAIL",
            f"{len(missing)} parity gap(s):\n         - "
            + "\n         - ".join(missing))
        return
    add("ao3-recommender parity", "PASS",
        f"{len(checks)} parity features wired, browser suite present")


# ---------------------------------------------------------------- driver

def update_baseline():
    rc, out, _ = sh("go test ./... -count=1", timeout=1800)
    if rc != 0:
        print("refusing to write a baseline from a red run", file=sys.stderr)
        sys.exit(1)
    print(f"measured {count_packages(out)} green packages; set BASELINE_PACKAGES = "
          f"{count_packages(out)}")


def main():
    if "--update-baseline" in sys.argv:
        update_baseline()
        return

    clause_build()
    clause_vet()
    clause_gofmt()
    clause_suite()
    clause_untested_packages()
    clause_page_coverage()
    clause_parity()

    print()
    failed = [r for r in results if r[1] == "FAIL"]
    unknown = [r for r in results if r[1] == "UNKNOWN"]
    if failed:
        print(f"INCOMPLETE -- {len(failed)} clause(s) failed:")
        for c, _, d in failed:
            print(f"  - {c}: {d}")
        sys.exit(1)
    if unknown:
        print(f"CANNOT CLAIM COMPLETE -- {len(unknown)} clause(s) unevaluated:")
        for c, _, d in unknown:
            print(f"  - {c}: {d}")
        sys.exit(2)
    print(f"COMPLETE -- all {len(results)} clauses pass.")


if __name__ == "__main__":
    main()