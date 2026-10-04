# kindred — decisions and why

Alternatives that were rejected, with the reason. Kept because a
decision without its rejected alternatives gets re-litigated every time
somebody asks "why not just…".

## Static Go over a Python ML service

**Rejected:** keeping the Python recommender and making it fit. The old
deployment was 800 MB–1.2 GB, which is the entire reason it came off
thinkcentre. Cutting Python to 220 MB means leaving out the model, and
the model is the expensive part.

**Chosen:** a static 12 MB binary that does the same job. The embedding
is a power iteration over the co-occurrence graph, which is 40 lines of
linear algebra rather than a dependency. No Python, no BLAS, no
model file to ship.

**Cost:** the embedding is a spectral approximation, not a trained
model. It is genuinely worse at capturing semantics and genuinely better
at fitting 220 MB. That trade is the project.

## Embeddings off in lite mode

**Rejected:** computing a small embedding for lite. A k=8 embedding on
634,232 tags is 20 MB — inside the cap, but it is 20 MB for a signal
lite mode already decided not to use.

**Chosen:** lite runs on tag-overlap and neighbourhood only, and the
weights renormalise so nothing divides by a zero. lite is 58 MiB
against a 60 MiB cap, and the alternative made the cap a coin flip.

## Capped adjacency, built capped

**Rejected:** build the full 15,500,668-entry adjacency, then prune to
top-N. Simple to reason about, and it measured **352 MB** — both forms
live at once.

**Chosen:** cap while filling. The cap is applied in descending
co-occurrence order, so the kept edges are the strongest, and the
unpruned form is never allocated. 26.8 MB.

**The same reasoning, applied to the embedding matrix.** It was
uncapped first — 15.5 M entries, 124 MB each way, eigensolver peak
440 MB. Capped at top-24 it is 2,884,447 entries, 23 MB each way.

## CSR streamed in both directions

**Rejected:** `os.ReadFile` the 28 MB index and slice it. The loader
then held the file and the arrays it described.

**Chosen:** a 256 KiB read buffer and a length-tracked write buffer, and
a check that the file is exactly the size its header claims. The size
check is not defensive padding — it is what would catch a
writer/reader layout disagreement in production, which is the bug class
that a round-trip test cannot see.

## float32 in the eigensolver

**Rejected:** float64 throughout. 32 × 634,232 × 8 B is 162 MB per
matrix, three live at once: **measured 560 MB** against a 220 MB cap.

**Chosen:** float32 storage, float64 accumulation. Accuracy settles at
~1.4e-7, which is float32 epsilon and is asserted as a floor in
`TestEmbedConvergesToFloat32Precision`. Accumulation stays in float64 so
the error is subtractive rather than growing.

## Components to disk, then transpose

The solver produces components: k=32 vectors of 634,232 float32,
component-major. The store wants one dim-vector per tag: entity-major.
Transposing in memory is 162 MB on top of the solver's own needs — that
was the 379 MB peak.

A per-component sink does not help, and this is the non-obvious part: a
single component carries only 1 of the 32 coordinates, so it cannot
write a complete row without the other 31. The two layouts are genuinely
incompatible at the granularity the solver emits.

**Chosen:** write each component to disk as it is produced (2.5 MB each,
through a 64 KiB buffer), then a second pass reads the 32 files in
lockstep and writes the rows. Working set is 32 file positions, not 32
vectors. Peak stays at the solver's.

## Pool defaults differ by mode

**Rejected:** one default. A default tuned for ranking quality on a
16 GB host is the wrong default on a 512 MB Pi, and the pool is the
largest per-request allocation there is.

**Chosen:** 200 in lite, 1000 in full, both overridable with `?pool=`.
The full-mode budget step walks a 1000-strong pool to prove the ceiling
holds, rather than assuming the smaller default implies it.

## Bounded memo, not a cache

**Rejected:** memoising `Neighbourhood`'s vote map in a package-level
`map` keyed by the seed set. This is what the previous deployment
shipped, as `_neighborhood_cache`, and it is an unbounded leak on a
process that runs for weeks.

**Chosen:** an 8-entry LRU. A test asserts the bound, not just the hit
rate — the bound is the property that matters and the one a
performance-motivated change would remove.

## A migration that reaches existing databases

**Rejected:** `CREATE TABLE IF NOT EXISTS` and call it a schema.

