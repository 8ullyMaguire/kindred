# PLAN — owner-weighted taste everywhere, and arenas for every entity

> Implements the owner's request (verbatim): *"take into account I want my
> taste to influence recommendations for all users; the arena should work in a
> way that it should be influenced mostly by users with a similar taste to
> mine, if users with opposite taste didn't vote the changes should be minimal.
> there should be arenas for all ao3 entities, tags, authors, etc, not just
> works."*
>
> Source context: `docs/full-context.md` (the owner's compiled research doc —
> SPEC, the 100-ideas list, the frontend spec, the de-biased CF spec, the
> online-learning phases, and `PLAN-me-first.md`), plus the tree at HEAD
> `830eae8`. This plan assumes the me-first plan lands first where it depends
> on it (owner identity, taste signal); each step states its dependency.
>
> **This plan is a living contract.** When a step is found wrong, fix the plan
> in the same commit as the code fix and say in the commit message what you
> changed and why.

---

## 0. The two properties being built

**P1 — Owner taste shapes every reader's rankings.** The owner's rated
profile (`leather`, built from 257 calibre ratings via `profile import`, and
constantly updated by like/dislike/arena learning) becomes a *global prior*:
every reader's ranking includes an owner-taste term. Other readers keep their
own personalisation on top of it; the owner's term is the floor, not the
ceiling.

**P2 — Arena weights voters by taste agreement.** The arena is Glicko-2 over
comparisons, and `LearnTags` already teaches distinguishing tags per
comparison (`internal/arena/batch.go:269`, wired at `internal/web/arena.go:1020`
and `internal/api/arena.go:246`). The change: a comparison's influence on
ratings AND on the shared tag signals is scaled by the voter's taste agreement
with the owner — computed as the cosine between the voter's tag-weight vector
and the owner profile's. Voters with orthogonal taste get normal weight on
*their own* session learning but near-zero influence on the *global* signal;
voters with opposite taste get minimal influence, exactly as requested.

**P3 — Arenas exist per entity kind, not just works.** The arena tables are
`arena_ratings(work_id, …)` — works-only by accident of the primary key
(`internal/store/arena.go:207`). The mirror already holds every other entity:
634,231 tags (206 characters, 129 relationships, 53 fandoms filed — everything
else freeform), authors as `works.authors` strings (the established split
convention), fandoms, ships. A generic `arena_entities(kind, entity_id, …)`
schema makes every one of them rankable by the same Glicko-2 machinery.

## 0.1 What exists, verified (do not trust a symbol not on this list)

- `internal/arena`: `Rating`, Glicko-2 math (`glicko.go`: `Expected`,
  `Delta`, `variance`, `residualSum`), batch application (`batch.go:87
  Apply`), idle decay (`:148 DecayIdle`), pairing (`pairing.go`),
  `LearnTags` distinguishing-tags rule (`batch.go:269`),
  `BuildTagSignals` rarity-weighted deltas (`batch.go:188`),
  `Normalise` (`:322`), `TopTags` (`:342`).
- `internal/store/arena.go`: `arena_ratings` (work_id-keyed, :207),
  `arena_comparisons`, `arena_rating_history` (:834),
  `arena_user_tag_weights` (owner_key, tag_id, weight, n — :784), `OwnerKey`
  HMAC (:139), `EffectiveRatings` damped-by-uncertainty (:523, this is what
  `peer_rating` reads), `Rating`/`RatingsFor`, session tables.
- Feedback: `internal/store/feedback.go` (`RecordFeedback`, `LearnFromFeedback`
  :206, rates like 0.5 / dislike 0.7 :66), web POST `/feedback`
  (`internal/web/arena.go:485`), `feedbackStances` (`handlers.go:2244`).
- Rated profile: `internal/profile/rated.go` (`BuildFromRatings`, signed
  shrunk weights), `Profile.Tags/BaseTags/Evidence/Feedback`
  (`profile.go:38`), `internal/ratedlist` matching (61% of the calibre list
  resolves), CLI `profile import`/`validate`.
