# 2026-10-24a — What the order is worth (#42)

Every recommendation carried `Evidence{Signal, Value, Weight, Reason}`. That says how
a work got into the list. It does not say whether the ORDER means anything, and a
weight of 0.41 invites a reader to read it as a chance — which no weight in this
project has ever been.

## Built

`kindred eval` now records, per position 1..k, how many readers' lists reached it and
how many of those held a work the reader later bookmarked (`eval_positions`). The
recommendation page prints three bands of that with the run id, and
`GET /api/v1/calibration` serves the unrounded numbers and every position, so a page
that says "26%" can be checked against 29 of 111.

## Measured, live (run #18, k=10)

| band | right | reached | rate |
|---|---|---|---|
| first three | 29 | 111 | 26% (1 in 4) |
| next four | 21 | 148 | 14% (1 in 7) |
| the rest | 10 | 111 | 9% (1 in 11) |

Position 1 alone is 12 of 37 = 32%, and the tail is **3× worse than the top, not the
10× the ordering implies**. Two honest readings: the weighting is doing real work,
and the eighth slot is not worthless — a page that implied "positions 8-10 are
noise" would be overstating a measurement that says 9%.

## Three facts the block refuses to merge

- **No run** → "not calibrated yet" plus the command that would fix it. Never `0%`;
  the endpoint carries `calibrated:false` and a `reason` rather than an empty object.
- **A band nothing reached** → an em dash with its counts. `0 of 0` divided is `0%`,
  which reads as a total miss where the recommender never aimed.
- **A run under 20 lists** → no percentage at all. Three readers produce rates of 0,
  0.33, 0.67 and 1; "1 in 3" for that is a coin flip with a decimal point.

And the label is the rate. "Strong" / "speculative" would be an opinion the data does
not contain: it says 1 in 4 or 1 in 11, and a reader can act on that without being
told which one to call strong.

## The defect worth recording

`LatestCalibration` selected `run_id` from `eval_runs`, where the column is `id`. The
query errored, the code treated **any** scan error as "no rows recorded", and the page
honestly reported "not calibrated yet" while the rows sat in the table. Only
`sql.ErrNoRows` is a state now; everything else is returned.

That is the same shape as the roadmap total and the gate's `curl -f`: a failure path
that produces a plausible state. The test that would have caught it earlier is the
one that now exists — the view is built from a real store, not a hand-made struct.

## A count that was not derived

The block first said "40 readers' lists" beside denominators of 111 — and 40 × 3 is
120. 40 was the split's user count; the lists are 38 evaluated, 37 long enough to have
a first item. The reader count is now derived from position 1's `observed`, so it
cannot drift from the denominators, and a test pins 40/38/37 → "37 readers".

## Tests and mutants

8/8 killed: every reader assumed to fill every position; the raw index used as the
position (so a repeat shifts its neighbours); the position counter carried across
readers; a rate printed under 20 readers; a missing position returning an empty record
instead of none; the store quoting the OLDEST run; a band with no observations printing
a percentage; the thin-run warning dropped from the sentence.

## State

27/27 packages; deployed; provenance IN SYNC; gate **99 checks**; run #18 recorded and
rendering. Roadmap: **22 built, 7 partial, 37 open, 4 without a premise** (the checker
agrees).
