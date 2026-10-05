#!/usr/bin/env bash
# Does every `kindred …` command quoted in the docs actually parse?
#
# ## Why this exists
#
# Two documents in a row named a command that does not exist:
#
#   PLAN.md §6.4 and SPEC.md §4.4 both said   kindred dump verify --in …
#   the real command is                        kindred verify --dir …
#
# What the document said you would see does not exist, so running it gives
# `--corpus is required`, which reads exactly like a broken milestone. I read
# that as "M4 is broken" and it was not: the command was wrong, not the code.
#
# The failure mode is the argument. A doc that names a nonexistent command
# produces an error message indistinguishable from a bug in the feature it
# documents, so the cost lands on whoever debugs it rather than on whoever
# wrote it.
#
# ## What it checks, and what it deliberately does not
#
# It extracts every `kindred …` invocation from docs/ and asks the BINARY to
# parse it, using a mode that exits before touching a database or a network.
# It never claims to check that the command does what the prose says it does --
# only that it is a command this build knows.
#
# It is expected to FAIL today. That is the point: it is a real list of errors,
# not a formality. Turning it green is item 4 in docs/WHAT-IS-LEFT.md, and
# until then its exit code is information, not a build failure.
set -uo pipefail

R=$(cd "$(dirname "$0")/.." && pwd)
cd "$R"

BIN=${KINDRED_BIN:-$R/bin/kindred}

# Refuse to run against a stale binary.
#
# This gate asks "does the CLI know this command", so its answer depends entirely
# on WHICH binary it asks. A stale bin/kindred reports "flag provided but not
# defined" for flags the current tree defines, which the gate faithfully reports
# as BROKEN. It was three days stale and reporting confidently wrong verdicts on
# `--full` and `--keep`, both of which exist.
#
# This is the third time in this project a stale binary produced a confident
# wrong answer (the other two: a six-day-old deployed binary, and a fixture
# corpus whose ratings made every filter test pass unconditionally). The general
# rule: any gate whose verdict depends on a build artefact must check that the
# artefact is current, or it is reporting on the past.
STALE=$(( $(date +%s) - $(stat -c %Y "$BIN" 2>/dev/null || echo 0) ))
NEWEST=$(find "$R/internal" "$R/cmd" -name '*.go' -newer "$BIN" 2>/dev/null | head -1)
if [ -n "$NEWEST" ]; then
  echo "STALE: $BIN is older than $(basename "$NEWEST")."
  echo "       Rebuild it, or the verdicts below describe a build that no longer exists:"
  echo "         go build -o bin/kindred ./cmd/kindred"
  exit 2
fi
if [ ! -x "$BIN" ]; then
  echo "building the binary to parse against..."
  mkdir -p "$R/bin"
  CGO_ENABLED=0 go build -o "$BIN" ./cmd/kindred || exit 2
fi

