# Handoff 2026-10-24l — #52: the first signal that reads prose, and the column nobody selected

## What shipped

`summary_style` — cosine over IDF-weighted words shared between a candidate's summary and the
seeds' — at **a share of 0.1** in the default tune.

- `corpus.BuildSummaryDocFreq` (`kindred ingest --summary-only`): document frequency over
  `works.summary`, banded to **df ∈ [3, 20% of documents]**. Measured on the live mirror:
  112,136 summaries → **39,046 terms, 625 KB, 1.6 s**. Sorted vocabulary, so two ingests are
  byte-identical.
- `signal.SummaryStyle`: seeds **unioned** (three summaries of different fandoms must not be
  averaged into a midpoint that resembles none), candidate vector a **set** (repetition is a
  writer's habit, not agreement), evidence naming the words that did the work.
- 112k summaries are never held in memory: the table is a map of 39k term-hashes, and the
  summaries themselves arrive with the candidates, which already carried a summary field that
  nothing had ever filled.

## The ablation (deterministic, 295-reader holdout, k=10)

| weight share | recall@10 | nDCG@10 | coverage |
|---|---|---|---|
| 0.00 (previous) | 0.1196 | 0.1570 | 0.5220 |
| **0.10** | **0.1219** | **0.1586** | **0.5322** |
| 0.20 | 0.1215 | 0.1581 | 0.5254 |

All three metrics rise at both non-zero points and the middle is the peak — a dose-response,
not a single lucky configuration. The eval split is seeded by user id, so repeat runs of one
configuration are byte-identical: the difference between rows is the tune, not the sampler.

**The honest caveat:** +0.0023 recall is 5 hits out of 2,317 held-out bookmarks. It is a real
difference *on this holdout*, and its direction agrees across three metrics and two weights,
but it is not a *significance* result — the evaluator stores per-position aggregates
(`eval_positions`), not per-reader outcomes, so a paired test is not computable from what is
recorded. Storing per-reader hits is the next improvement to the evaluator, and until then no
ablation in this project can claim more than "the metrics moved together".

`0.1911` in `DefaultTune` **is a share of 0.1**: the defaults sum to 1.72, and what decides a
ranking is each weight over the total (0.1911 / 1.9111 = 0.1). The stored tune
`with_summary` (share 0.1, sum 1.0) reproduces it exactly — the default run and the tuned run
return the same three numbers, which is the check that the weight change is live.

## The bug: a registered signal that was inert on every candidate

`summary_style` scored **nothing for anything**, and the first evidence was not an error but
two byte-identical metric triples: the default tune and a tune that differs by a tenth of the
total weight produced `0.1196 / 0.1570 / 0.5220` twice. That is not a plausible coincidence —
with one arm dead, rescaling the others changes nothing, because ranking depends on ratios.

Cause: `corpus.EntitiesByIDs` — the candidate pool query — selected
`id, url, title, word_count, hits, kudos, bookmarks, language, complete, update_date,
first_seen` and **not `summary`**. Every candidate carried an empty summary, so the signal
returned `rank.ErrSkip` for all of them, on every request, for as long as the row existed.

Three things now stop it recurring:

- the pool query selects the column and `Entity.Summary` is set;
- the existing single-vs-pool test (`TestEntityUsesTheSamePathAsThePool`) compares the summary
  too — it compared every stat and **no prose field**, which is exactly how a field added to
  one path and not the other survives review;
- the deploy gate asserts the signal's evidence appears in the **default** list and names the
  words, which can only happen if the column travels.

**A second, identical trap in the same session:** the first re-run of the ablation used a
binary built *before* the fix, and returned the same identical numbers again. The binary that
measures must be rebuilt after the fix — the artefact, not the source, is what ran.

## Mutation round

Seven mutants; five killed on the first attempt:

| mutant | verdict |
|---|---|
| tokeniser keeps apostrophes (`dragon's` as one token) | KILLED |
| df ceiling removed (boilerplate stays) | KILLED |
| df floor removed (hapax stays) | KILLED |
| vocabulary unsorted | **SURVIVED** → then KILLED |
| absent term returns weight 1 instead of 0 | KILLED |
| cosine not normalised by the candidate | **SURVIVED** → then KILLED |
| seeds not unioned (`MaxSeeds` effectively 1) | KILLED |

Both survivors were **weak tests**, the same shape as the two in #35:

1. **The determinism test compared two empty tables.** Its fixture had 8 identical summaries,
   so the 20% ceiling was 1.6 documents, nothing survived the band, and two empty files are
   trivially identical. Now 40 documents with four terms on 8 each, plus an assertion that the
   fixture produced at least two terms — "determinism over an empty table proves nothing".
2. **The repetition test cannot see the normalisation.** The candidate vector is a *set*, so
   `dragon dragon dragon dragon` and `dragon` are the same vector by construction; the norm
   only matters across candidates. The new test pads a summary with measured words the seed
   does not carry: cosine prefers the precise one, a raw dot product scores them equal.

## Cost

- Ingest: 1.6 s, 625 KB beside the corpus, no service memory.
- Request: the candidate query returns summaries (~170 KB for a 400-work pool); measured
  latency unchanged in the gate's response times.
- Signal: 39k map lookups per candidate token set, no allocation of the corpus text.

## Next

**#47 (work cluster labels)** is now unblocked in principle — there is a term table and a
per-work vector can be built from it — but clustering needs per-work vectors *stored*, which
is the same memory question the ALS factors answered with a table in the state DB.

Remaining OPEN rows: #36 (taste vector export), #37 (tag hierarchy from embedding clusters),
#44/#80 (seasonal), #47 (cluster labels), #48 (reader velocity). **#48** is now the cheapest
and the most measurable: `user_work_interactions.timestamp` exists, the session model already
tracks three instants per reader, and a velocity signal is testable against the same holdout.
