# Handoff 2026-10-24i — #89 ALS: the biggest number I have measured, and why it was a leak

## What shipped

- `internal/als` — implicit-feedback ALS (Hu/Koren/Volsky), closed form, Cholesky per row,
  with the `YᵀY + α·Σ y_i y_iᵀ` identity so the shared matrix is built once per sweep.
- `corpus.Interactions` / `InteractionKinds` over `user_work_interactions`.
- `work_factors` in the state DB (separate from `embeddings` — that table holds TAG
  vectors; one table would make a row's meaning depend on which id it holds).
- `signal.Latent` — `max over seeds of cosine(factor(candidate), factor(seed))`, clamped
  at zero, loaded lazily behind a func so a request that does not use it pays nothing.
- `kindred als` (train and store) and `kindred eval --als-honest` (retrain minus the
  held-out pairs, then score).
- Gate: **120 checks**, three of them proving the opt-in tune is honoured and that a live
  list carries a `latent` component.

## The measurement, in the order it happened

**First run, factors trained on the whole mirror (WRONG):**

| tune | latent weight | recall@10 | nDCG@10 | coverage |
|---|---|---|---|---|
| default | 0.00 | 0.1063 | 0.1889 | 0.6579 |
| `als08` | 0.08 | 0.1244 | 0.2342 | 0.6842 |
| `als16` | 0.16 | **0.1373** | **0.2702** | 0.6842 |

+29% recall and +43% nDCG — larger than every signal this project has built, from 21
seconds of training. That is not a result, it is a **leak**: the factors had been trained
on every bookmark in the mirror, including the eval's held-out set, so the model had been
told that each reader bookmarked exactly the works the test asks it to find. Recall of a
memorised list is a memory test.

**Second run, `--als-honest` trained on `split.Train` only (also wrong, differently):**

`als-honest: retrained 1951 factors on 1990 training pairs` — the split's 38 evaluated
readers and
their ~50 training bookmarks each. Leak-free, and a test of a model one hundredth the size
of the deployed one. It reported recall 0.1063 / nDCG 0.1869 and would have been written
up as "ALS does not help", which would have been a statement about 1,990 pairs.

**Third run, `--als-honest` minus the held-out pairs (the fair test):**

| tune | latent weight | recall@10 | nDCG@10 | coverage |
|---|---|---|---|---|
| default | 0.00 | 0.1063 | 0.1889 | 0.6579 |
| `als08` | 0.08 | 0.1130 | 0.1861 | 0.6579 |
| `als16` | 0.16 | 0.1071 | 0.1832 | 0.6579 |

`112,703 factors on 180,182 pairs (495 held-out pairs excluded), 20.6 s`.

## What the honest numbers say

At 0.08 the signal buys **+2.5 hits across 380 slots** (0.1063 → 0.1130) and loses 0.0028
nDCG; at 0.16 it is worse than not having it. That is inside the noise this sample can
resolve, and it fails the same bar the walk faced: a difference that cannot clear a bar
does not retune a default. **`latent` stays at weight 0**, reachable through
`--tune als08`.

The interesting part is WHY, and it is a statement about the corpus rather than the model:
**ALS is a lossy re-encoding of the evidence `collab` already holds exactly.** Both read
`user_work_interactions`; collab counts pairs, which is the sufficient statistic for
"readers who bookmarked X also bookmarked Y", and the factorisation compresses those same
pairs into 32 dimensions. With collab already weighted at 0.16, the factors add almost
nothing — and on the live recommendation the latent signal scores 0.4–0.65 while collab
scores 0 on the same items, which is the factorisation *replacing* collab's pair evidence
with its own smoothed version rather than adding to it.

That is a real answer to the row, and it is the reason this unit is worth keeping: the
next learned dimension should be measured against "what does collab already know", not
against zero.

## The two lessons

1. **A signal that looks too good is the one to distrust.** +29% from 21 s of Go was the
   most suspicious number in this project's recent history, and it was a leak.
2. **There are two ways to close a leak, and one of them is amputation.** Excluding the
   held-out pairs from 180k interactions is the same protocol as training on 2k; only one
   of them measures the model you would ship.

## Verification

- `internal/als`: Cholesky against a hand-solved system, co-bookmarked works similar and
  disjoint works not, readership-group recovery, error falling and movement dying,
  determinism (reversed input, identical model), empty input, bounded norms.
- `internal/signal`: best-seed not average, zero-clamp (a cosine of exactly 0 is a score,
  not a skip), skips without a model or a factor row, `MaxSeeds` honoured, loader called
  once per Score.
- A test I wrote and then deleted: "ALS relates works two hops apart" **failed against
  correct code** (cosine 0.000 vs 0.001 on three readers). With binary data an item's
  factor is a function of which readers touched it, so disjoint-reader items are only
  related once the model has learned those readers resemble each other from other
  evidence. The test now asserts readership-group recovery, which the model does promise.
- Live: `?tune=als08` returns `"tune":"als08"` with `"signal":"latent"` at 0.65/0.40/0.54
  on the top three items.
