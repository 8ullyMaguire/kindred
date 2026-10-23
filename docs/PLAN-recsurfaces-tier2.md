# Kindred — recommendation surfaces, tier 2 (audit + plan)

Source: the owner's 100-idea list for recommendation surfaces and evaluation,
received 2026-10-22. This file exists because **most of the list was already
built**, and a roadmap that re-proposes shipped work is a document that makes
the project look unfinished.

The audit below was done by reading the tree, not the tier-1 plan. Every
"built" row names the code that does it. Every "planned" row says what is
missing and what it costs. Nothing here is claimed as shipped without a
command that shows it.

Status of the tier-1 plan (`PLAN-recsurfaces-tier1.md`): S1-S11 all shipped.
That plan covers ideas 1, 2, 3, 5, 6, 7, 8, 9, 10, 14, 56, 57 — a large part of
the "high score" half of the list.

---

## 1. Audit of the top of the list

| # | Idea | State | Evidence |
|---|---|---|---|
| 1 | Arena A/B for algorithms | **built** | `internal/store/algo.go`, `internal/api/algo.go`, `/algo`, `algo_comparisons` table |
| 2 | Similar tags sidebar on `/tag` | **built** (as PMI neighbours) | `tag.html` "Tags that travel with it"; `GET /api/v1/tags/{id}/similar` |
| 3 | Author recommendations by tag-profile cosine | **built** | `Engine.SimilarAuthors`, `GET /api/v1/authors/similar`, "Authors who write like this" on `/author` |
| 4 | Interleaved ranking with attribution | **shipped 2026-10-22** | `internal/rank/interleave.go`, `internal/store/interleave.go`, `GET /api/v1/interleave`, `/algo?mode=interleave` |
| 5 | "Readers who bookmarked this also bookmarked" | **built** | `Engine.CoBookmarked`, work page panel, `co-bookmark` tests |
| 6 | Per-session implicit taste vector | **built** | `session_taste` signal, `engine.Request.SessionTaste` (tier-1 S4) |
| 7 | Fandom-scoped arena | **built** | `arena.go` accepts `tag_id`/`fandom` (name or id), `arena_fandom_test.go` |
| 8 | "Why this author?" evidence | **built** | `author_tag_profile` evidence, `authors_test.go` |
| 9 | Offline eval harness (held-out bookmarks) | **built** | `internal/eval`, `kindred eval`, recall@K + NDCG@K |
| 10 | Popularity-dampened default sort | **partly built** | `quality` and `popularity` signals; no dampening exponent slider |
| 11 | Tag embedding nearest neighbours | **BUILT** | Candidate source (`pool_mode=tags+embed`, the default since 2026-10-23c on a measured +11.5% pool recall) AND the surface: `GET /api/v1/tags/{id}/similar?space=embed` plus the tag page’s “Tags that sit near it”, with `center_sim` and a note when the tag is too central for “nearest” to mean anything. Verified live: jasmine → aurora / aladdin / phillip / belle at 0.88–0.94. |
| 12 | Relationship recs via character co-occurrence | not built | needs character tags to be distinguished from relationship tags; the corpus has both as plain tags |
| 13 | Author leaderboard in the arena | **built** | `ao3_author` is an arena kind (`arena_kinds_test.go`) |
| 14 | "More like this" on every work card | **built** | `layout.html` workrow: "rank from this" → `/recommend?seed=` |
| 15 | Cross-kind "tags you'd like from arena history" | not built | arena tag weights exist; the cross-kind surface does not |
| 16 | `/eval` dashboard | **shipped 2026-10-22** | `eval_runs` table, `internal/store/eval.go`, `internal/web/eval.go`, `/eval` in `registry.go`; every `kindred eval` run persists with k and mode |
| 17 | Tag relationship types (rivals/complements) | not built | PMI edges only, unsigned |
| 18 | Session "not interested" soft block | **built** (as blocks + seen) | `/block` page, `blocked_tags`, seen marks |
| 19 | Fandom similarity matrix | not built | `/api/v1/ao3/tags/{id}/similar` covers co-occurrence, not fandom vectors |
| 20 | Glicko-2 confidence intervals on the leaderboard | **built** | `leaderboard.html` prints RD; ranking pages show it too |
| 21 | Tag PageRank | not built | one-time graph computation, no column |
| 22 | MMR diversity rerank | **built** | `engine.Request.Lambda`, diversifier tests |
| 23 | "Surprise me within fandom" | not built | `/surprise` takes no fandom parameter |
| 24 | RRF instead of weighted sum | not built | the weight vector IS the algorithm definition; RRF would replace the tune concept |
| 25 | Author tag entropy | not built | cheap, no surface |
| 26 | Work freshness signal | **built** | `recency` signal, exponential decay |
| 27 | Cross-kind arena "which tag fits this work" | not built | arena kinds are homogeneous pairs today |
| 28 | "Hidden gems" | **built** | `/underrated` corpus query |
| 29 | Embedding-based work similarity | **built** | `embedding` signal + embedding neighbours |
| 30 | Reader cohort clustering | not built | needs ~100 sessions; no clustering step |

