# Kindred — SPEC: Recommendation Surfaces & Evaluation (Tier 1, session target)

## What and why

The user asked for 40 candidate recommendation features at score = impact ÷ effort,
each one addressable as an engineering ticket. This spec takes the **Tractable Tier 1**
(score ≥ 8.9): 11 of them, enough to ship a real evaluation backbone and a dozen new
reading surfaces in one build cycle without touching the corpus schema or the engine's
signal layer. The other 29 are written into the doc as deferred ticket numbers
(`TIER2-X`) so they stay in the backlog, not lost behind an unrun plan.

The north star is **three pairwise evaluation mechanisms** (ideas 1, 4, 9): the A/B arena
kind (`algo_comparison`), a Team Draft interleaving wrapper over two algorithms
(`InterleaveRanking`), and a held-bookmark prediction eval harness on `user_work_interactions`
(offline). Every shipped feature is wired into one of these three.

## Scope boundary

**In this build cycle:** these 11 features only. Feature scope expandable in two ways:
(a) the deferred tickets can be pulled in when Tier 2 becomes Tier 1, and (b) the
spec reserves space at the bottom for a future expanded spec section `§10.2 Eval Extensibility`.

**Out of scope (explicitly deferred):** GNNs (§87), RL (§90), transformers (§92),
cross-lingual (§91), dream queries (§99). These need research infra not yet available.

## 11 shipped features

**THE AUDIT THAT CHANGED THIS TABLE.** Every one of the 11 was checked against the
tree before a line was written, and **two were already built**: S2 (the tag page
already renders "Tags that travel with it" from the CSR graph, via
`d.neighbours(ctx, id)` in `internal/web/handlers.go:1832`) and S8 (the "rank
from this" link already exists in the shared `workrow` template,
`internal/web/assets/layout.html:159`). Claiming them as new work would have
been a plan that lied about its own starting point.

A third, S4, turned out to be **half-built**: `arena_user_tag_weights` stores
per-reader tag preferences and `Store.TagWeights` reads them
(`internal/store/arena.go:896`), and the arena already uses them to build pair
preferences (`internal/api/arena.go:225`) — but the RECOMMENDER does not read
them. The session taste vector exists as a learned artefact and is simply not
wired into ranking. That is the cheapest real win in the set: the data is
already there, the writer already runs.

| # | Idea | Status after audit | Code path touched |
|---|---|---|---|
| S1 | Arena A/B framework | **SHIPPED (2026-10-22)** as a tune comparison — see §S1 | `internal/store/algo.go`, `internal/api/algo.go`, `internal/web/assets/algo.html` |
| S2 | Tag-page "Similar Tags" sidebar | **ALREADY BUILT** | — (verified, not rewritten) |
| S3 | Author recommendations via tag-profile cosine | NEW | `internal/signal`, `internal/api` (route) |
| S4 | Per-session implicit taste vector | **HALF-BUILT** — wire the existing `arena_user_tag_weights` into ranking | `internal/engine`, `internal/signal` |
| S5 | "Readers who bookmarked this also bookmarked" | NEW | `internal/api` (route), `internal/web/assets/work.html` |
| S6 | Fandom-scoped arena | NEW | `internal/api/arena.go`, `internal/web/assets/arena.html` |
| S7 | "Why this author?" evidence | NEW | `internal/signal` (evidence strings for S3) |
| S8 | "More like this" button on every work card | **ALREADY BUILT** | — (verified, not rewritten) |
| S9 | Offline eval harness `kindred eval` | NEW | `cmd/kindred/eval.go`, `internal/eval` |
| S10 | Weighted tag matching with IDF | NEW | `internal/signal` (idf-weighted overlap) |
| S11 | Cross-fandom bridge works | NEW | `internal/api` (route) |

S9 is in this cycle: the eval harness is the offline arm of the north star and
it is a pure computation over tables that already exist (`works`, `work_tags`,
`works.authors` bylines), so it needs no write path into the mirror.

**What the audit cost and what it bought.** Two of eleven features were found
already built and one half-built. That is a 27% reduction in the work, and it
arrived before any code was written, which is the whole argument for auditing
before planning. The features that remain are the ones the tree could not
answer.

## Data model additions

None beyond the current schema. All new reads hit existing tables:

- `entity_tags`, `edges`, `embeddings` for S2/S3/S11
- `seen_works`, `arena_*` for S4/S5/S7
- `kinds` for S6's `fandom` kind assertion

One new column worth noting even though unused: no schema migration is required
because every feature reads what already exists, unlike the embed_dim tunable which is
configured via env var (S10 does not need it either).

## Engine changes

S1 adds NO signal and no `SignalKindName`. That part of this spec was wrong: an algorithm is
a named TUNE — a weight vector the engine already ranks with — so there is nothing for
`signals()` to learn and nothing for the match-definer to recognise. S3 adds an `AuthorProfileSignal`; S7 reuses it. S4 reuses the
existing scoring infrastructure and adds a re-rank pass that drops session-dismissed works.
None of these touch the signal scoring itself — each adds one signal type and one MMR call site.

## Wiring to the three-star eval loop

| Component | What it measures | Online/Offline |
|---|---|---|
| **S1 A/B** | Reader preference between two rankers, explicit | Online, explicit |
| **S4 implicit taste** | Re-ranking improves clickthrough (login guarded) | Online, implicit |
| **S9 held-bookmark** (deferred) | Recall@K, NDCG@K | Offline, automatic |

Every S-feature ships with at least one of the three measures. S1 is explicitly #1. S4 is
the implicit measure. S9 fills the third arm and is a plain CLI command (`kindred eval`)
that takes a corpus, db and a held-out ratio — zero web surface, pure eval backend.

## Non-goals

- No new web pages beyond `tag.html` and `work.html` (sidebar + card button are partial
  modifications, new pages are just bigger edits). No redirect chains: `/work/{id}` continues
  to exist unchanged so SEO and bookmarks keep working.
- No write path changes to `ao3_metadata.db`: all ranking stays read-only. S4's session vector
  is in `kindred.db`, never the mirror.
- No seed list building: every recommendation uses `seeds from the request or the query`.

## Verification

`make check` is the gate: gofmt, vet, test, budget (see scripts/budget.sh), page sweeps
(render_pages_test.go asserts new templates carry the empty-state marker on both populated and
empty cases — non-obvious failure mode), gate coverage (scripts/check-deploy.sh assert every
new route is probed), and the goal check (docs/goal-check.py). Each feature has its own D-row
with the specific command + expected output the plan writes verbatim.

## S1 — the algorithm A/B arena, as shipped

An algorithm is a **named tune**: a weight vector over the scoring signals,
stored in `tunes` and set with `kindred tune set`. That is the registry this
deployment has, and it is the only definition that cannot disagree with the
engine: the ranking IS a weighted sum, so two algorithms differ exactly when
two weight vectors differ.

**Votes are in their own table.** `algo_comparisons` holds
(owner_key, seed_kind, seed_id, algo_a, algo_b, winner) where winner is
`a`, `b` or `neither`. It is NOT `arena_comparisons` with a strategy column,
and the reason is the constraint that deferred this surface:

- `arena_comparisons` records a preference between two CORPUS ENTITIES, and
  its judgements feed the tag learner. Choosing work A over work B is evidence
  about the tags both carry.
- An algorithm preference is evidence about WEIGHTS. Written into the arena's
  table, "this reader prefers the tune that weights recency" would be read as
  "this reader likes the tags shared by this pair of works".

`TestAlgoVotesNeverReachTheTagLearner` records votes and asserts that
`arena_comparisons` and `arena_user_tag_weights` do not move. It was proved
to bite by mutating the insert into `arena_comparisons`.

**The comparison.** `GET /api/v1/algo/compare?seed=kind:id&a=NAME&b=NAME`
resolves BOTH tunes before ranking (an unknown name is a 400 that names it),
refuses `a == b` (two identical columns produce a vote about nothing), and
ranks the SAME pool twice, so the only variable is the weights. The response
carries both lists, both weight vectors, and how many ids they share —
two lists that agree on membership but not order are still a comparison, and a
caller who cannot tell that apart from a real disagreement will misread the
vote.

**Draws are stored and count for both sides.** Dropping `neither` would
inflate every algorithm's win rate exactly as the reader grows indifferent,
which is when the numbers matter least.

**The page** (`/algo`) renders the two columns and the signals whose weights
differ, and judges through a plain form POST to `/algo/judge` — the same
control shape `/arena` uses, and for the same measured reason: a button with a
click handler has already made this project's arena control inert once.
