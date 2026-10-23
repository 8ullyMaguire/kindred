# Handoff 2026-10-23h — works you keep skipping, embedding drift, and #43 settled by measurement

Three roadmap rows, one of which turned out not to need building.

## 1. #60 "Works you keep avoiding" — BUILT

`/skipped`, `store.SkippedWorks`.

The input existed and nothing read it. `interleave_impressions` stores **one row
per work per showing**, which is exactly the shape "was this put in front of them,
and did they do anything about it" wants.

**Why the algo log is not the source.** `algo_impressions` stores a whole list of
ids as a comma string per row, so grouping it by work would mean parsing a CSV in
SQL. Two logs, two shapes; the code says which one it read and why.

**Three exclusions, each a claim that would otherwise be false:**

| exclusion | why |
|---|---|
| shown ≥ 3 separate times | one unclicked list is a coincidence — they may not have scrolled, or were looking for something else |
| no credited showing | a work they acted on is not a work they avoided |
| not already marked read | a read from the bookmark screen never touches this log, so without the filter a finished work could be called "skipped" |

The page prints the threshold, because "you keep skipping this" is a claim about
the reader and has to be earned.

**Two actions per row.** "Not interested" is polarity −1 through
`LearnFromFeedback` — the same path the like button uses, so a reader's taste stays
one set of numbers instead of two adjusted separately. "I read it" marks read.

**Live, on the deployed binary.** Three staggered showings seeded for a real
owner (one sighting per work per hour, so three shows means three hours: seeding
is the only way to exercise this quickly, and the log is the thing under test):

```
data-testid="skipped-items"
shown 3 times, last on 2026-10-23 18:45:15
2 x "Not interested"
```

then the POST:

```
POST /skipped -> 200
work_feedback: 17249318|-1
arena_user_tag_weights: 74 rows, min -0.081, max -0.081
row gone from the list -> data-testid="skipped-empty"
```

**Mutations killed:** threshold ignored (shown once is enough), a credited showing
still counted, read works offered, and the owner filter dropped (everyone's
impressions leaking in).

**No JSON twin**, and the file says why: web renders HTML and api does not import
it, so an endpoint would mean either a second implementation of the query or a
layering exception for one page. `/new` made the same call for the same reason.

## 2. #43 Anti-recommendations — the premise was FALSE, settled by measurement

The row said "nothing negates the taste vector". Two writers negate it, and one of
them was already there:

1. `arena.LearnTags(byWork, winners, losers, …)` gives the losing work's tags
   **negative** deltas (`internal/api/arena.go:326`) — every pair judgement does
   this.
2. `/skipped`'s "Not interested" writes polarity −1 through `LearnFromFeedback`
   (`internal/store/feedback.go:231`).

And `signal/session_taste.go` accepts negatives by construction: it clamps to
[−1, 1] and its own comment says a negative must not be clamped away.

**Measured, not reasoned.** After one "Not interested" on work 17249318,
`/api/v1/recommend?seed=ao3_work:17249318` returned five candidates carrying:

| candidate | session_taste value | weight | contribution |
|---|---|---|---|
| A Happy Accident | −0.732 | 0.14 | **−0.1025** |
| Not Your Dog, Won't Play Dead | −0.529 | 0.14 | **−0.0740** |
| An Apex Predator | −0.448 | 0.14 | **−0.0627** |
| Eye of the Beeholder | −0.407 | 0.14 | **−0.0570** |
| A Part of Me | −0.448 | 0.14 | **−0.0627** |

The ranking is actively reduced, not merely unattributed.

**What is still not built, and the row now says so:** the *magnitude*. The eval
harness has no session-taste user model, so demotion is proven to exist and not
proven to be enough. Work-level negation independent of tags is also absent.

## 3. #76 Embedding drift monitoring — BUILT

`kindred embed` rewrites all 634,232 rows and **nothing compared the old space with
the new one**. Every stored reader tag weight was trained against the old
positions, so a rebuild that moved the space silently invalidated what the site had
learned — indistinguishable from a rebuild that reproduced it.

**A fingerprint, not a hash of the table.** A hash answers "did anything change",
cannot say whether the space moved or merely reshuffled, and changes when one tag
is added. Each run stores rows, width, mean L2 norm, the centroid, and a **sample
of 64 tag vectors**.

**The sample is load-bearing**, and a test proves why: two tags moving in opposite
directions leave the centroid exactly where it was, so a centroid-only monitor
reports "reproduced" for a space where every tag changed place.
`TestAnAverageCanHideOppositeMovements` is that case, and the mutant that compares
the centroid instead of the sample dies on it.