**Chosen:** `CREATE TABLE IF NOT EXISTS` for fresh databases, plus a
`migrate` step that inspects `pragma_table_info` and rebuilds the table
when a column is missing. `IF NOT EXISTS` does nothing for a table that
already exists, so a schema change silently applied only to new
databases — the worst shape for one, because the code that reads the new
column works on a new database and fails on an old one, which is a
failure only operators upgrading ever see. That is exactly how the
`embeddings.kind` column failed on the first real run.


## Shards are plain JSON, and named for it

They were written as `shard-NNN.json.zst` with no compression, on the
assumption a compression step would come later. It never did — the
project has three dependencies on purpose and a static 12 MB binary for a
Pi is the point.

Nothing in the codebase was misled: `fetch` moves shards as opaque bytes
and `verify` hashes them. But a **snapshot is the interface to other
people running other implementations**, so the only place the lie was
visible was the only place that mattered. A peer running `zstd -d` gets
an error.

Renamed to `.json`. Compression is still worth it — 7.0 MB to roughly
2 MB over Tor — but as its own decision with its own measurement, not as
a file extension.

## Privacy verification has to match whole tokens

The first leak check searched for identifier bytes anywhere in the
snapshot. It reported **4 usernames and 11 work ids present**, which is
alarming and entirely false:

- `mmyin`, `elrond` — substrings of tag *names* (`tom[m]yinnit`,
  `elrond peredhel`)
- `82492`, `608a` — hex fragments of SHA-256 shard hashes
- work ids — inside pseudonyms (`"p":"bef0a…19863145e"`) and inside
  float weights (`0.04158004158004158`)

Matching on **parsed JSON string values** instead:

| what | published | of |
|---|---|---|
| usernames | 0 | 6,261 |
| work URLs | 0 | 112,935 |
| tag ids | 255 | 634,231 |
| work ids | 2 | 112,935 |

The 255 and the 2 are both shard-index keys `"0"`–`"255"` in
`manifest.json`, plus two **tag names that are literally numbers** — AO3
has tags named `15` and `200`. A tag named "15" is not an identifier of
anything; it is what the k-anonymity threshold is protecting.

The rule this earns: a privacy check over an opaque byte stream proves
nothing, because hashes, floats and pseudonyms all contain digits. Parse
the format and compare whole values, and remember that a filter matching
nothing reads as clean.


## v0.1.0 released, and two bugs that only deploying could find

Public in two places, both at the same sha:

- https://github.com/8ullyMaguire/kindred (topics, AGPL auto-detected)
- https://git.polarisocial.xyz/hirrolot19/kindred (Forgejo ignored the
  license and topics API fields — a server bug, not worth fighting; the
  LICENSE file is what governs)

Deployed on thinkcentre as `kindred.service`, running as an unprivileged
`kindre` account, 134 MiB peak against a 220 MiB cap.

### 1. Go identifiers were leaking into the API

`graph.ScoredNeighbour` is serialised directly by the tag-similarity
endpoint and had no json tags, so the response carried `TagID`, `PMI`,
`Count` in a body where every other key was snake_case. A client reading
`tag_id` got nothing, with no error.

**Why the suite missed it:** no API test builds a graph. The only
similarity test covers the 503 no-index path, which returns *before*
serialisation. A test that never reaches the broken line cannot find the
bug in it.

The test I added asserts the exact key set in both directions, and I
checked it fails when the tags are reverted. A test that has never been
seen to fail is not evidence.

### 2. `Environment=` does not expand the executable path

```
ExecStart={ path=${KINDRED_BIN} ; ... }   status=203/EXEC
```

systemd execs the literal string `${KINDRED_BIN}`. The expansion works in
the *arguments*; in the command position it does not. 203/EXEC reads as
a permissions or sandbox problem and is neither — which is why I guessed
twice and was wrong twice (MemoryDenyWriteExecute, then the sandbox
directives) before asking the question that answered it in one call:

```sh
systemctl show kindred -p ExecStart     # shows the UNEXPANDED path
```

`systemd-analyze verify` had already said "Command ${KINDRED_BIN} is not
executable". I dismissed it as a false positive. It was the bug, reported
correctly, and I read past it because `${VAR}` in ExecStart is genuinely
a documented thing and I pattern-matched to the man page instead of the
output.

### 3. `%h` is not a safe substitute for a hard-coded path

In `ReadWritePaths=`, `%h` expands to the *service* user's home, not the
installer's. With no `User=` that is `/root`, and the unit died
`226/NAMESPACE` before the binary ran.

So the privacy fix for a public repo and the sandbox hardening are in
direct tension: removing the hard-coded path is right for the repository
and wrong for the unit. The unit now uses absolute paths and a
documented `User=kindre`, and the header says which three lines to
change. `%h` survives only in `Environment=` for the flags, where it is
harmless.

