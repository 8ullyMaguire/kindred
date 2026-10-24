# Handoff 2026-10-24d — #74: the walk, and three measurements that were wrong before they were right

## What shipped

`signal.Walk` — a bounded personalised PageRank over the 634k-node / 7.75M-edge tag
graph, restarted uniformly on the seeds' tags, damping 0.85, power iteration to 1e-6 or
20 rounds — plus a **pool source** (`walkpool.go`) without which it cannot do its job.

Two decisions make it a dimension rather than a second `tag_overlap`:

1. **The restart mass is excluded from the score.** The restart distribution sits on the
   seed tags and every candidate sharing them would collect it equally, so scoring it
   would double `tag_overlap`'s effect. What is scored is the mass the walk found on its
   own.
2. **Weights are normalised per node each step.** A hub distributes its mass rather than
   multiplying it by its degree; without that the total grows every iteration and every
   score inflates with the walk's length while still looking like a ranking.
   `TestMassIsConserved` is the number that says so.

Dead ends return their mass to the restart set; duplicate seed tags are one restart
target; the frontier is bounded at 20k nodes keeping the heaviest.

## The pool source, and the claim that justified it

The signal's candidates came from the tag pool — "works sharing a seed tag" — while the
walk's whole contribution is on tags the seeds do **not** have. A two-hop signal scoring a
one-hop candidate set can only re-rank what `tag_overlap` already found.

I verified that with a comparison of ten-item lists under `tune=no_walk` and the default,
and got "IDENTICAL" on two seeds. **That measurement was wrong**: I read the field
`work_id` out of an API whose items are keyed `id`, so both sides were five `None`s and
the comparison was of two empty lists. Re-run correctly, the lists differ at every weight
tried — the walk was never inert.

So the pool source is justified by the structure rather than by that measurement, and the
commit message that says "changed no list" states something I had not shown. Corrected
here rather than rewritten: a claim that a feature does nothing is as consequential as a
claim that it works.

## The measurement that decided the weight

Four attributed runs (`eval_runs.tune`, added this session because four unattributable
rows of numbers had accumulated), 37 lists, k=10:

| tune | walk weight | recall@10 | nDCG@10 | coverage | tag overlap | fandoms |
|---|---|---|---|---|---|---|
| `no_walk` | 0.00 | 0.1063 | 0.1888 | 0.6579 | 0.099 | 2.2 |
| default | 0.06 | 0.1063 | 0.1889 | 0.6579 | 0.099 | 2.2 |
| `walk12` | 0.12 | 0.1063 | **0.1901** | 0.6579 | 0.099 | 2.3 |
| `walk25` | 0.25 | 0.1004 | 0.1556 | 0.6316 | 0.102 | 2.1 |

Read honestly: at 0.06 and 0.12 the walk **reorders** lists without changing membership,
and the aggregate metrics cannot distinguish either from zero — the 0.0013 nDCG gap
between 0.12 and 0.06 is smaller than anything this sample can resolve. At 0.25 it is
**worse than not having it** (recall −5.5%, nDCG −18%, coverage −4%), which is what an
overweighted two-hop guess does: it outranks the direct match.

The default therefore stays at 0.06, and the reason is the same standard #84 built one
row earlier: a difference that cannot clear a significance bar does not retune a default.
0.12 is not adopted on a 0.0013 nDCG difference over 37 lists.

## Verification

- 28/29 packages green; `internal/stats` and `signal` tests include the walk's shape on
  hand-solvable graphs (decay with distance, mass conservation, symmetric leaves, leaf
  seeds not leaking, frontier bound, duplicate restart tags).
- Engine-level: a component > 0 on a graph-backed fixture (the registered-but-inert
  trap), the signal named in `meta.degraded[]` when there is no graph, and the pool
  source returning works the seed does not reach.
- Deploy gate **108 checks** (was 105), including three that would have caught every
  failure mode here: a live list carries a `walk` component, the `degraded` array exists,
  and `walk` is not inside it (an absence assertion with its subject counted).
- Live: `pool_sources` reports `walk: 152` — the source contributes 152 candidates to a
  1000-work pool for one seed.
- Roadmap: #74 BUILT, **25 built, 6 partial, 35 open, 4 without a premise**.

## The three wrong measurements, because they are the transferable part

1. **`work_id` vs `id`** — compared two lists of `None` and called a difference absent.
   An absence claim needs its comparison to have a subject; ten `None`s comparing equal is
   not evidence of equality.
2. **`denselyConnected` built from the state DB** — tags live in the corpus, so it queried
   no rows and returned a nil graph, which reads as "no graph" rather than "wrong handle".
3. **The fixture that "proved" the walk ran** asserted the signal was not degraded on a
   60-work corpus where every work shares the same few tags — where a correct walk
   legitimately has nothing to score. Replaced with a dense graph over the fixture's real
   tag ids.

## The `eval_runs.tune` column, and the second thing it caught

Added this session because four ablation runs had produced four rows of numbers that
nothing could attribute. Reading the deployed table back immediately found a defect in
the column itself: run #35 — the default tune, certainly — was labelled "(unrecorded)",
because an empty `--tune` flag was stored as `''`, and `''` is also what a run written
before the column looks like.

Two different statements had become one string, in the column that exists to separate
them. The default is now stored as `"default"`; `''` keeps its honest meaning (this run
predates the column) and the earlier rows keep their numbers rather than being relabelled
— inventing provenance is worse than admitting its absence.

Live after the fix:

```
36 | default | 0.1063 | 0.1889
35 |         | 0.1063 | 0.1889   (predates the column)
34 | walk25  | 0.1004 | 0.1556
```
