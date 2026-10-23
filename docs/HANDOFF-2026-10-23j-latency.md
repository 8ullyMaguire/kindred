# 2026-10-23j — Latency: the profile said the query, not the code

The roadmap carried one unowned item after the last session: `/tag/{id}` took
3.67 s and nobody had established why. This session established it, fixed it, and
found a second, larger defect underneath.

Everything below is measured on the live deployment (thinkcentre, corpus
`/home/alvaro/kindling-data/ao3_metadata.db`, 634,232 works, 3.89M `work_tags`).

## What the numbers were

| request | before | after |
|---|---|---|
| `GET /api/v1/recommend?seed=…` (same seed again) | 2.62 s | **0.41 s** |
| `GET /api/v1/recommend?…&n=200` | 10.39 s | **0.36 s** |
| `GET /tag/29` (default sort = the taste blend) | 3.67 s | **0.92 s** warm |
| `GET /tag/29?sort=kudos` | 0.53 s | 0.53 s (unchanged — no blend) |

## How the diagnosis went, including the part that was wrong

1. **Guessed.** The blend, the pool, the filters, "the tag page got slower when
   the statistics landed". Every guess was cheap to check and none of them was
   measured.
2. **Timed the SQL in the sqlite3 CLI.** The pool statement is 0.47 s there,
   with the deployment's standing filters 0.50 s, and with the blocked-tag
   `NOT IN` 0.15 s. So the SQL was NOT the 2.4 s.
3. **Built a profiling harness.** `cmd/kprof` (temporary, deleted): the same
   engine construction as `serve.go`, an `httptest` server in front of
   `api.Server.Routes()`, `-cpuprofile`. Compiled here (the deploy host has no Go
   1.27.1) and shipped to the host. It reproduced the 2.6 s exactly — and split
   it: `http 200 in 2.6 s`, `request` (engine only) `0.92 s`.
4. **The profile.** 87% of the request in `_sqlite3VdbeExec`, 82% of that under
   `poolFor.func3` — the single tag-pool statement, 1.44 s per call against
   0.47 s for the byte-identical SQL in the CLI. **The gap is the driver**:
   `modernc.org/sqlite` is a pure-Go port, and this statement does ~99k random row
   lookups into `works` to evaluate the standing filters.
5. **Then a second finding, from asking "does n matter?"** It did, quadratically:
   n=20 → 0.37 s, n=60 → 1.30 s, n=100 → 3.12 s, n=200 → 10.39 s, with the pool
   scored once either way. That is `diversify.Apply`'s MMR pass, not the engine.

## Fix 1 — the pool query is memoised (`internal/engine/poolcache.go`)

The statement is a pure function of its inputs and the inputs repeat: a tag page
seeds from "the tag's five most-kudoed works", so every reader of tag 29 issues the
same query. A bounded cache (512 entries, 10 min) now sits in front of it.

Only the tag half is cached. Collab is an in-memory walk and the embedding index is
loaded once per process; what costs is the SQL.

**What is deliberately not cached:** a request carrying `seen` ids. Those sets are
per-reader and unbounded — in the key they would never be hit twice and would crowd
out the entries that are; out of the key they would serve one reader's exclusions to
another. Such requests bypass and increment a counter, so the hit rate is read
against cacheable traffic.

`/healthz` now reports `pool_cache{hits,misses,evictions,expired,bypassed,entries}`.

Eight mutations killed: key ignoring the filters; `get` returning the stored slice;
`put` storing the caller's slice; expiry never checked; unbounded eviction;
`cacheable` always true; the block set keyed in map-iteration order (which makes the
cache never hit — a "the cache does not help" bug rather than a wrong-answer bug);
and `poolFor` not consulting the cache at all.

## Fix 2 — MMR was quadratic in the number of results (`internal/diversify/mmr.go`)

Two compounding faults:

- `indexOf(working, s)` scanned up to 500 candidates to map a chosen candidate back
  to its tag set, called inside the loop over `selected` inside the loop over
  `working`: n × k × k identity comparisons, 50M at n=200.
- the "maximum similarity to anything already chosen" was recomputed from scratch
  every round.

Now chosen candidates are tracked by index and one pass per selection raises the
unchosen candidates' maximum. `BenchmarkApply200`: **4.69 s → 47.7 ms (98×)**.

**The ordering is unchanged, and that is the point of the test.** The expectation in
`TestTheRewriteKeepsTheSameOrdering` was captured from the OLD implementation, not
derived from the new one. An optimisation that changes which works a reader sees is
not an optimisation.

Two mutants SURVIVED the first version of the discriminating test, which asked for
two results: with one choice made, "max over all chosen", "similarity to the last
choice" and "similarity to the first choice" are all the same number. The fixture
asks for three now, arranged so each wrong rule picks a different work.

## The method notes worth keeping

- **A number is only a comparison against a measurement of the same thing.** Last
  session I blamed the tag page's latency on the statistics block because a 0.35 s
  figure from before the taste default existed looked like the same page.
- **Compare the SQL in the CLI to the SQL in the service before blaming the SQL.**
  Same statement, 3× difference: that comparison is what located the driver.
- **`?pool=` was not doing what I assumed.** `pool=10` and `pool=500` both report
  `candidates: 1000`; the pool parameter is clamped by `FullPoolSize`. A bisect
  built on an ignored parameter produces confident nonsense.
- **`+` in a curl query string is a space.** `pool_mode=tags+collab` answered in
  2 ms because it was a 400. Percent-encode it (`tags%2Bcollab`) or the experiment
  measures the error path.
- **A cold process is not a slow product.** The deploy gate fetched `/tag/$TAG`
  with `--max-time 20` and reused the body; on a cold start the first tag request
  loads the 81 MiB tag-vector matrix and builds the graph, so the body was empty and
  the gate reported "complete control is MISSING" on a working page. 90 s allowance
  now, commented as an allowance and not a latency assertion.
- **Profiling harness recipe** (now in `kindred-recommender-dev`): copy the state DB
  (and `.graph`) to `/tmp` with `sudo cp`, `chown` it, run the harness with
  `-corpus` + `-db`, and read the profile locally with `go tool pprof -peek` /
  `-list`.

## State

- HEAD after this session's four commits; 27/27 packages green.
- Deployed; provenance IN SYNC; behaviour gate **94 checks** (was 92: the cache-hit
  check and the n=200 ceiling).
- `/healthz` pool_cache after the gate's two identical requests: `hits` rising, as
  asserted.

## What is still slow, and why it is acceptable for now

- **The first tag request after a restart** is 2.5 s: it pays the 81 MiB
  tag-vector load, the graph build and a cold pool query. Subsequent requests for
  the same tag are 0.92 s.
- **The cold pool query itself** is ~1.4 s for a 5-seed tag page, because a work
  with 76 tags expands to 99k candidate works through the pure-Go driver. Caching
  amortises it; making it intrinsically cheap means either a per-tag pre-limit
  (which changes semantics: a filtered-out work could never take a slot — the
  documented invariant forbids it) or a driver with a C core. Both are bigger
  decisions than this session should have taken alone.