- Engine: signals registered unconditionally, `ErrSkip` → `meta.degraded[]`;
  `DefaultTune` weights; named tunes in the state DB; `Request.TasteWeights`
  does NOT exist yet (that is `PLAN-me-first.md` step 4 — this plan consumes
  it); `internal/collab` item-item cosine (per-seed limit, seeds never vote
  for themselves, `NeighboursFor` :727).
- Identity: `arenaSession` mints `kindred_arena` cookie per browser
  (`internal/web/arena.go:1206`); `Store.OwnerKey` = HMAC(identifier, salt).
  Per-me-first D1, the owner is `ownerKey == HMAC(KINDRED_OWNER_SESSION)`.
- Standing restrictions (shipped `c97b6e7`): site-wide blocked tags +
  `min_words`/`lang`/`not_updated_within_days` defaults, merged UNDER request.
- Memory budget: 220 MiB cap, currently ~200 MiB peak under the behaviour
  gate's blended-tag-page load. Every new index must state its cost.

## 0.2 Design decisions

### D1 — taste agreement is a cosine, computed per voter, cached per period

`agreement(voter ownerKey, owner ownerKey) ∈ [-1, 1]` =
cosine between the two `arena_user_tag_weights` vectors (limit 200 heaviest
each, L2-normalised). Zero overlap → 0. Opposite signs → negative.

The scale it feeds, per the request:

| voter agreement a | influence on GLOBAL arena signal | influence on own session learning |
|---|---|---|
| a ≥ 0.5 (similar taste) | full (1.0) | full |
| 0 < a < 0.5 | linear: a² (quadratic dampening — mild disagreement shrinks fast) | full |
| a ≤ 0 (opposite/unrelated) | **0** (minimal changes, as requested) | full |

Voters below zero still see their own votes affect *their own* profile
learning (`arena_user_tag_weights`, feedback) — they just do not move the
global ratings or the tag signals everyone shares. This is the whole trick:
global state moves only with owner-aligned evidence; per-reader state moves
with any evidence.

The owner's own comparisons carry agreement 1.0 by definition.

### D2 — two rating spaces, one table, no fork

`arena_ratings` stays works-scoped today; the generalisation adds a new
`arena_entities` table with `kind TEXT` + `entity_id INTEGER` primary key and
identical columns, and a **migration** that copies `arena_ratings` into
`arena_entities(kind='ao3_work', …)` and keeps `arena_ratings` as a
compatibility view (or dual-writes during the transition — decision at
implementation; the store API (`Rating`, `RatingsFor`, `EffectiveRatings`,
`Apply`) gains a `kind` parameter with a default of `ao3_work` so every
existing caller compiles unchanged).

Entity kinds shipped at once: `ao3_work` (existing), `ao3_tag` (any of the
634k tags), `ao3_character`, `ao3_relationship`, `ao3_fandom`, `ao3_author`
(authors are strings; `entity_id` = hash of the normalised name, resolved
through the same author-split convention used by `/author?q=`). Series and
collections wait until the crawl holds them (mirror gap, measured).

### D3 — owner taste enters OTHER readers' rankings as a low-weight global signal

A new engine signal `OwnerTaste{Weights map[int32]float64}` — the owner
profile's signed tag vector, loaded once at startup and refreshed when the
profile changes (the profile store already versions by `updated_at`). It
behaves exactly like the per-reader `Taste` signal from me-first step 4 but
is registered for every request, not just the owner's. Weight: small and
explicit (start `owner_taste: 0.06` in `DefaultTune`), so it nudges — it does
not override a reader's own strong signals. On the owner's own requests the
per-reader taste replaces it (agreement 1.0 with itself).

Evidence line names it honestly: `the owner cohort enjoys "gamer" (+1.8),
"system" (+1.2)` — never "you would enjoy" unless it IS the reader's own
taste signal. The `?personal=0` rule from me-first applies unchanged.

### D4 — arenas rank ENTITY PAIRS, using the same evidence machinery

