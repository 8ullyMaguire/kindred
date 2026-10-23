# Ideas 31-100, audited one row at a time

Status: **written 2026-10-22**, after the tier-2 audit (`PLAN-recsurfaces-tier2.md`).
Supersedes that document's four buckets: it said 70 ideas were "grouped, not
audited". This is the audit.

## Method, because "not built" is a claim

Every row was decided by looking for **the specific thing the idea names** — a
route, a table, a signal, a runner, a template field — and not by a title-word
grep. The grep-first method was measured as useless here: `follower`, `gallery`
and `voice` hit nearly every file in the sibling project, and in this tree
`rss` matches `PeakRSSKiB` only, while `series` matches JSON key names in
`arena_handlers.go`.

The three structural checks behind every OPEN row:

| Check | What it can and cannot prove |
|---|---|
| Routes (`internal/web/registry.go`, `internal/api/server.go`) | A surface that is not routed is not reachable. A route that exists is not a feature: the page may render an empty state. |
| Tables (`internal/store/*.go` `CREATE TABLE`) | The cheapest refutation of a premise: no `series` table means no series. |
| Signals and runners (`internal/signal/*`, `internal/corpusquery/corpusquery.go`) | `rank.Score` sums whatever is registered; a score term that is not a registered signal cannot be in a ranking. |

Four statuses:

- **BUILT** — the tree does it; the row names where.
- **PARTIAL** — the input or half the surface exists; the row names the half that does not.
- **OPEN** — nothing in the tree computes it.
- **NO PREMISE** — the thing the idea needs does not exist in this archive, and its
  absence is a property of the mirror or the product, not a missing feature.

## The table

