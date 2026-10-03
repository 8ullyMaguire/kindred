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