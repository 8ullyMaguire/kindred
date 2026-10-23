# kindred: what is left

Status as of 2026-10-07. Every line below was verified by running it, not by
reading the code. A line marked DONE names the test or the tool output that
established it.

## Tier 2 + the memory cap (2026-10-22, commit 9733c57)

The owner removed the SPEC §6 memory cap and asked for the ideas worth having
from the hundred-idea recommendation list. `docs/PLAN-recsurfaces-tier2.md`
carries the audit (most of the list was already built; every "built" row names
its code) and the two units that followed from it.

- [x] **The 220 MiB cap is gone.** `budget.CapKiB` and `budget.Check` deleted;
      `/healthz` always answers 200 and reports `peak_rss_kib`, `heap_mib`,
      `sys_mib`, `goroutines`; `serve` logs the same figures every five
      minutes. SPEC §6 rewritten around what to do when the number grows.
      Live: a process using 264 MiB answers `{"status":"ok"}` where it used to
      answer 503 and fail the deploy gate.
- [x] **Interleaved ranking with attribution.** `rank.TeamDraft` (three tested
      invariants), `interleave_impressions`, `GET /api/v1/interleave`,
      `GET /api/v1/interleave/standings`, `/algo?mode=interleave`. A read mark
      credits the arm that showed the work; only a read credits; one impression
      earns one credit; one sighting per work per owner per hour; 30-day
      retention. Live: 6 slots, 3/3 exposure, a read mark credited `default`
      (3 slots / 1 read / 0.333) and not `quality` (3 / 0 / 0.000).
- [x] **`/eval` dashboard.** `eval_runs` persists every `kindred eval` run with
      `k` and `mode` as columns; the page shows the offline numbers beside the
      algorithm votes and the interleaved credits, plus the corpus measured
      across. Live run #1: recall@10 0.0067, NDCG@10 0.0096, coverage 0.0789
      over 38 readers — low, and reported as measured.
- [x] **Two alternate tunes exist on the deployment** (`quality`, `fresh`).
      Without a second algorithm the comparison surfaces can only compare
      `default` with itself, which they refuse by name. Created with
      `kindred tune --name ... --set ...` (weights must sum to 1.0).

Verified: `go test ./...` 26 packages; playwright 131 passed; goal-check
COMPLETE 7/7; provenance IN SYNC at `9733c57`; live checks above. The merge
and the sighting-dedupe are mutation-checked (each rule removed turns its own
named test red and leaves its neighbours green).

## Tier 1 recommendation surfaces (2026-10-22, S1 closed 2026-10-22)

The plan and spec are `docs/PLAN-recsurfaces-tier1.md` and
`docs/SPEC-recsurfaces-tier1.md`; the status table there is authoritative.
All eleven surfaces are live: IDF-weighted tag overlap (`idf_overlap`), the
reader's learned taste in ranking (`session_taste`), co-bookmarked works,
cross-fandom bridges, author similarity with shared-tag evidence, the
fandom-scoped arena, the tag-page reachable alias, `kindred eval`, the two
surfaces that were already built (tag sidebar, "rank from this"), and the
algorithm A/B arena.

**S1 shipped as "an algorithm is a named tune".** Two named weight vectors
rank the same seed from the same pool; `/algo` shows both columns and the
signals whose weights differ, and a vote goes to `algo_comparisons`. That
table is separate from `arena_comparisons` on purpose: arena judgements feed
the tag learner, and a preference between weight vectors is not evidence about
the tags on two works. `TestAlgoVotesNeverReachTheTagLearner` holds that line.

What is still NOT built, and the honest name for it: the arena compares WEIGHT
VECTORS, not two independent ranking implementations. A learned ranker would
need its own registry and candidate shape; the page and the votes table are
the harness it would plug into.

Two live findings from closing S1, both fixed:

- **`idf_overlap` cost 12.5 s per request.** It queried `work_tags` for the
  pool's tag ids on every ranking — measured on the 1.7 GB mirror, and it
  made /recommend take 98 s. Document frequencies are now built ONCE by
  `kindred ingest` into `<corpus>.docfreq` (14.5 s to build, 68 ms to load)
  and the signal reads the loaded table. With no table the signal SKIPS and
  `meta.degraded` names it, rather than quietly degrading into a duplicate of
  `tag_overlap`.
