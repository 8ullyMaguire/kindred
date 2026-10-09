# PLAN-owner-weighted-impl — implementation plan, v2 (supersedes the how of PLAN-owner-weighted.md)

> Written for an implementing LLM with **no memory of the conversations that
> produced it**. Every signature, file:line anchor and rule below was verified
> against the tree at commit `9ad7944` (2026-10-09). Do not re-derive designs
> that are stated here as decisions; if an anchor has drifted, re-verify by
> reading the named file, fix the anchor in this plan in the same commit, and
> carry on. The rationale for each step lives in `PLAN-owner-weighted.md`
> (P1/P2/P3, D1–D5); this file is the concrete build order.
>
> Owner request being implemented, verbatim: *"take into account I want my
> taste to influence recommendations for all users; the arena should work in a
> way that it should be influenced mostly by users with a similar taste to
> mine, if users with opposite taste didn't vote the changes should be minimal.
> there should be arenas for all ao3 entities, tags, authors, etc, not just
> works."*

## Ground truth: what is already in the tree (do NOT rebuild any of this)

Committed at `9ad7944`:

- Schema in `internal/store/store.go` (inside the big `EnsureSchema` DDL
  string, after the `idx_arena_pending` index, before `arena_ratings`):
  `arena_entities(kind, entity_id, mu, phi, sigma, comparisons, wins, losses,
  draws, period, updated_at, PRIMARY KEY(kind, entity_id))`,
  `arena_entity_comparisons(kind, winner_id, loser_id, owner_key, agreement,
  period, created_at, PRIMARY KEY(kind, winner_id, loser_id, owner_key,
  period))`, `arena_entity_history(kind, entity_id, period, mu, phi, sigma,
  PRIMARY KEY(kind, entity_id, period))`.
- `internal/store/arena.go`: `Rating(ctx, workID)` and
  `RatingFor(ctx, workIDs)` are now thin wrappers over unexported
  `ratingOf(ctx, kind, id)` and `ratingsFor(ctx, kind, ids)`. `kind ==
  "ao3_work"` reads the legacy `arena_ratings` table; every other kind reads
  `arena_entities`. Both read paths return `arena.Initial()` for absent rows.

Uncommitted in the working tree (the only pre-existing edit):

- `internal/store/arena.go` contains a `AgreementWithOwner(ctx, voterKey)`
  method (~line 644, after `EffectiveRatings`). **It is wrong and must be
  deleted, not kept**: it queries `arena_user_tag_weights WHERE owner_key =
  voterKey` for BOTH the voter and the owner side, so the "owner vector" is
  the voter comparing against themself (always 1.0). Step A replaces it.

Everything else in this plan is new work. There is **no** owner-identity
machinery in the tree yet (`grep -rn KINDRED_OWNER_SESSION --include='*.go'`
returns nothing) and **no** `signal.Taste`; where this plan needs them it
builds the minimal version itself (Step D).

## Verified anchors you will edit or call (re-read each before editing it)

