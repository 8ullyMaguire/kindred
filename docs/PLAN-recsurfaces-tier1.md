# Kindred — PLAN: Tier 1 Recommendation Surfaces (session target)

One build cycle. Order is forced by dependency: S1 first (eval backbone), then
S4 (implicit measure), then S2/S3/S7 (tag surfaces), then S5/S8/S11 (extra
reading surfaces), then S9 (offline arm, CLI only). Each step ends in one command
and its expected output.

## Status (2026-10-22)

| # | State | Where it lives | Evidence |
|---|---|---|---|
| S1 | **DEFERRED** — see below | — | — |
| S2 | already built (verified, not rewritten) | `tag.html`, `GET /api/v1/tags/{id}/similar` | existing tests + render sweep |
| S3 | shipped | `Engine.SimilarAuthors`, `GET /api/v1/authors/similar?name=` | `internal/engine/authors_test.go`, `internal/api/authors_test.go` |
| S4 | shipped in `0c6b2b7` | `session_taste` signal, `Request.SessionTaste` | `internal/engine/session_taste_test.go` |
| S5 | shipped | `Engine.CoBookmarked`, `GET /api/v1/works/{id}/co-bookmarked`, work panel | `cobookmark_test.go`, `cobookmark_test.go` (API), render sweep |
| S6 | shipped | `?fandom=` alias in the arena API and web, tag-page link | `internal/api/arena_fandom_test.go` |
| S7 | shipped with S3 | `author_tag_profile` evidence naming the shared tags | `authors_test.go` |
| S8 | already built (verified, not rewritten) | work cards link `/recommend?seed=` | existing browser suite |
| S9 | shipped | `kindred eval`, `internal/eval` | `internal/eval/eval_test.go`, `docs/CLI.md` |
| S10 | shipped in `b4397a9` | `idf_overlap` signal | `internal/signal/idf_overlap_test.go` |
| S11 | shipped | `Engine.Bridges`, `GET /api/v1/works/{id}/bridges`, work panel | `bridges_test.go`, `bridges_test.go` (API), render sweep |

Two implementations deliberately differ from this plan, because the plan named
symbols the tree does not have:

- **S3/S7 are addressed by author NAME, not `/authors/{id}/similar`.** The mirror
  has no author table and a byline is free text, so there is no id to put in a
  path. `/author?q=` already addresses a person that way; inventing a hashed id
  would give one person two names. The profile is a cosine over the seed's
  rarest tags (IDF-weighted, binary candidate vector), and the evidence string
  names the shared tags, which is S7's "why this author?".
- **S11 recognises fandoms by name shape, not `tag_type = 'fandoms'`.** Measured
  on the live mirror, 3,890,504 of 3,891,300 tag rows are `freeforms`, fandom
  tags included. A tag-type query finds 51 works out of 113,995 and would
  report "no crossovers" on a corpus full of them.

### Why S1 is deferred rather than half-shipped

The plan's own M1 is internally inconsistent: it adds an `algo_comparison`
`SignalKindName` to a "match-definer" that does not exist (`signals()` builds
scoring signals, not entity kinds), then says no web change is needed, then M3
requires a two-column template. More importantly, the arena's candidate and
rating machinery is built over corpus entities: a pair is chosen from
`entity_candidates`, rated with Glicko, and work judgements feed tag learning.
An "algorithm" is not a corpus entity, there is no algorithm registry, and
recording algorithm votes in `arena_comparisons` with work ids would make the
tag learner treat an algorithm preference as evidence about two works' tags.

Shipping that shape would produce a comparison page whose votes mean something
other than what the page says, which is the failure this project spends most of
its tests preventing. The honest version needs a design pass first: an algorithm
registry, a kind whose candidates come from it, and a judgement path that does
not touch work tag weights. The offline arm (S9) and the existing work arena
already give the two measurements that exist today; S1 remains open rather than
pretending otherwise.


## Before you start

**Read the conventions from the source, not the spec.** The signal module is `internal/signal/signal.go`;
each `Score()` returns `(float64, string, error)` with a human-evidence reason. The store schema lives
in `internal/store/store.go` as a `const schema = ...` block, run by `Migrate`. The arena kind set is
in `internal/arena/kinds.go`; the web templates live under `internal/web/assets/`; the registry
is `internal/web/registry.go` (template name → pageSpec → handler); the route switch is `internal/api/server.go`'s
`mux.HandleFunc("GET /api/v1/...", ...)` lines. The render sweep in `render_pages_test.go` is the gate:
a new `List` page needs TWO rows in the sweep — one populated asserting the empty marker is absent,
one empty asserting it is present. **Reference real symbols; inventing an API produces non-compiling code.**

## M1 — Arena A/B framework (S1)

Engine change: add a new `SignalKindName` string constant ("algo_comparison") to the ranker's match
section so two algorithms can be stacked. Arena change: add a 5th user comparison row when reading
the seeds; no new table — existing `arena_comparisons` stores the user's pick among the paired works.
The wrapper is one new kind, no new handler; existing `handleArenaPair` already reports both works.

