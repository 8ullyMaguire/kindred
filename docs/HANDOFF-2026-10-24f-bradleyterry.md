# Handoff 2026-10-24f — #53: the leaderboard cross-checked against a second model

## What the row asked and what the page does now

`#53 Bradley-Terry vs Glicko`: "Glicko-2 only; no BT fit and no rank-correlation
comparison." The leaderboard now fits a Bradley-Terry model on the **same judged
comparisons**, with the **same agreement weights**, and reports one number: the Spearman
correlation between BT strength and the Glicko-2 effective rating, over the works both
models rank — plus the counts it was computed from.

Not a second table. A second leaderboard beside the first invites a reader to pick the
column they prefer; the question worth answering is whether the ORDER survives the choice
of model, and that is a correlation with an n.

## The fit

MM iteration (Hunter 2004), the standard fixed point for this model:

```
p_i ← W_i / Σ_j n_ij/(p_i + p_j)
```

normalised to geometric mean 1 after every sweep — BT identifies only ratios, so the
scale is free, and an unnormalised sweep oscillates between magnitudes instead of
converging rather than settling.

**The Jeffreys prior is the load-bearing decision.** The MLE of an item that won every
match is infinite, and a work compared once and preferred is the commonest row in a young
arena's table. So each item gets half a win and half a loss against an average-strength
opponent (strength 1 after normalisation): a draw against the field, which moves no
ordering and keeps every strength finite.

## The test that was wrong, and what it taught

I wrote `TestRepeatingEveryDuelChangesNothing` — a fit that moves when the same data is
duplicated is measuring row counts, not results. **It failed**, by up to 16% on the
weakest item, and the failure was correct behaviour: with a fixed prior, doubling the data
legitimately halves the prior's relative weight.

The fix was not to loosen the tolerance but to split the property into the two true ones:

- **Plain MLE (prior = 0) is exactly scale-invariant** — duplication changes nothing at
  1e-9. This pins the MM fixed point itself.
- **With the prior, duplication sharpens** — the winner's strength rises, the loser's
  falls, and the fit moves away from the prior toward the record. An implementation whose
  prior does NOT shrink with the data would pass the invariance test and be wrong: it
  would let one comparison outweigh a thousand.

The prior's weight became a parameter (`FitBTWithPrior`) for exactly this reason, and the
plain fit's domain limits are documented rather than hidden: with no prior, an item that
never won has strength 0 and one that never lost has +Inf, so `prior = 0` is only defined
on a table where both degeneracies are absent.

## Other properties asserted (not by re-deriving the formula)

- Symmetry: two items that each won once get equal strength, to 1e-9.
- Transitivity: a chain orders itself.
- Perfect record stays finite (`< 1e6`, no Inf/NaN) and above the item it beat.
- A table of draws lands exactly at the prior (1), not at whatever the half-weight
  happens to produce.
- `DuelsFromOutcomes` states each outcome from the item's own side: a LOSS for 7 is a win
  for 9, which is what makes the fit possible; self-duels and negative weights are
  dropped rather than inverted.
- `BTDuels` reads one kind only (the arena's id spaces alias, so a fandom 7 and a work 7
  are different entities) and weights each duel by the comparison's agreement — the same
  weight the Glicko batch uses, or a disagreement between the models would be
  unattributable.

## The honest qualifier

The live arena holds **16 judged comparisons over 15 rated works**. A correlation over
that is a number about two priors agreeing, not about the works — so the page prints, when
`comparisons < 3 × items`, "**Read this as unmeasured**", in the same voice as the
significance test's power floor. `data-bt-thin` marks it for the gate, and the element
also carries `data-bt-converged` so a fit that hit its iteration cap reports itself as a
failed check rather than a close one.

## Verification

- `internal/arena/bradleyterry_test.go`: the properties above, on hand-solvable tables.
- `internal/store/arena_kinds_test.go`: the kind filter and the agreement weight, with a
  fandom row and a work row carrying the same ids.
- Deploy gate **114 checks** (was 111), three of them on `/leaderboard`: the cross-check
  element, the correlation, and the comparison count.
- Roadmap: `#53` BUILT; **27 built, 6 partial, 33 open, 4 without a premise**.