A comparison of two tags asks "which tag's works would you rather read more
of?" (for authors: "whose next work would you open first?"). The pair
selector reuses `internal/arena/pairing.go` logic per kind; the tag-learning
step (`LearnTags`) is skipped for tag-kind arenas (a tag comparison IS the
tag signal — learning from it into tag weights would be circular), but IS
run for author/fandom arenas (a "Winry Rockbell over Riza Hawkeye" choice
teaches about *character* tags on their works... no — it teaches nothing
about work tags reliably; keep it simple: **only work-arena comparisons
feed `LearnTags`**; other kinds feed only their own kind's ratings). State
that as a principle: tag learning comes only from work choices, because
those are the only choices whose distinguishing dimensions are work tags.

### D5 — memory: the owner vector and agreement cache are bounded

Owner profile: ~2-6k tag weights (measured from the real `leather` profile:
well under 1 MB). Voter agreement cache: per-session, computed on first
global-effecting vote, one float per session in a map — thousands of
sessions is still trivial. `arena_entities` adds one row per rated entity
(across all kinds the practical ceiling is tens of thousands — Glicko rows
are ~100 bytes). No new large index. The CSR/embeddings/collab indexes are
untouched.

---

## Step 1 — generic arena store (`arena_entities`)

**`internal/store/store.go` (schema)**: add

```sql
CREATE TABLE IF NOT EXISTS arena_entities(
    kind TEXT NOT NULL, entity_id INTEGER NOT NULL,
    mu REAL NOT NULL, phi REAL NOT NULL, sigma REAL NOT NULL,
    comparisons INTEGER NOT NULL DEFAULT 0,
    wins INTEGER NOT NULL DEFAULT 0, losses INTEGER NOT NULL DEFAULT 0,
    draws INTEGER NOT NULL DEFAULT 0, period INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(kind, entity_id)
);
CREATE TABLE IF NOT EXISTS arena_entity_comparisons(
    kind TEXT NOT NULL, winner_id INTEGER NOT NULL, loser_id INTEGER NOT NULL,
    owner_key TEXT NOT NULL, agreement REAL NOT NULL DEFAULT 1.0,
    period INTEGER NOT NULL, created_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(kind, winner_id, loser_id, owner_key, period)
);
CREATE TABLE IF NOT EXISTS arena_entity_history(
    kind TEXT NOT NULL, entity_id INTEGER NOT NULL, period INTEGER NOT NULL,
    mu REAL, phi REAL, sigma REAL,
    PRIMARY KEY(kind, entity_id, period)
);
```

`agreement` is recorded **at vote time** — the store keeps history, so a
later change of the agreement formula cannot rewrite the past.

**`internal/store/arena.go`**: generalise the rating API:
`Rating(ctx, kind, id)`, `RatingsFor(ctx, kind, ids)`,
`EffectiveRatings(ctx, kind)`, `RecordComparison(ctx, kind, w, l, ownerKey,
agreement)`, `RatingHistory(ctx, kind, id)`. Existing signatures get
kind-taking wrappers; the old names delegate with `kind = "ao3_work"` so all
current call sites (web `renderArena`, api `handleArenaPair`, batch, CLI)
compile and behave identically. The migration copies existing
`arena_ratings` rows in on `EnsureSchema` (idempotent, one-time guarded by a
`meta` flag).

**Verification**: `go test ./internal/store` — round-trip per kind; the
migration preserves every existing row's numbers exactly (compare before/after
in a test fixture built from `testcorpus`); double-apply is a no-op.

## Step 2 — taste agreement

**`internal/store/arena.go`** or new `internal/store/agreement.go`:

```go
// AgreementWithOwner computes the cosine between a voter's learned tag
// weights and the owner profile's tag weights, from arena_user_tag_weights
// and the owner's profile tags. Both sides L2-normalised over their
// intersection-free full vectors; disjoint vectors score 0.
func (s *Store) AgreementWithOwner(ctx context.Context, voterKey string) (float64, error)
```

The owner's weights come from `profile.Load(ctx, ownerProfileName)` joined
with the owner's own `arena_user_tag_weights` (base ⊕ learned, same assembly
me-first step 5 uses — factor that assembly into one store method both
consume). Computed per vote, cached in-process for the session's lifetime
(map[string]float64 keyed by session, invalidated never — a session's taste
drifts slowly and the vote already recorded its snapshot).