```go
// internal/engine/engine.go
// Add to the match-definer section:
case "algo_comparison":
    return &Signal{Name: "algo_comparison", PoolSize: e.PoolSize, TopN: e.TopN}
```

Wait — that won't compile because the case list isn't a switch. Look at `signals()`:
it takes `tuneName string` and selects per signal. Read signals(), patch the returned slice
to add an algo_comparison entry when name=="algo_comparison".

M2 — `kindred tune --type algo_comparison` flags a second algorithm spec. Actually simpler:
each ranker call sets a second Tune via the Request struct `TuneOverride`, which already exists
for the owner preference. Not needed for this feature: we don't need two rankers live. Instead:
add a `--algos` flag to `recommend` that takes two seed requests; the engine runs them both with
different topN and scores each separately; the pair picks wins by arena judgement recorded on each
request's seed id. Simplest form:

```go
// cmd/kindred/serve.go recommend --algos "A:5:32:999,B:5:32:999"
```

The two requests are parallel recommendations on different Tune slices, results listed side by side
on the same page; the reader judges via the existing `POST /api/v1/arena/compare` with seed ids
0 and 1 mapped. Implementation: parse `--algos` into two `Request` structs, run both concurrently,
return both results. **No web page changes needed** — the recommend page lists both columns.

M3 — Arena display: render two result columns on `/recommend?seed=ao3_work:N&n=5&algos=A,B`.
Template change to `recommend.html`: wrap the item list in two sub-sections. Add the empty-state sweep row:
populated with N items each, empty with data-empty="true".

**Verification:** `go test ./...` must show `recommend_test` green with the new flag path returning
two candidate slices. `grep -c "algo_comparison" internal/engine/*.go` > 0. `scripts/check-cli-coverage.py`
does not need a change — the flag is documented inline in serve.go's usage text.

## M4 — Tag-page Similar Tags sidebar (S2)

Zero new computation. `GET /api/v1/tags/{id}/similar` already returns top-K neighbours from the CSR graph.

M5 — Sidebar: modify `tag.html` to fetch `/api/v1/tags/{id}/similar?limit=10` and render a sidebar
panel. Handlers unchanged. Template contract: reuse the existing `data-empty="true"` on the inner list;
the sidebar has its own empty state. **Add a second sweep row**: populated with ten items, empty
with the marker present.

**Verification:** `go test ./...` green. Browser check via `web/e2e/tests/tag.spec.ts` — add a section
asserting the sidebar renders when the tag has neighbours, absent otherwise. `grep -c "sidebar\|similar"
internal/web/assets/tag.html` >= 1.

## M6 — Author recommendations via tag-profile cosine similarity (S3)

Author entity index: `entity_index(entity_id, kind)` with kind="author"; `entity_tags` joins author→works→tags.
Author profile vector = weighted sum of author's works' tag embeddings; work similarity to author = cosine.

```go
// internal/signal/signal.go: new AuthorProfile type
func (s AuthorProfile) Score(c rank.Candidate, seeds []rank.Candidate, st rank.Store) (float64, string, error) {
    // 1. For each seed author, load their profile vectors from embeddings table weighted by work word count
    // 2. Load candidate author's profile vector
    // 3. Cosine similarity, skip if zero vector
    // evidence: "cosine sim X.XX"
}
```

Must add AuthorProfile signals to the match-definer list in `signals()`. Store interface
needs a method to load an author's embedding vector by id: `Store.EmbeddingVec(ctx, kind, id) ([]float32, bool, error)` reusing the existing Embedding() method.

M7 — Endpoint `GET /api/v1/authors/{id}/similar?limit=10` returns similar authors ranked. Template:
either new `author.html` section (sidebar for similar authors) or a second partial in `author.html`.
Add one more sweep row in `render_pages_test.go`: the author list populated with one row + the "why" line.

**Verification:** `go test` green on the new signal. Check `http://127.0.0.1:8010/api/v1/authors/123/similar?limit=10` returns JSON `{items: [...], meta:{...}}`.

## M8 — Per-session implicit taste vector (S4)

From `seen_works` rows owned by the session: for each seen work, sum its tag vectors weighted by seen weight (seen = ±1, skipped = 0). From `arena_*` judgements: win margins on the winner. Materialize lazily in memory per request.

```go
// internal/engine/engine.go: recomputed at Recommend() time
func (e *Engine) sessionTagVector(ctx, session string) (corpus.TagSet, []float32) {
    sum := make(corpus.TagSet, 0)  // or map[int32]float32
    weight := map[int32]float64{}
    // seen works
    rows, _ := e.Store.SeenWorks(ctx, session, ...)
    for each row: sum += tagWeights[tagID], weight[tagID]++
    // arena judgements — requires judge lookup by owner_key
    return sum, normalizedTagsToVec(sum, tags, weight)
}
```

