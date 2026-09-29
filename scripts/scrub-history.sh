#!/bin/bash
# Scrub personal paths and hostnames from every commit in the repository.
#
# A public repository exposes its whole history, not just its tip. The
# systemd unit named a user account and their exact corpus and state
# directories in the seven commits from fa1fd0f onward, so fixing the file
# at the tip alone would leave it readable with `git show <old-sha>:...`.
#
# There is no remote and no second clone, so a rewrite costs nothing: no
# force-push, no coordination, no stale references. The original .git is
# copied to /tmp/kindred-git-backup first regardless, because a history
# rewrite with no undo is the kind of thing you only notice afterwards.
set -euo pipefail

cd "$(dirname "$0")/.."
repo=$(pwd)
backup=/tmp/kindred-git-backup

if [ ! -d "$backup" ]; then
	cp -a .git "$backup"
	echo "backed up .git to $backup"
fi

# Two passes, because the two flags do different things and the first
# attempt used only one:
#
#   --replace-text    rewrites file CONTENT
#   --replace-message rewrites COMMIT MESSAGES
#
# The first run cleaned every file and left "the 16 GB host" in two commit
# messages, which is the one place a reader is most likely to read it. A
# history scrub that only does half the job is worse than none, because it
# reports success.
git-filter-repo --force \
	--replace-text <(cat <<'EOF'
~/code/kindred==>~/code/kindred
~/.local/share/kindred==>~/.local/share/kindred
~/kindling-data==>~/kindling-data
~/.local/bin==>~/.local/bin
$HOME==>$HOME
the 16 GB host==>the 16 GB host
the second host==>the second host
EOF
) \
	--replace-message <(cat <<'EOF'
the 16 GB host==>the 16 GB host
the second host==>the second host
EOF
) \
	--message "privacy: scrub personal paths and hostnames from all history

git-filter-repo --replace-text over every commit, so no old sha can
surface a version of deploy/kindred.service naming a user account, their
corpus path, or their state directory.

The tip was fixed in 266b615; this makes the fix true of the history
too, which is what a public repository actually publishes."
