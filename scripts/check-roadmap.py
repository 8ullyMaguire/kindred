#!/usr/bin/env python3
"""Check that the roadmap's declared totals agree with its own table.

Why this exists: on 2026-10-23k the roadmap's totals block had said "38 built,
9 partial, 49 open" for two sessions, while the table it summarises holds rows
31-100 -- 70 rows -- and said 21/8/37/4. The wrong figures were arithmetically
tidy (they add to 100), which is exactly why nobody questioned them: a total that
is REMEMBERED instead of derived reads as a measurement and is not one.

The same pass found three spellings of the same status ("BUILT", "**BUILT**",
"OPEN (count built)") in one column, so no mechanical count could agree with the
hand-written one. The vocabulary is now fixed, and this script enforces both.

DECLINED joined the vocabulary on 2026-10-24, when every remaining row was triaged
against the data the mirror actually holds. It is a DIFFERENT statement from OPEN: an
open row is work waiting, and a declined row is work whose premise the data cannot
support. Keeping them in one bucket is how a backlog becomes a wish list that nobody
can size -- and a status column that hides a decision is no better than one that hides
a spelling.

Usage: scripts/check-roadmap.py [path]     (default: docs/PLAN-recsurfaces-31-100.md)
Exit 0 = consistent, 1 = drifted, 2 = the file could not be read or parsed.
"""
import re
import sys

STATUSES = {"BUILT", "PARTIAL", "OPEN", "DECLINED", "NO PREMISE"}
DEFAULT = "docs/PLAN-recsurfaces-31-100.md"


def main(argv):
    path = argv[1] if len(argv) > 1 else DEFAULT
    try:
        text = open(path, encoding="utf-8").read()
    except OSError as exc:
        print(f"roadmap: cannot read {path}: {exc}", file=sys.stderr)
        return 2

    rows = {}
    problems = []
    for lineno, line in enumerate(text.splitlines(), 1):
        if not line.startswith("|"):
            continue
        parts = [p.strip() for p in line.strip().strip("|").split("|")]
        if len(parts) < 3 or not parts[0].isdigit():
            continue
        status = parts[2]
        if status not in STATUSES:
            problems.append(
                f'line {lineno}: row {parts[0]} has status {status!r}, which is not one '
                f"of {sorted(STATUSES)}. A status column with two spellings cannot be counted."
            )
            continue
        rows[int(parts[0])] = status

    if not rows:
        print(f"roadmap: no rows parsed from {path}", file=sys.stderr)
        return 2

    declared = {}
    declared_count = {}
    for line in text.splitlines():
        parts = [p.strip() for p in line.strip().strip("|").split("|")]
        if len(parts) == 3 and parts[0] in STATUSES and parts[1].isdigit():
            declared[parts[0]] = set(int(x) for x in re.findall(r"\d+", parts[2]))
            declared_count[parts[0]] = int(parts[1])

    if not declared:
        print("roadmap: no declared totals found (the summary block is missing)", file=sys.stderr)
        return 2

    for status in STATUSES:
        actual = {rid for rid, st in rows.items() if st == status}
        want = declared.get(status, set())
        # The NUMBER in the summary cell must match the list beside it. "21" next
        # to a list of 21 ids is the form that reads as verified; a number that
        # disagrees with its own list is drift even when the ids happen to be
        # right, and it is the half a reader trusts.
        if status in declared_count and declared_count[status] != len(want):
            problems.append(
                f"{status}: the summary says {declared_count[status]} row(s) but lists {len(want)}"
            )
        if want != actual:
            only_declared = sorted(want - actual)
            only_table = sorted(actual - want)
            problems.append(
                f"{status}: declared {len(want)} row(s), the table has {len(actual)}"
                + (f"; declared but not {status.lower()} in the table: {only_declared}" if only_declared else "")
                + (f"; {status.lower()} in the table but not declared: {only_table}" if only_table else "")
            )

    if problems:
        print(f"roadmap DRIFTED ({path}):")
        for p in problems:
            print("  - " + p)
        print("\nThe table is the authority. Fix the totals, not the table, unless a row is")
        print("actually wrong -- and when a row ships, change its status HERE first.")
        return 1

    counts = {s: sum(1 for st in rows.values() if st == s) for s in sorted(STATUSES)}
    print(
        f"roadmap consistent: {counts['BUILT']} built, {counts['PARTIAL']} partial, "
        f"{counts['OPEN']} open, {counts['DECLINED']} declined, "
        f"{counts['NO PREMISE']} without a premise ({len(rows)} rows)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