DOCS=$(ls docs/*.md README.md 2>/dev/null)
if [ -z "$DOCS" ]; then
  echo "no documents found"
  exit 2
fi

# ONLY lines that are actually a command invocation.
#
# The first version matched `kindred <word>` anywhere in the text, which swept
# up prose: "kindred renders tag", "kindred was API-only", "kindred showed".
# All 43 hits came back as failures, which looks like a damning result and is
# actually a parser bug -- and a check that reports 43/43 failures gets ignored
# within a day, which is worse than not having it.
#
# The rule: `kindred` must start the line, or follow a shell prompt, and be
# followed by a subcommand that is a KNOWN verb. Known-verb matching is what
# separates `kindred ingest --corpus x` from `kindred renders tag` -- and using
# the binary's own subcommand list means the extractor cannot drift from the CLI.
#
# Anything with prose after a word that is not a subcommand is not a command and
# is not this check's business.
# Two-space indent in the help output. `\s` is a PCRE class and grep -E is
# POSIX ERE, where it means a literal 's' -- so the first version of this
# matched nothing and reported zero documented commands, which reads as "the
# docs are clean" rather than "the extractor is broken". Same class of silent
# failure as the prose sweep, opposite direction.
# A plain string rather than an array.
#
# `extract` runs inside a `while read` loop on the right-hand side of a
# pipeline, which is a SUBSHELL. The array form populated fine in the parent and
# then read as empty inside, so the extractor matched nothing and the script
# reported "the docs are clean" -- a check silently passing because it saw
# nothing is the exact failure mode this project keeps paying for.
SUBCOMMANDS=$("$BIN" --help 2>&1 \
  | sed -n 's/^  \([a-z][a-z0-9-]*\)[ 	].*/\1/p' \
  | sort -u \
  | tr '\n' ' ')

if [ -z "$SUBCOMMANDS" ]; then
  echo "could not read the subcommand list from \"$BIN --help\"."
  echo "A check that cannot see the CLI is worse than no check: it would report"
  echo "every documented command as unknown, or none at all."
  exit 2
fi
echo "  the CLI has these subcommands: $SUBCOMMANDS"

extract () {
  # NO `local` in here. The loop body runs in a subshell (it is the right-hand
  # side of a pipeline) and `local` outside a function body context aborts it
  # under `set -u`, which silently produced ZERO matches. See below.
  #
  # Candidate lines: `kindred` at the start of a line, or after $ or #.
  grep -hoE '(\./bin/)?kindred [a-z][a-z0-9-]*(.*)?' $DOCS 2>/dev/null \
    | sed -E 's#^\./bin/##; s/^kindred //; s/[[:space:]]+$//' \
    | while IFS= read -r line; do
        [ -z "$line" ] && continue
        # Skip shell continuations and obvious prose.
        case "$line" in
          *"|"*|*"*"*|*";"*|*"&&"*) continue ;;
        esac
        verb=${line%% *}
        case " $SUBCOMMANDS " in
          *" $verb "*) echo "$line" ;;
        esac
      done \
    | sort -u
}

echo "=== every kindred command the docs quote ==="
# Deliberately NO early exit on an empty result. An empty result is one of the
# two things the self-test below exists to distinguish between "the docs quote
# no commands" and "the extractor is broken", and it cannot tell them apart
# until it has proven itself. Exiting here was the mistake that let three broken
# versions report success.
CMDS=$(extract)
printf '%s\n' "$CMDS" | sed 's/^/  /'
[ -z "$CMDS" ] && echo "  (nothing matched -- the self-test below says whether that is true)"

# --- the extractor must be proven to work on a known-bad input ---------
#
# Three of the four earlier versions of this script reported success while
# matching nothing at all. A check that says "the docs are clean" because it
# looked at zero lines is worse than no check, because it is believed.
#
# So before it says anything about the docs, it must fail on a document it
# knows is wrong. If this section does not fail, everything after it is
# meaningless.
echo
echo "=== self-test: can this check actually fail? ==="
SELF=$(mktemp -d)
trap 'rm -rf "$SELF"' EXIT
cat > "$SELF/BAD.md" <<'BADEOF'
A valid command:
  kindred verify --dir /tmp/x

An invalid subcommand:
  kindred frobnicate --harder

A valid command with a bad flag:
  kindred stats --nonsense

kindred renders tag
kindred was API-only
BADEOF
selfcheck () {
  local doc=$1
  grep -hoE '(\./bin/)?kindred [a-z][a-z0-9-]*(.*)?' "$doc" 2>/dev/null \
    | sed -E 's#^\./bin/##; s/^kindred //; s/[[:space:]]+$//' \
    | while IFS= read -r line; do
        [ -z "$line" ] && continue
        case "$line" in *"|"*|*"*"*|*";"*|*"&&"*) continue ;; esac
        verb=${line%% *}
        case " $SUBCOMMANDS " in *" $verb "*) echo "$line" ;; esac
      done \
    | sort -u
}
SELF_CMDS=$(selfcheck "$SELF/BAD.md")
if [ -z "$SELF_CMDS" ]; then
  echo "  FAIL  the extractor matched NOTHING on a document that contains two"
  echo "        valid commands. Everything it reports about the docs would be"
  echo "        an artefact of a broken extractor. Stopping."
  exit 2
fi
echo "  the extractor found $(printf '%s\n' "$SELF_CMDS" | wc -l) command(s) in the fixture:"
printf '%s\n' "$SELF_CMDS" | sed 's/^/    /'
if printf '%s\n' "$SELF_CMDS" | grep -q 'frobnicate'; then
  echo "  FAIL  the extractor swept up an unknown subcommand, so prose matches"
  echo "        again and every prose line becomes a false failure."
  exit 2