## Reading the numbers honestly

`/api/v1/tags/1/similar` returns zero results, and it is correct. Tag 1
is "general audiences" with 13,123 edges; it co-occurs with everything
far *less* than independence predicts, so its PMI is negative and the
handler drops non-positive scores rather than returning a zero that would
sort among genuine negatives. Tags with 0–2 uses and no edges have
nothing to be similar to.

Half the tags I sampled return results and half return none, which looks
like a bug until you sort by frequency: 621/49/225 edges → results,
0 edges → none. It is the statistics being right, and the handler is
honest about it. I was twice wrong here before checking — first a query
against `src`/`dst` columns that do not exist (the table is `tag_a`/
`tag_b`), so every count came back empty and read as "no edges anywhere".


## v0.2 — the web UI, and what deploying it actually taught

Live at https://kindred.polarisocial.xyz, nginx on 8009 -> kindred on
8010, tunnel route `kindred.polarisocial.xyz -> http://localhost:8009`.

### The diagnosis was wrong twice before it was right

"Still doesn't show anything" was reported, and I attributed it entirely
to a missing DNS record. The record genuinely was missing — and it was
not the cause. `/` returned 404, because kindred was API-only. Gravity,
Lorehaven and Concord all serve HTML at `/`; comparing what the other
sites return would have found it immediately.

**The lesson worth keeping: when a symptom is "nothing shows", check what
the neighbours return for the same path before theorising about DNS.**

### The theme had to be read, not remembered

`~/code/ruby/otwarchive/public/stylesheets/site/2.0/` has the real
values. My from-memory draft was wrong on both of the two that matter:

| | I wrote | actually |
|---|---|---|
| body | serif throughout | **sans** (`Lucida Grande` stack) |
| links | `#1f7ec2` blue | **`#900` maroon** (35 uses) |

Body text is `#2a2a2a`, borders `#ddd`, alt `#eee`/`#f3efec`, and a pale
rose `#efd1d1`. Tags are not pills: `a.tag` is `#111` with a dotted
underline inverting to `#900` on hover. Verified in a real browser —
computed `font-family` and `rgb(153,0,0)`.

### Four bugs, all found by rendering rather than by reasoning

1. **Every page was 200 with one byte.** `ParseFS` over `assets/*.html`
   into one set resolves duplicate `{{define "content"}}` to the last
   parsed, so one survived; executing `"search.html"` found a name with
   no body. One template set per page now.
2. **`{{range $k, $v := (dict ...)}}` over a map** yields values in
   sorted KEY order — the page printed the words count under the label
   "Hits". Silent, and plausible-looking.
3. **`stat` returned `(string, bool)`.** html/template accepts a second
   return of `error` only; a bool is a **panic at init**, which killed
   every test in every package at once.
4. **The tag page 500'd on three NULLable columns in a row.** The error
   page named the column, but finding out one 500 at a time is backwards.

### The button that was inert, and no JavaScript at all

The work page pre-filled a hidden seed input with every recommendation
it was already showing, and the click handler's `add()` refuses an id
already in the list. All ten buttons were no-ops. A DOM probe: 10 hidden
inputs before the click, 10 after.

Fixed by deleting the mechanism: the control is now a real
`<a href="/recommend?seed=...">`. `app.js` is gone. The frontend is one
stylesheet and plain links, so it works with scripting off — which is the
right shape for a 512 MB Pi anyway.

**A button that looks enabled, is enabled, is wired to a real form, and
does nothing is the failure mode no test finds.** It took a real click.

### Two claims, proven rather than asserted

Mutating `guardAPI` so the web layer swallowed `/api/`: the API tests
fail on all seven routes with "not JSON: invalid character '<'".
Swapping `html/template` for `text/template`: the escaping test fails.
A test never seen to fail is not evidence.

### Memory after the UI

| | |
|---|---|
| serve, full | **185.7 MiB** peak / 220 cap (was 145) |
| serve, lite | ~25 MiB / 60 cap |
| binary | 14 MB (was 12) |

The rise is the CSR being faulted in on the first page request, not a
leak: 30 work-page requests left RSS flat at ~177 MiB. 42 MiB headroom
remains, which is thinner than I would like for a public endpoint — worth
watching if `/recommend` ever gets load.

## 2026-09-29 — the arena UI, and three bugs only the real corpus found

`internal/arena` had the Glicko-2 engine, four pairing strategies and tag-weight
learning the whole time. Nothing reached it: no templates registered, no routes
mounted, no link anywhere. The feature existed and was unreachable, which is a
different failure from missing, and it is invisible to `go build` and to a green
test suite.