**Verification**: unit tests — identical vectors → 1.0; disjoint → 0;
opposite-signed → negative; empty voter vector → 0 (a brand-new cookie has
no taste yet and must not influence the global signal — the default for the
unproven voter is NON-influence, which is the conservative reading of the
request).

## Step 3 — agreement-weighted global learning

**`internal/arena/batch.go`**: `Apply` and `LearnTags` calls gain the
agreement factor. Concretely:

- Comparison rows already record `agreement` (step 1). The batch (which runs
  per period) computes each comparison's effective weight:
  `w = clamp(agreement, 0, 1)²` per D1, with the special case
  `w = 1` for the owner's own comparisons.
- Glicko-2 has no per-outcome weight in its closed form; the standard trick
  is to weight the *outcome repetition*: a comparison with weight w is
  applied as one outcome with probability-1 influence when w ≥ 0.5, and as
  fractional influence otherwise by scaling the update on the loser's
  uncertainty reduction — implement as: comparisons with w below a floor
  (0.05) are EXCLUDED from the batch entirely; included ones pass `w` into a
  new `ApplyWeighted` that multiplies each outcome's `Delta` contribution
  before the variance sum. This is a documented approximation of weighted
  Glicko (cite the approximation in the doc comment — an exact weighted
  Glicko does not exist in closed form).
- `BuildTagSignals`/`LearnTags` deltas multiply by `w` — a low-agreement
  voter's comparison teaches the shared tag weights almost nothing, and a
  negative-agreement voter's teaches nothing at all. This is where "if users
  with opposite taste didn't vote the changes should be minimal" is literally
  implemented: their rows sit in `arena_entity_comparisons` with `w = 0` and
  the batch skips them.
- `EffectiveRatings` (what `peer_rating` reads) is unchanged in formula —
  but since low-agreement outcomes no longer move `mu`, the peer signal
  becomes owner-taste-aligned by construction.

**Verification**: `go test ./internal/arena` — a fixture with two voters, one
at agreement 1.0 and one at −0.8: after 100 identical comparisons from each,
the rating moved only by the aligned voter's; `LearnTags` deltas are 0 for
the opposite voter; `ApplyWeighted` matches plain `Apply` when all weights
are 1 (regression guard).

## Step 4 — the taste signal for everyone (OwnerTaste)

Depends on me-first steps 2–4 (owner identity, persistence, `signal.Taste`).
If that plan has landed, this step is small: register `signal.OwnerTaste` in
`signals()` unconditionally (weights passed via `Request.OwnerTasteWeights`,
assembled at startup from the owner profile + learned weights, refreshed when
`profile.updated_at` moves). Default tune gains `owner_taste: 0.06`; the
named `blend70` tune for the owner's own requests sets it to 0 (their own
taste signal carries them). Non-owner requests get both `taste` (their own,
usually empty → `ErrSkip` → degraded, honest) and `owner_taste` (active).

Evidence strings (D3): the signal's reason names its top contributing tags
with the owner-cohort phrasing. The `taste-note`/`taste-error` pattern
applies: a page must SAY which taste signals were active — silence is a bug.

**Verification**: unit — candidate carrying owner-liked tags outranks an
otherwise-equal candidate carrying owner-disliked tags, for a non-owner
request; owner request unchanged vs me-first behaviour; `meta.degraded`
honesty when the owner profile is missing.

## Step 5 — entity arenas: engine + pairing

**`internal/arena/pairing.go`**: `PickPair(kind)` — for `ao3_work` keep
today's selector; for other kinds pick two entities of that kind with
similar ratings and fewest comparisons (the existing pairing principles,
applied per kind), drawing candidates that exist in the kind's id space
(tags: from `tags` where the id appears on ≥3 works in the mirror; authors:
from the parsed-author reverse map; fandoms: the 53 filed + name-shape
canon).

**`internal/corpus`**: add the entity-name resolvers the arena pages need:
`TagName(ctx, id)` (exists in graph), `AuthorName(ctx, hash)` (reverse map
from `works.authors`), `FandomName(ctx, id)`. All cheap, all cached at
startup into the same bounded-memo pattern the project already uses.

