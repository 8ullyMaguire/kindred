# 2026-10-24b — Length, and a lambda that depended on the scale of the weights (#41)

Two units in one: the length signal the roadmap asked for, and the defect the
ablation exposed in the thing that was supposed to measure it.

## #41 — length_fit

`min_words`/`max_words` answer a different question from the one readers ask. A
filter deletes everything outside a range and leaves the inside unordered, so a
1,200-word drabble ranks beside a 90,000-word epic. The reader who spends an evening
on 3k fics and the one who wants a 300k epic are different readers, and no tag
signal here separates them.

    score = min(words, seedMedian) / max(words, seedMedian)

1.0 at the seeds' median length, 0.5 at twice or half, 0.25 at four times, 0.06 at
sixteen. Written as a ratio rather than `exp(-|ln ratio|)` so the number printed on
the page is the number a reader can check by hand from two word counts. The median is
over up to 40 seeds, because one 400k epic among twenty bookmarks must not set the
target. A seed with no word count is silence, not a zero-length work; a work with no
length skips for that candidate; a request whose seeds all lack lengths skips and
names itself in `meta.degraded[]`.

**The measurement is the part that makes it falsifiable.** Recall cannot see the
SHAPE of a list, so a signal that reshapes lists by length and one that does nothing
score identically on every metric the evaluation had. Every run now records
`length_fit`: the ratio of the returned list's median length to the median of the
works that reader went on to bookmark, averaged per reader, with `length_runs` and a
reason when unmeasured. Measured against the HELD-OUT works rather than the seeds the
signal saw — the other way round is "the list matches the thing that shaped it",
true by construction and worth nothing.

## The ablation (37 lists, identical sample, both tunes summing to 1.0)

| tune | recall@10 | NDCG@10 | length fit | overlap | fandoms |
|---|---|---|---|---|---|
| with length_fit (0.048 share) | **0.1063** | 0.1888 | **0.713** | 0.099 | 2.2 |
| without | 0.1035 | **0.1953** | 0.575 | 0.099 | 2.2 |

The signal does what it claims — length agreement up 24% relative — recall is flat to
slightly better, diversity is untouched, and nDCG pays about 3%. That is a trade
worth taking for a preference that no other dimension can see, and it is now a
measured trade rather than an assumption.

## The defect it surfaced: lambda depended on the scale of the weights

`diversify.Apply` blends `lambda*score - (1-lambda)*similarity`, and similarity is a
Jaccard in [0,1]. So the ABSOLUTE scale of the scores decides how much diversity
matters. Two vectors with identical ratios ranked differently:

| variant | recall@10 | overlap | fandoms |
|---|---|---|---|
| built-in default (weights sum 1.66) | 0.1050 | 0.086 | 2.4 |
| same ratios normalised (sum 1.0) | 0.1152 | 0.074 | 2.7 |

And `kindred tune` refuses any stored vector that does not sum to 1.0 — with the
comment "the sum IS the scale" — while the engine's own default sums to 1.66. Every
stored tune therefore re-tuned diversification as a side effect, and every weight
ablation this project has run compared a weight change with a lambda change attached.

Scores are now normalised by the list's own top score before the blend (taken by scan,
not read off `working[0]`, so a caller breaking the sorted-input contract still gets a
bounded blend). The invariant test was red first:

    TestTheOrderDoesNotDependOnTheScoreScale
      scale 0.5 reordered the list: [1 3] vs [1 2] at scale 1.0

and green after, at 0.5×, 2× and 10×.

**Proof at product level:** run #21 (tune `with_length`, sum 1.0) and run #24 (built-in
default, sum 1.66, same ratios) now produce *identical* numbers — recall 0.1063, NDCG
0.1888, overlap 0.099, 2.2 fandoms, length fit 0.713. Two scales, one ranking.

## A correction to the first report of this fix

The commit that fixed the scale said normalising was "a 10% recall gain measured over
the same readers". **That was wrong.** 0.1050 vs 0.1152 compares two different
*effective* lambdas, not an improvement to the algorithm; after normalisation the same
comparison is 0.1063 vs 0.1152, i.e. normalising cost about 8% recall relative to that
accident while making lambda mean one thing everywhere. What the fix buys is
interpretability and honest ablations, not recall. Recorded here because a number that
flatters a change is the easiest kind to leave in.

## The diversity result, re-measured after the fix

The lambda comparison this project has quoted since 2026-10-23 was taken at the
scale-blind blend, so it was re-run on the same 37 lists:

| lambda | recall@10 | NDCG@10 | overlap | fandoms | length fit |
|---|---|---|---|---|---|
| 0.75 (default) | 0.1063 | 0.1888 | 0.099 | 2.2 | 0.713 |
| 1.0 (diversification off) | 0.0921 | 0.1697 | 0.130 | 2.0 | 0.690 |

The conclusion survives and is slightly stronger: turning diversification off still
costs recall (−13% relative), NDCG (−10%) and makes lists more repetitive (+31%
overlap). The pre-fix run said the same thing at a different scale.

## Tests and mutants

- `internal/signal/lengthfit_test.go` — the ratio at 1×, 2×, 4×; log symmetry (one
  doubling costs the same at 10k and 500k); a silent seed does not drag the median;
  median not mean; the caller's seed order is not sorted in place; skip-not-zero for
  an unknown length and for a request with no lengths at all.
- `internal/eval/lengthfit_test.go` — unmeasurable sides are not zeros; zero and
  negative counts are absent rather than short; the medians are medians; a
  recommender without `WordCounts` reports why instead of reporting 0.
- `internal/diversify/mmr_order_test.go` — the scale invariant, plus the existing
  ordering pin (still green, so the rewrite's output on its own fixture is unchanged).

## State

27/27 packages; deployed; provenance IN SYNC; gate **100 checks**; runs #21, #22 and
#24 in `eval_runs` and rendering on `/eval`; roadmap **23 built, 6 partial, 37 open,
4 without a premise**.