Ideas 31-100 are audited row by row in **`PLAN-recsurfaces-31-100.md`**. What
that audit found, and what this grouping got wrong:

- **4 are built, not grouped**: 56 (IDF, `signal/idf_overlap.go`), 57 (bridges,
  `engine/bridges.go`), 63 (Jaccard/PMI), 70 (`/api/v1/arena/batch`).
- **12 are partial**, not "needs a signal": the input exists and the surface or
  the weight does not (32 completion filter without a rank term, 41 words filter
  without binning, 45 `tagfamily` without an alias table, 49 blocks without
  polarity, 66 the author API without a link, 79 MMR without a diversity metric).
- **50 are genuinely open** and 4 have no premise in this archive (51 series,
  97 haptics, 98 chapter arcs, 100 an agent).
- Two rows this section called "theatre" are now merely unbuilt, because
  `interleave_impressions` records what was shown: 60 (shown, never marked) and
  84 (the counterfactual is the other algorithm's pick).
- The sentence "31 trends, 32 completion sort, 40 reading path … are already
  covered" was wrong in five of nine cases: none of 31, 40, 50, 65, 67 exists.

## 1a. What is left, counted (2026-10-22, after tier 2 shipped)

Of the 30 ideas audited row by row: **18 built**, **1 partly built** (#10 —
signals exist, no dampening exponent), **11 not built**. Ideas 31-100 are
grouped rather than audited individually (see below); naming them one by one is
itself the next unit of work, because a status per idea is a claim that needs
the tree read, and a title-word grep proves nothing (measured: "follower",
"gallery" and "voice" hit nearly every file).

The 11 still open, with what each actually needs:

| # | Idea | What is missing |
|---|---|---|
| 10 | Popularity-dampened default sort | a dampening exponent (`score * kudos^-0.3`) applied to the default sort; deliberately not built while the `quality` signal is unmeasured |
| 11 | Tag embedding nearest neighbours | **enumeration shipped 2026-10-23b** (`engine/embedindex.go`, `pool_mode=tags+embed`); what remains is a reader-facing nearest-tags surface |
| 12 | Relationship recs via character co-occurrence | the corpus holds character and relationship tags in the same shape; telling them apart is a classification problem, not a query |
| 15 | Cross-kind "tags you'd like from arena history" | a surface that reads arena tag weights for a different kind |
| 17 | Tag relationship types (rivals/complements) | signed PMI: edges are unsigned today, so "rivals" cannot be expressed |
| 19 | Fandom similarity matrix | fandom-level vectors (the existing endpoint is tag co-occurrence) |
| 21 | Tag PageRank | one graph computation and a column to hold it |
| 23 | "Surprise me within fandom" | a fandom parameter on `/surprise`; cheap, and the only open idea that is a template-plus-argument |
| 24 | RRF instead of a weighted sum | a different ranking maths, which replaces the tune concept the whole site is built on |
| 25 | Author tag entropy | the statistic plus a decision about where it shows |
| 27 | Cross-kind arena "which tag fits this work" | heterogeneous arena pairs; every existing pair is same-kind |
| 30 | Reader cohort clustering | ~100 sessions and a clustering step; the deployment has neither yet |

## 2. What tier 2 implements, and why these

The north star in the source list is ideas 1 + 4 + 9. Idea 1 (arena A/B) and
idea 9 (offline eval) were already built. Idea 4 was the missing third, so it
is the first thing this tier builds:

1. **#4 Interleaved ranking with attribution — SHIPPED.** Both algorithms
   contribute slots to one list; a read mark credits the arm that showed the
   work. See SPEC §3.2.6. This is the online evaluation backbone, and it is
   the only one of the three that costs the reader nothing.
2. **#16 `/eval` dashboard — SHIPPED.** The offline harness already computed
   recall@K and NDCG@K and then threw the numbers away. The page is only worth
   building if the numbers persist, so this tier adds `eval_runs` and records
   every run.
3. **#11 Tag embedding nearest neighbours — enumeration SHIPPED 2026-10-23b.**
   The brute-force scan over 634k x 32 floats was ~5ms as predicted, but the
   "bulk load path" framing understated the problem: the matrix had to be
   resident (81 MiB, `sync.Once`) because a per-request read streams the whole
   table out of SQLite. It is now a candidate source in the pool, not a page.
   The measurement that decides whether it becomes the default is the next
   unit — see `docs/HANDOFF-2026-10-23b-embedding-enumeration.md`.

Two design rules carried over from tier 1, both learned the expensive way:

- **A surface that records nothing is not finished.** The merged `/algo` view
  asserts in its test that impressions land in the table, because a page that
  renders and records nothing produces no evidence and looks complete.
- **A rate needs its denominator printed beside it.** Interleave standings
  print slots and credits, not a bare percentage: the first version of the
  stats reported a rate over an unknown population, and the test that caught
  it compared the standing's impression count against the list's own exposure.
- **A reload is not a measurement.** The live browser check found the exposure
  sentence rendering "default contributed 3quality contributed 3" (a template
  range over a map with no separator), and thinking about what a refresh does
  turned up a worse problem: every reload added slots nobody could act on, so a
  reader holding F5 could drive any rate to zero. Impressions are now one per
  work per owner per hour, and a credited sighting does not block the next one.
  Both rules have tests that fail when the rule is removed.

## 3a. Live evidence (2026-10-22, kindred.polarisocial.xyz)

- `/healthz` -> `{"status":"ok","peak_rss_kib":271240,...}` on a process that
  used 264 MiB. Under the old cap this was a 503 and the deploy gate failed;
  the same process now reports the number and is healthy.
- `GET /api/v1/interleave?seed=ao3_work:10057010&a=default&b=quality&n=6` ->
  6 slots, exposure `{default:3, quality:3}`, 6 shared ids.
- A read mark on the item shown by `default` -> standings `default 3 slots / 1
  read / 0.333`, `quality 3 / 0 / 0.000`, pair difference `+0.333`.
- `kindred eval --users 40` recorded run #1: recall@10 **0.0067**, NDCG@10
  **0.0096**, coverage **0.0789** over 38 readers and 495 held-out bookmarks.
  That is a low number and it is reported as measured: the offline arm now has
  a baseline, and any weight change can be compared against it. It is the first
  honest answer to "is this recommender any good", and the answer is "not very,
  at k=10, on this corpus" — which is a starting point rather than a verdict.

## 3. Verification (what to run)

```sh
cd ~/code-local/go/kindred
go test ./... -count=1                       # 26 packages
go test ./internal/rank/ -run TestTeamDraft  # merge invariants
go test ./internal/store/ -run TestInterleave
go test ./internal/api/ -run 'TestInterleave|TestReadMarkCredits'
go test ./internal/web/ -run TestTheMergedAlgoPage
bash scripts/check-deploy.sh http://127.0.0.1:8019
```

Live check, after deploy:

```sh
curl -s 'http://127.0.0.1:8019/api/v1/interleave?seed=ao3_work:1&a=default&b=COPY&n=4'
curl -s http://127.0.0.1:8019/api/v1/interleave/standings
curl -s http://127.0.0.1:8019/eval
```

## 4. Open questions this tier does not answer

- **Should the read mark stay the only vote?** It is the strongest signal the
  deployment has, and it is rare. At the current traffic the standings will be
  slow to fill; that is honest, and the alternative (counting shown marks)
  measures the recommender's choice rather than the reader's.
- **Should impressions be logged for every recommendation, not just merged
  lists?** That would give #60 and #71 something to measure. It is a schema
  and volume decision, not a template one, and 30-day retention was chosen so
  the table cannot grow without bound.
- **Does the 220 MiB removal change the Pi story?** No: lite mode's 60 MiB
  target is still the design goal. What changed is that missing it is a
  measurement to act on, not a deploy-blocking verdict (SPEC §6).