| # | Idea | Status | Evidence / what is missing |
|---|---|---|---|
| 31 | Tag trend detection (90-day vs corpus average) | OPEN | `corpusquery` has four runners — `FandomRanking`, `Underrated`, `TagNeighbours`, `Surprise` — and none compares time windows; `/eval` has no time slice. |
| 32 | Completion-aware ranking | PARTIAL | Completion is a tri-state **filter** (`engine.Request.Complete`: `CompleteAny`/`CompleteOnly`/`CompleteWIP`) and a tag-page sort; no tune carries a completion weight. |
| 33 | Multi-seed intersection scoring | **BUILT** | `signal.SeedAgreement`: IDF-weighted coverage of each seed, averaged over seeds INCLUDING zeros, so a perfect match on 1 of 4 seeds scores 0.25 and partial matches on all 4 score 0.625. Skips (and names itself in `meta.degraded`) with fewer than two usable seeds, because one seed is `idf_overlap` under a second name. Weight 0.10, and the ablation is a flag (`kindred eval --tune ablate_no_agreement`). |
| 34 | "Works this tag needs" gap analysis | OPEN | No writer-facing surface, and no query diffs a tag's neighbours against its own co-occurrence. |
| 35 | Author-author co-readership | OPEN | `/api/v1/authors/similar` is **IDF-weighted tag-profile overlap** (`engine/authors.go`), not a co-bookmark matrix aggregated to authors. |
| 36 | Session taste vector export/import | OPEN | No `/api/v1/taste`; session taste is derived per request (`signal/session_taste.go`) and never serialised. |
| 37 | Tag hierarchy from embedding clusters | OPEN | `internal/tagfamily` holds a hand-written `families.json`; nothing clusters the embeddings into a tree. |
| 38 | Quality-adjusted co-occurrence | PARTIAL | Co-occurrence edges are shrunk by `DefaultShrinkK` (`collab.go`) — evidence weighting, not per-work kudos weighting. |
| 39 | Bayesian average for tag quality | OPEN | Shrinkage exists for collab lift only; no per-tag Bayesian average of kudos across a tag's works. |
| 40 | Ordered "reading path" | **BUILT** | `GET /api/v1/works/{id}/path?steps=N` and `/path/{id}`. Each step is chosen against the PREVIOUS work (`sim_to_prev - 0.5*sim_to_seed`), so the list drifts instead of describing the seed N times; every step publishes both distances. Measured on work 17249318: seed distance falls 0.966 -> 0.896 -> 0.798 -> 0.701 over four steps while hops stay 0.89-0.97, against 0.966 flat for plain nearest-neighbour. |
| 41 | Length binning for similar-length recs | PARTIAL | `MinWords`/`MaxWords` filter and a `words` sort exist; no same-quartile boost inside a ranking. |
| 42 | Calibrated recommendation confidence | PARTIAL | Every hit carries `Evidence{Signal, Value, Weight, Reason}` and the page prints it; there is no calibrated probability and no speculative/strong label. |
| 43 | Anti-recommendations | BUILT | The premise was FALSE as of this session: negatives flow from two writers, and both were verified on the deployed binary. (1) An arena pair judgement calls `arena.LearnTags(byWorks, winners, losers, …)`, which gives the losing work's tags NEGATIVE deltas (`api/arena.go:326`), and (2) the new `/skipped` page's "Not interested" writes polarity −1 through `LearnFromFeedback` (`store/feedback.go:231`). `session_taste` accepts a negative value by construction (`signal/session_taste.go` clamps to [−1,1] and its own comment says a negative must not be clamped away). MEASURED on the live binary: after one "Not interested" on work 17249318 the reader held 74 tag weights at −0.081, and `/api/v1/recommend?seed=ao3_work:17249318` returned five candidates whose session_taste evidence was value −0.732/−0.448/−0.529 · weight 0.14 = **−0.1025 / −0.0627 / −0.0740** — the ranking is actively reduced, not merely unattributed. What is NOT built: a *magnitude* measurement (the eval harness has no session-taste user model, so demotion is proven to exist and not proven to be enough) and work-level negation independent of tags. |
| 44 | Seasonal/temporal context | OPEN | No tag→month mapping and no calendar term. |
| 45 | Tag canonical form resolution | PARTIAL | `tagfamily` (embedded `families.json`) plus `fandom.base()` collapse variants; there is no alias table and the index is never merged. |
| 46 | Per-fandom quality tracking | **BUILT** | `eval_slices` + `eval.ByGroup` from per-work `Placements`; `/eval` prints the newest run's weakest fandoms first (groups of ≥20 held-out works). |
| 47 | Work cluster labels | OPEN | No work-level clustering; embeddings are `kind='tag'` only. |
| 48 | Reader velocity signal | OPEN (count built) | The session model now exists: `reader_visits` (three instants per reader — `previous_at`, `current_at`, `last_at`) and `MarksSince`, which `/new` prints ("2 work(s) marked read since then, across 1 visit(s)"). The SIGNAL is deliberately not built, and the reason is measured, not assumed: a weight needs a distribution, and the live state database on 2026-10-23 held **33 marked works across 8 owners and 8 reader-days — no owner with five, max 13 marks in one day**. Fitting a weight to that is fitting noise, and this project refuses unmeasured weights. The number that would justify the next step: ≥20% of owners with ≥5 reader-days and a spread between the 90th and 50th percentile mark rate wide enough to change a ranking. Mutant killed: the boundary ignored, so "since your last visit" counted every mark ever made. |
| 49 | Tag polarity detection | PARTIAL | A reader can block a tag (`arena_user_blocked_tags`, `POST /block`) and blocks are honoured in ranking; nothing infers polarity corpus-wide from block/bookmark ratios. |
| 50 | "Complete your collection" for an author | PARTIAL | `/author` lists an author's works and `/seen` excludes read ones; no "3 of 7 read, 4 to go" surface. |
| 51 | Series-aware recommendations | NO PREMISE | No series column or table; AO3 series metadata is not crawled. |
| 52 | Writing-style similarity via summary embeddings | OPEN | Embeddings are tag-level (`embeddings.kind='tag'`); there is no text model and no summary vector. |
| 53 | Bradley-Terry vs Glicko | OPEN | Glicko-2 only (`internal/arena/glicko.go`); no BT fit and no rank-correlation comparison. |
| 54 | Rich tag profile page | PARTIAL | `/tag/{id}` carries the count, the sorted works and a neighbours sidebar; no median kudos/words, no most-bookmarked work, no trend line. |
| 55 | Explanation quality evaluation | OPEN | `/algo` compares two tunes' **result lists** and records a preference; two explanations are never shown side by side. |
| 56 | Weighted tag matching with IDF | **BUILT** | `signal/idf_overlap.go`, plus document-frequency weighting in `engine/authors.go`. |
| 57 | Cross-fandom bridge works | **BUILT** | `engine/bridges.go` + `/api/v1/works/{id}/bridges`, kudos-capped scan, other-fandom names as the reason. |
| 58 | Productivity vs quality scatter | OPEN | No scatter surface; `/fandoms` and `/author` print numbers, not a plot. |
| 59 | t-SNE/UMAP tag map | OPEN | No projection and no `/tag-map`. |
| 60 | "Works you keep avoiding" | BUILT | `/skipped` + `store.SkippedWorks`. Reads `interleave_impressions` (one row per work per showing) — and does NOT read `algo_impressions`, which stores a whole list as CSV per row, so grouping it by work would mean parsing CSV in SQL. Three exclusions, each a claim that would otherwise be false: shown ≥3 separate times (one unclicked list is a coincidence), no credited showing, and not already marked read (a read from the bookmark screen never touches this log). Two actions per row: "Not interested" (polarity −1 through `LearnFromFeedback`, the one path that can make a work rank LOWER) and "I read it". LIVE: seeded 3 staggered showings for a real owner → the row rendered "shown 3 times, last on 2026-10-23 18:45:15"; the POST wrote polarity −1 and 74 tag weights, and the row disappeared from the list. Mutants killed: threshold ignored, credited showing still counted, read works offered, owner filter dropped. |
| 61 | RSS feed of recommendations | **BUILT** | `/feed/tag/{id}` (works under a tag, newest first) and `/feed/seed/{kind}/{id}` (the ranking for one seed), both RSS 2.0 with absolute guids. Ordering is `corpus.ChronologicalUpdateDateDesc`, because the mirror stores AO3's "31 Oct 2025" and a string sort names 31 Oct 2025 the newest work when 26 Jun 2026 is. The tag page discovers its feed via `rel=alternate` and links it in words. Two defects the DEPLOYED feed showed: the seed came first in its own feed (`Request.Exclude` defaults false; only the API handler set it), and a work with an empty `update_date` rendered `<pubDate></pubDate>`, which readers parse as a date. `internal/api/feed.go`, `feed_test.go`. |
| 62 | OpenSearch descriptor | **BUILT** | `/opensearch.xml`, OpenSearch 1.1, one Url pointing at `/search?q={searchTerms}`. A second `format=rss` Url was removed: `/search` has no format parameter, so the browser offered a subscription that returned HTML — a promise the reader sees broken. The test counts the Urls. |
| 63 | Jaccard co-occurrence similarity | **BUILT** | `signal.go` computes and prints a Jaccard ratio; `TagNeighbours` ranks by PMI; `idf_overlap.go` is the weighted variant. |
| 64 | "Tag DNA" distinctive tags | **BUILT** | `internal/web/distinctive.go` + the work page's "What makes this work distinctive" block: rarest by document frequency, a tag on every work dropped (zero evidence), a zero-frequency tag dropped (the table and the work disagree). |
| 65 | "New since your last visit" | **BUILT** | `/new`, backed by `reader_visits` (previous_at = the boundary, current_at = the visit's start, last_at = when we last heard from them). Both halves of `seen_works` are excluded — read AND shown. A first visit says there is no "before" instead of using zero time, and the page prints the boundary it measured from. Live: boundary 1 May 2026 → 40 works, newest first; marking one seen removes it and the list refills. |
| 66 | Author recs from a single work | PARTIAL | `/api/v1/authors/similar` exists (`engine/authors.go`); the work page does not link to it and no template renders it (`grep` for `authors/similar` in `work.html`/`handlers.go`: 0). |
| 67 | Smart default seeds for new readers | **BUILT** | `/recommend` with no seed ranks from the most-kudosed work (`corpus.AO3.TopByKudos`; ONE, because three from three fandoms measured 4.2 s a page against 2.6 s for one) and names it as popularity in `data-testid="seed-origin"`; no corpus still renders the empty form. |
| 68 | Tag "temperature" (contested tags) | OPEN | No per-tag variance over comparisons. |
| 69 | Work controversy score | OPEN | `arena_entity_history` holds per-entity history; nothing computes a divisiveness score from it. |
| 70 | Batch arena with keyboard shortcuts | **BUILT** | `/api/v1/arena/batch` + the speed round in the arena UI. |
| 71 | Per-tag recommendation quality leaderboard | **BUILT** | Same table, `axis = 'tag'`: every tag a held-out work carries, weakest first, top 25 by size per run. |
| 72 | Embedding dimension search | PARTIAL | `kindred embed --dim` exists (default 32, `embedcmd.go:47`); eval runs never vary it, so no width is compared. |
| 73 | Multi-objective Pareto ranking | OPEN | One scalar score; no dominance relation. |
| 74 | Random walks (personalised PageRank) | OPEN | No walk; ranking is overlap/affinity/collab signals. |
| 75 | Cross-fandom trope transfer | OPEN | Bridges surface works that **are** in both fandoms; "same non-fandom tags, a different fandom" is a different query and no runner does it. |
| 76 | Embedding drift monitoring | BUILT | `embed_runs` + `store.RecordEmbedRun`, written by `kindred embed` after the table lands; `/mirror` prints the verdict; `GET /api/v1/embed/drift`. A fingerprint, NOT a hash: a hash says "something changed" and cannot say whether the space moved or merely reshuffled. Each run stores rows, width, mean L2 norm, the centroid, and a SAMPLE of 64 tag vectors — the sample is load-bearing, because two tags moving in opposite directions leave the centroid exactly where it was (`TestAnAverageCanHideOppositeMovements`; the centroid-only mutant dies on it). Floor 0.98, not 0.9: a deterministic solver over an unchanged corpus landing at 0.95 has moved positions rather than re-rolled a seed. Two refusals: the page will not print "0% drift" when fewer than two fingerprints exist (no comparison ≠ good result), and a failed fingerprint does not fail the build (the embeddings are correct; a monitoring gap must not become a broken deploy) — it is printed by name. Mutants killed: floor 0, centroid-instead-of-sample, width change not its own verdict, sample compared against itself. |
| 77 | Tag suggestions for writers | OPEN | No writer surface. |
| 78 | Work-pair similarity API | **BUILT** | `GET /api/v1/works/{id}/similarity/{other}` and `/work/{id}?compare={other}`. Two components, published separately: IDF-weighted Jaccard over the tag sets and the cosine between the works' tag centroids; the shared tags travel rarest-first, and every axis that could not run is named in `missing`. Popularity/recency are deliberately NOT components. Live: works 17249318 vs 65141041 share 18 tags, overlap 0.111, cosine 0.869, score 0.490. |
| 79 | Recommendation diversity metric | PARTIAL | `diversify` enforces MMR and per-group caps and reports what it dropped; `eval_runs` stores recall/NDCG only, so intra-list diversity is not tracked over time. |
| 80 | Seasonal tag auto-detection | OPEN | No temporal frequency model over tags. |
| 81 | Federated taste profiles | OPEN | `dump`/`fetch` move mirror snapshots (`cmd/kindred/dumpcmd.go`); no taste vector is exchanged. |
| 82 | Bandit-based exploration | OPEN | No exploration policy and no reward stream. |
| 83 | Transfer from AO3's wrangling graph | OPEN | `families.json` is hand-written; no synonym supervision and no contrastive training. |
| 84 | Counterfactual evaluation | OPEN | `interleave_impressions` stores the alternative only as the other algorithm's pick; no IPS estimator reads it. |
| 85 | Causal inference for impact | OPEN | No instrument, no estimator. |
| 86 | Bandit weight optimisation | OPEN | Weights are hand-set per tune (`store/tunes.go`) and compared by `/algo` votes. |
| 87 | Graph neural network | OPEN | Embeddings are truncated SVD by power iteration (`embedcmd --iters`), which is the thing a GCN would replace. |
| 88 | Attention-based multi-seed fusion | OPEN | Multi-seed handling is per-signal best-seed, not learned weights. |
| 89 | Implicit-feedback matrix factorisation (ALS) | PARTIAL | Collab is item-item cosine over co-bookmarks (`internal/collab`) — counting, not factorisation. |
| 90 | Reinforcement learning for sequences | OPEN | No session MDP. |
| 91 | Cross-lingual recommendations | OPEN | One language in the corpus; no multilingual vectors. |
| 92 | LLM-generated explanations | OPEN | Explanations are structured evidence strings (`Evidence.Reason`), never generated prose. |
| 93 | Fairness auditing | OPEN | No group attributes are collected, by design. |
| 94 | Social recommendation layer | OPEN | No cross-instance identity and no social graph. |
| 95 | Work lifecycle prediction | OPEN | No per-work crawl time series to fit. |
| 96 | Variable embedding dimension per tag | OPEN | One width for every row (`embeddings.dim`). |
| 97 | Haptic mobile arena | NO PREMISE | The product is HTTP-only with no JavaScript (SPEC §1.1) and there is no mobile app. |
| 98 | Narrative arc matching | NO PREMISE | The mirror stores no chapter structure. |
| 99 | Natural-language query | OPEN | The query language is structured (`tag:`, `words:`, `kudos:`); no model maps prose onto it. |
| 100 | Autonomous recommendation agent | NO PREMISE | No runtime, no product basis, and it would need a taste model that does not exist. |

## Counted

| Status | Count | Ideas |
|---|---|---|
| BUILT | 17 | 33, 40, 43, 46, 56, 57, 60, 61, 62, 63, 64, 65, 67, 70, 71, 76, 78 |
| PARTIAL | 12 | 32, 38, 41, 42, 45, 49, 50, 54, 66, 72, 79, 89 |
| OPEN | 37 | 31, 34, 35, 36, 37, 39, 44, 47, 48, 52, 53, 55, 58, 59, 68, 69, 73, 74, 75, 77, 80, 81, 82, 83, 84, 85, 86, 87, 88, 90, 91, 92, 93, 94, 95, 96, 99 |
| NO PREMISE | 4 | 51, 97, 98, 100 |

With the tier-1/2 audit in `PLAN-recsurfaces-tier2.md` (18 built, 1 partial, 11
open of 30), the whole list stood at 22 built, 13 partial, 61 open, 4 without a premise.
Five of the open ones (33, 46, 64, 67, 71) shipped on 2026-10-23, and #11's
enumeration half shipped on 2026-10-23b (it moves from OPEN to PARTIAL: the
candidate source exists, the nearest-tags surface does not), so the live total
is **32 built, 14 partial, 50 open, 4 without a premise** — 100 accounted
(#61, #62, #78, #40 and #65 shipped on 2026-10-23d: a tag feed, a seed feed,
the search descriptor, the work-pair report, the reading path and the
new-since-visit list, all verified on the deployed binary; 2026-10-23g added the
per-kind algorithm registry and its live A/B, which is infrastructure for every
row below rather than one of them -- see
`docs/HANDOFF-2026-10-23g-per-kind-algorithms.md`).
2026-10-23h shipped #60 ("works you keep skipping"), #76 (embedding drift
fingerprints) and settled #43 by MEASUREMENT rather than by building: negatives
already flowed from arena pair judgements, and the new "Not interested" action
gives a reader a direct one. The total is now **35 built, 14 partial, 47 open,
4 without a premise** -- see `docs/HANDOFF-2026-10-23h-skip-drift.md`.