Re-rank hook: after scoring candidates, subtract a penalty for tags the reader dismissed (block.html dismissed works use `seen_works` with a `dismissed` flag — **check what column exists**, defer if absent). If dismissed exists, intersect recommendation candidate tag IDs with block-tag ID set; drop or discount.

**Verification:** unit test one seed, two seeds A and B; assert the B run is ranked above A when the reader's taste strongly favours B. `grep -c "sessionTagVector" internal/engine/engine.go` > 0.

## M9 — "Readers who bookmarked this also bookmarked" (S5)

Collab matrix already computed; this is a join over `work_feedback` and `entity_tags`:
count bookmarked works shared between the seed's authors and other works' authors. One SQL query:

```sql
SELECT w.id AS work_id, SUM(ef.weight) AS collab_score
FROM work_feedback ef
JOIN entity_index ei ON ei.entity_id = ef.work_id
WHERE ef.owner_key = ? AND ei.kind = 'author' AND ef.bookmarked = 1
  AND ei.tag_id IN (SELECT tag_id FROM entity_tags WHERE entity_id = ?)
GROUP BY w.id
ORDER BY collab_score DESC LIMIT 5
```

Endpoint: `GET /api/v1/works/{id}/co-bookmarked?limit=5`. Template: add "Readers Also Loved" panel on `work.html`.

**Verification:** `go test` green with one seed. Browser check: open `/work/58013143` and look for the panel.
`scripts/budget.sh` probe list needs a new GET row — add it there too (see note in routes.txt: add the route here so the two agree).

## M10 — Fandom-scoped arena (S6)

`GET /api/v1/arena/pair?fandom=TAG_ID` skips works not tagged with that fandom. Template: the current arena.html takes no param; add `?fandom=` form field or a dropdown. One query param pass-through.

```go
// internal/api/server.go handleArenaPair: read r.URL.Query().Get("fandom"), set req.TagID if present
```

**Verification:** `go test` green on the scoped pair path. No web test needed — the UI is the same template with a default param.

## M11 — "Why this author?" evidence (S7)

Add two signals: `author_tag_overlap` and `author_cosine`. Evidence strings come from the existing signal framework; the "why" line is just the printed `reason` field.

**Verification:** signal tests green. No endpoint needed — reuses the author similarity endpoint from M7.

## M12 — "More like this" button (S8)

Change `work.html` to link `/recommend?seed=ao3_work:{id}` on each work card. One line. No API change.

**Verification:** browser check that clicking opens `/recommend?seed=ao3_work:<id>` with the seed's tags.

## M13 — Offline eval harness (S9, deferred but spec'd)

A new command `kindred eval --corpus ~/kindling-data/ao3_metadata.db --db ~/.local/share/kindred/kindred.db --ratio 0.2 --n 10`. Reads all works in the corpus, holds out 20% of bookmarks, scores recommendations on the held-out portion, computes recall@10 and NDCG@10. Pure computation, no web surface. Runs once per corpus rebuild.

**Verification:** `go run ./cmd/kindred eval --help` prints usage; actual run on live DB returns JSON with recall≥0, ndcg≥0. Defers to next turn after S1–S11 ship.

## M14 — IDF-weighted tag matching (S10)

A function `tag_idf(tagIDs) -> map[int32]float64` counts document frequency per tag and applies inverse document frequency in tag_overlap's Jaccard calculation. Cheap: one pass over the work_tags index. Reuse in scoring without changing the Tune weights.

**Verification:** unit test on synthetic seed; assert rare tags outrank common tags under equal overlap count.

## M15 — Cross-fandom bridge works (S11)

Reuse the graph CSR: find works tagged with ≥2 fandoms where the fandom-fandom similarity to a preferred fandom is low. One endpoint `GET /api/v1/works/{id}/bridges` returning works tagged with tag_id1 and a different tag_id2 (both in the same fandom-family pair). Logic: from `edges` + `entity_tags`, count co-occurring fandom tags per work; works with ≥2 fandom tags are bridge candidates.

**Verification:** `go test` green. Browser check: open a fandom page and look for the bridges panel.

## End-to-end

Each milestone ends in `go test ./... && go vet ./... && scripts/check-provenance.sh && go build -o bin/kindred ./cmd/kindred`. Deploy with `./bin/kindred serve --corpus <mirror> --db ~/.local/share/kindred/kindred.db`. Live check: `curl http://127.0.0.1:8010/healthz` → 200, every new route responds 200 on a GET with a seed param.

## Commit shape

Commit spec+plan now. The plan catches its own drafting defects the same way it always does:
extract every Go block to scratch, `go mod tidy && go build ./... && go test ./...` against the same deps, fix. Say in the commit message what the extraction found (ideally none — this plan is short enough to verify).

##Deferred tickets

The other 29 ideas (Tiers 2–9) are listed as deferred ticket numbers under docs/PLAN-recsurfaces-deferred.md so they stay in the backlog. This spec is the contract for the ones shipping in this cycle.
