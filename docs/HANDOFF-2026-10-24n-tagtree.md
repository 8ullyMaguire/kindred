# Handoff — #37: a tag hierarchy from the embeddings, and two things it refused to hide

2026-10-24. `internal/tagtree`, `kindred tagtree --write`, two tables, and a gate check that
asserts a *semantic* property rather than a row count.

## What it builds

From the 32-dimension tag embeddings in the state database (634,232 rows; 14,049 tags survive
`--min-support 25` and `--max-tags 40000`), a two-level tree:

    tag_family          id, parent, label, size      300 families + 1,274 subfamilies
    tag_family_member   tag_id, family, dist         14,049 tags

Parent is what makes it a tree and not a partition. Ids: a family is its own index, a subfamily
is `1_000_000 + family*100 + sub`.

## The evidence that it is not arbitrary

Nothing told the clustering that these belong together:

* `haruno sakura`, `hatake kakashi`, `uzumaki naruto` and `naruto (anime & manga)` land in
  **one family** (18), in three neighbouring subfamilies.
* `worm - fandom` lands in a subfamily whose members are `transhumanism`, `lily | flechette |
  foil` — Worm characters. The gate asserts the first of these on the deployed database.
* Coherence: mean cosine to a member's own family centroid **0.888**, to a neighbouring
  family's **0.246** (gap 0.643).
* Stability: **0.87** adjusted Rand between two builds with different seeds. Without ground
  truth this is the number that distinguishes a property of the data from a property of the
  seed.

## Two findings, both kept visible

**1. Centring is not optional, and the first build proved it.** Uncentred, Lloyd put **11,753 of
14,049 tags in one family** — the embedding space has a huge shared component (the popular
fandoms), and Euclidean distance on raw vectors is mostly distance along it. Mean-subtract, then
L2-normalise: the largest family fell to 3,560 and coherence went from 0.194 (own) against
−0.007 to 0.888 against 0.246. Build copies its input, because re-normalising an already-centred
slice perturbs ties — which made two identical builds disagree.

**2. The largest family still holds 25% of the archive, and the fix is worse than the disease.**
29 subfamilies inside it, so it is usable as an index. But a capacity-limited first level
(`--balance 1.5`) evens sizes to 1% and **halves stability to 0.45**: it is cutting through a
dense region with no gaps in it. So `Balance` defaults to 0, the skew is printed with its
explanation, and the option exists for a caller who wants equal buckets.

The second level IS capacity-limited, and the asymmetry is deliberate: at the top the skew is
structure and cutting through it invents families; inside a family the question is "which corner
of this region" and a division leaving 76% in one subfamily answers nothing. Its labels are the
three biggest member names — a sample a reader can check, not a claim that the tags agree.
Shared-word labels on these buckets read as mush ("star wars potter") precisely because capacity
cuts across subjects; the first attempt at subfamily labels produced exactly that.

## The measurement that refused to be evidence

`families.json` is the only hand-written ground truth, and the report says so out loud: only 3
of its spellings exist as tag names in the mirror ("Marvel" 3 of 3, "Harry Potter" 3 of 3, "Worm"
3 of 4). The tool prints `too few to measure` for anything under five embedded tags rather than
a purity over three, because one tag moving would change the answer by more than 20 points.
Earlier versions of this report printed `0 of 0 universes` as if it were a result.

## Files

    internal/tagtree/tagtree.go          Load, Build, BuildAndMeasure, Purity, Labels, AdjustedRand
    internal/tagtree/tagtree_test.go     planted groups, purity both directions, determinism
    internal/tagtree/stability_test.go   ARI on known partitions; low on noise, high on structure
    cmd/kindred/tagtree.go               the command, the write, the purity report

Runtime: **20 s** for 14k tags including the second seed. Deterministic for a given `--seed`.

## The mutation ledger, including the two that could not be killed

Eight mutants, six killed, two survived — and the survivors are reported rather than papered over:

| mutant | verdict |
| --- | --- |
| purity always 1.0 | killed |
| adjusted Rand always 1.0 | killed |
| label keeps water words | killed |
| capacity check removed | killed (after moving the test to `assignBalanced`, where it lives) |
| no centring in Build | killed (via the inertia ≤ 4n bound, not via a clustering claim) |
| Build mutates its input | killed |
| members not sorted by support | killed (after fixing a fixture whose support order already matched) |
| subfamily `len >= 2` filter removed | **survived** — unreachable while the capacity constraint runs |

Two test lessons from this pass:

1. **A fixture cannot reproduce a property of 14k real vectors.** I tried three times to build a
   synthetic family where unconstrained k-means collapses the way the live archive does
   (2,711 of 3,560 in one bucket), and each attempt either split normally or produced the
   OPPOSITE of the live result (centred 40% against uncentred 28%). The mechanism is tested
   where it lives (`assignBalanced`, directly) and the phenomenon is reported as a measurement.
   Fitting a fixture until the assertion passes would have been fitting the test to the answer.
2. **A determinism test cannot see a sort.** Two builds with the same seed sort identically
   whether or not the sort exists, so the ordering needed its own assertion — and the first
   fixture's support order already matched the input order, making the sort a no-op.

## Not done

No consumer. The obvious one is tag-gap (`internal/corpusquery/taggap.go`): its 30-neighbour
limit is a co-occurrence neighbourhood, and a family could widen it. That is a ranking change and
wants an ablation, not a wish. The hierarchy is also unbuilt for tags below `--min-support 25`
(about 600k spellings), which is deliberate: those tags are met rarely enough that a family adds
nothing a reader can use.
