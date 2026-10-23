# Plan — 2026-10-23 (second session): candidate generation, measured per source

Follows `docs/HANDOFF-2026-10-23.md`, whose measured conclusion was: **the
recommender's ceiling is its candidate set, not its ranking** (pool recall@1000
= 0.0284 against recall@10 = 0.0056). Its next-unit list, in order:

1. instrument where candidates come from,
2. #11 — embeddings as an *enumeration* source,
3. then re-tune, because only now can the weights matter.

## What the first read of the code found (defects, before any new work)

**D1 — the embedding signal has never contributed on the live deployment.**
`Engine.signals()` calls `renormalise(tune.Weights, "embedding")` *inside*
`if !e.Lite` — i.e. exactly when the embedding signal IS registered. That
deletes the weight and rescales the others, so the signal is registered,
weighted at 0, never scored, and absent from both `evidence` and
`meta.degraded`. Measured on the host: `?tune=quality` (weight 0.0654 stored)
printed 7 signals per item, `embedding` among neither them nor `degraded`.

**D2 — there are no work embeddings.** The `embeddings` table holds
`kind='tag'` rows only (634,232 × 32 on the live state DB). The signal asks
`Embedding(c.Kind, c.ID)` for `ao3_work` ids and `seedVector` asks for the
seeds' *work* vectors, so with D1 fixed it would still skip for every
candidate. The handoff's "already scores at weight 0.10" is wrong twice.

So the embedding dimension is currently a registered, weighted, expanding
concept that measures nothing — the same shape as the blind evaluator the last
session fixed. Fixing it is a prerequisite for unit 2, not a side quest:
the same 32-dim tag space supplies both the scorer and the finder.

## Design decisions (recorded so they can be argued with)

**S1 — the embedding is over TAGS, not works, and that is the useful thing.**
A work's position in tag space is the (equal-weight) mean of its tags'
vectors; a seed set's position is the mean of each seed's position, so a
50-tag seed does not outvote a 5-tag one. Score = cosine(candidate, seeds),
clamped to [0,1] by skipping non-positive.

**S2 — the matrix is loaded once per process, lazily.** 634,232 × 32 × 4 B =
81 MiB in a flat `[]float32`, plus 2.5 MiB of precomputed norms. A per-request
full scan would read 81 MiB of blobs from SQLite on every call, which is
strictly worse than holding it. The memory cap is gone (SPEC §6 revised
2026-10-22) and the host has 15.8 GiB with 4.4 GiB available. Loaded behind
`sync.Once` so a lite deployment and every test that never touches embeddings
pay nothing.

