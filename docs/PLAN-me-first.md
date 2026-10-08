# PLAN — the me-first site

> Implements the owner's request (verbatim): *"similar to what we did before,
> changing the work recommendations to be 'works similar to x that I would
> enjoy', I want to refactor the whole site to be about me first, making it as
> useful as possible for me while still being somewhat useful to other
> users."*
>
> The "before" is the α=0.7 taste blend (`docs/handoff-2026-10-08-taste-blend.md`):
> `score = 0.7·similarity + 0.3·enjoyment`. This plan generalises that from one
> ranking mode to the organising principle of every page, and finishes the
> engine half of the handoff (the taste signal) that its step list left open.
>
> **This plan is a living contract.** When a step is found wrong, fix the plan
> in the same commit as the code fix and say in the commit message what you
> changed and why.

---

## 0. What "me first" means here, and what it does not

The site has exactly one reader who matters: the owner (AO3 account
`Leather_Release_9057`). Every surface must answer the owner's question —
*what would I enjoy* — not the corpus's question (*what exists*). A visitor
who is not the owner gets today's site: honest, generic, still fully usable,
with their own session learning from their own gestures. Nobody else ever
sees owner data.

Three rules follow, and every step below serves one of them:

1. **Personalisation is the default view, not a mode.** Where a page ranks,
   the owner's taste is in the ranking unless the URL explicitly opts out
   (`?sort=`, `?alpha=0`). Where a page seeds (For You, surprise, tag
   blends), the owner's own likes are the seeds.
2. **The generic view stays reachable and honest.** Every personalised page
   says which view it is showing (the `taste-note`/`taste-error` pattern from
   SPEC §2.6 — silence is a bug). `?personal=0` shows the generic view.
3. **No owner leakage.** Owner weights, profile names, and liked-work lists
   are resolved only for owner requests. A non-owner page must not contain a
   single byte of owner state (tested, not promised).

Deliberately NOT in scope: accounts, login, JavaScript, multi-owner support,
changing the collab algorithm, deleting any existing page.

## Verified before this plan was written

