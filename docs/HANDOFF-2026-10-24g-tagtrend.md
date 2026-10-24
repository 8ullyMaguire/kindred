# Handoff 2026-10-24g — #31: tag trend, and three things that made the first two answers wrong

## What shipped

`ModeTagTrend` — the fifth `corpusquery` runner and the first that compares time
windows. Given a tag, it returns the tags travelling WITH it, ranked by

```
lift = (the pair's share of works in the window) / (the corpus's own share in the window)
```

so `1.0` means "grew exactly as fast as the corpus", `2.0` twice as fast, `0.5` half.
Each row carries both counts (`works` in the window, `co_works` lifetime), and the notes
carry the window, the anchor and the corpus's growth rate.

Reachable as `kindred corpus-query --mode tag-trend --tag angst` and
`GET /api/v1/corpus-query/tag-trend?tag=angst&limit=25`.

## Round 1: the anchor

The row's phrase is "90-day vs corpus average". A window measured back from **today** is
wrong on a mirror: this one's newest work is 2026-10-15, and today is 2026-10-24, so a
wall-clock window is a coincidence away from being empty. The window is measured back
from the mirror's own newest date, and that date is printed. This part was right first
time, and it is the part that would have been invisible if wrong: every tag would report
the same collapse.

## Round 2: the dates — caught by the deploy gate, not by the tests

The live endpoint answered **500**: `anchor "31 Oct 2025" is not a date`. The mirror
stores `05 Sep 2007` on 112,893 rows and ISO on two. My fixtures all wrote ISO, so the
tests passed and the corpus refused.

The shallow bug is the parse. **The bug underneath is the one worth recording:** the
window comparison was a raw string comparison, and

```
"31 Oct 1999" >= "2020-12-12"   →  TRUE   (because '3' > '2')
```

Every work written on the 30th or 31st of any month would have counted as recent,
whatever its year — silently, and in the direction of "this tag is rising". So the
normalisation moved into SQL: ISO passes through, `DD Mon YYYY` is rebuilt as
`YYYY-MM-DD` from `substr` plus a month index over the three-letter name, and an empty
date normalises to NULL, which no comparison matches (an undated work is neither in the
window nor outside it).

Pinned by a test where the wrong answer is a specific number: ten works all dated the
31st, five in 1999 and five in 2021, a window that must report **5 of 10**. The mutation
`w.update_date >= ?` reports **10 of 10** — the exact diagnosis — and restoring gives
green.

## Round 3: the window was too thin to measure

With the dates fixed the endpoint answered — and every lift was `0.00`. The window held
**2 of 112,895 works**: this mirror's newest date is a single crawled row from 2026 while
the body of the archive ends in 2025. A corpus growth of `0.00002` turns every ratio into
noise or zero, and a table of zeros reads as "nothing is rising" when the truth is
"nothing was measured".

That is the same substitution the significance work refuses with its power floor, so the
same answer applies: the window **widens** (90 → 180 → 365 → 730 → 1460 → 3650 days)
until it holds enough, and says so. The floor is `max(5, min(200, corpus/10))` — two
hundred works on this archive, a fraction of a forty-work fixture that could never hold
two hundred.

Live after: **180 days, 16,153 works**, corpus growth 0.14x, and the top pair is
`ryland grace × project hail mary (2026)` at lift **6.32** (19 of 21 works inside the
window) — a crossover rising six times faster than the archive. That is a number someone
can act on, and it is checkable from the row's own two counts.

## The note that was not true

The widening note read "…only 16,153 of 112,895 works fall inside the shorter one" —
printing the **widened** count beside the word *shorter*. Both branches were in the same
sentence and only one could hold. It now reports the original window's count (2 of
112,895 live), and the test asserts that number, so the contradiction cannot come back
quietly.

## Verification

- `internal/corpusquery/tagtrend_test.go` (seven tests): anchor on the mirror, display
  dates compared as dates, both sides of the window measured, an empty window named, a
  thin window widening and saying so with the ORIGINAL count, and lift cancelling the
  corpus's growth.
- Mutation-checked: removing the widening loop reddens the thin-window test; the lexical
  predicate reddens the display-date test at 10 of 10.
- Deploy gate **117 checks** (was 114), three of them on the live endpoint: the mode
  answers, the window is named, the growth is named.
- Roadmap: `#31` BUILT; **28 built, 6 partial, 32 open, 4 without a premise**.
