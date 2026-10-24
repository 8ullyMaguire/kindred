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
| 31 | Tag trend detection (90-day vs corpus average) | BUILT | `ModeTagTrend` / `*Runner.TagTrend` — the fifth runner and the first that compares time windows. Rows are the tags travelling WITH the seed, ranked by `lift = (pair's growth) / (corpus's own growth)`, so a trope doubling because the corpus doubled lifts to 1.0. Three things had to be true and each cost a round: the window is anchored on the MIRROR'S newest date (2026-10-15 here, not today — a wall-clock window is empty); dates are normalised in SQL because 112,893 rows read `05 Sep 2007` and `"31 Oct 1999" >= "2020-12-12"` is TRUE lexically; and the window WIDENS until it holds enough works (90→180 days here, 2 → 16,153 works) instead of computing lifts from a handful. Live: corpus grew 0.14x, top pair `ryland grace × project hail mary (2026)` lifts 6.32 (19 of 21 works inside the window). Reachable as `--mode tag-trend --tag angst` and `GET /api/v1/corpus-query/tag-trend?tag=angst`. `/eval` still has no time slice. |
| 32 | Completion-aware ranking | PARTIAL | Completion is a tri-state **filter** (`engine.Request.Complete`: `CompleteAny`/`CompleteOnly`/`CompleteWIP`) and a tag-page sort; no tune carries a completion weight. |
| 33 | Multi-seed intersection scoring | BUILT | `signal.SeedAgreement`: IDF-weighted coverage of each seed, averaged over seeds INCLUDING zeros, so a perfect match on 1 of 4 seeds scores 0.25 and partial matches on all 4 score 0.625. Skips (and names itself in `meta.degraded`) with fewer than two usable seeds, because one seed is `idf_overlap` under a second name. Weight 0.10, and the ablation is a flag (`kindred eval --tune ablate_no_agreement`). |
| 34 | "Works this tag needs" gap analysis | BUILT | `corpusquery.TagGap` + `GET /api/v1/corpus-query/tag-gap`: two hops (seed → its 30 highest-ASSOCIATION tags → the tags those works carry and the seed's do not), ranked by `(enrichment−1) × (1−lift)` with support as the evidence gate. Rows print own/corpus/lift/both rates so any row is checkable by hand. Three popularity traps were fixed in sequence (support as the score, a neighbourhood of archive-wide tags, candidates chosen by support); the live archive's top rows are still rating/meta attributes, and the documented fix is a fandom-anchored neighbourhood. 125 gate checks; the query is 14 s (NOT EXISTS as a materialised CTE, 600→150 candidates). See `docs/HANDOFF-2026-10-24j-taggap.md`. |
| 35 | Author-author co-readership | OPEN | `/api/v1/authors/similar` is **IDF-weighted tag-profile overlap** (`engine/authors.go`), not a co-bookmark matrix aggregated to authors. |
| 36 | Session taste vector export/import | OPEN | No `/api/v1/taste`; session taste is derived per request (`signal/session_taste.go`) and never serialised. |
| 37 | Tag hierarchy from embedding clusters | OPEN | `internal/tagfamily` holds a hand-written `families.json`; nothing clusters the embeddings into a tree. |
| 38 | Quality-adjusted co-occurrence | PARTIAL | Co-occurrence edges are shrunk by `DefaultShrinkK` (`collab.go`) — evidence weighting, not per-work kudos weighting. |
| 39 | Bayesian average for tag quality | BUILT | The tag page printed `mean kudos` over as few as three works — a number that reads as a quality claim and is really one hit. Now a fourth number, shrunk toward the corpus mean: `(sum + 10*prior)/(n + 10)`, with the prior and the pseudo-work weight printed beside it (`data-prior`, `data-weight`). The prior excludes zero-kudos works (112,937 works, most unkudosed — the all-in mean would shrink every tag toward "nobody liked this") and is cached per process on a detached context. Symmetric by construction and tested both ways: a tag worse than the corpus is pulled up. No ranking consumes it — `quality` already measures a work's own kudos, and a second tag-level quality dimension arrives with a measurement or not at all. |
| 40 | Ordered "reading path" | BUILT | `GET /api/v1/works/{id}/path?steps=N` and `/path/{id}`. Each step is chosen against the PREVIOUS work (`sim_to_prev - 0.5*sim_to_seed`), so the list drifts instead of describing the seed N times; every step publishes both distances. Measured on work 17249318: seed distance falls 0.966 -> 0.896 -> 0.798 -> 0.701 over four steps while hops stay 0.89-0.97, against 0.966 flat for plain nearest-neighbour. |
| 41 | Length binning for similar-length recs | BUILT | A `length_fit` SIGNAL, not a bin: `score = min(words, seedsMedian) / max(words, seedsMedian)` over up to 40 seeds (the ratio of the smaller length to the larger — 1.0 at the median, 0.5 at twice or half, 0.06 at sixteen). A seed with no word count is silence, not a zero-length work; an unknown-length work and a request whose seeds all lack lengths both SKIP into `meta.degraded[]` rather than scoring 0. Registered at 0.08 (a real weight: `rank.Score` skips weight-0 signals, so a dimension shipped at zero never runs and never names itself). MEASURED, 37 lists, both tunes summing to 1.0: `length_fit` 0.713 with vs 0.575 without (+24% relative), recall 0.1063 vs 0.1035, nDCG 0.1888 vs 0.1953 (−3%), diversity identical (overlap 0.099, 2.2 fandoms). The eval grew `length_fit` + `length_runs` (unmeasured is never 0.0) and `--tune` carries the ablation. THE ABLATION FOUND A REAL DEFECT: `diversify.Apply` mixed `lambda*score` with a [0,1] Jaccard, so the tune vector's absolute SCALE changed the ranking — identical ratios at sum 1.66 vs 1.0 gave recall 0.1050 vs 0.1152, while `kindred tune` refuses any vector that does not sum to 1.0. Scores are normalised by the list's top score now; the invariant test was red first ([1 3] vs [1 2] at half scale) and the product-level proof is runs #21 and #24 agreeing to four decimals at two different scales. |
| 42 | Calibrated recommendation confidence | BUILT | The recommendation page now says what the ORDER is worth, measured rather than asserted. `kindred eval` records, per position 1..k, how many readers' lists reached it and how many of those held a work the reader later bookmarked (`eval_positions`, written by every run); `/recommend` prints three bands of it with the run id, and `GET /api/v1/calibration` serves the unrounded numbers and every position so the page's "26%" can be checked against 29 of 111. LIVE (run #18, 37 lists): first three 26% (29/111), next four 14% (21/148), the rest 9% (10/111) — the tail is 3x worse than the top, not 10x, and position 1 alone is 32%. Three facts the block refuses to merge: no run says "not calibrated yet" and names the command (never 0%), a band nothing reached prints an em dash with its counts, and a run under 20 lists prints no percentage at all. The LABEL IS THE RATE — "strong"/"speculative" would be an opinion the data does not contain. The reader count is derived from position 1 rather than from the split, so it cannot drift from the denominators. 8/8 mutants killed. |
| 43 | Anti-recommendations | BUILT | The premise was FALSE as of this session: negatives flow from two writers, and both were verified on the deployed binary. (1) An arena pair judgement calls `arena.LearnTags(byWorks, winners, losers, …)`, which gives the losing work's tags NEGATIVE deltas (`api/arena.go:326`), and (2) the new `/skipped` page's "Not interested" writes polarity −1 through `LearnFromFeedback` (`store/feedback.go:231`). `session_taste` accepts a negative value by construction (`signal/session_taste.go` clamps to [−1,1] and its own comment says a negative must not be clamped away). MEASURED on the live binary: after one "Not interested" on work 17249318 the reader held 74 tag weights at −0.081, and `/api/v1/recommend?seed=ao3_work:17249318` returned five candidates whose session_taste evidence was value −0.732/−0.448/−0.529 · weight 0.14 = **−0.1025 / −0.0627 / −0.0740** — the ranking is actively reduced, not merely unattributed. What is NOT built: a *magnitude* measurement (the eval harness has no session-taste user model, so demotion is proven to exist and not proven to be enough) and work-level negation independent of tags. |
| 44 | Seasonal/temporal context | OPEN | No tag→month mapping and no calendar term. |
| 45 | Tag canonical form resolution | PARTIAL | `tagfamily` (embedded `families.json`) plus `fandom.base()` collapse variants; there is no alias table and the index is never merged. |
| 46 | Per-fandom quality tracking | BUILT | `eval_slices` + `eval.ByGroup` from per-work `Placements`; `/eval` prints the newest run's weakest fandoms first (groups of ≥20 held-out works). |
| 47 | Work cluster labels | OPEN | No work-level clustering; embeddings are `kind='tag'` only. |
| 48 | Reader velocity signal | OPEN | The session model now exists: `reader_visits` (three instants per reader — `previous_at`, `current_at`, `last_at`) and `MarksSince`, which `/new` prints ("2 work(s) marked read since then, across 1 visit(s)"). The SIGNAL is deliberately not built, and the reason is measured, not assumed: a weight needs a distribution, and the live state database on 2026-10-23 held **33 marked works across 8 owners and 8 reader-days — no owner with five, max 13 marks in one day**. Fitting a weight to that is fitting noise, and this project refuses unmeasured weights. The number that would justify the next step: ≥20% of owners with ≥5 reader-days and a spread between the 90th and 50th percentile mark rate wide enough to change a ranking. Mutant killed: the boundary ignored, so "since your last visit" counted every mark ever made.  (the count half is built; the weight is not)  |
| 49 | Tag polarity detection | PARTIAL | **Remaining half declined 2026-10-24:** `arena_user_blocked_tags` holds 0 rows, so there is no corpus-wide polarity to infer. Blocking itself is built and honoured in ranking; the row is not completed. A reader can block a tag (`arena_user_blocked_tags`, `POST /block`) and blocks are honoured in ranking; nothing infers polarity corpus-wide from block/bookmark ratios. |
| 50 | "Complete your collection" for an author | BUILT | `/author?q=` prints "you have read **2** of **4** — **2** to go" for a reader with a session, and `?unread=1` lists the rest ("Showing 2 of 2"). `store.ReadAmong` reads the read set for a bounded id list (400 per statement) because `works` (corpus) and `seen_works` (state) are different SQLite files with no possible join. Three exclusions: a work only SHOWN is not read; another reader's reads are not this one's; and no session means NO CLAIM — an anonymous reader gets no line at all rather than "0 of 4". The count is over the shelf (capped at 5,000 ids, and the page says "counted over the first 5,000" when it hits the cap), not the page: "2 of 20" where 20 is the page size is a number about pagination. `OwnerForSession` is not `TouchSession`: rendering a list must not look like activity. LIVE: signed in, two of Titus621's four works marked read → data-read="2" data-total="4", "2 to go", and ?unread=1 answered "Showing 2 of 2" with exactly the two unread ids. Mutants killed: shown counted as read, filter inert, filter unannounced, shelf counted as one page. |
| 51 | Series-aware recommendations | NO PREMISE | No series column or table; AO3 series metadata is not crawled. |
| 52 | Writing-style similarity via summary embeddings | OPEN | Embeddings are tag-level (`embeddings.kind='tag'`); there is no text model and no summary vector. |
| 53 | Bradley-Terry vs Glicko | BUILT | `arena.FitBT`: MM fit on the SAME judged comparisons with the SAME agreement weights, Jeffreys prior (half a win + half a loss vs the field) so a perfect record stays finite — the MLE is +Inf there, and a work compared once and preferred is the commonest row. Reported on `/leaderboard` as one number, Spearman(BT strength, Glicko-2 effective) with its counts, not as a second table. The live arena's 16 comparisons over 15 works is flagged "read this as unmeasured" by the same `< 3 per item` rule the significance test uses. `prior = 0` is exposed for the pure MLE and is scale-invariant to 1e-9; with the prior, duplication sharpens — both properties tested, because my first test asserted invariancy of the shrunk fit and failed against correct code. |
| 54 | Rich tag profile page | BUILT | `/tag/{id}` gained "This tag in numbers": median and mean length and kudos, total words, the most-saved work, and a trend by update year (12 years, bars scaled to the busiest). Medians with the mean beside them, because AO3 is skewed — live tag 29 reads median 30,603 words against a mean of 76,222, and 105 kudos against 201. The trend is by UPDATE year and says so (no first-published column exists here) and undated works are counted out loud. Two defects the tests found: `work_tags` is keyed by (work_id, tag_id, tag_type) so a work carrying one tag under two types was counted TWICE (43 where there are 42 — every query now starts from `SELECT DISTINCT work_id`), and the fixture writes ONE update date for every work, so "oldest-first" and "share of total" both SURVIVED until the trend test stamped its own years. Computed in one pass: the SQL alone is 67 ms, the page 0.53 s for sort=kudos. 5/5 mutants killed. |
| 55 | Explanation quality evaluation | OPEN | `/algo` compares two tunes' **result lists** and records a preference; two explanations are never shown side by side. |
| 56 | Weighted tag matching with IDF | BUILT | `signal/idf_overlap.go`, plus document-frequency weighting in `engine/authors.go`. |
| 57 | Cross-fandom bridge works | BUILT | `engine/bridges.go` + `/api/v1/works/{id}/bridges`, kudos-capped scan, other-fandom names as the reason. |
| 58 | Productivity vs quality scatter | DECLINED | **Declined 2026-10-24:** a chart of two numbers `/author` already prints. What follows is the premise that was true before the decline. No scatter surface; `/fandoms` and `/author` print numbers, not a plot. |
| 59 | t-SNE/UMAP tag map | DECLINED | **Declined 2026-10-24:** a projection is a picture and not a ranking; 634k points need downsampling and a library the repo does not have. What follows is the premise that was true before the decline. No projection and no `/tag-map`. |
| 60 | "Works you keep avoiding" | BUILT | `/skipped` + `store.SkippedWorks`. Reads `interleave_impressions` (one row per work per showing) — and does NOT read `algo_impressions`, which stores a whole list as CSV per row, so grouping it by work would mean parsing CSV in SQL. Three exclusions, each a claim that would otherwise be false: shown ≥3 separate times (one unclicked list is a coincidence), no credited showing, and not already marked read (a read from the bookmark screen never touches this log). Two actions per row: "Not interested" (polarity −1 through `LearnFromFeedback`, the one path that can make a work rank LOWER) and "I read it". LIVE: seeded 3 staggered showings for a real owner → the row rendered "shown 3 times, last on 2026-10-23 18:45:15"; the POST wrote polarity −1 and 74 tag weights, and the row disappeared from the list. Mutants killed: threshold ignored, credited showing still counted, read works offered, owner filter dropped. |
| 61 | RSS feed of recommendations | BUILT | `/feed/tag/{id}` (works under a tag, newest first) and `/feed/seed/{kind}/{id}` (the ranking for one seed), both RSS 2.0 with absolute guids. Ordering is `corpus.ChronologicalUpdateDateDesc`, because the mirror stores AO3's "31 Oct 2025" and a string sort names 31 Oct 2025 the newest work when 26 Jun 2026 is. The tag page discovers its feed via `rel=alternate` and links it in words. Two defects the DEPLOYED feed showed: the seed came first in its own feed (`Request.Exclude` defaults false; only the API handler set it), and a work with an empty `update_date` rendered `<pubDate></pubDate>`, which readers parse as a date. `internal/api/feed.go`, `feed_test.go`. |
| 62 | OpenSearch descriptor | BUILT | `/opensearch.xml`, OpenSearch 1.1, one Url pointing at `/search?q={searchTerms}`. A second `format=rss` Url was removed: `/search` has no format parameter, so the browser offered a subscription that returned HTML — a promise the reader sees broken. The test counts the Urls. |
| 63 | Jaccard co-occurrence similarity | BUILT | `signal.go` computes and prints a Jaccard ratio; `TagNeighbours` ranks by PMI; `idf_overlap.go` is the weighted variant. |
| 64 | "Tag DNA" distinctive tags | BUILT | `internal/web/distinctive.go` + the work page's "What makes this work distinctive" block: rarest by document frequency, a tag on every work dropped (zero evidence), a zero-frequency tag dropped (the table and the work disagree). |
| 65 | "New since your last visit" | BUILT | `/new`, backed by `reader_visits` (previous_at = the boundary, current_at = the visit's start, last_at = when we last heard from them). Both halves of `seen_works` are excluded — read AND shown. A first visit says there is no "before" instead of using zero time, and the page prints the boundary it measured from. Live: boundary 1 May 2026 → 40 works, newest first; marking one seen removes it and the list refills. |
| 66 | Author recs from a single work | BUILT | `/work/{id}` names its author and links each byline to `/author?q=` (`data-testid="work-byline"`), and renders "Authors who write like this one" from the same engine call `/api/v1/authors/similar` serves. The row said the endpoint had no entry point; the page did not name its author anywhere at all, so the byline is the part that was missing. `corpus.Byline(ctx, id)` exists because `corpus.Entity` deliberately carries no author string: every ranking path holds tens of thousands of entities and nothing in the ranking reads one. Live on `/work/17249318`. |
| 67 | Smart default seeds for new readers | BUILT | `/recommend` with no seed ranks from the most-kudosed work (`corpus.AO3.TopByKudos`; ONE, because three from three fandoms measured 4.2 s a page against 2.6 s for one) and names it as popularity in `data-testid="seed-origin"`; no corpus still renders the empty form. |
| 68 | Tag "temperature" (contested tags) | DECLINED | **Declined 2026-10-24:** needs a per-tag judgement stream; the live arena holds 149 comparisons and 0 algo comparisons. What follows is the premise that was true before the decline. No per-tag variance over comparisons. |
| 69 | Work controversy score | DECLINED | **Declined 2026-10-24:** needs per-entity comparison history; the arena has 149 comparisons in total. What follows is the premise that was true before the decline. `arena_entity_history` holds per-entity history; nothing computes a divisiveness score from it. |
| 70 | Batch arena with keyboard shortcuts | BUILT | `/api/v1/arena/batch` + the speed round in the arena UI. |
| 71 | Per-tag recommendation quality leaderboard | BUILT | Same table, `axis = 'tag'`: every tag a held-out work carries, weakest first, top 25 by size per run. |
| 72 | Embedding dimension search | PARTIAL | `kindred embed --dim` exists (default 32, `embedcmd.go:47`); eval runs never vary it, so no width is compared. |
| 73 | Multi-objective Pareto ranking | DECLINED | **Declined 2026-10-24:** needs a second objective a reader actually weights; with one scalar the front is decoration. What follows is the premise that was true before the decline. One scalar score; no dominance relation. |
| 74 | Random walks (personalised PageRank) | BUILT | `signal.Walk`: bounded personalised PageRank over the tag graph (damping 0.85, 20 rounds, 20k-node frontier, restart mass excluded so it is not a slower `tag_overlap`), plus `walkpool.go` — the pool source without which a two-hop signal only re-ranks one-hop candidates. Measured over 37 lists at k=10: walk 0.25 is WORSE than not having it (recall 0.1004 vs 0.1063, nDCG 0.1556 vs 0.1888); 0.06 and 0.12 reorder lists without moving recall and are indistinguishable from each other (nDCG 0.1889 vs 0.1901), so the default stays 0.06 pending a difference that clears a bar. `pool_sources.walk` reports its contribution (152 for a 169-work seed pool). |
| 75 | Cross-fandom trope transfer | OPEN | Bridges surface works that **are** in both fandoms; "same non-fandom tags, a different fandom" is a different query and no runner does it. |
| 76 | Embedding drift monitoring | BUILT | `embed_runs` + `store.RecordEmbedRun`, written by `kindred embed` after the table lands; `/mirror` prints the verdict; `GET /api/v1/embed/drift`. A fingerprint, NOT a hash: a hash says "something changed" and cannot say whether the space moved or merely reshuffled. Each run stores rows, width, mean L2 norm, the centroid, and a SAMPLE of 64 tag vectors — the sample is load-bearing, because two tags moving in opposite directions leave the centroid exactly where it was (`TestAnAverageCanHideOppositeMovements`; the centroid-only mutant dies on it). Floor 0.98, not 0.9: a deterministic solver over an unchanged corpus landing at 0.95 has moved positions rather than re-rolled a seed. Two refusals: the page will not print "0% drift" when fewer than two fingerprints exist (no comparison ≠ good result), and a failed fingerprint does not fail the build (the embeddings are correct; a monitoring gap must not become a broken deploy) — it is printed by name. Mutants killed: floor 0, centroid-instead-of-sample, width change not its own verdict, sample compared against itself. |
| 77 | Tag suggestions for writers | OPEN | No writer surface. |
| 78 | Work-pair similarity API | BUILT | `GET /api/v1/works/{id}/similarity/{other}` and `/work/{id}?compare={other}`. Two components, published separately: IDF-weighted Jaccard over the tag sets and the cosine between the works' tag centroids; the shared tags travel rarest-first, and every axis that could not run is named in `missing`. Popularity/recency are deliberately NOT components. Live: works 17249318 vs 65141041 share 18 tags, overlap 0.111, cosine 0.869, score 0.490. |
| 79 | Recommendation diversity metric | BUILT | Every `eval_runs` row now carries `tag_overlap` (mean pairwise Jaccard inside each reader's returned list; 0 = no shared tags, 1 = one work repeated), `distinct_fandoms` (using the same shape-based grouper the per-group cap uses — a `tag_type='fandoms'` key matches nothing on this mirror) and `diversity_runs` (how many readers they cover, so "not measured" is distinguishable from "perfectly diverse"). Averaged per reader and bounded by the same k as recall. `kindred eval --lambda 1` is the ablation that makes them mean something. MEASURED on the live mirror, 40 readers, k=10: lambda 0.75 → recall 0.1050, NDCG 0.1959, overlap 0.086, 2.4 fandoms; lambda 1.0 → recall 0.0953, NDCG 0.1766, overlap 0.141, 1.8 fandoms. The surprise: turning diversification OFF made recall WORSE, so at 0.75 the MMR pass is not a trade on this corpus, it is a straight win on both axes. 8/8 mutants killed, including the duplicate work counted as a pair (which would score a repeated list as maximally diverse). |
| 80 | Seasonal tag auto-detection | OPEN | No temporal frequency model over tags. |
| 81 | Federated taste profiles | DECLINED | **Declined 2026-10-24:** no second instance exists — `dump`/`fetch` move mirror snapshots, and a taste exchange with no peer is a format nobody consumes. What follows is the premise that was true before the decline. `dump`/`fetch` move mirror snapshots (`cmd/kindred/dumpcmd.go`); no taste vector is exchanged. |
| 82 | Bandit-based exploration | DECLINED | **Declined 2026-10-24:** no reward stream: 0 algo comparisons, 149 arena comparisons, 108 reader visits. What follows is the premise that was true before the decline. No exploration policy and no reward stream. |
| 83 | Transfer from AO3's wrangling graph | DECLINED | **Declined 2026-10-24:** the mirror carries no AO3 wrangling or synonym data; `families.json` is 83 hand-written pairs. What follows is the premise that was true before the decline. `families.json` is hand-written; no synonym supervision and no contrastive training. |
| 84 | Counterfactual evaluation | BUILT | Two-sided Fisher exact on each interleaved pair (`internal/stats`), plus a 50-slot power floor that refuses a winner before any p is quoted; `/eval` says "no result yet" until a pair clears it. IPS is deliberately NOT built: this recommender is deterministic, so propensity is 1 for served items and 0 for the rest — no overlap, and the estimator would be the identity. Interleaving is the design that answers the question without propensities; what it lacked was the test, not the weighting. |
| 85 | Causal inference for impact | DECLINED | **Declined 2026-10-24:** no instrument and no counterfactual log; IPS was tried here and is vacuous on a deterministic policy (propensity 1/0, estimator = identity). What follows is the premise that was true before the decline. No instrument, no estimator. |
| 86 | Bandit weight optimisation | DECLINED | **Declined 2026-10-24:** no vote stream: `algo_comparisons` is empty, so there is no reward to optimise. What follows is the premise that was true before the decline. Weights are hand-set per tune (`store/tunes.go`) and compared by `/algo` votes. |
| 87 | Graph neural network | DECLINED | **Declined 2026-10-24:** needs a training pipeline and a tensor runtime; the repo is pure Go with no GPU. What follows is the premise that was true before the decline. Embeddings are truncated SVD by power iteration (`embedcmd --iters`), which is the thing a GCN would replace. |
| 88 | Attention-based multi-seed fusion | DECLINED | **Declined 2026-10-24:** needs learned per-signal weights and a data volume the 37-list eval cannot judge. What follows is the premise that was true before the decline. Multi-seed handling is per-signal best-seed, not learned weights. |
| 89 | Implicit-feedback matrix factorisation (ALS) | BUILT | `internal/als` (closed-form implicit ALS, c = 1 + 40·p, 32 factors, Cholesky per row, `YᵀY + α·Σy yᵀ` shared per sweep), `work_factors` in the state DB, `signal.Latent` (max-over-seeds cosine, loaded lazily), `kindred als`, and `kindred eval --als-honest`. **Measured 2026-10-24, and the first number was a leak**: with factors trained on the whole mirror the signal scored recall 0.1063 → 0.1373 / nDCG 0.1889 → 0.2702, which is the model recognising the held-out bookmarks it was trained on. Honest (`--als-honest`, 112,703 factors on 180,182 pairs, 495 held-out pairs excluded, 20.6 s): 0.1130 / 0.1861 at weight 0.08 and 0.1071 / 0.1832 at 0.16 — +2.5 hits over 380 slots and a small nDCG loss, inside the noise. **Weight stays 0** (reachable via `--tune als08`). Why: ALS is a lossy re-encoding of the evidence `collab` already holds exactly, and collab is weighted 0.16. |
| 90 | Reinforcement learning for sequences | DECLINED | **Declined 2026-10-24:** no session MDP: 108 `reader_visits` rows is not a trajectory dataset. What follows is the premise that was true before the decline. No session MDP. |
| 91 | Cross-lingual recommendations | DECLINED | **Declined 2026-10-24:** one language dominates the corpus and there are no multilingual vectors. What follows is the premise that was true before the decline. One language in the corpus; no multilingual vectors. |
| 92 | LLM-generated explanations | DECLINED | **Declined 2026-10-24:** no runtime LLM by design; generated prose would be unmeasurable and unverifiable here. What follows is the premise that was true before the decline. Explanations are structured evidence strings (`Evidence.Reason`), never generated prose. |
| 93 | Fairness auditing | OPEN | No group attributes are collected, by design. |
| 94 | Social recommendation layer | DECLINED | **Declined 2026-10-24:** no cross-instance identity graph. What follows is the premise that was true before the decline. No cross-instance identity and no social graph. |
| 95 | Work lifecycle prediction | DECLINED | **Declined 2026-10-24:** the mirror is a snapshot; `first_seen` + `update_date` is two observations, not a time series. What follows is the premise that was true before the decline. No per-work crawl time series to fit. |
| 96 | Variable embedding dimension per tag | DECLINED | **Declined 2026-10-24:** fit and storage cost for a precision win nothing here can measure. What follows is the premise that was true before the decline. One width for every row (`embeddings.dim`). |
| 97 | Haptic mobile arena | NO PREMISE | The product is HTTP-only with no JavaScript (SPEC §1.1) and there is no mobile app. |
| 98 | Narrative arc matching | NO PREMISE | The mirror stores no chapter structure. |
| 99 | Natural-language query | DECLINED | **Declined 2026-10-24:** needs a model to map prose onto the query language; the structured language already exists. What follows is the premise that was true before the decline. The query language is structured (`tag:`, `words:`, `kudos:`); no model maps prose onto it. |
| 100 | Autonomous recommendation agent | NO PREMISE | No runtime, no product basis, and it would need a taste model that does not exist. |

## Counted

| Status | Count | Ideas |
|---|---|---|
| BUILT | 30 | 31, 33, 34, 39, 40, 41, 42, 43, 46, 50, 53, 54, 56, 57, 60, 61, 62, 63, 64, 65, 66, 67, 70, 71, 74, 76, 78, 79, 84, 89 |
| PARTIAL | 5 | 32, 38, 45, 49, 72 |
| OPEN | 12 | 35, 36, 37, 44, 47, 48, 52, 55, 75, 77, 80, 93 |
| DECLINED | 19 | 58, 59, 68, 69, 73, 81, 82, 83, 85, 86, 87, 88, 90, 91, 92, 94, 95, 96, 99 |
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
2026-10-23h shipped #60 ("works you keep skipping") and #76 (embedding drift
fingerprints), and settled #43 by MEASUREMENT rather than by building: negatives
already flowed from arena pair judgements, and the new "Not interested" action
gives a reader a direct one.

2026-10-23i shipped #66 (the byline as links, authors who write alike), #50
("complete your collection") and #54 (a tag's shelf in numbers). See
`docs/HANDOFF-2026-10-23i-byline-and-shelf.md`.

2026-10-23j was a measurement pass rather than a row: the unowned latency item is
closed. `/api/v1/recommend` answers a repeat request in 0.41 s (was 2.62 s) because
the tag-pool query — 87% of the work, per a CPU profile — is now memoised, and
`n=200` answers in 0.36 s (was 10.39 s) because the MMR pass was quadratic in the
number of results. `/tag/{id}` is 0.92 s warm (was 3.67 s). See
`docs/HANDOFF-2026-10-23j-latency.md`.

2026-10-23k shipped #79 (intra-list diversity, stored with every offline run) and
corrected this file's totals. 2026-10-24b/c/d shipped #41 (length_fit), #84 (the significance test the interleave log
was missing), #74 (personalised PageRank), #39 (the tag shelf's shrunk kudos) and #53
(the Bradley-Terry cross-check) and #31 (tag trend). **The counts below are the table's,
not a memory of them: 30 built, 5 partial, 12 open, 19 declined, 4 without a premise.**

**Every remaining row was triaged on 2026-10-24 against the data actually in the mirror
(`docs/HANDOFF-2026-10-24h-triage.md`), and the verdict is now a STATUS: `DECLINED` means
the data cannot support the row, which is a different statement from `OPEN` (work
waiting). Nineteen rows declined; each reason is in its own row and in the triage. Most
are blocked by data the project does not have rather than by effort — no judgement stream
(149 arena comparisons, **0 algo comparisons**, 108 reader visits), no wrangling data, no
second instance, no training pipeline. The 13 open and 6 partial rows are what is worth
building. Read that triage before starting from this table; the table records status, and
status is not a reason. For two sessions this
block said "38 built, 49 open" — arithmetically consistent with 100 rows and wrong
by about thirty, because this roadmap holds rows 31–100 (70 rows) and the phantom
thirty made the backlog look half finished when it is a third done. The lesson is
the same one the deploy gate keeps teaching: a total that is not derived from the
thing it counts is a claim, not a measurement.

What #79 measured, on the live mirror, 40 readers at k=10:

| lambda | recall@10 | NDCG@10 | tag overlap | fandoms per list |
|---|---|---|---|---|
| 0.75 (default) | 0.1050 | 0.1959 | 0.086 | 2.4 |
| 1.0 (diversification off) | 0.0953 | 0.1766 | 0.141 | 1.8 |

Turning diversification OFF made recall and NDCG WORSE while making lists more
repetitive — so at 0.75 the MMR pass is not the recall-for-diversity trade the
design assumed. On this corpus it is a straight win on both axes, because the
top of the relevance-only ranking fills with near-duplicates that crowd held-out
works out of the ten.

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