| What | Where | Signature / shape |
|---|---|---|
| Glicko outcome | `internal/arena/batch.go:28` | `PeriodOutcome{Work int64; Score float64; OppWork int64}` |
| Opponent | `internal/arena/glicko.go:98` | `Opponent{Mu, Phi, Score float64}` |
| Batch apply | `internal/arena/batch.go:87` | `Apply(pre map[int64]Rating, outcomes []PeriodOutcome) map[int64]Rating` (pure; returns new map) |
| Opponent builder | `internal/arena/batch.go:52` | `OpponentsFor(outcomes []PeriodOutcome, pre map[int64]Rating) map[int64][]Opponent` — appends BOTH sides of each outcome |
| Rating update | `internal/arena/glicko.go:303` | `func (r Rating) Update(ops []Opponent) Rating` — guards non-finite via `finiteOps`, returns r unchanged when variance is +Inf |
| Glicko internals | `internal/arena/glicko.go` | `g(phi)` :117, `Expected(mu,muJ,phiJ)` :139, `residualSum(mu,ops)` :153, `variance(mu,ops)` :169, `Delta(mu,ops)` :188 |
| Tag learning | `internal/arena/batch.go:269` | `LearnTags(byWork map[int64][]int64, positives, negatives []int64, tagCount map[int64]int, totalWorks int) map[int64]float64` |
| Tag delta math | `internal/arena/batch.go:188` | `BuildTagSignals(TagSignals{PositiveWork, NegativeWork, PositiveTags, NegativeTags}, tagCount, totalWorks) map[int64]float64` |
| Batch runner | `internal/api/arena.go:360` | `func (a *ArenaService) RunBatch(ctx) (BatchResult, error)` — watermark `MetaInt("arena_last_run")`, reads `JudgedAfter`, builds outcomes, calls `arena.Apply` at `:425`, period counter from `MetaInt("arena_period")` |
| Judgement reader | `internal/store/arena.go:416` | `JudgedAfter(ctx, since time.Time, limit int) ([]JudgedComparison, error)`; `JudgedComparison{WorkA, WorkB, Winner, Loser int64; Score float64}` :460 |
| Judgement write | `internal/store/arena.go:312` | `RecordJudgement(ctx, sessionKey, choice string) error` — stores choice as side 'a'/'b'/'neither' |
| Last judged | `internal/store/arena.go:1123` | `LastJudgedInSession(ctx, sessionKey) (Comparison, bool, error)`; `Comparison{ID, WorkA, WorkB, Choice, SessionKey, OwnerKey, Strategy, PresentedAt, JudgedAt}` :259 |
| Immediate tag learning | `internal/web/arena.go:1020` AND `internal/api/arena.go:246` | both call `arena.LearnTags(...)` then loop `st.UpsertTagWeight(ctx, comp.OwnerKey, id, deltas[id])` |
| Tag weight upsert | `internal/store/arena.go:848` | `UpsertTagWeight(ctx, ownerKey string, tagID int64, delta float64) error` — running average with n+1 |
| Tag weight read | `internal/store/arena.go:888` | `TagWeights(ctx, ownerKey string, limit int) ([]TagWeight, error)`; `TagWeight{TagID int64; Weight float64; N int}` :880 |
| Pair selection | `internal/arena/pairing.go:196` | `ChoosePair(req PairRequest) (Pair, error)`; `PairRequest{Candidates []Candidate; Seen map[[2]int64]bool; Preferred map[int64]float64; Strategy Strategy; Explored float64; Rand func(int) int}` :126 |
| Arena candidate | `internal/arena/pairing.go:66` | `Candidate{ID int64; Rating Rating; Score float64; Tags []int64; Comparisons int}` |
| Strategies | `internal/arena/pairing.go:31` | `StrategyRandom/StrategyMaxInfo/StrategyClose/StrategyExplore`; `Strategy.Valid()` |
| API pair | `internal/api/arena.go:124` | `func (a *ArenaService) Pair(ctx, w, r, tagID int64) (PairResult, error)` — resume-unjudged first, then `candidates(ctx, tagID)`, `SeenPairs`, `TagWeights`, `ChoosePair`, `RecordPresentation(store.Comparison{...})` |
| Session identity | `internal/web/arena.go` + `store.OwnerKey` `internal/store/arena.go:139` | `OwnerKey(ctx, identifier) (string, error)` = HMAC; sessions are anonymous cookies minted per browser |
| Effective ratings | `internal/store/arena.go:523` | `EffectiveRatings(ctx) (map[int64]float64, float64, error)` — median-damped; this is what `peer_rating` reads via `Engine.ArenaSignal()` (`internal/engine/engine.go:89`) |
| Signals list | `internal/engine/engine.go:1143` | `func (e *Engine) signals(seeds []rank.Candidate, collabVotes map[int64]float64, collabScale float64, overrides map[string]float64, tuneName string) ([]rank.Signal, rank.Tune)` — peer_rating and collab are registered UNCONDITIONALLY with ErrSkip semantics; embedding absent → `renormalise` |
| Default tune | `internal/engine/engine.go:379` | `DefaultTune() rank.Tune` — `Weights: map[string]float64{"tag_overlap":0.22,"neighbourhood":0.30,"quality":0.09,"recency":0.09,"popularity":0.09,"embedding":0.10,"collab":0.16, ...}` (peer_rating has its own entry — read the function before editing) |
| Signal interface | `internal/rank/rank.go:31` | `Name() string; Score(c Candidate, seeds []Candidate, s Store) (value float64, reason string, err error)`; `ErrSkip` :43 → `Meta.Degraded` |
| Engine request | `internal/engine/engine.go:123` | `Request{Seeds []Seed; Kind string; N int; Tune string; MaxPerGroup int; GroupBy string; PoolSize int; Exclude bool; PoolMode PoolMode; BlockedTagIDs ...; Filter ...}` |
| Corpus tag lookup | `internal/corpus/ao3.go:190` | `TagIDByName(ctx, name string) (int64, error)` |
| Mirror schema | thinkcentre `/home/alvaro/kindling-data/ao3_metadata.db` | `works(authors TEXT, last_updated TEXT, ...)`, `work_tags(work_id, tag_id, tag_type)` with tag_type ∈ {archive_warnings, categories, characters, fandoms, freeforms, rating, relationships}, `tags(...)` (634,231 rows). **`work_authors` and `series` are EMPTY** — authors only exist as strings in `works.authors`, format `-Roulette- (Roulette_CV)` |
| Deploy | `scripts/deploy.sh` (build → scp → install → `sudo systemctl restart` → health), unit `deploy/kindred.service` with `Environment=STATE=/var/lib/kindred`, `Environment=CORPUS=/srv/ao3/ao3_metadata.db`, and the standing-restrictions `EnvironmentFile=` (`/etc/default/kindred-restrictions`, already live) |
| Gates | `go build ./... && go vet ./... && go test ./...`; `make e2e`; `bash scripts/deploy.sh`; `scripts/check-deploy.sh http://127.0.0.1:8010` (46 checks); `scripts/check-provenance.sh` |

