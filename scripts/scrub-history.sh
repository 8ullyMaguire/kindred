#!/bin/bash
# Scrub personal paths and hostnames from every commit in the repository.
#
# A public repository publishes its whole history, not just its tip. The
# systemd unit named a user account and their exact corpus and state
# directories in the seven commits from the budget-gate work onward, so
# fixing the file at the tip alone would leave it readable with
# `git show <old-sha>:deploy/kindred.service`.
#
# There is no remote and no second clone, so a rewrite costs nothing: no
# force-push, no coordination, no stale references. The original .git is
# copied to /tmp/kindred-git-backup first regardless, because a history
# rewrite with no undo is the kind of thing you only notice afterwards.
#
# THREE TRAPS, all of which produced a scrub that exited 0 and left the
# hostname in the repository. A scrub that reports success while failing
# is worse than no scrub, because it is trusted.
#
#   1. --replace-text rewrites FILE CONTENT; commit messages need
#      --replace-message. Same expression-file syntax, different flag.
#      Doing only the first leaves the hostname in the commit log, which
#      is the one place a reader is most likely to read it.
#
#   2. git-filter-repo expands ~ and $HOME inside the expression file, so
#      a rule written as /home/USER==>$HOME is parsed as replacing
#      "/home/USER" with the literal string "$HOME"... and because the
#      shell had already expanded the heredoc, the file ended up
#      containing "~/code/kindred==>~/code/kindred" -- a self-mapping no-op
#      that applies cleanly and changes nothing. The first run "worked"
#      only because the substitutions it did apply had already been made
#      by hand at the tip.
#
#      So: print the rules before running them, and audit the RESULT
#      rather than the exit code. Both of those are below.
#
#   3. The rules must be in a real file, not a process substitution, when
#      two flags share them -- and the heredoc must be closed before the
#      next flag, or the rest of the script is swallowed as heredoc body
#      and the script silently does something else.
set -euo pipefail

cd "$(dirname "$0")/.."
backup=/tmp/kindred-git-backup
rules=/tmp/kindred-scrub-rules

if [ ! -d "$backup" ]; then
	cp -a .git "$backup"
	echo "backed up .git to $backup"
fi

# Written with printf and %s so nothing expands: the point is that these
# are literal characters, not shell substitutions.
printf '%s\n' \
	'~/code/kindred==>~/code/kindred' \
	'~/.local/share/kindred==>~/.local/share/kindred' \
	'~/kindling-data==>~/kindling-data' \
	'~/.local/bin==>~/.local/bin' \
	'/home/USER==>/home/USER' \
	'the 16 GB host==>the 16 GB host' \
	'the second host==>the second host' \
	>"$rules"

echo "--- rules about to be applied ---"
cat "$rules"
echo "--- end rules ---"

git-filter-repo --force \
	--replace-text "$rules" \
	--replace-message "$rules"

echo
echo "--- audit: what a public clone would see ---"
hits=$(git log HEAD -p | grep -inE '/home/[a-z]|alvaro|the 16 GB host|the second host' | head -5 || true)
if [ -n "$hits" ]; then
	echo "$hits"
	echo "AUDIT: FAIL -- hits above are still in the published history."
	exit 1
fi
echo "AUDIT: clean -- no personal paths or hostnames in the tip or its history."