- **`serve`'s auto-crawler failed one job at a time, forever, on a mirror it
  could not write** (`attempt to write a readonly database`, per fetched
  work, while still fetching from AO3). This deployment's corpus is on an
  sshfs mount. `openMirrorForWrite` now proves the write lock with
  `BEGIN IMMEDIATE`/`ROLLBACK` at startup, so the condition is one clear
  "auto-crawl disabled" line instead of an endless stream.

Verification for the shipped set:

    go build ./... && go vet ./... && go test ./... -count=1
    python3 docs/goal-check.py
    python3 scripts/check-cli-coverage.py
    bash -n scripts/budget.sh

The budget probes for the three new API routes are in `scripts/budget.sh` and
`routes.txt`; they have not been run against the live mirror since the last
deploy, so `scripts/check-provenance.sh` still reports the deployed binary as
behind HEAD until `scripts/deploy.sh` runs.

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
- [x] **Entity shortlists (authors, pseudonyms, series, collections).** Authors and pseudonyms are extracted from works.authors by splitting on '- ' and '(', trimming, and dropping empties. Items that start with a digit or underscore go to the pseudonym column; others to the authors column with pseudonym empty. Comma is also a separator for the tiny fraction (<0.1%) that uses it. Position is 0..n per work. Reverse map stored in pseudonyms(pseudonym -> authors_text). Series and collections are not exposed in the public AO3 API, so the tool returns an honest 'not present in public data' note.
- [ ] **Tests per new path; update goal-check clauses and docs.**
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

## Session 2026-10-07 (later): search, authors, taste default, auto-crawl

Done and verified this session, with the evidence:

- [x] **Search by AO3 URL/ID and title+author.** `/search?q=` resolves a
      work URL or bare `/works/<id>` to an exact match first (rendered in a
      distinct `exact-work` block, excluded from the fuzzy list below so a
      bare ID cannot appear twice), then falls back to tokenised title +
      author search. A URL for a work this mirror lacks says the id out
      loud (`exact-missing`). Pinned by `internal/api` tests and
      `e2e/tests/tasteauthorsearch.spec.js`.
- [x] **Author pages.** Every byline renders `href="/author?q=..."`;
      `renderAuthor` lists that author's works (substring match, `?n=`
      up to 500) and an unknown name is an honest empty, not a blank page.
- [x] **`?n=` everywhere.** Work, tag, search and author pages accept it,
      capped per page (tag 200, author 500), with a "raise N" link when a
      list is truncated; the sort form carries `n` so changing order does
      not reset the count.
- [x] **Taste-blended tag default.** `/tag/<t>` opens on `sort=taste`:
      exact tag hits unioned with the reader-loved neighbours, with a note
      carrying both counts (`taste-note`), or a named reason when the blend
      cannot run (`taste-error`); silence is the failure state. An explicit
      `?sort=` opts back into the plain exact list.
- [x] **Auto-crawl while serving.** `serve` (default; `--no-crawl` opts
      out) drains a `crawl_queue` table: reader 404s enqueue the work,
      viewed works missing summaries enqueue themselves, startup queues up
      to 500 stored works missing summaries. Per-work `done_at` checkpoint,
      existing metadata skipped before any request, jobs parked after 5
      failures and re-armed by a fresh reader request. The request path
      never touches the network — only a background goroutine does, under
      the same robots-delay client as `kindred crawl`.
- [x] **Deploy gate covers the new surface.** `scripts/check-deploy.sh`
      gained a section asserting the taste heading + note-or-error, the
      order choice, the raise link, a byline href taken from the page and
      then visited, exact-match search on a real mirror id, and the
      URL-miss note. Measured `46 checks pass` against the live instance.

Suite: `gofmt`/`vet`/`build` clean, `go test ./...` green (all packages),
**115 Playwright tests** (`make e2e`, was 96). Deployed to thinkcentre:
behaviour gate `BEHAVIOUR IN SYNC: all 46 checks pass`,
`check-provenance.sh` `IN SYNC` (built from HEAD `86e5e89`),
`/healthz` 200 with its memory report (the `budget_ok` field was removed on
2026-10-22 with the cap itself).

### Open — found by this session's verification

- [ ] **Auto-crawl cannot fetch AO3 in production.** The systemd unit
      (`deploy/kindred.service`) sets `IPAddressDeny=any /
      IPAddressAllow=localhost` — correct for the read-only mirror it was
      written for, but the background crawler dials out and gets
      `i/o timeout`, so every queued job burns its 5 attempts and parks.
      Outside the sandbox (standalone instance) the fetch reaches AO3 but
      the first probe got `no works:title meta tag; not a work page`, so
      the fetch itself needs a re-check against real AO3 HTML before the
      unit is relaxed. Two options, decide explicitly: allow
      archiveofourown.org through the unit (the comment there says the
      service "should not be reaching the network"), or drop auto-crawl
      from `serve` and run it as a separate sandboxed timer invoking
      `kindred crawl`. Until then the queue is honest but inert in
      production; it works under test (stub client) and outside the
      sandbox.
