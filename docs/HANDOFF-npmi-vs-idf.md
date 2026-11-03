# Handoff: nPMI against IDF for tag similarity — measured, and it loses

**Verdict: keep IDF. nPMI is worse at the share where it was tested, in both
positions, with intervals that exclude zero.** The new code ships inert: `tag_npmi`
is registered at weight `0.0`, so the served ranking is byte-identical to the
previous build until a future measurement earns it a weight.

Question asked: "we use IDF for tag similarity but I think we should use something
like nPMI — thoughts?" The answer is now a number rather than an opinion.

## What shipped

| commit | content |
|---|---|
| `39f8775` | `internal/graph/npmi.go` — nPMI beside `PMI`, bounded in [−1,1] by construction |
| — | `internal/signal/signal.go` — `Neighbourhood.Metric`, so the biggest signal can vote by nPMI with no saturation constant, and the vote cache is keyed by metric |
| `90d614d` | `internal/signal/tag_npmi.go` — hybrid: `max(idf norm if shared, best nPMI to any seed tag)`, every term in [0,1] |
| `b587141` | `internal/engine/engine.go` — registered at weight **0.0**, with its `tune --set` key so it can be weighted by measurement alone |
| `6b3ff5c` | `--neighbour-metric pmi\|npmi` on `eval` |
| `dcfb8cf` | `--dump-users` + `kindred evalcmp`: per-reader dump, paired percentile bootstrap, and a printed **NO VERDICT** |
| `47e1571` | `docs/IDEAS-100-improvements.md`, `scripts/eval-arms.py` |

The instrument came first on purpose: the stored run held per-position aggregates
only, so "0.1063 vs 0.1071" was untestable and at cohort sizes this evaluator can
afford it was not a difference at all.

## The measurement

80 readers, first 78 evaluable, one split, `k=10`, same corpus, same graph, same
embeddings; only the tune differs. Every run recorded `degraded = all`, meaning no
signal skipped for any candidate — the nPMI signal ran.

| run | arm | recall@10 | NDCG@10 | tag overlap | fandoms |
|---|---|---|---|---|---|
| 53 | `np_base` (default rescaled to 1.0) | **0.1188** | **0.2033** | 0.096 | 2.43 |
| 57 | `np_no_idf` (IDF removed) | **0.1193** | 0.1975 | 0.099 | 2.35 |
| 54 | `np_swap12` (IDF out, nPMI in at 0.12) | 0.0971 | 0.1685 | 0.104 | 2.19 |
| 56 | `np_npmi_12` (nPMI added at 0.12, IDF kept) | 0.1035 | 0.1779 | 0.101 | 2.19 |
| 55 | `neighbour_npmi` (bounded neighbourhood metric) | 0.1120 | 0.1948 | 0.093 | 2.47 |

Paired percentile bootstrap, 2000 resamples, seed 12345, **n = 78 readers**:

| comparison | recall Δ [95% CI] | NDCG Δ [95% CI] | verdict |
|---|---|---|---|
| base → no_idf | +0.0005 [−0.0021, +0.0035] | −0.0058 [−0.0116, −0.0008] | recall **no verdict**, NDCG **worse** |
| base → swap12 | −0.0217 [−0.0315, −0.0123] | −0.0348 [−0.0491, −0.0208] | **WORSE** both |
| **no_idf → swap12** | **−0.0222 [−0.0321, −0.0128]** | **−0.0290 [−0.0416, −0.0167]** | **WORSE** both |
| no_idf → npmi_12 | −0.0158 [−0.0237, −0.0086] | −0.0196 [−0.0295, −0.0104] | **WORSE** both |
| base → neighbour_npmi | −0.0068 [−0.0154, +0.0010] | −0.0085 [−0.0213, +0.0042] | **no verdict** |

The third row is the clean one. `no_idf → swap12` adds `tag_npmi` at 0.12 and changes
nothing else, so the loss is nPMI's and not the removal of IDF's: **−18.6% relative
recall** on the same 78 readers.

## Why it loses (mechanism, not vibes)

Two properties of this corpus punish an association measure and forgive a rarity
measure:

1. **77.5% of tags are singletons.** 491,414 of 634,188 freeform tags sit on exactly
   one work. Under `log(N/df)` they take the corpus maximum, so they dominate any tag
   vector; nPMI then rewards their coincidences at full strength because a rare pair
   that occurs twice has a very high nPMI.
2. **71.8% of edges are single-work coincidences.** Of 7,750,334 co-occurrence edges,
   5,564,829 have `count = 1`. nPMI ratings on `count = 1` pairs are high by
   construction — the `npmiMinCount = 2.0` floor stops the worst of it, but the
   surviving edges are still dominated by "these two rare tags appeared together once
   in a small fandom", which is not a preference.

IDF is monotone in rarity alone and only speaks about tags the two works *share*, so
it cannot manufacture an association. That is why removing it costs nothing on recall
(+0.0005, no verdict) while adding nPMI costs 1.9 points of recall: the IDF term was
nearly free, and the nPMI term is actively negative.

The bounded neighbourhood metric is the interesting survivor: −0.0068 recall with a CI
touching zero. It removes the hand-calibrated `NeighbourSaturation = 100` without
measurably changing quality, so it is a **maintainability win, not a quality win**, and
it did not earn adoption under the pre-registered rule (must not lose NDCG; a NO
VERDICT is not a win).

## Decision, applied as pre-registered

The rule fixed before the run: nPMI must win recall at both non-zero doses with
intervals excluding zero and not lose NDCG; otherwise IDF stays.

- nPMI lost at both placements → **IDF stays**.
- `tag_npmi` stays registered at **0.0**; `--neighbour-metric` stays as a flag. Neither
  changes a served list.
- The bounded neighbourhood metric is *not* adopted. Its value is that the constant is
  deletable if a future corpus makes the association meaningful.

## What was not run, and why

- `np_npmi_06` and `np_npmi_24`: two placements at share 0.12 both lost by wide
  intervals. The question was settled; a dose-response curve on a signal that hurts
  buys nothing.
- The 295-reader matrix: **the evaluator cannot afford it.** At 295 readers a single
  arm exceeded 38 minutes and was still running; `/proc/PID/io` showed 1.57 TB read and
  384M read syscalls in that window, i.e. syscall-bound, not CPU-bound. At 80 readers
  an arm takes 5 minutes. Any campaign larger than ~100 readers needs the pool query
  fixed first (ideas 28 and 29 in `docs/IDEAS-100-improvements.md`: cache the pool per
  seed-tag set, materialise works-per-tag).

## Next

1. **Idea 29 (materialise works-per-tag) is now the highest-value row in the backlog**:
   it is what makes a 295-reader campaign possible, and every future measurement is
   capped by it.
2. If nPMI is tried again, prune the adjacency by nPMI (idea 6) rather than adding it
   as a scoring term — the failure above is a scoring failure, not a proof that the
   association is useless for *selecting* edges.
3. Cap IDF for singleton tags (idea 5) before judging IDF further: the +0.0005 recall
   for removing it says the term is currently near-inert, not that it is well shaped.
