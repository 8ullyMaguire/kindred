# Handoff 2026-10-24e — #39: the tag page's mean was a claim it could not support

## What the page said

The tag shelf printed three numbers for a tag's works — median kudos, mean kudos, and
the work count. On a tag with three works, that mean is not an estimate of anything: one
hit and two nobody read gives a number that reads as "this tag's works are loved", and
the page had no way to say how much of it was the tag and how much was one Thursday.

The median was already there and is robust against exactly that. But a median over three
works is the middle of three; robust and uninformative are not opposites.

## What it says now

A fourth number, shrunk toward the corpus's own mean:

```
shrunk = (Σ kudos + m·prior) / (n + m),   m = 10
```

with `data-prior` and `data-weight` on the element. A shrunk value whose prior is hidden
cannot be checked or argued with — it is an opinion with a decimal point.

Two decisions, both about what the prior IS:

- **Works with zero kudos are excluded from the prior.** This mirror holds 112,937 works
  and most carry none, so the all-in mean sits near zero and every tag would be shrunk
  toward "nobody liked this". The prior is meant to answer "what is a kudos worth", not
  "how do the archive's readers behave".
- **The prior is cached per process, computed on a detached context.** It is a property
  of the mirror, not of a request, so re-reading it per page view is the cost the cache
  exists to avoid; and a reader who closes a tab must not decide what the process knows
  about its own corpus — the same rule the embedding index learned earlier this week,
  after a cancelled health check poisoned it for the life of the process.

## The two ends of the curve, which is where shrinkage implementations break

- **n = 0 returns the prior exactly**, not 0. Every shrinkage implementation has this
  branch, and the wrong branch returns a number that reads as "this tag's works are
  terrible" rather than "nothing is known".
- **Ten thousand works land within the prior's last pseudo-work's worth** — 0.88 below a
  mean of 900, not at it. My first test demanded within 0.1 and **failed against correct
  code**: `10·880/10010 = 0.879`. A test that demanded 0.1 would have been asserting
  arithmetic the code does not implement, and the "fix" would have been to stop shrinking
  at some threshold — which is a different, worse formula.
- **Symmetry is tested**: a tag worse than the corpus is pulled UP. A one-sided version
  is a popularity boost in disguise.

## What it is not

It is not a ranking input. The `quality` signal already measures a work's own kudos, and a
second tag-level quality dimension competing for weight in the same blend is a change that
should arrive with a measurement, not with a row. `#39` names a per-tag Bayesian average;
that is what shipped, at the surface that was already making the claim.

## Verification

- `internal/corpus/prior_test.go`: the prior's exclusion and its caching (a second read
  after mutating the table returns the first value, which is what "cached" means), the
  shelf carrying prior + weight + a value between the tag's own mean and the prior, and
  the curve's ends on hand-checkable numbers.
- Deploy gate: three checks on the live `/tag/1` — the shrunk value, the prior, the
  weight. An element with a hidden prior would pass none of them.
- Roadmap: `#39` BUILT; **26 built, 6 partial, 34 open, 4 without a premise**.