Conventions that are repo law (violating any of these is a review reject):
signals register **unconditionally** and speak through `ErrSkip` +
`meta.degraded[]`, never silently vanish; map iteration is random so writes
go in sorted-key order; every SQL identifier that appears in a doc comment
must exist; new columns are added by `ALTER TABLE` with `COALESCE` defaults,
never by table rebuilds; Go comments are written in the voice the repo
already uses (plain, opinionated, first person allowed); docs stay in
English like the rest of the repo (the "Spanish" note in older summaries was
wrong — the repo's docs are English).

---

## Step A — agreement engine (replace the broken `AgreementWithOwner`)

New file `internal/store/agreement.go`. Delete the `AgreementWithOwner`
method from `internal/store/arena.go` (lines ~635–700) entirely.

```go
// BuildOwnerVector blends the owner's imported profile tags and the owner's
// learned arena tag weights into one vector, each source L2-normalised and
// contributing half. Both sources always contribute, so a cold arena cannot
// hollow the vector out and a thin profile cannot be drowned by it.
func BuildOwnerVector(profileTags map[int32]float64, learned []TagWeight) map[int64]float64
```

- Normalise each source to unit L2 **over its own non-zero entries**, then
  `out[t] = 0.5*unitProfile[t] + 0.5*unitLearned[t]` over the union of tag
  ids (a tag in only one source keeps that source's half-weight).
- Empty both → empty map.

```go
// Agreement is the cosine between two tag-weight vectors. Disjoint vectors
// score 0; either side empty scores 0. It never returns NaN.
func Agreement(a, b map[int64]float64) float64
```

```go
// AgreementFor returns the voter's taste agreement with the given owner
// vector, from the voter's arena_user_tag_weights rows. The owner's own key
// short-circuits to exactly 1.0 without a query.
//
// A voter with fewer than minEvidence total observations (n summed) has no
// taste worth trusting yet and scores 0 — the conservative default: the
// unproven voter does not move the global signal.
func (s *Store) AgreementFor(ctx context.Context, voterKey string, ownerVector map[int64]float64) (float64, error)
```

- `const minEvidence = 5`.
- Voter vector: for each row, `applied = weight / sqrt(n)` (evidence
  dampening — same convention as the profile's signed shrunk weights).
- **In-process cache**: `map[string]agreementEntry{value float64; built time.Time}`
  on the Store guarded by a `sync.Mutex`, TTL 10 minutes. The owner vector is
  supplied by the caller (Step D builds it once at startup); the cache keys on
  voterKey only — document that a caller which swaps owner vectors must flush
  (provide `s.FlushAgreementCache()`).

Tests (`internal/store/agreement_test.go`, table-driven, hermetic — open the
test DB the way `internal/store/store_test.go` does):
identical vectors → 1.0; disjoint → 0; opposite-signed overlap → negative;
empty voter → 0; voter with Σn=4 → 0; Σn=5 → computed; owner key → 1.0;
BuildOwnerVector: profile-only, learned-only, blend halves, both empty.

Commit A: `store: agreement engine — owner vector blend, cosine, evidence floor`.

## Step B — record agreement at judgement time

The agreement of a comparison is a property of the VOTER at the moment they
vote, so it is written when the vote is written. One column, not a ledger:

1. `EnsureSchema` gains an idempotent migration:
   `ALTER TABLE arena_comparisons ADD COLUMN agreement REAL` — run it, and if
   the error string contains "duplicate column name", ignore it (that is the
   idempotency; there is no information_schema in SQLite). Legacy rows keep
   NULL. Add `CHECK`-free (ALTER cannot add CHECKs; fine).
2. `JudgedComparison` gains `Agreement float64`; `JudgedAfter` selects
   `COALESCE(agreement, 1.0)` — NULL (legacy rows) means "feature predates
   agreement", and those voters influenced today's ratings already, so 1.0
   preserves behaviour rather than retroactively demoting history.
3. Both judgement paths record it. The flow in BOTH
   `internal/api/arena.go` `Judge` (~:233, after `LastJudgedInSession`
   resolves winner/loser) and `internal/web/arena.go` (~:1000, same shape)
   becomes:

```go
agree := 1.0
if a.ownerVector != nil { // web: d.ownerVector; nil = owner features off
    v, err := store.AgreementFor(ctx, comp.OwnerKey, ownerVector)
    if err != nil { return err } // a failed agreement lookup is a failed judge
    agree = v
}
// UPDATE arena_comparisons SET agreement = ? WHERE id = ?
if err := st.RecordAgreement(ctx, comp.ID, agree); err != nil { return err }
```

   - `RecordAgreement(ctx, comparisonID int64, agreement float64) error` —
     one UPDATE, new method next to `RecordJudgement`.
   - The owner vector handle: `ArenaService` and web `Deps` each gain a field
     `ownerVector map[int64]float64` (nil = disabled). Step D fills it; until
     then it is nil everywhere and the whole feature is inert — that is the
     safe intermediate state.
   - Per the plan's D1 table: record the RAW cosine here; the influence
     formula `w = clamp(agree,0,1)²` is applied at batch time (Step C), so
     changing the formula later does not rewrite recorded history.

`arena_entity_comparisons` stays unused this step — it belongs to non-work
kinds (Step E), where there is no legacy `arena_comparisons` to extend.

Tests: judge twice through the service with a stubbed agreement (one aligned,
one not) → the two rows carry the two values; legacy row reads back as 1.0
through `JudgedAfter`.

Commit B: `arena: record voter taste agreement on every judgement`.

## Step C — agreement-weighted batch (the literal "opposite taste → minimal change")

One math path, weights folded into the existing functions — no `ApplyWeighted`
clone, no parallel code:

1. `PeriodOutcome` (batch.go:28) gains `Weight float64` — doc comment:
   "the voter's taste influence, 0..1. Zero means 'unset'; OpponentsFor
   treats it as 1. A batch that wants ZERO influence excludes the outcome
   before building opponents (RunBatch does exactly that)."
2. `Opponent` (glicko.go:98) gains `Weight float64`, same 0→1 semantics.
3. `OpponentsFor` (batch.go:52): `w := o.Weight; if w <= 0 { w = 1 }` and
   append that weight to BOTH sides' `Opponent` (same comparison, same voter,
   same weight).
4. glicko.go: add `func weightOf(op Opponent) float64 { if op.Weight <= 0 {
   return 1 }; return op.Weight }`. Then:
   - `variance` → `v = 1 / Σ w_j·g(phi_j)²·E_j(1−E_j)` (w from weightOf);
     the Σ=0 → +Inf case is already guarded in `Update` (returns r unchanged).
   - `residualSum` → `Σ w_j·g(phi_j)·(s_j − E_j)`.
   - `Delta` → same shape with weights (read its current body first; it
     computes v internally).
   - `Update` itself is UNCHANGED in signature and now runs weighted math.
     Every existing caller and test passes `Opponent{Mu,Phi,Score}` with
     Weight 0 → weightOf → 1 → bit-identical results. That is the regression
     contract; pin it with a test (below).
5. `RunBatch` (`internal/api/arena.go:360`) wiring — the only place the
   influence formula lives:

```go
// Influence per the owner-weighted plan: similar taste full, mild
// disagreement quadratic-dampened, opposite taste zero. The owner's own
// comparisons always carry full influence.
const agreeFloor = 0.05
influence := func(agree float64) float64 {
    if agree <= 0 { return 0 }
    return agree * agree
}
outcomes := make([]arena.PeriodOutcome, 0, len(judged)*2)
for _, j := range judged {
    w := influence(j.Agreement)
    if w < agreeFloor { continue } // excluded outcomes teach nothing
    outcomes = append(outcomes, arena.PeriodOutcome{Work: j.Winner, Score: j.Score, OppWork: j.Loser, Weight: w})
    if j.Score != arena.Draw {
        outcomes = append(outcomes, arena.PeriodOutcome{Work: j.Loser, Score: 1.0 - j.Score, OppWork: j.Winner, Weight: w})
    }
}
if len(outcomes) == 0 { res.Skipped = "no influencial comparisons"; return res, nil }
```

   The owner's own rows: at Step D the owner's judgements get agreement 1.0
   written (AgreementFor short-circuits), so no special case is needed here.
6. Per-user tag learning (`LearnTags` at web:1020 / api:246) is **NOT
   weighted** — it writes `arena_user_tag_weights` for the voter themself,
   and per D1 every voter keeps FULL influence on their own profile. Do not
   touch `LearnTags`, `BuildTagSignals`, or the two call sites. (The
   earlier plan draft said to weight them; that was wrong — there is no
   global tag-weight store in this codebase, only per-voter rows. Fix this
   plan-vs-reality note in DECISIONS when you land Step C.)

Tests (`internal/arena/glicko_test.go` additions + one new
`internal/arena/weight_test.go`):
- `TestApplyWeightsMatchPlainWhenUnset`: outcomes built twice, once with
  Weight 0 everywhere, once with Weight 1 everywhere → identical maps.
- `TestApplyOppositeTasteMovesNothing`: two voters' worth of outcomes over
  the same works, aligned voter Weight 1, opposite voter excluded by the
  floor → final ratings equal the aligned-only run.
- `TestVarianceWeightsReduceToStandard`: weighted variance/residualSum with
  all w=1 equal the unweighted functions (compare against the pre-edit
  formulas copied into the test).
- `TestUpdateHandlesMixedWeights`: one w=1 and one w=0.3 opponent — result
  finite, between the two extremes, NaN-free.

Commit C: `arena: batch influence weighted by recorded taste agreement`.

## Step D — owner identity + the OwnerTaste signal for every reader

D1. **Identity** (minimal, no accounts):
- `internal/config`: `OwnerSession string` ← env `KINDRED_OWNER_SESSION`
  (empty string = owner features disabled; the site behaves exactly as
  today). `OwnerProfile string` ← env `KINDRED_OWNER_PROFILE`, default
  `"leather"`.
- Ownership test: a session is the owner when its `session_key` (the cookie
  value minted by the existing session machinery) equals
  `cfg.OwnerSession`. Wire: wherever `ArenaService.SessionKey(w, r)` /
  web `Deps` resolve the session, compare and set
  `isOwner := sessionKey == cfg.OwnerSession && cfg.OwnerSession != ""`.
  Constant-time compare is unnecessary (the value is a high-entropy secret
  compared for equality, and the deployment is loopback-only — note that
  trade-off in DECISIONS).
- `main`/server assembly: after the store opens, if `OwnerSession != ""`:
  compute `ownerKey := store.OwnerKey(ctx, cfg.OwnerSession)`, load the
  profile (`profile.NewStore(profileDB).Load(ctx, cfg.OwnerProfile)` —
  tolerate `profile.ErrNotFound` by logging and using an empty profile),
  build `ownerVector := store.BuildOwnerVector(profile.Tags, learned)` where
  `learned` comes from `store.TagWeights(ctx, ownerKey, 200)`, and inject it
  into `ArenaService.ownerVector`, web `Deps.ownerVector`, and the engine
  (D2). Refresh: rebuild on the same 10-minute tick that flushes the
  agreement cache (one goroutine with a `time.Ticker`; also call
  `store.FlushAgreementCache()` there).

D2. **Signal** — new file `internal/signal/owner_taste.go`:

```go
// OwnerTaste is the owner cohort's taste as a global prior. It scores a
// candidate by the share of the candidate's tag mass that the owner's
// vector likes. It is registered for EVERY request, owner or not; the
// owner's own personalisation layers on top of it rather than replacing it.
type OwnerTaste struct{ Weights map[int32]float64 }
func (OwnerTaste) Name() string { return "owner_taste" }
```

- `Score`: no weights → `(0, "", rank.ErrSkip)`. Candidate with no TagIDs →
  `(0, "", rank.ErrSkip)`. Otherwise:
  `liked, total, top := 0.0, 0.0, top-2-contributors`;
  for each candidate TagID t: `w, ok := Weights[t]; if !ok { continue }`;
  `total += |w|`; `if w > 0 { liked += w }`; track the two largest-|w|
  contributors with their signs. Value = `liked/total` if total > 0 else 0
  (no overlap is a legitimate 0, not a skip). Reason, mirroring the house
  evidence style: `owner cohort: +gamer, +found family` (or `−…` for
  negative contributors; if none qualify, `owner cohort: no tag overlap`).
- Register in `signals()` (engine.go:1143) UNCONDITIONALLY right after
  `PeerRating`, from a new engine field `OwnerTaste map[int32]float64`
  (int64→int32 conversion at the assembly site). Nil field → the signal
  still registers and ErrSkips → `meta.degraded` says so. That is the house
  convention; do not guard it.
- `DefaultTune()` (engine.go:379) gains `"owner_taste": 0.06`. Read the
  function first and add the entry alongside the existing ones; keep the
  sum of weights sane the way the existing comments discuss.
- `Request` (engine.go:123) gains `Personal bool` (default true). The API
  parse (`parseRequest` in `internal/api/server.go`, where the standing
  defaults merge lives) sets `Personal = false` when `?personal=0`; when
  false, the engine's `signals()` omits the OwnerTaste weights (append with
  nil → skip → degraded names it). Document `?personal=0` in the endpoint
  docs at Step G.

Tests: liked-tags candidate outranks otherwise-equal disliked-tags candidate
(fixtures with a two-entry weight map); no weights → ErrSkip surfaces in
`meta.degraded`; `?personal=0` drops it.

Commit D: `engine: owner_taste — the owner cohort's taste as a global prior (KINDRED_OWNER_SESSION)`.

## Step E — arenas for every entity kind

E1. **Kinds and id spaces** (one allowlist, defined once in
`internal/arena/kinds.go`):

```go
type EntityKind string
const (
    KindWork    EntityKind = "ao3_work"
    KindTag     EntityKind = "ao3_tag"       // any tag, but candidates drawn from freeforms
    KindCharacter EntityKind = "ao3_character" // work_tags.tag_type='characters'
    KindShip    EntityKind = "ao3_relationship" // 'relationships'
    KindFandom  EntityKind = "ao3_fandom"    // 'fandoms'
    KindAuthor  EntityKind = "ao3_author"
)
func (k EntityKind) Valid() bool
func ParseKind(s string) EntityKind // invalid → KindWork
```

- Work tag id spaces: `entity_id` = `work_tags.tag_id` for the matching
  `tag_type`. Tags/characters/ships/fandoms therefore share the `tags` id
  space and resolve names through the corpus.
- Authors: `entity_id = fnv64a(lower(trim(nameWithoutPseud)))` where
  `nameWithoutPseud` drops a trailing ` (…)` when the parenthesised suffix is
  the pseud (mirror format `-Roulette- (Roulette_CV)`). Put the normaliser in
  `internal/arena/kinds.go` as `AuthorEntityID(name string) int64` with
  tests, and document that same-name-different-person collision is accepted
  (the `/author` route has the same convention).

E2. **Candidate loaders** on the corpus side (`internal/corpus/ao3.go`):

```go
func (a *AO3) EntityCandidates(ctx context.Context, kind EntityKind, limit int) ([]int64, error)
```

- tag/character/ship/fandom:
  `SELECT tag_id FROM work_tags WHERE tag_type = ? GROUP BY tag_id HAVING
  COUNT(*) >= 3 ORDER BY RANDOM() LIMIT ?` (the ≥3-works floor is the plan's
  rule for tag arenas).
- author:
  `SELECT authors FROM works WHERE authors <> '' ORDER BY RANDOM() LIMIT
  2000` then normalise+dedupe in Go down to `limit` (SQLite cannot GROUP BY
  the Go-side normaliser; 2000 rows is bounded and cheap).
- Name resolution for display: tags/characters/ships/fandoms via the corpus
  (`tags` table — mirror `TagIDByName` with a by-id variant
  `TagNameByID(ctx, id)`); authors via a lazy cache table in the STATE db:

```sql
CREATE TABLE IF NOT EXISTS entity_names(
    kind TEXT NOT NULL, entity_id INTEGER NOT NULL,
    name TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY(kind, entity_id));
```

  `Store.EntityName(ctx, kind, id)` reads it; `Store.RememberEntityName(ctx,
  kind, id, name)` writes `INSERT OR REPLACE`. The pair/judge/leaderboard
  paths call Remember whenever they learn a name (author candidate loader
  returns ids AND names; feed both through).

E3. **Storage plumbing** — the decision that keeps this tractable:
presentations/judgements for ALL kinds stay in `arena_comparisons`; the
table gains `kind TEXT` (same idempotent ALTER dance as Step B, default
`'ao3_work'`), and `work_a`/`work_b` are read as generic entity ids for
non-work kinds. Specifically:

- `store.Comparison` gains `Kind string`; `RecordPresentation` writes it;
  `UnjudgedInSession(ctx, sessionKey, kind)`, `SeenPairs(ctx, ownerKey,
  kind, limit)`, `JudgedAfter(ctx, since, limit)` (returns Kind in
  JudgedComparison), `LastJudgedInSession` all filter/carry kind. Existing
  callers pass `arena.KindWork`.
- Ratings: `ratingOf`/`ratingsFor`/`SaveRating` already carry kind for
  non-work kinds; add `SaveRatingKind(ctx, kind, RatingRow)` (the existing
  `SaveRating` = kind work). History: non-work kinds write
  `arena_entity_history`; works keep `arena_rating_history` (both tables
  exist; do not migrate).
- Leaderboard: `Leaderboard(ctx, kind, minComparisons, limit)` — the
  existing query (`internal/store/arena.go:754`) branches: work →
  `arena_ratings` join works for title/authors; else → `arena_entities`
  join `entity_names` for the display name. `LeaderboardEntry` gains
  `Kind` and reuses WorkID as the entity id (rename risk: keep the field
  name, add a comment, do NOT rename — the JSON API consumers depend on it).
- `EffectiveRatings` stays works-only (`peer_rating` only ranks works);
  do not generalise it.

E4. **Pairing + judge flow**:

- `ArenaService.Pair(ctx, w, r, tagID)` gains kind resolution
  (`ParseKind(r.URL.Query().Get("kind"))`): kind == work → existing path;
  else → candidates from `EntityCandidates`, ratings from
  `RatingFor(kind, ids)` mapped into `arena.Candidate` (Tags nil, Score 0 —
  `ChoosePair` already degrades explore→random with empty Preferred, and
  `pairing.go` handles nil), `SeenPairs(ownerKey, kind)`, and
  `RecordPresentation(Comparison{Kind: kind, ...})`. Resume path
  (`UnjudgedInSession`) passes the kind too.
- `Judge`: identical flow; after recording agreement, run the LearnTags
  block ONLY when `kind == ao3_work` (per the plan's D4: tag learning comes
  only from work choices; a tag-vs-tag vote feeding tag weights is
  circular). For non-work kinds skip LearnTags entirely — ratings only.
- `RunBatch`: already kind-agnostic (it consumes JudgedAfter rows and calls
  `RatingFor`/`SaveRating`), but must now route per row: group by
  `j.Kind`, and for non-work kinds read/write via the kind-taking
  rating methods and write history to `arena_entity_history`. Idle decay
  (`DecayIdle`) applies per kind group.

E5. **Web + API surfaces**:

- `/arena` (`internal/web/handlers.go:352` route): `?kind=` switcher.
  The pair page shows two entity briefs — works keep the existing rich
  brief; entities render name + kind label + current rating/comparisons.
  The judge form must carry `kind` through the POST (hidden input read by
  the `/arena/judge` handler at `handlers.go:261`).
- New route `/rank/{kind}/{id}`: one entity's standing — rating, rank among
  kind, comparisons, history sparkline data from
  `RatingHistory(kind, id)` (add the kind-taking wrapper). Hrefs from
  rendered data, never hand-built (house rule).
- JSON API: `/api/v1/arena/pair|judge|leaderboard` gain the same `?kind=`
  parameter with the same validation. Unknown kind → 400 with a message
  naming the allowlist.
- All new pages stay server-rendered HTML, no JavaScript (hard constraint).

Tests: e2e-style through the service — judge one `ao3_tag` pair and one
`ao3_author` pair via the form POST path; leaderboards for both kinds
render with names (author name came through entity_names); the work-arena
fixture replay (existing tests) produces byte-identical ratings through the
generalised path. Author id stability: `AuthorEntityID("A (a)") ==
AuthorEntityID("a")` and differs from `AuthorEntityID("ab")`.

Commit E1–E3: `store+corpus: entity kinds, candidate loaders, kind-aware comparison storage`.
Commit E4–E5: `arena+web: pair/judge/leaderboard for tags, characters, ships, fandoms, authors`.

## Step F — public surfaces reflect the weighting

- `/leaderboard` (web): when `cfg.OwnerSession != ""`, render one line
  under the heading: `Ranked by comparisons weighted by taste agreement
  with the owner cohort (votes from readers with ≤5 comparisons or
  opposing taste do not move these ratings).` When owner features are off,
  render nothing. Same on the per-kind leaderboards.
- Add the same sentence to the arena page footer note.
- `scripts/check-deploy.sh`: extend, in the same commit, with one check —
  fetch `/leaderboard`, assert the cohort note when the deploy env carries
  `KINDRED_OWNER_SESSION`, assert its ABSENCE when not (the gate runs
  without it; the with-owner assertion is best-effort and must print
  `skipped (no owner session in gate env)` rather than silently passing).
- Anti-recommendation / hidden-gems pages: **out of scope by decision** —
  they rank on inverted/global scores and would fight the owner prior.
  Record that in DECISIONS so nobody "fixes" it later.

Commit F: `web: leaderboard names the owner-cohort weighting; deploy gate checks it`.

## Step G — docs + deploy + gates

1. `docs/SPEC.md`: new §2.9 "Owner-weighted consensus" — P1/P2 semantics,
   the agreement→influence table (`a≥0.5 full; 0<a<0.5 → a²; a≤0 → none;
   evidence floor n≥5; legacy NULL → 1.0`), the entity kind list, and the
   evidence-phrasing rule ("owner cohort: …" never "you would enjoy" unless
   it is the reader's own taste). §2.5 gains the kind list; §3.2 documents
   `?kind=` on the arena endpoints and `?personal=0`.
2. `docs/DECISIONS.md` — entries: (a) weighted-Glicko-by-agreement instead
   of a separate owner-only arena; (b) per-user tag learning stays unweighted
   (no global tag store exists; the global channel is ratings→peer_rating);
   (c) agreement recorded raw at vote time, influence computed at batch time;
   (d) author identity = normalised display string, collisions accepted;
   (e) low-agreement voters keep full influence on their own profile;
   (f) owner session = literal session_key equality, loopback trade-off;
   (g) anti-rec/hidden-gems out of scope.
3. Deploy env: append to `/etc/default/kindred-restrictions` on
   thinkcentre (the unit already loads this file):
   `KINDRED_OWNER_SESSION=<32-byte hex, generate with openssl rand -hex 32>`
   and `KINDRED_OWNER_PROFILE=leather`. Do NOT commit the session value.
4. Gate sequence, in order, before and after deploy:
   `gofmt -l internal/ cmd/` (empty), `go vet ./...`, `go test ./...`,
   `make e2e` (report the real count), `bash scripts/deploy.sh`,
   `scripts/check-deploy.sh http://127.0.0.1:8010` (46 existing + new
   leaderboard check), `scripts/check-provenance.sh`.
5. Live verification on thinkcentre after deploy (do these, report the
   outputs): `curl -s http://127.0.0.1:8010/healthz`; judge one work pair
   through the web form and confirm `arena_comparisons.agreement` is
   non-NULL for that row; `?kind=tag` pair renders; `?personal=0` on a
   recommend call still returns 200 with `owner_taste` named in
   `meta.degraded` only when weights are empty; memory `scripts/budget.sh`
   peak ≤220 MiB (the additions are rows, not indexes — if the budget
   moves, find the allocation before shipping).

Commit G: `docs+deploy: owner-weighted consensus spec, decisions, env`.

## Order and dependency

A → B → C is strict (B stores what A computes; C consumes what B stores).
D depends only on A (BuildOwnerVector) and can run parallel to B/C, but its
signal's tune entry lands in one commit so the gate stays green. E is
independent of B/C/D EXCEPT that its `Judge` path calls the same
record-agreement helper (keep E after B). F and G last. One PR-sized commit
group per step; every step ends with `go build ./... && go vet ./... && go
test ./...` green and a push to both remotes (`forgejo`, `github`).

## If something does not match this plan

- An anchor drifted (renamed function, moved line): re-verify by reading
  the file, update the anchor HERE in the same commit, note it in the
  commit message.
- `arena.ChoosePair` rejects empty `Tags`/`Preferred` differently than
  described: read `pairing.go` and adapt E4 to its actual contract; do not
  modify pairing semantics for entity kinds.
- The 46-check deploy gate fails on something unrelated: fix or revert;
  never widen a gate check to make it pass.
- Anything that would require JavaScript, accounts, or a second owner:
  stop; it is out of scope by decision.