**Web pages**: `/arena` grows a kind switcher (`?kind=work|tag|author|ship|character|fandom`,
default work); `/rank/{kind}/{id}` renders any entity's standing; the pair
POST records `kind`. The JSON API endpoints mirror the same parameter.
Leaderboard per kind. Every page keeps the no-JS form-POST flow.

**Verification**: e2e — judge a tag pair and an author pair through the
forms; leaderboard per kind renders; the work arena regression-tests
unchanged (fixture replay of today's 44 comparisons produces identical
ratings through the generalised path).

## Step 6 — wire owner influence into the public surfaces

- `peer_rating` signal now reads owner-aligned ratings by construction
  (step 3); add one test that pins the alignment property: the top of the
  arena leaderboard for works correlates with the owner profile's liked tags
  (Spearman over a fixture; the real number reported in HANDOFF, not
  asserted).
- `/leaderboard` states whose taste the ordering reflects: a note naming the
  owner-cohort weighting and the agreement cutoff. Honesty over flattery:
  the page says "ranked by readers with taste aligned to the owner cohort."
- The `100 ideas` list's "Anti-recommendation" and "Hidden gems" pages
  (full-context tiers 3/5) are explicitly NOT in this plan — they rank by
  inverted/global scores and would fight the owner-taste prior. Record the
  decision so nobody "fixes" it later.

**Verification**: extend `scripts/check-deploy.sh` in the same commit: fetch
a leaderboard page, assert the cohort note renders; judge one pair through
the form and see the pair page update (hrefs from the page, never
hand-constructed).

## Step 7 — docs, gates, deploy

- **SPEC.md**: new §2.9 "Owner-weighted consensus" (P1/P2 semantics, the
  agreement table, the approximation note); §2.5 generalised to entity
  arenas with the kind list; §3.2 endpoint signatures gain `?kind=`;
  §2.6 evidence phrasing rules for owner-cohort lines. Status markers per
  claim, `[live]` only where the gate proves it.
- **DECISIONS.md**: why weighted-Glicko-by-agreement over a separate
  owner-only arena; why tag learning stays work-only (D4's circularity);
  why low-agreement voters keep full personal influence; the fractional-
  update approximation and its known drift.
- **PLAN-web-ui / what-is-left**: dated section, test list, open findings.
- Gates: `go build ./... && go vet ./... && go test ./...`; `make e2e`
  (report the real count); `scripts/budget.sh` (state the new peak; the
  additions are row-shaped, not index-shaped); `bash scripts/deploy.sh`;
  both deploy gates.

## Ordering, and why

Store generalisation (1) first because everything reads and writes through
it. Agreement (2) before weighting (3) because the batch needs the number
recorded at vote time. Weighting (3) before the signal (4) because the
signal's credibility claim ("aligned by construction") is only true after
step 3. Entity arenas (5) are independent of 2–4 and can land in either
order — sequencing them after keeps each commit reviewable. Public surfaces
(6) and docs (7) last, each gate run before the next step starts.

## If a step fails

- **Agreement is noisy for light voters** (50-tag vector): require a minimum
  evidence count before a vote influences the global signal (n ≥ 5 per side
  on the weights used); below that the vote is session-local only. State the
  threshold in the leaderboard note.
- **Weighted-Glicko approximation drifts** (ratings inflate when many
  mid-agreement voters pile on): tighten the floor from 0.05 to 0.2 and
  re-measure on the replayed fixture; the fixture replay (step 3 test) is
  the early warning.
- **Entity id space is ambiguous for authors** (same name, two people): the
  hash is over the normalised display string — that is the identity this
  codebase already uses for `/author`; disambiguating real people is a
  wrangling problem out of scope, noted in DECISIONS.
- **`owner_taste` swamp**: if the 0.06 weight dominates low-signal pools,
  fix the value normalisation in the signal, not the tune — the same rule as
  the me-first plan's step-4 warning.

## Out of scope, on purpose

Multiple owners (one owner profile, named in env); anonymous vote selling /
brigading defenses beyond the agreement gate (the gate IS the defense);
tag-learning from non-work arenas (D4); series/collection arenas (mirror
gap); changing `internal/collab`'s algorithm; accounts.