**S3 — the finder is the same centroid, not a second concept.** Nearest tags
to the seed centroid (cosine), top `EmbedPoolTags = 32`, excluding the seeds'
own tags (they are already the tag source's whole input), then works carrying
those tags ordered by summed similarity. This is the one thing tag-neighbour
enumeration cannot do: it reaches tags that never co-occur with a seed tag but
sit near it in latent space (#11's actual argument, and a second-order use of
the same co-occurrence matrix rather than a new data source — say so honestly).

**S4 — sources are additive and reported.** `meta.pool_sources` names every
source and how many ids it admitted after de-duplication, plus how many of its
picks were already in the pool. An instrument that reports only what the union
did cannot say which half failed.

**S5 — new pool modes are opt-in, and the default is decided by measurement.**
`pool_mode=tags|tags+collab|tags+embed|tags+collab+embed`. Default stays
`tags+collab` until run #9/#10 show pool recall moving; then the default is
whatever the numbers support, recorded in `eval_runs`.

## Steps

### Step 1 — embedding index (new file `internal/engine/embedindex.go`)

- `type tagIndex struct { slab []float32; dim int; norms []float32; byID map[int32]int32 }`
- `func loadTagIndex(ctx, db *sql.DB) (*tagIndex, error)`: one query
  `SELECT entity_id, dim, vec FROM embeddings WHERE kind='tag'`, grow the slab
  to `(maxID+1)*dim`, fill, compute norms. Returns nil (no error) when the
  table is empty or absent.
- `func (e *Engine) tagVectors(ctx) (*tagIndex, error)` behind `sync.Once`;
  caches a load error and a nil index (so a repeated request does not retry a
  634k-row scan that failed).
- `func (idx *tagIndex) centroid(tags []int32) []float32`
- `func (idx *tagIndex) cosine(a, b []float32) float64` (norms precomputed for
  stored vectors; candidate centroids are computed then normalised).

Verification: `go test ./internal/engine -run TestTagIndex -v` — a fixture
with three hand-written vectors asserts centroid, cosine, and that an absent
id is skipped rather than zero-filled.

### Step 2 — fix the signal (D1 + D2)

- `signal.Embedding` keeps its shape but gains `Centroid func(rank.Candidate) []float32`
  OR the engine computes the candidate centroid before calling — choose the
  simpler: the engine passes a `TagCentroid func([]int32) []float32` into the
  signal, so the signal stays a pure scorer over `rank.Candidate.TagIDs`.
- `seedVector` becomes `seedCentroid(seeds)`: tag-space, from `s.TagIDs`.
- `renormalise(tune.Weights, "embedding")` moves into the *lite/absent*
  branch: `if e.Lite { … } else { register signal }`. Also: if the index is
  empty, still register (skip → degraded) but renormalise, so a mirror with no
  embeddings ranks exactly as before and says why.

Verification: `go test ./internal/engine -run Embedding` — (a) full mode with
vectors: every ranked item carries an `embedding` evidence entry with weight
0.10; (b) lite mode: no embedding entry and the surviving weights sum to the
same total as before (renormalised); (c) no vectors: `meta.degraded` contains
"embedding". Mutation: restore the old `!e.Lite` condition → (a) must go red.

### Step 3 — pool sources instrumented

- `rank.PoolSources struct { Tags, Collab, Embedding, Overlap int }` with
  `json:"tags"` etc, `Meta.PoolSources *PoolSources json:"pool_sources,omitempty"`.
- `poolFor` returns `(ids, collabVotes, *rank.PoolSources, error)`.
- Counts are post-dedupe admissions: `tags` = tag SQL rows admitted, `collab` =
  collab ids NOT already in the pool, `embedding` likewise, `overlap` = ids a
  later source wanted that an earlier one already admitted.

Verification: `go test ./internal/engine -run PoolSources` — with a fixture
where one collab neighbour is also a tag neighbour, `tags+collab` reports
`{tags:N, collab:1, overlap:1}` and `len(pool) == tags+collab`.

### Step 4 — the embedding pool source

- `PoolMode` gains `PoolTagsEmbed` and `PoolTagsCollabEmbed`; `String()`,
  `ParsePoolMode` updated; the parse error names all four.
- `poolFor` budget: `embedReserve` and `collabReserve` (an eighth each, min 1),
  tag half keeps the rest.
- `embedCandidates(ctx, idx, centroid, limit, excludeIDs, f, blocked, seen, db)`:
  nearest tags, `VALUES` join against `work_tags`, `SUM(w)` ordered
  `DESC, work_id ASC`, filters/blocks/seen applied in SQL, `LIMIT ?`.
- Seed tags are excluded from the nearest-tag list: they are already fully
  enumerated by the tag half, so spending an embed slot on them is waste.

Verification: `go test ./internal/engine -run EmbedPool` — a fixture where the
only bridge between seed and a held-out work is a *latent* tag (never
co-occurring with the seed) is found under `tags+embed` and NOT under `tags`;
plus `pool_sources.embedding > 0`.

### Step 5 — eval can measure it

- `kindred eval --pool-mode tags|tags+collab|tags+embed|tags+collab+embed`
  (default `tags+collab`, matching the site) sets `engine.Request.PoolMode`.
- `eval_runs` gains nothing; `--tune` + `--pool-mode` + `--pool-depth` is
  already an ablation without a rebuild.

### Step 6 — measure on the host, then decide the default

Runs on the live mirror (40 readers / 495 held-out works / depth 1000):
- #9 `tags+collab` (baseline, embedding now live as a SCORER — D1/D2 fixed),
- #10 `tags+collab+embed` (finder on).
Compare pool recall and recall@10. If pool recall rises materially and
recall@10 does not fall, the default becomes `tags+collab+embed` with a
comment quoting the numbers; if not, it stays and the doc says so.

### Step 7 — deploy, walk, document

`bash scripts/deploy.sh > /tmp/deploy.log 2>&1` (never tail), both gates;
`curl` the live API for `pool_sources` and `embedding` evidence; page latency
before/after; update `docs/what-is-left.md`, the handoff's numbers table, and
`docs/PLAN-recsurfaces-31-100.md` row for #11 (it becomes built as an
enumeration source, partial until it beats the tag pool on measured recall).

## Status at the end of this session

Built, tested and committed: steps 1-5. **Not yet done: step 6 (the host
measurement) and step 7 (deploy + walk).** The code path is proven locally by
unit and wiring tests; the claim that it WIDENS pool recall is unmeasured, and
this document does not make it.

What changed against the design above, and why:

- **Step 2's renormalise rule changed during implementation.** The first
  version redistributed the embedding weight when a mirror held no vectors.
  That is the "absent without saying so" failure the tree forbids — the
  dimension would vanish silently. The rule is now: renormalise ONLY in lite
  mode (no signal registered at all); in full mode the signal is registered
  unconditionally, skips for every candidate when there is nothing to read,
  and names itself in `meta.degraded`. A skipped signal contributes no term,
  so the ranking is bit-identical to one built before the signal existed —
  which is what the deleted weight was trying to achieve, silently.
- **`signal.Embedding.Store` was deleted rather than kept.** It existed only
  for the per-work lookup that could never work; the tag index belongs to the
  engine, which owns the state database.
- **The engine's `signals()` gained a `ctx` parameter** (it now may load the
  index on first use), and the four test call sites were updated.
- **De-duplication moved into the pool's `admit` step.** The old code
  appended collab ids to the tag pool without checking membership, so a work
  both sources found could be ranked twice. `pool_sources` counts admissions,
  so `tags+collab+embedding == pool_size` exactly; `overlap` is the count of
  proposals a later source repeated and does NOT subtract from the total.

Verification actually run:

    go test ./... -count=1                     # 26 packages, all green
    go test ./internal/engine -run EmbedPool   # source + invariants
    mutation: embedCandidates returns nil      -> TestEmbedPoolReachesAWorkTheTagPoolCannot red
    mutation: filterSQL dropped from embed query -> TestEmbedPoolHonoursFiltersInTheQuery red
    mutation: `if e.Lite` back to `if !e.Lite`  -> TestEmbeddingSignalContributesOnEveryItem red

Each mutation was restored and the suite re-run green.

## Invariants this must not break

- Identical requests produce identical pools (ordering is fully specified;
  no map iteration in any ordering decision).
- A filter applied to the pool query stays in the pool query for every new
  source — a source that post-filters would silently return fewer.
- A signal that cannot run skips and names itself; it never scores 0.
- Lite mode is unchanged in behaviour: same signals, same weights, same
  ranking.
