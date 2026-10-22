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

| # | Idea | Code path touched |
|---|---|---|
| S1 | Arena A/B framework — new `algo_comparison` kind via the existing arena schema | `internal/arena`, `internal/web/arena.html` |
| S2 | Tag page "Similar Tags" sidebar | `internal/web/tag.html` (+ `/api/v1/tags/{id}/similar` exists) |
| S3 | Author recommendations via tag-profile cosine similarity | new `Signal` author_profile + `/api/v1/authors/{id}/similar` |
| S4 | Per-session implicit taste vector | reuse `seen_works` + arena judgement rows → tag weight vector, re-rank hook |
| S5 | "Readers who bookmarked this also bookmarked" | `collab` matrix signal already computed; 1 SQL join on work_feedback |
| S6 | Fandom-scoped arena (URL param passthrough) | `/api/v1/arena/pair?fandom=`, template change, one query param |
| S7 | "Why this author?" evidence | add `author_tag_overlap` and `author_cosine` signals |
| S8 | "More like this" button on every work card | change `work.html` to link `/recommend?seed=ao3_work:{id}` |
| S9 | Offline eval harness `kindred eval` | new cmd `eval`, fold from existing ranked candidates against held-bookmark data |
| S10 | Weighted tag matching with IDF | one function `tag_idf(tag_ids)` reused by tag_overlap's Jaccard cap |
| S11 | Cross-fandom bridge works / pick a disliked fandom | reuse tag graph + exclusions; 1 endpoint |

Excluding S9 because it is conceptually simple but writes to the user_work_interactions table
which needs write permission review; it is deferred to this spec §10.2 and run as a follow-up
command in this cycle. S9 shipped in the same feature-bundle as S1 A/B because the harness is
the eval backbone; the user asked for the full pack though, so both are spec'd, S9's implementation
runs in the next turn once the flag wiring exists.

## Data model additions

None beyond the current schema. All new reads hit existing tables:

- `entity_tags`, `edges`, `embeddings` for S2/S3/S11
- `seen_works`, `arena_*` for S4/S5/S7
- `kinds` for S6's `fandom` kind assertion

One new column worth noting even though unused: no schema migration is required
because every feature reads what already exists, unlike the embed_dim tunable which is
configured via env var (S10 does not need it either).

## Engine changes

S1 adds a new `SignalKindName` string ("algo_comparison") to the match-definer so the arena
 recognises the new kind. S3 adds an `AuthorProfileSignal`; S7 reuses it. S4 reuses the
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
