#!/usr/bin/env python3
"""check-cli-coverage.py — the REVERSE of check-doc-commands.sh.

## Why this exists

`check-doc-commands.sh` answers one direction:

    every `kindred …` command quoted in docs/  ->  does the binary know it?

That direction is green (30 invocations, exit 0). It was made green by fixing
three documents, and it is worth keeping.

This script asks the other direction, which nothing was asking:

    every flag the binary defines  ->  is it named anywhere in docs/?

The asymmetry is not neutral. The first direction finds a doc that names a command
which does not exist, and the symptom is a confusing error for whoever runs the
documented command. The second direction finds a capability that exists, works,
and is undiscoverable — no symptom at all, just absence. Only the direction with
a visible symptom got a gate.

Measured on 2026-10-05:

    29 command-specific flags defined by the binary, named in no document
    2 global flags never documented (--top-n, --embed-dim)
    7 of 12 commands have NO example invocation with any flag

    recommend, crawl, profile, rate, corpus-query, stats, fetch

Every one of those seven is named in prose somewhere. None is shown being used.
A reader who knows `kindred crawl` exists still cannot learn that `-workers 4` is
what makes it polite, because no document says so.

## What it deliberately does NOT do

It does not require every flag to be documented. `-h` and `-help` are excluded,
and so is `--` . Some flags are self-evident (`--json`) and documenting each one
in prose is worse than leaving it to `--help`. The gate therefore reports an
uncovered count and an ALLOWLIST of names that are exempt, so adding a flag
forces a decision rather than silently growing a number.

That is the important half. A gate that just counts cannot be satisfied
honestly — you would either document 29 flags nobody reads, or delete the gate.
An allowlist forces each exemption to be named, which is a decision a human can
audit.

## Exit codes

    0  every command-specific flag is either documented or explicitly exempt
    1  at least one flag is neither
    2  the gate could not run (no binary) -- NOT a pass

Exit 2 is separate from 1 on purpose. A gate that reports a coverage gap and a
gate that failed to measure must never print the same thing.
"""

from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

# Flags bound for every command by internal/config.Config.Bind. Documenting them
# once is enough, and several already are. They are excluded from the
# command-specific count but still reported separately, because "accepted
# everywhere, mentioned nowhere" is its own kind of confusion.
GLOBAL = {"listen", "db", "corpus", "mode", "top-n", "embed-dim", "k-anon"}

# Flags no document needs to name. `--help` is the documentation for these: the
# name says what it does, and a line of prose per boolean flag is noise. Add to
# this list only with a reason, because the whole point is that the exemption is
# a decision rather than a default.
EXEMPT = {
    "json": "--help says what it does; every CLI has one",
}

COMMANDS = [
    "serve", "ingest", "recommend", "crawl", "profile", "rate",
    "corpus-query", "stats", "dump", "verify", "fetch", "tune",
]


def binary() -> Path:
    b = ROOT / "bin" / "kindred"
    if not b.exists():
        print("check-cli-coverage: no bin/kindred; run `make build` first", file=sys.stderr)
        sys.exit(2)
    return b


def command_flags(binpath: Path, cmd: str) -> set[str]:
    """Flags the binary lists for one command.

    Read from `-h`, which is the same source `--help` shows a user. Asking the
    binary rather than parsing the source is deliberate: the claim is about what
    the CLI ADVERTISES, and a flag declared in Go but unreachable at runtime is
    advertised here and accepted there, which is the opposite failure.
    """
    out = subprocess.run(
        [str(binpath), cmd, "-h"],
        capture_output=True, text=True, timeout=60,
    )
    text = out.stdout + out.stderr

    # Parse Go's FlagSet help format, NOT a free-text search.
    #
    # The first version used `re.findall(r'-{1,2}([a-z][a-z0-9-]*)', text)` over
    # the whole output, and reported a phantom flag `-only` for every command.
    # It is not a flag: it is the English word "only" appearing in a flag's
    # description ("(default: the only correct answer)"). Nine of twelve
    # commands got a `-only` they do not have.
    #
    # Go prints one entry per flag, and the entry always starts at column zero:
    #
    #   -corpus string
    #        read-only corpus SQLite path
    #
    # So the flag NAME is the first token of a line that begins with whitespace,
    # then a dash. Anything else is prose.
    flags = set()
    for line in text.splitlines():
        if not line.startswith((" ", "\t")):
            continue
        m = re.match(r"\s+-{1,2}([a-z][a-z0-9-]*)\b", line)
        if m:
            flags.add(m.group(1))
    return flags - {"h", "help"}


def all_docs_text() -> str:
    parts = []
    for p in sorted((ROOT / "docs").rglob("*.md")):
        parts.append(p.read_text())
    return "\n".join(parts)


def main() -> int:
    binpath = binary()
    docs = all_docs_text()

    undocumented: dict[str, list[str]] = {}
    global_undocumented: set[str] = set()
    no_example: list[str] = []

    for cmd in COMMANDS:
        flags = command_flags(binpath, cmd)

        # One definition of "has an example", used for every command. A command
        # that lists no flags of its own (`profile`, which dispatches to
        # subcommands) still needs one, so this check is NOT inside a
        # `if not flags: continue` -- an earlier version skipped it there, and
        # the flag scan and the example scan then disagreed about `profile`.
        if not re.search(r"kindred\s+" + re.escape(cmd) + r"\b[^\n`]*-{1,2}", docs):
            no_example.append(cmd)

        if not flags:
            continue

        missing = []
        for f in sorted(flags):
            if f in GLOBAL:
                # A set, not a list: -top-n is listed by nine commands, and
                # appending per command printed "18 global flags" for two.
                if f"--{f}" not in docs:
                    global_undocumented.add(f)
                continue
            if f in EXEMPT:
                continue
            if f"--{f}" in docs or f"-{f}" in docs:
                continue
            missing.append(f)
        if missing:
            undocumented[cmd] = missing

    total = sum(len(v) for v in undocumented.values())

    print("CLI coverage — the direction check-doc-commands.sh does not ask")
    print()
    if undocumented:
        print(f"  {total} command-specific flag(s) defined but named in no document:")
        for cmd, fs in sorted(undocumented.items()):
            print(f"    {cmd:13} {', '.join('-' + f for f in fs)}")
    else:
        print("  every command-specific flag is documented")

    if global_undocumented:
        print()
        print(f"  {len(global_undocumented)} global flag(s) accepted by every command,")
        print("  named in no document:")
        print(f"    {', '.join('--' + f for f in sorted(global_undocumented))}")

    if no_example:
        print()
        print(f"  {len(no_example)} of {len(COMMANDS)} commands have NO example invocation")
        print("  with any flag, though all are named in prose:")
        print(f"    {', '.join(no_example)}")

    print()
    if total or no_example:
        print(f"UNDOCUMENTED: {total} flag(s), {len(no_example)} command(s) with no example.")
        print("  This is not a failure of the build. It is absence of documentation,")
        print("  which has no symptom and so nothing else in this project would")
        print("  report it.")
        return 1
    print("COVERED: every command-specific flag is documented.")
    return 0


if __name__ == "__main__":
    sys.exit(main())