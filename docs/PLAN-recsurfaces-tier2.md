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
| 11 | Tag embedding nearest neighbours | **not built** | embeddings exist (`kindred embed`); no endpoint scans them |
| 12 | Relationship recs via character co-occurrence | not built | needs character tags to be distinguished from relationship tags; the corpus has both as plain tags |
| 13 | Author leaderboard in the arena | **built** | `ao3_author` is an arena kind (`arena_kinds_test.go`) |
| 14 | "More like this" on every work card | **built** | `layout.html` workrow: "rank from this" → `/recommend?seed=` |
| 15 | Cross-kind "tags you'd like from arena history" | not built | arena tag weights exist; the cross-kind surface does not |
| 16 | `/eval` dashboard | **not built** | offline report prints to stdout; no persisted run, no page |
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

Ideas 31-100 were not audited row by row. They fall into four groups and the
grouping is the useful part:

- **Already covered by an existing surface** (31 trends, 32 completion sort,
  40 reading path, 50 author completion, 54 tag profile, 63 Jaccard, 65 new
  since last visit, 66 more-by-author, 67 defaults for new readers): each is a
  small query plus a template, and each is a separate decision about where it
  goes rather than a new mechanism.
- **Needs a signal that does not exist yet** (33 multi-seed intersection, 38
  quality-weighted co-occurrence, 39 Bayesian tag quality, 41 length
  bucketing, 42 confidence, 48 reader velocity, 49 tag polarity): cheaper than
  they look, because signals are pure functions with a registry, but each one
  needs an offline eval run before it earns a weight.
- **Needs machinery this project deliberately does not have** (52 sentence
  embeddings, 59 t-SNE map, 74 random walks, 82 bandits, 87 GCN, 89 ALS, 90
  RL, 92 LLM explanations): a model, a training loop, or a runtime dependency.
  The HTTP-only, no-JavaScript, single-binary constraint (SPEC §1.1) makes
  most of them a different project.
- **Rejected as measurement theatre** (60 "works you keep avoiding" needs
  impression logs that only exist for interleaved lists; 71 per-tag rec
  quality needs the same; 76 embedding drift needs a baseline per run; 84/85
  counterfactuals and causal inference need randomised assignment at a scale
  this deployment will never have). Building these would produce numbers
  nobody could act on.

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
3. **#11 Tag embedding nearest neighbours — planned, next.** The embeddings
   exist and a brute-force cosine scan over 634k x 32 floats is ~5ms. What is
   missing is a bulk load path (the current `Engine.Embedding` is one row per
   call), so this is a storage decision before it is a surface.

Two design rules carried over from tier 1, both learned the expensive way:

- **A surface that records nothing is not finished.** The merged `/algo` view
  asserts in its test that impressions land in the table, because a page that
  renders and records nothing produces no evidence and looks complete.
- **A rate needs its denominator printed beside it.** Interleave standings
  print slots and credits, not a bare percentage: the first version of the
  stats reported a rate over an unknown population, and the test that caught
  it compared the standing's impression count against the list's own exposure.

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
