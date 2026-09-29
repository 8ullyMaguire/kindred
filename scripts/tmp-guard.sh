#!/bin/bash
# Reclaim /tmp from runaway kindred ingest runs.
#
# WHY THIS EXISTS RATHER THAN A tmpfiles.d RULE
#
# The obvious tool is systemd-tmpfiles, and the obvious rule is
#
#     x /tmp/kr-* - - - 5min
#
# On one Arch host tested (systemd 255, 4 GB tmpfs) it does nothing, and
# the reason is worth recording
# because it cost an hour to establish empirically:
#
#   - A bare `R /tmp/kr-probe - - -` with NO age also removes nothing.
#   - A rule on a plain filename, `/tmp/zzz-t1`, removes nothing.
#   - strace shows systemd enumerating /tmp and stat-ing the target.
#   - The 15-minute timer fires and the service exits 0 every time.
#
# So --clean is not applying removal rules to /tmp on this host at all,
# whatever the rule says. Debugging that further belongs to whoever owns
# the host image; this job needs to work today.
#
# THREE THINGS THE FIRST ATTEMPT GOT WRONG, since the rule shape looks
# obvious and is not:
#
#   - The field order is Type Path Mode UID GID Age Argument. Writing
#     `X /tmp/kr-* 5min` puts "5min" where the mode belongs, and systemd
#     rejects the ENTIRE file with "Invalid mode '5min'" -- while
#     --cat-config still prints the broken line as though it were valid
#     configuration, so a grep-based check says everything is fine.
#   - Capital X is the opposite of what it looks like: it removes only
#     files something has OPEN. The lowercase x removes only files nothing
#     has open, which is what a cleanup rule wants.
#   - Lowercase x removes only FILES, never a directory. A run leaves
#     kr-<random>/ containing the database, so an x-only rule reclaims
#     nothing at all. R is the recursive form.
#
# WHAT THIS DOES INSTEAD
#
# One find(1) with the same three safety properties the tmpfiles rule was
# reaching for:
#
#   - older than 5 minutes of mtime, so an in-flight run is never touched;
#   - `-links 0` is implied by the open-fd test below;
#   - fuser/lsof is consulted so a file another process holds open is
#     skipped even if its mtime is old. A run that is writing its database
#     has it open, and that is the case a purely time-based rule gets
#     wrong: an ingest that stalls for six minutes mid-write is not
#     abandoned, it is slow.
#
# Paired with a 15-minute timer, steady-state use is the last ~20 minutes
# of runs rather than everything since boot.
#
# systemd/timer drop-in that runs this:
#   /etc/systemd/system/kindred-tmp-guard.service
#   /etc/systemd/system/kindred-tmp-guard.timer

set -uo pipefail

# The pattern is deliberately narrow. /tmp holds other people's scripts and
# scratch files; a blanket sweep would delete work in progress, and the
# whole point of scoping to kr-* is that nothing else here is ours.
readonly PATTERN='/tmp/kr-*'
readonly MIN_AGE_MIN=5

# Space figures come from df, so the guard reports the thing the operator
# cares about rather than a file count.
log() { logger -t kindred-tmp-guard -- "$*" 2>/dev/null || echo "kindred-tmp-guard: $*" >&2; }

before=$(df -k --output=avail /tmp 2>/dev/null | tail -1 | tr -d ' ')
[ -n "$before" ] || { log "cannot read /tmp free space; refusing to guess"; exit 1; }

# The in-use test. fuser is in psmisc; lsof is a fallback. If neither
# exists the guard still runs -- an age-only sweep is better than nothing,
# and being unable to check is worth a log line rather than a refusal.
have_in_use_check=1
if ! command -v fuser >/dev/null 2>&1 && ! command -v lsof >/dev/null 2>&1; then
	have_in_use_check=0
fi

# in_use reports whether anything holds a descriptor on the run.
#
# It must check the FILES INSIDE the directory, not the directory itself.
# The first version called fuser on /tmp/kr-abc, which an ingest never
# opens -- it opens /tmp/kr-abc/kindred.db -- so the check always answered
# "not in use" and the guard deleted a run that was actively writing. The
# test caught it: an open 25 MB database with a 30-minute-old mtime was
# removed, which is exactly the run the mtime rule is supposed to be
# protecting.
#
# So: walk the tree. `find -type f` and ask about each file, plus the
# directory itself in case something chdir'd into it.
in_use() {
	local victim=$1
	local f

	if command -v fuser >/dev/null 2>&1; then
		fuser -s "$victim" 2>/dev/null && return 0
		while IFS= read -r f; do
			fuser -s "$f" 2>/dev/null && return 0
		done < <(find "$victim" -type f 2>/dev/null)
		return 1
	fi

	if command -v lsof >/dev/null 2>&1; then
		lsof -t -- "$victim" >/dev/null 2>&1 && return 0
		while IFS= read -r f; do
			lsof -t -- "$f" >/dev/null 2>&1 && return 0
		done < <(find "$victim" -type f 2>/dev/null)
		return 1
	fi

	return 1
}

removed=0
freed_kib=0
skipped=0

# -maxdepth 1 so the sweep covers the run directory itself, and find
# descends into it to report sizes. The removal is `rm -rf` on the top
# level only, which is what makes one find both decide and measure.
while IFS= read -r -d '' victim; do
	[ -e "$victim" ] || continue

	if [ "$have_in_use_check" -eq 1 ] && in_use "$victim"; then
		skipped=$((skipped + 1))
		continue
	fi

	# Re-check the age at the moment of removal rather than trusting the
	# find pass: between the two, a new run could have replaced the
	# directory, and a rule that deletes whatever is at a path it decided
	# about thirty seconds ago is not a rule worth having.
	now=$(date +%s)
	mtime=$(stat -c %Y "$victim" 2>/dev/null || echo "$now")
	age_min=$(( (now - mtime) / 60 ))
	if [ "$age_min" -lt "$MIN_AGE_MIN" ]; then
		skipped=$((skipped + 1))
		continue
	fi

	size_kib=$(du -sk "$victim" 2>/dev/null | cut -f1)
	size_kib=${size_kib:-0}

	if rm -rf -- "$victim" 2>/dev/null; then
		removed=$((removed + 1))
		freed_kib=$((freed_kib + size_kib))
	else
		skipped=$((skipped + 1))
	fi
done < <(find $PATTERN -maxdepth 0 -mmin +$MIN_AGE_MIN -print0 2>/dev/null)

after=$(df -k --output=avail /tmp 2>/dev/null | tail -1 | tr -d ' ')

if [ "$removed" -gt 0 ]; then
	log "reclaimed $removed abandoned run(s), ~$((freed_kib / 1024)) MiB; /tmp free ${before}K -> ${after}K"
elif [ "$skipped" -gt 0 ]; then
	log "nothing reclaimed: $skipped run(s) in use or too recent (in-use check $([ "$have_in_use_check" -eq 1 ] && echo on || echo OFF))"
fi

exit 0
