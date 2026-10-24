# Handoff 2026-10-24c — #84: a difference is not yet a result

## What the row asked for, and why the answer is not IPS

#84 was titled "counterfactual evaluation" and its premise was: `interleave_impressions`
holds the alternative only as the other algorithm's pick, and no IPS estimator reads it.

IPS is the wrong instrument **here**, and the reason is structural rather than a
shortage of effort. Propensity is the probability the logging policy *would have served*
an item. This recommender is deterministic, so every item it served has propensity 1 and
every item it did not serve has 0. There is no overlap; the weighting has nothing to
weight; an IPS estimate would be the identity function on the data it was given.

Interleaving exists precisely to answer "which of two rankers is better" **without**
propensities — both arms contribute slots to one list, and a read credits the arm that
showed the work. That attribution was already implemented and correct (rows #17, #22).
What the log was actually missing was the **test**: with the credit attribution right,
"is A better than B" is a two-proportion question over slots, and nothing asked it.

So the row ships as the test, and the premise is corrected in the table rather than
quietly reinterpreted.

## What was built

`internal/stats` — a new package, so this is not buried in a handler:

- `FisherExact(a, b, c, d)` — two-sided Fisher exact on the 2×2 of read/not-read per
  arm, summed over every table at least as extreme as the observed one (the definition R
  uses), computed in log space via `Lgamma` so a few thousand slots cannot overflow.
- Degenerate tables return **1, not 0**. A table that could only have come out one way
  is no evidence, and `p = 0` would be the strongest possible claim about nothing.
- `Compare(...)` returns a `Verdict{P, Leading, Reason, Underpowered}`.
- **The power floor comes first: 50 slots per arm.** This is the part that matters and
  it is a separate rule from the p-value:
  - 2 of 2 against 0 of 2 → p = 1.0, and reads as "perfect" in both columns.
  - 12 of 20 against 0 of 20 → p ≈ 0.0002, and is still a coin landing.
  - A verifier that only computes p names a winner from the second. The floor refuses
    before the test runs, and the refusal names the slot counts.
- `Alpha = 0.05`, exported and stated, because a page that prints a p-value without
  saying what would count as evidence leaves the reader to invent a standard.

Wiring: `store.InterleavePairStat` gains `P`, `Leading`, `NoLeadReason`, `Underpowered`.
`/algo`'s head-to-head table gains a Verdict column; `/eval` gains the same table plus one
sentence for the whole section ("No result yet…" / "At least one pair has cleared the
test"), because on a young deployment every per-pair reason is the same sentence with
different numbers.

## The gate defect this produced

The first checks pointed at `/algo?mode=interleave`. The deploy failed:

```
FAIL the head-to-head table carries a verdict column — expected
     data-testid="interleaved-verdict" at http://127.0.0.1:8019/algo?mode=interleave
```

The page was not broken. `/algo` renders the standings **only after it has just served a
merged list**, so a bare GET shows the picker. Making that check pass would mean serving a
list on every deploy — which *records impressions*, i.e. the gate would fabricate the
experiment data it was verifying. **A check that makes traffic cannot be trusted about
traffic.** The pairs table moved to `/eval` (reads, never serves) and the checks point
there.

## Live state

`/eval` on the deployment, verbatim:

```
no lead — too few slots to compare: 3 slots for default and 0 slots for hybrid, and 50
each are needed. Read marks on this list are the evidence; until there are enough of them
a difference is a coin landing, whichever way it landed.
no lead — too few slots to compare: 11 slots for default and 11 slots for quality, and 50
each are needed. ...
No result yet.
```

`/api/v1/interleave/standings` carries `p: 1`, `underpowered: true` and the reason.

## Verification

- Package tests: 28/28 `ok` (was 27; `internal/stats` is the new one).
- Mutation gate: **11 mutants, 11 killed**, each by the test that names the defect —
  two-sidedness (sum every table → p ≡ 1), degenerate table claiming p = 0, the floor
  applied after the test, the lead following raw counts instead of rates, alpha at 0.5,
  the verdict computed and never attached, the underpowered flag cleared, and both
  dashboard sentences forced on and forced off.
- Deploy: provenance IN SYNC, **103 checks** (was 100).

## Roadmap

Row 84 → BUILT, with the IPS reasoning recorded in the row itself so the next reader does
not re-open it as an unimplemented estimator. Totals re-derived from the table:
**24 built, 6 partial, 36 open, 4 without a premise** (70 rows) — checker agreeing.

## The general lesson

A metric table invites a reader to treat ordering as ranking. The rates were always
correct; what was missing was a column that could say "not yet", and the refusal is worth
more than the test: on this deployment **every** comparison is currently refused, which
is the honest state of an experiment with 22 impressions and would have been invisible
before.