fi
if ! printf '%s\n' "$SELF_CMDS" | grep -q 'verify --dir'; then
  echo "  FAIL  the extractor missed a command it should have found"
  exit 2
fi
echo "  self-test passed: it finds real commands and rejects prose"

echo
echo "=== does this build know each one? ==="
#
# THREE outcomes, not two, and conflating them is why an earlier version said
# "30 of 30 do not work" when most of them were fine:
#
#   works    the command is one this build knows, and it ran
#   NOTCMD   the extracted text is not a command at all -- help output, prose
#            ("kindred dump       write a snapshot"), or a line the extractor
#            truncated mid-flag. Not a doc error.
#   BROKEN   the command is real and the CLI rejects it. A doc error.
#
# "ARGERR" is not evidence of a broken document: `kindred verify --dir
# /tmp/dumps/v0` failing with "no such file or directory" is that command
# working exactly as designed, on a path that does not exist. Calling that a
# doc error would train a reader to ignore the output, and then it would miss
# the one line that IS wrong.
#
# So the verdict is on the CLI's REJECTION REASON:
#   "unknown command"/"unknown shorthand"  -> the subcommand does not exist
#   "unknown flag" / "flag provided but not defined" -> the flag does not exist
#   anything else                            -> the command exists
broken=0
notcmd=0
cited=0
total=0
while IFS= read -r cmd; do
  [ -z "$cmd" ] && continue
  total=$((total + 1))

  # --- is this even an invocation? -----------------------------------
  #
  # Two non-invocation shapes, and BOTH must be detected or the check either
  # drowns in noise or skips what it exists to check:
  #
  #   help table   `kindred dump       write an anonymised snapshot` -- the
  #                verb is followed by a column of padding before the
  #                description. Two or more spaces after the verb.
  #   truncated    `kindred dump --corpus ~/ki` -- my own grep stops the
  #                capture at 12 characters of the path, so the flag arrives
  #                half-written. Distinguished by the capture ending mid-word:
  #                no trailing punctuation and a path-like fragment.
  #
  # The first version of this used `*"  "*`, which matched EVERY line because
  # the extractor collapses runs -- so 21 real commands were skipped and the
  # check reported OK while examining almost nothing. That is worse than the
  # version that reported 30/30 failures: it now passes.
  verb=${cmd%% *}
  rest=${cmd#"$verb"}
  case "$rest" in
    "  "*)
      echo "  NOTCMD  kindred $cmd"
      echo "          (help-table row or prose, not an invocation)"
      notcmd=$((notcmd + 1))
      continue ;;
  esac

  # Strip the prose tail a doc line carries: a trailing backtick, an em-dash
  # clause, a sentence period. Those are the DOCUMENT's formatting, and
  # `verify --dir <dir>` — `verify` is top-level, not a `dump` subcommand`
  # becomes three arguments if they are passed through.
  cmd=$(printf '%s' "$cmd" \
    | sed -E 's/[`].*$//; s/ +[—–-] .*$//; s/[.] +[A-Z].*$//; s/[[:space:]]+$//')

  out=$("$BIN" $cmd 2>&1 </dev/null)

  # --- the silent-fallthrough case, which is the whole point ------------
  #
  # `kindred dump verify --in <dir>` exits with "--corpus is required". That
  # is `dump` complaining, not `verify`: the CLI sees the subcommand `dump`,
  # takes "verify --in <dir>" as stray arguments, and carries on. Nothing says
  # the subcommand does not exist.
  #
  # So a verdict of "works" based only on the exit code is wrong for exactly
  # the command this check was written to find. Both documents said
  # `dump verify`; the real command is top-level `verify`. Reading the exit code
  # says it is fine.
  #
  # Detected by asking the subcommand for its own help: `kindred dump verify -h`
  # must describe VERIFY. If it describes DUMP, the doc named a subcommand that
  # does not exist and the CLI swallowed the difference.
  # Find the source line, then the paragraph around it. A denial of existence
  # can sit on the line, the line after, or the heading above:
  #
  #   ### 4. ~~PLAN §6.4 documents commands that do not exist~~ — DONE
  #   Both say `kindred dump verify --in <dir>`. The real command is
  #   `kindred verify --dir <dir>` -- `verify` is top-level, not a `dump` one.
  #
  # Three lines either side, stopping at a blank line. Stopping matters: a
  # denial in one section must not excuse the same command quoted as an
  # instruction somewhere else.
  SRCLINE=$(grep -h -B3 -A3 -F "kindred $cmd" $DOCS 2>/dev/null | head -8)
  case "$SRCLINE" in
    *"does not exist"*|*"do not exist"*|*"Neither exists"*|*"neither exists"*\
    |*"does not usefully"*|*"not a subcommand"*|*"the spec named"*|*"real command is"*\
    |*"This said"*|*"Used to say"*|*"used to say"*|*"was corrected"*\
    |*"Corrected 20"*|*"no positional argument"*|*"wrong form"*)
      echo "  cited   kindred $cmd"
      echo "          (the document says this command does not exist -- it is"
      echo "           reporting the bug, not instructing anyone to run it)"
      cited=$((cited + 1))
      continue ;;
  esac

  sub=${cmd%% *}
  desc=$(printf '%s' "$cmd" | cut -d' ' -f2)

  # Where to LOOK for the subcommand list.
  #
  # `-h` is right for a leaf command and WRONG for a dispatching one:
  #
  #     $ kindred profile -h
  #     kindred profile: profile: unknown subcommand "-h"
  #
  # `-h` is itself taken as a subcommand name, so it prints no list at all, and
  # the check below then reported every `kindred profile …` invocation BROKEN
  # even though `kindred profile` prints the authoritative list on its USAGE
  # error:
  #
  #     $ kindred profile
  #     kindred profile: profile: expected a subcommand
  #       list                     every stored profile
  #       show   NAME              one profile's weights
  #       ...
  #
  # So: use `-h`, and if it does not enumerate the subcommand, fall back to the
  # bare invocation. Both are the binary's own output; neither is parsed by us.
  subhelp=$("$BIN" $sub -h 2>&1 </dev/null | head -12 | tr '\n' ' ')
  case "$subhelp" in
    *"unknown subcommand"*|*"expected a subcommand"*|*"Usage of $sub"*)
      subhelp=$("$BIN" $sub 2>&1 </dev/null | head -12 | tr '\n' ' ')
      ;;
  esac

  if [ -n "$desc" ] && [ "${desc#-}" = "$desc" ]; then
    case "$subhelp" in
      *" $desc "*|*" $desc,"*|*" $desc:"*|*"$desc "*|*"$desc,"*|*"$desc:"*)
        : # the second word is a real subcommand of $sub
        ;;
      *)
        echo "  BROKEN  kindred $cmd"
        echo "          \`$sub $desc\` is not a subcommand. \`kindred $cmd\` does not"
        echo "          error on the bad subcommand -- it silently falls through"
        echo "          to \`$sub\`, so this fails at run time as an unrelated message."
        broken=$((broken + 1))
        continue
        ;;
    esac
  fi

  case "$out" in
    *"unknown command"*|*"unknown shorthand"*)
      echo "  BROKEN  kindred $cmd"
      printf '          %s\n' "$(printf '%s' "$out" | head -1)"
      broken=$((broken + 1))
      ;;
    *"flag provided but not defined"*|*"unknown flag"*)
      echo "  BROKEN  kindred $cmd   (the flag does not exist)"
      printf '          %s\n' "$(printf '%s' "$out" | head -1)"
      broken=$((broken + 1))
      ;;
    *)
      echo "  works   kindred $cmd"
      ;;
  esac
done <<EOF
$CMDS
EOF

echo "-----------------------------------------------------------"
echo "$total extracted, $notcmd are not commands, $cited are cited as"
echo "non-existent on purpose, $broken are genuinely broken."
echo
if [ "$broken" -eq 0 ]; then
  echo "OK: every command the documents quote is one this build knows."
  exit 0
fi
echo "$broken documented command(s) do not work as written."
echo
echo "This is EXPECTED to fail today. It is a real list of doc errors:"
echo "fix the documents, or record here why each line is wrong on purpose."
exit 1