Wired: `/arena`, `/leaderboard`, `/rank/<id>`, `/my-ranking`, plus POST
`/arena/judge` (the one POST a page accepts, since a form cannot POST to a JSON
API and land back on a page). Judging is a radio form, no JS — consistent with
the earlier decision to delete `app.js` entirely.

**Verified on thinkcentre against the real 1.7 GB mirror**, not just in tests:
pair served in 111 ms, judge returns 303 and advances, tag weights learned from
one choice (`marauders friendship` up, `human on mushroom violence` down), zero
panics in the log.

### The three bugs, and why the suite was green through all of them

1. `CandidateWorkIDs` ordered by a **`comments` column that does not exist** —
   not in the 1.7 GB mirror, not in the test fixture either. The code comment
   asserted it was "the field that is actually populated". I checked: `hits`,
   `kudos`, `bookmarks`, `word_count` and `summary` are populated for all
   112,935 rows. Now `hits`.

2. `tagLinksForWork` built its tag-name `IN` clause by **slicing the work-ID
   placeholder list to the tag count** — `placeholders[:len(allTags)]`. Two
   works, 78 tags, a two-element slice. `slice bounds out of range [:78] with
   capacity 2`, panic on every arena request, process dead. It compiled and
   passed every test that existed.

3. `internal/web` had **no test file at all**. The page tests live in
   `internal/api` (that is where `web_test.go` is), so the arena was only
   reachable through the API fixture — and nothing in the suite asked for the
   arena pages. Bug 2 compiled *and* passed because the path had zero coverage.

The regression test for bug 2 is proven: with the slice restored it fails `EOF`,
with the fix it passes. Restored the source in the same process that mutated it.

### Why the test suite could not have caught any of this

Every assertion in it was about the arena *package* — the arithmetic, which was
correct. The bugs were in SQL column names and slice bounds in the *web* layer,
against a 1.7 GB database, on a code path no test requested. A suite that only
ever checks the engine cannot see a query that names a column that isn't there.

**This is the same shape as the earlier `app.js` finding: a control that looks
enabled, is enabled, and does nothing is the failure mode no test finds.** The
arena took a real request.

Lite RSS after ten arena requests: **51 MiB of the 60 MiB cap**. Thin. Worth
watching — the arena's per-request work is heavier than a tag page.

Commit `8052070`, pushed to github and forgejo.

## 2026-09-29 — the batch that could never write a rating

Deployed, then `/leaderboard` was still empty. It looked like correct Glicko
behaviour (no batch had run), so I ran one — and it 500'd on every call.

**Bug 1: the write never worked.** The batch handed a `store.RatingRow` struct
as a single argument to a statement expecting nine. `database/sql` cannot
marshal a struct, so the driver rejected it before preparing. The store's own
`SaveRating` does the identical upsert correctly with positional args — so the
bug was a *second, wrong copy of working SQL* sitting in the API layer, which
no test could catch because the two copies looked fine side by side.

**Bug 2: every run re-folded all history.** `arena_last_period` was written as
the period NUMBER and read back as a timestamp. `time.Unix(1, 0)` is
1970-01-01T00:00:01Z, so every batch took the entire history as one period.
Counter and watermark are now separate keys: a period is a set of games *since
the last one*, which is a statement about time, and the number is derived from
it afterwards.

**Bug 3: the bound was inclusive.** `judged_at` is `datetime('now')` — one-second
resolution — and the watermark is whole seconds too, so `>=` re-included
everything judged in the same second the last batch finished. Renamed
`JudgedSince` → `JudgedAfter` and made it `>`.

**Zero tests called `RunBatch`.** That is the whole reason all three survived.
The arena suite tested the arithmetic (`internal/arena`), which was correct.
The defect lived entirely in the boundary between correct arithmetic and the
database.

### routes.txt was a lie

It claimed to be "the single source of truth for what `make budget` walks" and
**nothing read it** — `scripts/budget.sh` has its own hardcoded probe list. So
every route added to routes.txt was silently outside the memory budget, which
is how the arena shipped with zero budget coverage. The new arena probe asserts
on *content*, not status, for the reason the existing tag-similarity step
documents: a 200 that renders the error page is invisible to a status code.
Verified the probe fires against a stub that answers 200 with an error page.

### The pattern across all four of today's bugs

1. `comments` — a column that exists nowhere.
2. `placeholders[:len(allTags)]` — two lengths, one slice.
3. `RatingRow` as a query arg — a struct where nine scalars were needed.
4. period number read as a timestamp.