Every symbol below was checked against the tree (HEAD `ea0f31c` + the
uncommitted rated-profile work, which is step 1's baseline). Do not trust a
symbol not on this list — grep first, as `PLAN-web-ui.md` teaches.

- **Identity.** `ArenaCookieName = "kindred_arena"` — `internal/web/arena.go:26`.
  `func (d Deps) arenaSession(ctx, w, r) (sessionKey, ownerKey string, err error)`
  mints the cookie on demand — `internal/web/arena.go:1206`.
  `func (s *Store) OwnerKey(ctx, identifier) (string, error)` = HMAC-SHA256 of
  the identifier under the stable salt — `internal/store/arena.go:139`.
- **Per-reader learned state.** `arena_user_tag_weights(owner_key, tag_id,
  weight, n)`, read by `Store.TagWeights` (`internal/store/arena.go:752`),
  written by `Store.UpsertTagWeight` (:779) with n-damped averaging.
  Like/dislike feedback: `internal/store/feedback.go` — `FeedbackLike = 1` /
  `FeedbackDislike = -1` (:49), rates like 0.5 / dislike 0.7 (:66),
  `RecordFeedback` (:91), `LearnFromFeedback` (:206), `applyDeltas` (:221),
  `FeedbackCount` (:240), `FeedbackForWorks` (used at
  `internal/web/handlers.go:2260`). Web POST `/feedback` → `postFeedback`
  (`internal/web/arena.go:485`); buttons on the recommend page render pressed
  state via `feedbackStances` (`internal/web/handlers.go:2244`).
- **Rated profiles (uncommitted, green).** `profile.Profile` carries `Tags`,
  `BaseTags`, `Evidence`, `Feedback` (`internal/profile/profile.go:38`);
  `BuildFromRatings` (`internal/profile/rated.go`) builds signed shrunk
  weights `Σ(rating−5.5)·conf / (Σconf + 3)`; `internal/ratedlist`
  (`LoadFromFile` :332, `MatchToCorpus` :694) parses calibredb paste and
  matches to the mirror; CLI `kindred profile import --paste … --save` and
  `profile validate` (`cmd/kindred/profile_rated.go`); `openProfileStore`
  (`cmd/kindred/profile.go:390`). Store schema in
  `internal/store/store.go:273` + `EnsureSchema` profile tables.
- **Engine.** `Request` (`internal/engine/engine.go:123`) has Seeds, Kind, N,
  Tune, MaxPerGroup, GroupBy, PoolSize, Exclude, PoolMode, BlockedTagIDs,
  SeenIDs — **no taste field**. `signals()` (engine.go:~424) appends
  TagOverlap, Neighbourhood, Quality, Recency, Popularity, PeerRating, Collab,
  Embedding (full mode); signals are registered UNCONDITIONALLY and skip with
  `rank.ErrSkip` so absence lands in `meta.degraded[]`. `rank.Score`
  (`internal/rank/rank.go:167`) is linear: `total += tune.Weights[name] *
  value`. `DefaultTune()` (engine.go:366): tag_overlap .22, neighbourhood .30,
  quality .09, recency .09, popularity .09, embedding .10, collab .16,
  peer_rating .11. Named tunes resolve from the state DB (`resolveTune`
  engine.go:213; `kindred tune --name X --set a=1,b=2`). `n` cap 200
  (engine.go:436).
- **Collab evidence for the owner.** `user_work_interactions`: 180,677 rows /
  6,261 readers; the owner is user id **99** with **44** `bookmarked` rows
  (`users.bookmark_count` says 41 — the two sources disagree; trust the
  interaction rows). `collab.NeighboursFor` (`internal/collab/collab.go:727`)
  votes per-seed with a per-seed limit and never lets a seed vote for itself.
- **Web routes** (`internal/web/handlers.go:245 route`): `/` renders search;
  `/search`, `/author`, `/work/{id}`, `/tag/{id}` (taste blend default,
  `blendTagTaste` handlers.go:1802 — seeds are the tag's 5 most-kudoed works),
  `/recommend` (:1986), `/fandoms` (:364, `?profile=` already feeds ranking
  via `profileTagIDs`), `/underrated` (:431), `/neighbours` (:541),
  `/surprise` (:489, `corpusquery.Surprise`, no reader input),
  `/profiles`, `/profile`, `/arena`, `/leaderboard`, `/my-ranking`, `/block`,
  `/rank/{id}`; POSTs `/arena/judge`, `/block`, `/seen`, `/feedback`.
- **Config.** `internal/config/config.go:16` — env-resolved (`Load` :65),
  flag-bound (`Bind` :82). `StableSalt` is env-only on purpose (:26); the
  owner secret follows the same rule.
- **Corpus-side owner row**: `users` → `Leather_Release_9057` id 99; work ids
  resolvable via `user_work_interactions WHERE user_id=99 AND
  interaction_type='bookmarked'` (dedupe: the table stores duplicate rows per
  seed — `select distinct` is mandatory).
- **Gates.** `make verify`; `make e2e` (~115 Playwright tests, hermetic over
  `internal/testcorpus` via `cmd/e2eserver`); `scripts/budget.sh` (220 MiB
  cap); `scripts/deploy.sh` (the only deploy path) → `scripts/check-deploy.sh`
  + `scripts/check-provenance.sh`; `docs/goal-check.py`.
- **Hard constraints** (`docs/todo-everything.md` §8): no-JS, all state in the
  URL, deploy only via `scripts/deploy.sh`, sanitized filenames everywhere.

## Design decisions

### D1 — owner identity: one env secret, no accounts

`KINDRED_OWNER_SESSION` (env-only, like `StableSalt`) holds a session key the
owner installs in their browser once (a `kindred_arena` cookie with that exact
value). At startup the server computes `ownerKey = OwnerKey(ctx,
KINDRED_OWNER_SESSION)` once and holds it. A request is the owner's when its
cookie-derived ownerKey equals the held one. `KINDRED_OWNER_PROFILE` (env,
default `leather`) names the stored rated profile.

Why this shape: no new auth surface, no login page, no second cookie; the
existing `arenaSession` machinery keeps minting sessions for everyone else and
stays untouched; clearing cookies does not orphan the owner's data because
their ownerKey is a pure function of the env secret. The trade-off is stated
plainly: whoever holds the secret IS the owner — acceptable because the site
is loopback/TLS-proxy personal infrastructure, and the secret never appears in
process listings (env, not flag).

### D2 — taste enters the engine as a normal signal, and α as a derived tune

`signal.Taste{Weights map[int32]float64}` scores a candidate by the normalised
dot product of its tag vector with the reader's signed weight vector, into
[-1, 1], with the top contributing tags in the reason string. It is registered
UNCONDITIONALLY (the repo's five-times-confirmed rule) and skips with
`rank.ErrSkip` when weights are empty → `meta.degraded[]`.

α is NOT new scoring machinery. `rank.Score` stays linear; α is expressed as a
named tune: `blend70` sets `taste 0.30` and rescales the other eight weights
to sum 0.70, so `score ≈ 0.7·content + 0.3·enjoyment` exactly when taste is
the only personal signal with data. `Request.TasteWeights` carries the
weights; the tune carries the weight. The handoff's scale warning
(neighbourhood values ~180 vs a ~60 total) is handled in step 4's measurement,
not assumed away.

Weight assembly (`base + learned`) and its scale check are step 4's job, in
the web layer, not the engine's — the engine must rank with or without a
store, exactly like BlockedTagIDs.

### D3 — seeds come from the owner's own evidence, in this priority order

1. **Liked works** (`feedback` rows, polarity +1) — the strongest gesture.
2. **Corpus bookmarks** (user 99's 44, `select distinct work_id`).
3. **Rated works** — persisted at import time (step 3 adds
   `profile_works`), because `Profile.Works` is a count today.
4. **Top profile tags** — the cold-start fallback when 1–3 are empty.

Every seed-using page (For You, surprise, tag blend) draws from this list
through one helper, so the order is defined once. Cap 20 seeds; the per-seed
collab limit already bounds the vote.

### D4 — the generic view is the owner view with personalisation switched off

Not a fork. `?personal=0` (and absence of the owner cookie) runs the same
handlers with `TasteWeights = nil`, profile = nil, and the current seed
rules. One code path, two configurations, and the page note names which one
rendered. This is what "somewhat useful to other users" means concretely:
they get the corpus, search, filters, their own sessions and feedback —
minus the owner's taste.

---

## Step 1 — commit the rated-profile baseline

The tree carries the uncommitted rated-profile work (import, validate,
feedback learning, `/feedback` POST, profile evidence fields). It builds,
vets and tests green after two import-line fixes in
`cmd/kindred/profile_rated.go` (module path typo `polarosocial` →
`polarisocial`, and the `ratedlist` import alias). Commit it as its own
baseline commit so every later step diffs against a green tree, and push
`HEAD` to **both** remotes.

```bash
gofmt -w internal/ cmd/ && go build ./... && go vet ./... && go test ./...
git add -A && git commit -m "Rated profiles: calibre import, validation, like/dislike feedback learning"
git push forgejo HEAD && git push github HEAD && git ls-remote --heads forgejo main github 2>/dev/null | true
```

Expected: build/vet/test exit 0; both remote SHAs equal local HEAD.

## Step 2 — owner identity

**`internal/config/config.go`**: add `OwnerSession string` and
`OwnerProfile string`; resolve in `Load()` from `KINDRED_OWNER_SESSION` /
`KINDRED_OWNER_PROFILE` (default `leather` when a session is set). **No flags**
— env-only, same reasoning as `StableSalt`, with a comment saying so.

**`internal/web` Deps**: add `OwnerKey string` and `OwnerProfile string`
fields (empty = no owner configured). Wire them in `runServe` from config:
`OwnerKey` = `store.OwnerKey(ctx, cfg.OwnerSession)` computed once, failure to
compute logged and treated as no-owner. Add:

```go
// isOwner reports whether this request's ownerKey is the configured owner's.
func (d Deps) isOwner(ownerKey string) bool
```

and a small `owner(r) (ownerKey string, ok bool)` wrapper around
`arenaSession` that returns `ok=false` on any error.

**Verification**: unit test — with a store fixture, `OwnerKey("s1") != nil`;
`isOwner` true only for the matching key; a server built without
`KINDRED_OWNER_SESSION` has `OwnerKey == ""` and `isOwner` is always false.

## Step 3 — persist rated works, and expose the owner's evidence lists

**`internal/store/store.go` (`EnsureSchema`)**: add

```sql
CREATE TABLE IF NOT EXISTS profile_works(
    name TEXT NOT NULL, work_id INTEGER NOT NULL, rating INTEGER NOT NULL,
    PRIMARY KEY(name, work_id)
);
```

**`profile.Store.Save`** writes the rated rows (`BuildFromRatings` already has
them); `Load` returns them on `Profile.WorksList []RatedWorkRef`.

**`internal/store/feedback.go`**: add

```go
// LikedWorks returns the ids this reader pressed like on, newest first.
func (s *Store) LikedWorks(ctx context.Context, ownerKey string, limit int) ([]int64, error)
```

**`internal/collab` or `internal/corpus`**: add a corpus-side helper

```go
// BookmarkedWorks returns the distinct works a mirror user bookmarked.
func BookmarkedWorks(ctx context.Context, db *sql.DB, userID int64) ([]int64, error)
```

(`SELECT DISTINCT work_id FROM user_work_interactions WHERE user_id = ? AND
interaction_type = 'bookmarked'` — DISTINCT is mandatory; duplicate rows per
seed are a measured property of this table.)

**Verification**: unit tests over `internal/testcorpus` — liked works round-trip;
bookmarked works dedupes; a profile saved with rated works loads them back.

## Step 4 — the taste signal and the blend70 tune

**`internal/signal/taste.go`** (new):

```go
// Taste scores a candidate by the reader's signed tag weights.
// Value = (Σ w(t)·[t ∈ candidate tags]) / (Σ |w(t)| over the candidate's
// tags ∨ the weights' L1 norm, whichever is smaller) → [-1, 1].
// Reason names the top-3 contributing tags with signed contributions.
type Taste struct{ Weights map[int32]float64 }
```

Empty `Weights` → `rank.ErrSkip` for every candidate (degraded, never silent
zero). **`internal/engine/engine.go`**: add `TasteWeights map[int32]float64`
to `Request`; append `signal.Taste{Weights: req.TasteWeights}` in `signals()`
unconditionally; `DefaultTune()` gains `taste: 0` (off by default — zero
behaviour change for existing callers and tests). Create the named tune in
`cmd/kindred/tune.go`'s seeding path or document the one-liner:

```bash
kindred tune --name blend70 --set taste=0.3,tag_overlap=0.154,neighbourhood=0.21,quality=0.063,recency=0.063,popularity=0.063,embedding=0.07,collab=0.112,peer_rating=0.077
```

(other weights = old values × 0.7/1.16, preserving their ratios; `embedding`
excluded → renormalise handles lite mode as today).

**The scale measurement (do not skip)**: on thinkcentre against the real
corpus, run `recommend` with `--tune blend70` and taste weights loaded, dump
the per-signal decomposition for the top 20 (the JSON evidence array already
carries `signal/value/weight`), and check taste's contribution is neither
swamped (<5% of total) nor dominant (>60%). If swamped, the value
normalisation in `Taste` is the thing to fix — not the tune.

**Verification**: `go test ./internal/signal ./internal/engine` — new tests:
taste scores a candidate carrying a positively-weighted tag above one
carrying a negatively-weighted tag; empty weights → degraded, list unchanged
vs taste absent; tune blend70 resolves and sums to ~1.0.

## Step 5 — the web layer resolves taste once per request

**`internal/web/handlers.go`** (or a new `internal/web/personal.go`):

```go
// tasteFor resolves this request's personalisation inputs.
// Owner + configured profile: weights = profile.BaseTags ⊕ learned
// (Store.TagWeights, added on top; both sources already signed).
// Anything missing degrades independently and is reported in the page note.
func (d Deps) tasteFor(ctx context.Context, w http.ResponseWriter, r *request) (weights map[int32]float64, seeds []int64, note string, ok bool)
```

- `seeds` per D3's priority (liked → bookmarked → rated → top profile tags),
  capped 20, deduped, each as `engine.Seed{Kind: corpus.AO3Kind}`.
- `ok=false` for non-owners (and for the owner when no profile is stored —
  the note says the profile is missing, matching the taste-note/taste-error
  rule).
- The learned-weight scale check prints once at startup into the log:
  top-10 base vs learned weights, so a 10× mismatch between the two sources
  is visible rather than silently ranking.

**Verification**: unit test with a fixture store — owner request returns
weights containing a liked work's tags; non-owner request returns `ok=false`
and empty weights.

## Step 6 — `/` becomes For You; `/recommend` grows `?for=me`

**`/`**: when `tasteFor` returns ok, render `foryou.html` (new template +
`render_pages_test.go` case — the goal-check page-coverage clause fails
without it): seeds → `engine.Recommend{N: 50, Tune: "blend70", Exclude: true,
BlockedTagIDs, SeenIDs}`, standard work cards with evidence. Heading names
the view ("For you — 0.7 similar · 0.3 would-you-enjoy"); the note names the
seed source and count. `?personal=0` (or non-owner) renders today's search
page unchanged.

**`/recommend`**: `?for=me=1` (default for the owner, absent otherwise)
replaces the required `?seed=` with `tasteFor` seeds; an explicit `?seed=`
still wins. `?alpha=` (clamped [0,1], default 0.7) selects between `blend70`
and the plain tune by setting the taste weight to `1−α` and rescaling the
rest — one small helper, not a second tune table. The page keeps every
existing control; degraded signals still render.

**Verification**: e2e (hermetic fixture): owner-shaped request on `/` shows
the for-you heading and ≥1 card with evidence; `?personal=0` shows the search
box; `/recommend?for=me=1` returns 200 with ≥1 result and `meta` echoed;
non-owner gets today's behaviour on both pages.

## Step 7 — the rest of the site goes me-first

Each item: owner path uses `tasteFor`; `?personal=0`/non-owner keeps today's
behaviour; the page note names the view. No new routes except `/`'s template.

- **`/work/{id}`** — the "more like this" block passes `TasteWeights` and the
  owner's tune: the heading becomes "similar to this that you would enjoy".
  This is the original α=0.7 ask, applied at its natural home.
- **`/tag/{id}`** — `blendTagTaste` seeds: use the owner's liked works that
  carry this tag first (cap 5), fall back to the tag's top-kudos five; the
  taste-note states which seed source ran.
- **`/fandoms`** — `?profile=` defaults to the owner's profile for owner
  requests (mechanism exists: `profileTagIDs` → corpusquery seeds).
- **`/underrated`** — owner default `?profile=`; underrated pool re-ranked by
  taste before the cut, with the note carrying both halves.
- **`/surprise`** — owner: pick a random liked/bookmarked work as the seed
  (D3 list, `sessionRand` for the draw), `Recommend N=20`, show one; the page
  names the seed it surprised from. Non-owner: today's `corpusquery.Surprise`.
- **`/search`** — deliberately unchanged (lookup, not discovery; taste-ordering
  matches would hide exact hits). State this in the plan and leave it.

**Default blocked tags seeding** (SPEC §2.7, [planned] → live): on first
write to a fresh reader's block list (no rows yet), seed the defaults —
using the owner-corrected list: the SPEC §2.7 list MINUS `Romance` PLUS
`Scat` (the owner's explicit edit; fix §2.7's list in the same commit). A
reader's own `/block` edits still replace the list (mechanism already live).

**Verification**: e2e asserts per page: personalised element visible for the
owner shape, generic element visible with `?personal=0`, and the note present
in both (silence is the failure state — the taste-note/taste-error rule).

## Step 8 — no owner leakage (the privacy gate)

Unit + e2e assertions on the same template sweep: render every page twice
(owner/non-owner shapes) and assert the non-owner render contains none of the
owner's tag names, profile name, or seed work titles. The owner's evidence
lists are built per request and never cached process-wide beyond the startup
ownerKey.

**Verification**: new `internal/web` test walking all routes both ways;
`go test ./internal/web -run TestNoOwnerLeakage` exit 0.

## Step 9 — evidence wording, introspection, docs

- Taste evidence line names the movement: e.g. `matches your taste: "gamer"
  (+1.8), "system" (+1.2) — would-you-enjoy 0.62`; collab keeps its
  taste-overlap-percentage target phrasing (SPEC §2.6).
- `/profile` (introspection) shows `BaseTags` beside learned weights and the
  feedback counts (`FeedbackCount` already exists) — the reader can see what
  the ratings said vs what the buttons taught.
- **SPEC.md**: new §2.8 "Me-first surfaces" with per-page status markers
  updated as steps land; §2.7 list corrected (Romance out, Scat in); §3.2
  gains `?for=me`, `?alpha`, `?personal` parameters; §5.6 documents
  `profile import`/`validate`.
- **HANDOFF.md / docs/what-is-left.md**: dated section, tests that pin each
  item, open findings.

**Verification**: `docs/goal-check.py` (all clauses); `scripts/check-doc-commands.sh`
exit 0; `scripts/check-cli-coverage.py` COVERED.

## Step 10 — gates, deploy, verify

```bash
gofmt -w internal/ cmd/ && go build ./... && go vet ./... && go test ./...
make e2e                       # report the real count, not the remembered one
scripts/budget.sh bin/kindred  # ≤220 MiB — For You adds one pool walk, bound it
bash scripts/deploy.sh         # the ONLY deploy path
bash scripts/check-deploy.sh http://127.0.0.1:8010
bash scripts/check-provenance.sh
```

Extend `check-deploy.sh` in the same commit as step 6: assert the generic
view renders unauthenticated (hrefs taken from the page, never
hand-constructed) and — if the deploy env carries
`KINDRED_OWNER_SESSION` — the owner view for `/` (cookie sent by the gate).
Deploy sets `KINDRED_OWNER_SESSION` in the environment
(`deploy/kindred.service` Environment= line; it is loopback-only, so the
secret-in-unit trade-off is explicit and acceptable — note it in DECISIONS).

**Verification**: all gates green; live spot-checks: `/` owner-shaped shows
For You; `/tag/{id}` note names liked-seeds; `?personal=0` everywhere matches
today's site.

## Ordering, and why

Identity (2) before engine (4) because the engine step's verification needs a
principal to rank for. Engine (4) before web assembly (5) because the signal
must exist to be fed. Persistence (3) before seeds (5) because For You draws
the rated list. Pages last (6–7) because every one of them is a thin consumer
of `tasteFor`. The privacy gate (8) sits after the pages it guards, the docs
(9) after the behaviour they describe, deploy (10) last — each step's
verification command is the reason the next step can trust its inputs.

Steps 1–5 are one sitting; 6 and 7 are each independently deployable behind
their own commit; 8–10 ship together.

## If a step fails

- **Taste is swamped in the step-4 measurement** → fix `Taste`'s value
  normalisation (L1), re-measure; do NOT rescale the other signals' weights to
  compensate — that is the handoff's exact warning.
- **For You is slow on the real corpus** → the seed walk (20 seeds) plus one
  pool query is two queries; if it exceeds that, the bug is in the helper, not
  the budget. Measure before caching anything.
- **e2e fixture lacks bookmark rows** → `internal/testcorpus` grows a second
  user; the hermetic suite must be able to express the owner shape or the
  owner path is untested.
- **`check-deploy.sh` cannot hold the owner cookie** → assert the generic
  view only, and verify the owner view by hand on the live box; say so in the
  gate's output rather than letting a skipped check read as a pass.

## Out of scope, on purpose

Accounts/login (D1 replaces them), JavaScript (hard constraint), multi-owner,
AO3 account sync, changing `internal/collab`'s algorithm, deleting any page.
The generic view is a feature, not a fallback: it is what makes the site
shippable to anyone else at all.
