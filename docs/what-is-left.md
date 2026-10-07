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
- [x] **JSON routes for corpus-query modes.** `GET /api/v1/corpus-query/{mode}`
      covers all four working modes with `limit`, `profile`, `min_co_works`
      (signed), `min_tags` and `seed_tags`. A mode that is *declared but not
      implemented* answers **501** and names the working ones — an empty row
      set there would claim the corpus contains nothing.
- [x] **Pool budget is split so collab actually gets slots.** Found by running
      the live mirror, not by tests: `pool_mode=tags` and `tags+collab`
      returned **byte-identical** lists. The tag pool was capped at the FULL
      budget, so it filled every slot and the union's
      `room := limit - len(out)` was always 0. `collabReserve` now holds back
      an eighth of the pool for the collab half.
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
| the pool budget is split | tag half capped at `limit` | `TestThePoolIsSplitByBudgetNotByLuck` |
| corpus-query dispatch is per-mode | return a fixed mode | 2 API tests |
| unimplemented modes answer 501 | answer 200 | `TestADeclaredButUnimplementedModeAnswers501` |

## Three bugs found only by running the real thing

1. **The pool union was a no-op.** `pool_mode=tags` and `tags+collab`
   returned byte-identical results on the live mirror. Every unit test passed,
   because the fixture had fewer tag-reachable works than the pool under test,
   so the reserve could not matter. Fixed by `collabReserve`; the test now
   asserts the cap rather than the resulting ids, and it fails on the old
   code with a message naming the defect.
2. **The block control did not exist anywhere.** `POST /block` worked and
   `/block` listed blocked tags, but no page rendered the button. Found by
   writing a browser test that drove the flow a reader would.
3. **`?tune=` was inert.** Accepted, echoed into `meta.Tune`, and never
   applied — `signals()` always built `DefaultTune()`.

## Live verification (thinkcentre, real 112,935-work mirror)

Deployed to `/usr/local/bin/kindred` on :8010 and restarted the unit;
previous binary kept at `kindred.prev-20261007`.

```
index loaded     nodes=634232 edges=7750334
collab index     gate=2 pairs=12812 works=9365 users=6261
healthz          200
min_words=60000  -> 217753, 144151, 124220 words only
complete=in-progress -> complete:0 only
pool_mode tags    [11623035, 11648067, 53007217, 83275826, 21109976, 35705380]
pool_mode collab  [11623035, 11648067,           83275826, 21109976, 35705380, 67165009]
```

The collab works are reachable but do not win: `collab` carries 0.18 of the
tune against `neighbourhood` 0.33 and `tag_overlap` 0.24, and this seed's
measured neighbourhood is 1-4 works. That is the tune behaving as designed on
thin evidence, not a defect — raising `collab` is what `?tune=` is for.

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

- [x] **Seen-work history.** `seen_works` records both what a reader was shown
      and what they marked read, as SEPARATE facts. Read works are excluded
      unconditionally; shown works expire after 7 days, and `shown_at` is
      REFRESHED on every sighting so a work a reader keeps being shown never
      silently ages out of the window. The exclusion is applied in the POOL
      query, not after ranking — otherwise asking for 20 with 15 seen returns
      5, and a reader's list visibly shrinks every time they mark something
      read. Reachable from the page: `hide_seen` control on `/recommend` plus
      an "I have read this" button per result (POST `/seen`).
- [ ] **Entity shortlists (authors, characters, series, collections).** The
      sibling tool derives authors by parsing `works.authors` text, identifies
      fandoms by name shape, and reports series/collections as absent unless
      harvested. None of that exists in kindred yet. Characters need pairing
      attestation — three candidate filters are known to fail.
- [ ] **Seen-work history.** No seen/exclusion store. `/recommend` cannot
      suppress a work the reader has already read.
- [ ] **`similar` corpus-query mode.** Declared in `corpusquery` with an
      honest "NOT IMPLEMENTED" comment and a 501 from the API. It is the
      recommender's core question, but `/recommend` already answers it, so
      this is duplication unless a distinct contract is defined.
- [ ] **`fandom-landscape`.** Same status: declared, 501, unimplemented.

## Standing caveat

`complete=true` means COMPLETE ONLY, not "no filter". It reads like a vacuous
truth in isolation, but a tri-state control whose boolean spellings collapse
to the same request is indefensible. Pinned by
`TestParseFilterAcceptsTheSpellingsAFormSends`.