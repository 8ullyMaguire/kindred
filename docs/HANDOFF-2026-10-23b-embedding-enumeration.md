# Handoff — 2026-10-23 (second session): the embedding dimension was never running

Follows `docs/HANDOFF-2026-10-23.md`. That document ended with "candidate
generation is the lever; #11 (embeddings as an enumeration source) is next".
Reading the code to do that turned up two defects that had to be fixed first,
and they are the more useful finding of the two — the "next unit" is built, but
what was actually shipped today is a dimension that was dead in production and
looked alive in every response.

## State

| | |
|---|---|
| repo | `~/code-local/go/kindred`, branch `main` |
| HEAD | `b0383a3` — `engine: the embedding dimension actually runs, and it enumerates` |
| tests | `go test ./... -count=1` — 26 packages green (local) |
| deployed | **unchanged**: thinkcentre still runs the pre-`b0383a3` binary |
| measured | **NOT measured on the host** — no eval run against the live mirror yet |

Do not read the green suite as "this improves recommendations". It proves the
mechanism runs, is reachable, and applies the request's filters; it does not
prove the pool gets better.

## D1 — the weight was deleted exactly when the signal existed

`Engine.signals()` called `renormalise(tune.Weights, "embedding")` *inside*
`if !e.Lite` — i.e. only when the embedding signal IS registered. That deletes
the weight and rescales the others.

Observable consequence, measured live before the fix:

    curl "…/api/v1/recommend?seed=ao3_work:17249318&n=50&tune=quality"
    # 7 signals per item: neighbourhood, popularity, recency, tag_overlap,
    #                     quality, idf_overlap, peer_rating
    # meta.degraded: collab, owner_taste, seed_agreement, session_taste
    # embedding: in NEITHER list, with weight 0.0654 stored in the tune

A registered signal with a weight of zero is skipped before `Score` is called,
so it can never reach `evidence` and never reach `degraded`. The dimension was
invisible in both directions at once — the same shape as the blind evaluator
fixed the day before, one layer out.

## D2 — there were no work embeddings, and the signal asked for work embeddings

`signal.Embedding` called `store.Embedding(c.Kind, c.ID)` for `ao3_work` ids,
and `seedVector` called it for the seed works. The state database holds
**634,232 rows, all `kind='tag'`** (verified: `SELECT kind, COUNT(*) FROM
embeddings GROUP BY kind`). So even with D1 fixed the signal would have skipped
for every candidate.

The handoff's own line — "#11: the 634,232 × 32 matrix already exists and
already scores at weight 0.10" — was wrong twice. Both halves are now true, in
the space that exists.

## What shipped

- **`internal/engine/embedindex.go`** — the tag matrix resident in one
  `[]float32` slab (81 MiB), loaded lazily behind `sync.Once`, with per-row
  norms. `centroid(ids)` is the mean over the tags that HAVE a vector (a zero
  would pull works toward the origin, where everything is similar to
  everything). `nearestTags` is a bounded heap (k=32) over 634k rows, ties by
  id so the same centroid always gives the same list.
- **`signal.Embedding`** now scores `cosine(candidate centroid, seed centroid)`
  and skips when either side cannot be placed. `Store` field deleted: it only
  served the per-work lookup that could not work.
- **The weight rule**: renormalise ONLY in lite mode. In full mode the signal
  is registered unconditionally — a mirror with no vectors gets a skip for
  every candidate and `embedding` in `meta.degraded`, and the ranking is
  bit-identical to the pre-signal engine because a skipped term is zero.
- **A candidate SOURCE**: `pool_mode=tags+embed` and `tags+collab+embed`
  enumerate works carrying the tags nearest the seeds (cosine ≥ 0.25, top 32),
  ordered by summed similarity, with filters/blocks/seen applied in the SQL
  before the LIMIT. This is the #11 argument made real: reach is second-order
  through the same co-occurrence matrix, not a new data source.
- **`meta.pool_sources`** `{tags, collab, embedding, overlap}` — admissions per
  source (`tags+collab+embedding == pool_size`), with `overlap` counted
  separately as the proposals a later source repeated. This is the handoff's
  step 1: a union that reports only its total cannot say which source failed.
- **`kindred eval --pool-mode …`** so widening the pool is a flag.

One defect fixed on the way through: the old union appended collab ids to the
tag pool without checking membership, so a work both sources found could be
ranked twice. De-duplication now lives in the pool's `admit` step.

## Next step, with commands

The measurement is the whole point of the next session. On the host, with the
new binary installed (`bash scripts/deploy.sh > /tmp/deploy.log 2>&1`, never
`tail` it — the last session lost the names of five failing checks that way):

    ssh thinkcentre 'sudo kindred eval --corpus /home/alvaro/kindling-data/ao3_metadata.db \
      --db /var/lib/kindred/kindred.db --users 40 --n 10 --pool-depth 1000 \
      --pool-mode tags+collab'
    ssh thinkcentre 'sudo kindred eval … --pool-mode tags+collab+embed'

Compare, in this order: `pool recall@1000`, then `recall@10`, then `coverage`,
then whether `signals that never ran:` is now EMPTY (it should be — embedding
has vectors, collab has an index, idf/seed_agreement have the doc-freq table).

Decision rule, written before the numbers exist so the numbers cannot be
rationalised afterwards: the default becomes `tags+collab+embed` only if pool
recall rises materially AND recall@10 does not fall. Otherwise the mode stays
opt-in and the doc says what the measurement showed.

Two things to watch, both measured as risks:

1. **Latency.** The embed source joins 32 tags against `work_tags` and orders
   by summed similarity — on this mirror some of those tags carry tens of
   thousands of works. A `time` on both requests before and after is the
   cheapest guard; if it is bad, lower `EmbedPoolTags`, not the reserve.
2. **Resident memory.** +81 MiB once the first embedding-bearing request
   arrives. The host had 4.4 GiB available. Check `peak_rss_kib` on `/healthz`
   after the first request.

## Gotchas

- `tagVectors` caches the load EXACTLY once, error included. A test that writes
  embeddings after a request has already run gets the cached (empty) index and
  will look broken; write the rows first (see `putTagVecs` in the tests).
- `PoolMode` is now FLAGS (`PoolTagsOrCollab = 1<<0`, `PoolTagsEmbed = 1<<1`).
  Any comparison written as `mode == PoolTagsOrCollab` silently stops being
  true for `tags+collab+embed`; use `hasCollab()` / `hasEmbed()`.
- The nil `*tagIndex` receiver is load-bearing: `idx.centroid` is passed as a
  method value when there is no index, and `centroid` checks for nil. Removing
  that check turns "no embeddings" into a panic on the first candidate.
- `signals()` takes a ctx now (it may load 81 MiB on first use). The four
  positional test call sites were updated; a new one needs the same.