**Floor 0.98, not 0.9.** This is a deterministic solver over an unchanged corpus:
a rebuild landing at 0.95 has moved positions, not re-rolled a seed, and a 0.9
floor would bless it while the learned weights went stale.

**Two refusals:**

- the page does **not** print "0% drift" when fewer than two fingerprints exist.
  That is no comparison, not a good result, and it says so in words.
- a failed fingerprint does **not** fail the build. The embeddings are correct;
  refusing to finish would turn a monitoring gap into a broken deploy. It prints
  the failure by name instead, so a silent gap stays visible.

**Mutations killed:** floor 0, centroid-instead-of-sample, width change not its own
verdict, and the sample compared against itself (no cross-run lookup).

## 4. #48 Reader velocity — the count, and why not the weight

`MarksSince` + one line on `/new`: "2 work(s) marked read since then, across 1
visit(s)".

The row asks for a **signal**. A signal needs a weight, a weight needs a
measurement, and a measurement needs readers with more than one visit. Measured on
the live state database: **33 marked works, 8 owners, 8 reader-days, no owner with
five, max 13 marks in one day**. A weight fitted to that is a weight fitted to
noise. The count needs no model, is a fact about the reader, and is exactly what a
weight would later be computed from — so it is built and the row stays OPEN with
the number that would justify the next step recorded in it.

Mutant killed: the boundary ignored, so "since your last visit" counted every mark
ever made.

## Two defects the live table found and no test could

Both were in #76, both green in 27 packages, and both only appeared against
634,232 real rows:

1. **A single-connection deadlock.** `RecordEmbedRun` read the sample, then ran
   the centroid query, then the INSERT. The pool holds ONE connection
   (`store.go: MaxOpenConns(1)`) and an **unexhausted `Rows`** holds that
   connection until it is closed. On the live table the sample asks for 64 of
   634k rows and the loop `break`s — so the centroid query waited on the sample
   query forever: seven minutes at 0.5% CPU, nothing written, no error. The unit
   test passed because two fixture rows against a sample of eight EXHAUST the
   `Rows`, and an exhausted `Rows` closes itself. The regression test therefore
   has twelve rows and asks for four; removing the `Close` makes it fail after its
   own 20-second timeout.
2. **A false drift alarm.** This table records "no embedding for this tag" as an
   all-zero vector. Three of the first 64 tags carry zeros, and scoring two zero
   vectors at cosine 0.0 turned an honest 1.0000 into 0.9531 — so a build
   compared with ITSELF reported "the space MOVED", the loudest thing the monitor
   can say. Zero vectors are now excluded from the mean and counted in
   `EmptySample`, which the verdict and `/mirror` both name.

A third, smaller one: `run.Rows` was incremented inside the sample loop, so a
634k-row table would have been fingerprinted as "rows 64" and the drift report's
own row column would have been a lie.

## And one gate defect

`"the tag kind ranks with the default"` asserted `"algo": "embed"` on an
**unpinned** `/api/v1/related` request. An anonymous request is a new reader, and
the A/B arm is `fnv64a(kind|owner) % 2` — so the check was a coin flip: measured
**5 default / 1 challenger across six consecutive curls**. It passed three deploys
in a row on luck. Pinned with `?algo=embed`, and a second check now asserts every
related answer NAMES its arm.

## State

- HEAD is the commit above this file; 27/27 packages green.
- Deployed to thinkcentre, provenance IN SYNC, behaviour gate at 81 checks.
- Roadmap: **35 built, 14 partial, 47 open, 4 without a premise**.

## Next

Row 48 (reader velocity) is the last of the four named rows and is still OPEN: a
session model exists now (`reader_visits`), but nothing counts marks per visit. The
honest first question is what a velocity signal would change — "this reader is
bingeing" and "this reader is sampling carefully" imply different trust in the
session taste, and that claim needs a measurement, not a weight.

## Instrument notes

- `interleave_impressions` dedupes to **one sighting per work per hour**, so "three
  shows" cannot be produced by three quick curls. Live verification needs seeded
  rows at staggered timestamps; the log is the thing under test, so seeding it is
  honest, and the page that reads it is real.
- `python3 - <<'PY'` inside an `ssh '…'` heredoc mangles escapes twice. Write the
  file to the host and run `python3 /dev/stdin <<PY` (or a real file) instead of
  nesting quotes in f-strings.
- `/api/v1/recommend` accepts **work** seeds only. A tag seed answers
  `unsupported kind "ao3_tag"; this mirror holds "ao3_work"` — use the arena for
  tag seeds.