None is exotic. Every one is **a boundary between a correct computation and
the thing it talks to**, and in every case the arithmetic was tested while the
boundary was not. `internal/arena`'s tests are thorough and they all passed
through every one of these. A unit test of a function proves what the function
does with the values you hand it; four bugs in one day lived entirely in what
the caller hands it and what the callee can accept.

Commit `22b6d29`.

## 2026-09-29 — a bug the user found that I had verified past

The user pasted an error page from `/arena`:

```
unjudged in session: sql: Scan error on column index 7, name "presented_at":
unsupported Scan, storing driver.Value type string into type *time.Time
```

`presented_at` is written by `datetime('now')`, which has no type in SQLite, so
the column is TEXT. Scanning TEXT into a bare `time.Time` is a **runtime**
error — it compiles, and every test passes.

Two things hid it:

- `judged_at` sits immediately beside it as a `sql.NullTime`, which absorbs a
  string silently. The struct *looked* handled. One field was a landmine and one
  was fine, adjacent, and only the landmine was ever read on the failing path.
- The only query reading `presented_at` is the **resume** path. Every test
  judged the pair immediately, so nothing ever resumed one.

**I verified straight past this.** My deployment check was: fetch a pair, POST
a judgement, fetch the next pair. Three requests, all green — and it never once
abandoned a pair and came back, which is the flow a real user hits on their
second visit. A verification that only exercises the path you already know
works is a regression test of the known-good path.

The generalisable form: **a test that only completes the happy path cannot see
a bug in the path taken when the happy path is abandoned.** Resume, retry,
cancel, timeout, and re-login are all the same shape of untested code, and all
of them are what production does when someone changes their mind.

`TimeValue`/`NullTime` wrappers with a shared `scanTime` now handle every TEXT
timestamp in the state DB. The regression test presents a pair, abandons it,
and returns — and with the fix reverted it reproduces the production error
verbatim, column index and all.

This is the **fifth** bug in the arena, and the first one a user reported rather
than one I found. Four of the five are again at a boundary (SQL column type,
query args, time range, struct marshalling) and none is in the arithmetic.

## 2026-09-29 — peer_rating, and a batch that runs on its own

The last two gaps. Both were in SPEC all along, which is the lesson: I had
been treating the spec as a description of what exists rather than as a list
of obligations.

**`peer_rating` was specified.** SPEC §1 lists it as a named signal and §7.1
requires a signal's absence to be reported in `meta.degraded[]` — and §1
records kindling's exact failure as *"arena signals were inert in production
and silent about it."* So this was not a product decision I was entitled to
defer; it was an unbuilt requirement I had been calling a choice.

First version guarded it on `e.ArenaRatings != nil`. The test caught that
immediately, and it caught it for the reason it should: **a signal only added
when it has data is a signal absent without saying so** — the bug restated
one layer down. Registered unconditionally now; with no ratings it returns
`rank.ErrSkip` and names itself.

Default weight 0.11, subtracted from the others so the tune still sums to 1.
It is the only human-judgement signal, so its weight is the one that should
earn its place. `kindred tune` raises it without a code change.

**The batch runs on a timer**, and it POSTs to the *running server* rather
than invoking a second binary. That is not a style choice: the ratings live
in the serving process's memory, so a separate writer updates the database
and leaves the recommender scoring stale values until restart. A batch that
ran, logged success, moved the leaderboard, and changed nothing a reader sees
is worse than no batch.

### The concurrency test that passed with the mutex removed — twice

I introduced an RWMutex (a batch swaps a map that in-flight requests read —
a fatal concurrent map write, not a subtle wrong answer) and wrote a race
test to match. **It passed with the lock deleted.**

1. Judge twice, batch three times → batches 2 and 3 *skip*, return before
   touching the map. Reader and writer never met.
2. Interleave judge-then-batch 12 times → measured 12 judgements, **one**
   real batch, 11 skipped. The watermark is wall-clock and the loop runs
   inside one second, so `judged_at > watermark` excludes everything after
   the first batch.

So the interleaving was right and the *trigger* was wrong. Fixed by asserting
the property through the HTTP surface instead — is `peer_rating` still in
`meta.degraded[]` after a batch? — which does not depend on scheduling at
all, and fails with the reload removed.

**A test that hopes a race gets scheduled is a test that may not run.** This
is the sixth bug today and the second time a test of mine was green for the
wrong reason. The tell is always the same: ask what the test would do if the
code it guards were deleted, and actually delete it and run it.

Commit `263174a`.