The measurement that matters more than the count: **pool recall@1000 was
0.0284** (run #8) and is **0.2113** after the candidate work (run #13), i.e. even a perfect ranking of the 1000 candidates could
find at most 2.8% of held-out works, and the current recall@10 of 0.0056 is a
fifth of that ceiling. The remaining ideas are worth less than candidate
generation, and #11 (embeddings over the whole corpus) is the lever with the
most headroom.

## What the audit changed about the group story

1. **The embeddings are built, were NOT being used, and now have an
   enumeration.** This paragraph used to say every recommendation scores an
   `embedding` signal at weight 0.10. Measured on 2026-10-23 it scored at
   nothing, twice over: `renormalise` deleted the weight whenever the signal
   was registered, and the signal asked for per-WORK vectors that no deployment
   has (only `kind='tag'` rows exist). Both are fixed in 2026-10-23b, which
   also adds the enumeration: `pool_mode=tags+embed` reaches works whose tags
   never co-occur with a seed's. Still missing among the scans: no
   K-nearest-tags ENDPOINT (11 is partial for that reason), no 2-D projection
   (59), no drift check (76), no width search (72), no per-tag width (96).
2. **Impressions now exist**, so two ideas that were previously impossible are
   merely unbuilt: 60 (shown but never marked) and 84 (counterfactual). The
   interleave table records the alternative, which is exactly the counterfactual.
3. **Four ideas were already built and were being counted as open** in the
   group text: 56 (IDF), 57 (bridges), 63 (Jaccard/PMI), 70 (batch arena).
4. **The cheapest genuinely-open ideas are eval slices, not rankers**: 46 and 71
   are one `GROUP BY` each on data `eval_runs` already stores; 33 and 65 are
   small scoring/reading changes; 64 is a sort. None of them needs a new signal.