- [x] **`/healthz` no longer flips to `over_budget` — the cap is GONE
      (2026-10-22, owner's decision).** `VmHWM` is monotonic: a fresh restart
      sat at ~150 MB, and a burst of `n=100` blended tag pages pushed the peak
      past the 220 MiB line (measured 251-257 MB, and 298 MB on the deployed
      instance), after which the service reported itself unhealthy until it
      restarted. The gate did exactly what it was built to do and the verdict
      was still wrong: a process serving every request correctly is healthy.
      `budget.CapKiB` and `budget.Check` are deleted, `/healthz` always answers
      200, and the memory numbers are still reported (`peak_rss_kib`,
      `heap_mib`, `goroutines`) and logged every five minutes. SPEC §6 was
      rewritten to say memory is measured, not gated. The transient allocation
      itself was never fixed, and that is now a MEASUREMENT to act on rather
      than a deploy blocker: the next unit is to persist the collab index at
      ingest and mmap the CSR instead of parsing 28 MiB into heap.

## Session 2026-10-21: the family-intersection query, generalised

The one-off `worm-isekai` command is gone, replaced by a general capability:
works carrying a tag from a **fandom family** AND a tag from a **concept
family**, updated within N days.

- [x] **The query speaks ONE vocabulary — the family key (`worm`, `isekai`)**
      — so a new fandom or a new idea is a data edit in
      `internal/tagfamily/families.json`, not a code edit.
- [x] **FOLDED INTO `/search` (2026-10-22), after the owner judged the
      separate page too much surface for the function.** `/browse` (page, nav
      entry, template), `GET /api/v1/browse` and the route's handler are GONE;
      the OLD ADDRESS survives as a 301 to `/search` with the query string
      intact, because a bookmark outlives a redesign and a 404 there breaks a
      link the owner already has. A redirect is not the surface returning: no
      template, no handler, no nav, no API. The form is now
      a fieldset on `/search` (a GET to `/search?universe=&concept=&recent=`),
      and the results are a "Both families" section there. What survives:
      the `kindred browse` verb (scriptable, no UI surface) and the families
      vocabulary. The API capability was never unique — `/api/v1/ao3/works`
      already answers `?universe=` + `?concept=` intersections, so the second
      endpoint was a duplicate; the pool counts it alone reported now live on
      the page and in the CLI. The fold keeps the CAPABILITY (nothing else can
      answer "recent worm isekai": text search cannot, and a tag page filters
      one tag) while removing the surface.
- [x] **THE FOLD EXPOSED A REAL DEFECT IN PRODUCTION, and this is the part
      worth keeping.** The new page-level tests matched NOTHING against the
      40-work fixture, and the cause was not the fold: the live mirror stores
      `works.update_date` as AO3's display date (`17 Sep 2008`), and
      `internal/corpus`'s parser only accepted ISO layouts, so **every real
      work parsed as undated and the `recency` signal skipped all of them** —
      silently, at whatever weight the tune gave it. No test saw it because
      the test fixture wrote `2026-01-15`: a fixture in the wrong format
      validates the wrong behaviour. Fixed: the parser accepts `2 Jan 2006`
      (plus RFC3339 with microseconds, the `first_seen` shape), the fixture
      writes `15 Jan 2026`, and the date-format cases are asserted. Verified
      against the live mirror: 112,935 works, `update_date` is display-format
      in every sampled id range, so the recency signal was inert on the whole
      corpus before this and contributes now.
- [x] **The families are DECLARED, not derived, and that is a measured
      decision, not a shortcut.** The corpus cannot derive them: one
      franchise's spellings are near-disjoint populations (Worm's two biggest
      spellings share 17 of ~2,000 works each) and character tags do not
      bridge franchises ("harry potter - j. k. rowling" and "star wars - all
      media types" share 4,619 character tags). What the corpus CAN do is
      VALIDATE a declaration, and `ValidateUniverse` reports a missing or
      non-fandom-shaped spelling rather than ignoring it.
- [x] **The fandom-shape rule has precision 1.000 and recall 0.354.**
      Measured against a 302-fandom / 3,286-freeform ground truth scraped
      from AO3's own `fandom tags` markup. The gap is entirely PLAIN-named
      fandoms: all 90 of them are missed, and 2,984 of the 3,286 freeform
      names are plain too, so the corpus genuinely cannot separate them
      (`futa - fandom` and `bdsm - all media types` really do exist as tags).
      Two extensions were tried and both cost precision (1.000 -> 0.564 and
      -> 0.508). **Precision over recall is the standing choice**: a false
      "this crosses into Harry Potter" is a wrong claim in a reader's face.
- [x] **The families are now FOUR universes and THREE concepts, and the
      export proves the generalization works.** `harry potter`, `naruto`
      and `marvel` were added alongside `worm`; `alternate universe` and
      `reincarnation` alongside `isekai`. **No Go code changed to add
      them** -- each is a data edit in `internal/tagfamily/families.json`.
      The 200-work export is `worm x alternate universe` over 730 days
      (272 matches, 200 exported) to
      `exports/worm-alternate-universe.txt`, one work per line as
      `title<TAB>url`.
- [x] **A family is DECLARED, so a declaration can be wrong, and now
      something CHECKS.** `internal/tagfamily/live_validate_test.go` opens
      the real mirror and validates every declared member. It found that
      the `worm` family had three members the shape rule does not vouch
      for and that **no test had ever looked**, that `naruto - fandom` and
      `marvel - fandom` do not exist in the mirror at all, and that four of
      the eight `reincarnation` tags were invented. All fixed. The
      per-family `spellings_explicit` list (formerly
      `spellings_explicit_freeform`, renamed because the members that need
      declaring are not all freeform) is what admits a member the rule
      cannot recognise.
- [x] **Two recency-ordering bugs fixed.** `works.update_date` is TEXT
      `DD Mon YYYY`, so `ORDER BY update_date DESC` is lexicographic, not
      chronological: it yielded `31 Oct 2025` as the most recent work while
      the true maximum is `26 Jun 2026`. Fixed in `internal/web/handlers.go`
      (`tagOrder`) and `internal/api/server.go`, both now using
      `internal/corpus.ChronologicalUpdateDateDesc`.

## Session 2026-10-22: the algorithm arena finished, and what the gates caught

S1 (the algorithm A/B arena) was committed with handler tests and a page, and
three things were still wrong. Each was found by a check rather than by reading.

- [x] **`/algo` was reachable only by typing the URL.** The registry carried
      `Nav: "/algo"` — which marks the page current once you are on it — and
      `layout.html` had no link to it. `goal-check.py`'s page-coverage clause
      failed the commit for having no browser test, and chasing that found the
      missing nav item. A Go render test cannot see this: the page renders
      perfectly and the defect is in the layout on every OTHER page. Now: a nav
      entry, a `pages.spec.js` row, and `e2e/tests/algo.spec.js` walking the
      form, a comparison, a vote, and both refusals.
- [x] **The comparison was measuring the diversifier, not the algorithm.** The
      page promises "only the weights change", and the engine ran
      `diversify.Apply` at Lambda 0.75 on both columns. MMR scores
      `lambda*score - (1-lambda)*similarity`, so its penalty shares the score's
      units: tunes whose scores differ in MAGNITUDE are penalised unequally.
      Measured on the fixture, `default` and an extreme quality/recency-only
      tune returned the SAME five works in the SAME order, and the printed
      scores were not even descending (0.48, 0.27, 0.32 — the order was MMR's).
      `engine.Request.Lambda` now exists (zero keeps the site's 0.75 for every
      existing caller; the comparison surfaces pass 1.0). Live on the real
      mirror: `default` vs `quality-lean` shared 2 of 5 works, both columns
      descending, 1.2 s.
- [x] **`idf_overlap` was degraded in production.** The fix that moved
      document frequencies to `<corpus>.docfreq` shipped, but nothing had
      built the artefact on the deploy host, so the signal skipped and named
      itself in `meta.degraded`. Built there (634,249 tags, 112,937 works,
      5.07 MB) and now live: weight 0.13 in the evidence, absent from
      `degraded`.
- [x] **`serve` claimed to listen before it bound.** The log line came first,
      so a port owned by another service printed `msg=listening` and then
      `bind: address already in use`. It cost a verification walk: a demo
      instance printed "listening" on a port a PieFed backend owned, the walk's
      healthz returned 200, and the 200 was PieFed's. Now it binds first and
      logs `ln.Addr()`, and a test occupies a port and asserts no listening
      claim.

### Still open, with the measurement

The deployed `serve` reached **298,416 KiB peak RSS** against what was then the
`full` cap of 225,280 KiB, so `/healthz` answered 503 `over_budget` and
`scripts/check-deploy.sh` exited 2. As of 2026-10-22 that cap no longer exists:
`/healthz` reports the number and answers 200, so the gate can see a product
failure and a large process as the different things they are. The number is
still the number — it is a measurement now, not a verdict.

This pre-dates the round: the same host logged 266,436 KiB on a binary two
commits older, and `deploy.sh` has recorded 265,984 KiB since 2026-10-16. What
this session added is measured, not guessed: the document-frequency map is
**18.0 MiB** of the heap (loaded with the real file at 634,231 tags).

The components, measured on the host: heap 147 MiB, peak 298 MiB — so ~150 MiB
is Go's non-heap retention after the startup transients (graph load parses a
28 MiB CSR plus 634k names; the collab builder walks 3.4M pairs to keep
12,812). Embeddings are 81 MiB of the heap (634,232 x 32 floats).

Neither a cap change nor a `lite` switch is the answer: 220 MiB is a SPEC §6
constraint with its own test, and raising it because the code exceeds it is the
accepted-and-ignored shape this project refuses. The honest lever is the graph
load (mmap the CSR instead of parsing it into Go maps) plus persisting the
collab index at ingest, as the graph index and `.docfreq` already are. That is
a unit of work, not a tail-end edit, and it is recorded here so the next
session starts from the number rather than from "it seems slow".

## Session 2026-10-23: four ideas off the 31-100 audit, and what the slices showed

- [x] **67 — an unseeded `/recommend` ranks from the mirror's most-kudosed
      work.** One seed (three from three fandoms measured 4.2 s a page against
      2.6 s for one, and the page's own hint says seeds average each other
      away), id tie-break so a reload is stable, and the page names the id as
      popularity in `seed-origin` — a default presented as a
      taste match would be a claim nothing measured. No corpus still renders
      the empty form. Pinned by
      `TestAnUnseededRecommendStartsFromTheMostKudosedWorks`, whose first
      version was VACUOUS: it asserted the id string appeared in the body, and
      the result rows carry their own `ao3_work:` links, so an ASC ordering
      (three wrong seeds) still passed. It now asserts the seed INPUT and the
      number of work-id inputs; ASC reddens it.
- [x] **64 — "What makes this work distinctive" on the work page.** The
      work's rarest tags by document frequency: the same table the IDF signal
      reads, read the other way round. A tag on every work is dropped (zero
      evidence), and so is a tag the table reports on zero works — this work
      carries it, so the table and the work disagree, and calling a stale row
      the rarest thing here would make the strongest claim in the block on the
      one row whose evidence contradicts itself.
- [x] **46 + 71 — per-fandom and per-tag eval slices.** `eval.Report` now
      carries one `Placement` per held-out work (rank, 0 = not found);
      `eval.ByGroup` groups them (a crossover counts in both fandoms, groups
      under 20 held-out works are dropped); `kindred eval` stores both axes in
      `eval_slices`; `/eval` prints the newest run's weakest groups first.

**The live run, and the number it explains.** Run #2 on the 112,937-work
mirror, 38 readers, 495 held-out works: **recall@10 0.0067, ndcg@10 0.0096,
coverage 0.0789** — the same aggregate as before this work, so slices did not
move the ranking; they made its failure legible. The weakest groups are not
long-tail noise: *my hero academia* 0 of 40 found, *naruto* 1 of 42, and the
relationship tags **f/m 0 of 232** and **f/f 0 of 113**. The recommender is
failing on the archive's largest, most ordinary corners, which is a different
fact from "recall is low" and points at the candidate pool / signal weights
rather than at sparsity.

### Still open after this session

Two `degraded` signals on an anonymous API request are NOT faults and should
not be "fixed": `owner_taste` reports degraded because no owner is configured
(the service log says so at startup) and `session_taste` because an anonymous
request carries no weights. `collab` is built (12,812 pairs, gate 2) and
reports degraded only for a seed work with no co-bookmark neighbours — the
index covers 9,365 of 112,937 works. Worth stating because the three appear
together in `meta.degraded` and read like three broken signals.

## Standing caveat

`complete=true` means COMPLETE ONLY, not "no filter". It reads like a vacuous
truth in isolation, but a tri-state control whose boolean spellings collapse
to the same request is indefensible. Pinned by
`TestParseFilterAcceptsTheSpellingsAFormSends`.