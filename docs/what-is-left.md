# kindred: what is left

Status as of 2026-10-07. Every line below was verified by running it, not by
reading the code. A line marked DONE names the test or the tool output that
established it.

## Done and verified

- [x] **collab package.** `internal/collab` builds an item-item co-bookmark
      index from `user_work_interactions`. Verified against the live 1.7 GB
      mirror with `cmd/kcollabverify`: gate chosen adaptively, **12,812 pairs
      over 9,365 works, 23 MB RSS, 21 s build**.
- [x] **Adaptive evidence gate.** `ChooseGate` measures the co-bookmarker
      distribution and picks gate 2 when >=1000 pairs survive at co>=2, else
      gate 1. A hard gate of 3 leaves 511 pairs / 583 works and cannot rank.
- [x] **collab wired into ranking.** `poolFor` resolves collab votes before
      the tagless early return and UNIONs them with the tag pool; `signals()`
      takes the votes instead of recomputing them. `Engine.Collab` is built at
      serve startup and degrades to "signal skips, names itself in
      meta.degraded[]" on failure.
- [x] **PoolMode over HTTP.** `?pool_mode=tags|tags+collab` on both surfaces. An
      unknown value is a 400 that NAMES the parameter.
- [x] **Filters over HTTP.** `min_words`, `max_words`, `min_kudos`,
      `complete`, `rating`, `lang` on both surfaces, parsed by one shared
      `engine.ParseFilter` so the two layers cannot drift. Every unparseable
      value is a 400; a reversed range is refused by name.
- [x] **Tune selection, resolved for real.** `?tune=` was **accepted, echoed
      into metadata, and completely inert** — `signals()` unconditionally
      built `DefaultTune()`. Now `resolveTune` loads the stored weights,
      `WithOverrides` applies them, an unknown name is a 400 naming the
      alternatives, and a corrupt weight blob is an error rather than a
      silent fallback to defaults. The `tunes` table had no reader or writer
      at all; `Store.Tune`/`TuneNames`/`SaveTune` are new.
- [x] **Blocks remove results.** `blockSQL` applies inside the pool query AND
      inside the collab half's query. Previously blocking a tag had no effect
      on ranking whatsoever.
- [x] **Blocking is reachable at all.** `POST /block` existed and `/block`
      listed what was blocked, but **no page anywhere rendered the button** —
      blocking could not be performed from the site. The tag page now carries
      it, with the reverse action and an explanation.
- [x] **Every ranking control is on the page.** The filters, pool mode, pool
      size, tune and result count were all URL-only. `/recommend` now renders
      them, echoing the submitted request. `n` in particular was missing, so
      touching any control silently reset the result count.
- [x] **Pool size and `n` from the page.** `?pool=` bounded 0-5000.
- [x] **Degraded signals are shown on `/recommend`.** A missing signal changes
      the ranking, and a reader cannot see that from the list.

Gates: `gofmt`/`vet`/`build` clean, 22 packages green, **96 Playwright tests
pass**, `docs/goal-check.py` reports 15 parity features (was 9) and all 7
clauses pass.

## Verified non-vacuously

Every claim below was checked by reverting the behaviour and confirming a
test or clause fails. Three tests in this table were written only after a
mutation went undetected — the union test in particular: "union, not
replacement" was argued at length in a comment and nothing tested it.

| Decision | Mutation | Caught by |
|---|---|---|
| block predicate is in the pool query | `_ = blockSQL` | 2 Go tests, 1 browser test, goal-check clause |
| tag pool and collab votes are UNIONed | `out = collabCandidates(...)` | `TestTagPoolAndCollabVotesAreUnionedNotReplaced` |
| tagless seeds still reach collab | restore the early return | `TestACollabOnlySeedStillRecommends` |
| blocks reach the collab half | drop `blockSQL` there | `TestBlocksApplyToTheCollabHalf` |
| a named tune changes the weights | ignore the stored weights | 4 Go tests, goal-check clause |
| an unknown tune is an error | fall back to defaults | 2 Go tests |
| `blockSQL` is deterministic | shuffle the map | `TestBlockSQLIsStable` |
| `blockSQL` no-ops on an empty set | always emit SQL | `TestBlockingNothingAddsNoSQL` |

## Corrected measurements (2026-10-07, live mirror)

`COUNT(DISTINCT user_id)` per co-bookmark pair:

```
co=1  3,383,627      co=4     52
co=2     12,301      co=5     12   (7=2, 9=1)
co=3        438      total    3,396,439
```

An earlier figure of **129,830 pairs at co>=3 was wrong by 254x** — it came
from `COUNT(*)` on a table that stores duplicate rows per user. The real
count at co>=3 is **511**.

Mirror sparsity, also measured: 28,972 of 112,935 works have >=2 distinct
bookmarkers; 39 have a non-zero `works.bookmarks`; 6,261 users total. Per-seed
collab neighbourhoods are therefore 1-4 works, which is why the union with
the tag pool is load-bearing rather than belt-and-braces.

## Still open

- [ ] **Entity shortlists (authors, characters, series, collections).** The
      sibling tool derives authors by parsing `works.authors` text, identifies
      fandoms by name shape, and reports series/collections as absent unless
      harvested. None of that exists in kindred yet. Characters need pairing
      attestation — three candidate filters are known to fail.
- [ ] **Seen-work history.** No seen/exclusion store. `/recommend` cannot
      suppress a work the reader has already read.
- [ ] **JSON routes for corpus-query modes.** Page and CLI can run them; the
      JSON API cannot.
- [ ] **Playwright e2e.** The browser suite covers the 16 existing routes.
      It does not yet cover the filters, pool mode, tune selection or blocks,
      all of which are new form controls.
- [ ] **`docs/goal-check.py` clauses** for the four above, plus the parity
      count, currently frozen at 9.

## Standing caveat

`complete=true` means COMPLETE ONLY, not "no filter". It reads like a vacuous
truth in isolation, but a tri-state control whose boolean spellings collapse
to the same request is indefensible. Pinned by
`TestParseFilterAcceptsTheSpellingsAFormSends`.