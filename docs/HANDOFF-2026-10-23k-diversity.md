# 2026-10-23k — Diversity is measured now, and the trade it was supposed to be is not one

Row #79 said: "`eval_runs` stores recall/NDCG only, so intra-list diversity is not
tracked over time." It is the other half of yesterday's MMR work: the pass exists
to stop a reader receiving twelve variants of one fic, and nothing measured
whether it did that or what it cost.

## What was built

Every `eval_runs` row now records:

| column | meaning |
|---|---|
| `tag_overlap` | mean pairwise Jaccard inside each reader's returned list; 0 = no shared tags, 1 = one work repeated |
| `distinct_fandoms` | mean number of distinct fandom groups per list, using the same shape-based grouper the per-group cap uses |
| `diversity_runs` | how many readers the two were averaged over |

Averaged per reader, like recall, and bounded by the same `k`. `kindred eval
--lambda 1` turns the MMR pass off, which is what gives the numbers something to
be compared against; `/eval` grew two columns and a legend saying what they cost.

**"Not measured" is not "zero overlap".** `eval.Recommender` returns ids, which
carry no tags, so the evaluator grew an optional `TagSource` interface rather than
importing the engine. A recommender that cannot supply tags reports
`DiversitySkipped` with the reason, the CLI prints it as its own sentence, and the
page prints *not measured* instead of `0.000` — because zero overlap is a good
result and the two must not render the same way. `diversity_runs` is what makes
that survive the database: a legacy row reads 0 and therefore reads as unmeasured.

## What it measured (live mirror, 40 readers, k=10)

| lambda | recall@10 | NDCG@10 | tag overlap | fandoms per list |
|---|---|---|---|---|
| 0.75 (default) | 0.1050 | 0.1959 | 0.086 | 2.4 |
| 1.0 (diversification off) | 0.0953 | 0.1766 | 0.141 | 1.8 |

**The result contradicts the assumption the design was built on.** Diversifying
was supposed to spend recall to buy variety. Turning it off made recall and NDCG
*worse* (by 9% and 10% relative) while making the lists more repetitive (overlap
+64%, fandoms −25%). At lambda 0.75 the MMR pass is a straight win on both axes,
because the top of a relevance-only ranking fills with near-duplicates that push
held-out works out of the ten.

That is worth knowing for three reasons: the default is now empirically supported
rather than merely chosen; a future "MMR costs us recall" argument has a
counter-example to answer; and it means the next diversity question is whether
0.75 is even the right point, not whether diversification is worth having.

## Tests and the mutants

`internal/eval/diversity.go` computes one list's numbers; `diversity_test.go`
pins the arithmetic and `diversity_run_test.go` pins the wiring (averaged per
reader, `k` shared with recall, single-item lists excluded, unmeasured reported as
unmeasured). `internal/store/eval_diversity_test.go` pins the round trip and the
migration path over a table that predates the columns.

Mutations killed, 8/8:

- the duplicate work counted as a pair — which would have scored a list that
  repeats a work as maximally DIVERSE, the exact inversion of the metric's purpose;
- overlap as the complement of the mean;
- `k` ignored;
- untagged works compared as empty sets;
- a failed tag lookup swallowed into 0.0;
- `Run` measuring an empty list instead of the one it returned;
- a single-item list counted as measured;
- the store dropping `diversity_runs`.

## A total that was a memory rather than a measurement

While updating the roadmap I found its own totals block had said "38 built, 9
partial, 49 open" for two sessions. This roadmap holds **rows 31–100, i.e. 70**, so
those numbers were consistent with 100 rows and wrong by about thirty — the backlog
read as half finished when it is a third done. Corrected to **21 built, 8 partial,
37 open, 4 without a premise**, and the line now says it is derived from the table.
Four of the shipped rows (#54, #66, #79 and the status of #50) had also been left
stale in the table.

## State

27/27 packages; deployed; provenance IN SYNC; behaviour gate **97 checks** (three
for the eval diversity columns, including the legend that names the trade). Live
runs #16 (default) and #17 (`--lambda 1`) are in `eval_runs` and render on `/eval`.
