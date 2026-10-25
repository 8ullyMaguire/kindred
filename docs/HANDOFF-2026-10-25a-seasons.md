# Handoff — #80: which tags belong to which month, and the defect the live run found

2026-10-25. `internal/tagseason`, `kindred seasons --write`, `tag_season`. The calendar TERM
(#44) is deliberately not built, and the row says why.

## The model

112,444 of the mirror's 112,937 works carry AO3's update date in its display format
("02 Jan 2006"); 493 do not. From those, per tag:

* **Lift, not volume.** `share of the month's works / share of the archive`. December has more
  works than June for reasons that have nothing to do with Christmas, so a raw monthly count
  mostly measures how much the archive was posting.
* **Year-consistency.** The peak month must be the same month in ≥75% of the years the tag
  appears in, with ≥3 works in a voting year. A single-year spike cannot fake it — and see below
  for why "≥3 years" is a separate floor.

Defaults: support ≥ 80, ≥ 20 works in the peak month, lift ≥ 1.5, year share ≥ 0.75, years ≥ 3.

## Measured on the live archive

    December   christmas            6.6x    166 works   13/14 years agree
               christmas smut       8.5x     56 works    7/8
               christmas fluff      6.5x     80 works    7/9
    February   valentine's day      7.5x     51 works    7/7
    October    halloween            6.0x     51 works    7/9

That is the cultural calendar, recovered from tag names and posting dates alone. It is also all
that survives: at `--min-support 300` only `christmas` remains, which is why the default is 80
and why the report says how many tags passed rather than implying a rich model.

## The defect the live run found

With the consistency test alone, **a tag that exists in only one year is 100% "consistent"**. The
first live run reported `project hail mary (2026)` as a MAY tag with `1/1 years agree` — every
work carrying it was posted in one month of one year. That is a tag going viral, not a season,
and no per-year test can tell the two apart.

`MinYears 3` is the fix. The tests now pin it (`TestAOneYearTagIsNotSeasonal`), and the row
records it, because the failure is invisible on synthetic data that spans three years by
construction.

A second finding from the same output: the loose run also surfaced show-release clusters
(`the amazing digital circus` characters all peaking in May, 2 of 3 years). Those are REAL
temporal regularities and they are not seasons — the 0.75 year-share floor is what separates
"aired in May twice" from "appears every December for fourteen years".

## What is NOT built, and why

#44's **calendar term** — using the table to move a ranking today. `kindred eval` replays a fixed
split at ONE instant, so a term that depends on "today's month" would be a constant for every
configuration and the ablation could not see it. It becomes measurable only if the harness
replays each held-out mark at the month the reader made it (`seen_works.read_at`), which is an
evaluator change before it is a ranker change. That is written in the roadmap row rather than
guessed at in code.

## Also recorded this session: #75 is data-blocked

The mirror stores every tag as `tag_type = 'freeforms'` — the crawl flattens AO3's categories.
Counted: 3,890,543 freeform rows and **110** fandom rows across 112,937 works. "Same non-fandom
tags, a different fandom" has no fandom column to filter on. #47's theme tree recovers fandoms
from embeddings, but a theme is not an attribution. Re-open when the crawl keeps `tag_type`.

## Tests and mutants

Five mutants, five killed: lift replaced by volume, the consistency test disabled, `MinYears`
removed, the `DISTINCT` de-duplication removed (a tag with two types on one work doubles its
count), and the month denominators zeroed.

One fixture lesson worth keeping: my first one-year-spike fixture gave the tag June in TWO years,
which made "June" a correct answer — the model was right and the test was wrong. The fixture now
uses a different month in each year.

## Files

    internal/tagseason/tagseason.go        LoadCounts, Build, consistency, ParseMonth
    internal/tagseason/tagseason_test.go   volume vs seasonality, one-year tag, consistency
    cmd/kindred/seasons.go                 the command, the write, the report
