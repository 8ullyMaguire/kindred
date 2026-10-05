# Kindred — SPEC

> A general-purpose entity recommender and read-only mirror API. One static
> binary, no runtime services, ~90 MB resident on a 512 MB Pi and ~220 MB on
> thinkcentre. Succeeds **Kindling** (AO3 fanfiction recommender, 14k lines
> Python, 6.4 GB across two gunicorn workers, removed from thinkcentre
> 2026-09-29 for memory).

**Status:** drafted 2026-09-29. ~~Nothing built.~~ **Built** — see the state note
below, which supersedes this line. Working name **Kindred**
(`kindred`, module `git.polarisocial.xyz/kindred/kindred`) — a one-word
change to a config value if it goes somewhere else.

---

## 0. What I changed from the brief, and why

The brief: "as generally useful as possible, recommending every entity not
just works." Taken literally that is a recommender with no content, which
recommends nothing. I read it as **the engine must be entity-kind-agnostic
and the AO3 corpus must be the first source, not the architecture.** §2 and
§3 are that reading made concrete. Everything below follows from it.

Three measurements from the live system contradict assumptions worth
stating up front, because the spec's numbers come from them and not from
estimates.

### 0.1 The memory is in the graph's data structure, not in the language

Kindling's tag co-occurrence graph is **3.0 GB resident, 7,750,334 edges
over 123,047 tags**, held as Python dicts of dicts. A dict entry costs
~100 bytes of overhead against 8 bytes of payload; the same graph as a CSR
array in Go is **~124 MB** — a 24× reduction from representation alone,
before a single line of algorithm changes. The second worker doubles it.
That is why kindling needed 6.4 GB and Kindred needs 140 MB of index.

The honest caveat: the graph's own metadata row claims 20,262 nodes
(`cooccurrence_graph_meta`, built 2026-06-26) while the edges table
actually references 123,047 distinct tags. The metadata is stale. **Every
number in this spec that comes from the graph is re-measured in PLAN step
M1.0, and the plan prints what it found rather than trusting the row.**

### 0.2 Most of the tag signal is `freeforms`, not structured metadata

`work_tags.tag_type` across the 3,891,300 rows:

| tag_type | rows |
|---|---|
| freeforms | 3,890,504 |
| characters | 304 |
| relationships | 175 |
| fandoms | 104 |
| archive_warnings | 83 |
| categories | 79 |
| rating | 51 |

Structured categories are 0.02% of the data. The structured axis is
effectively unused, so the generalisation to "any entity" costs nothing
here — tags are already a freeform bag with an optional type label. That
is the shape every source adapter can produce, which is why §2 works.

`works.bookmarks` is empty for 112,890 of 112,935 rows (measured
previously; ordering by it sorts by NULL). Kindling ranks on `kudos`. The
new schema makes NULL-vs-zero explicit rather than inheriting the trap.

### 0.3 Serving an unofficial AO3 API does not have to touch AO3

"Take some load off AO3" is best achieved by not making the request at
all. Kindred serves **only from its mirror**; there is no code path from a
request handler to an outbound client, and a test asserts the package does
not link one. Every request served is a request AO3 never sees. This is
both the safest design and the most useful one, and it is a §11
requirement, not an optimisation.

---

## 1. What it is

A single Go binary that does three things:

1. **Indexes** a corpus of *entities* from one or more pluggable *sources*.
2. **Recommends** entities of any kind from any seed, via kind-agnostic
   *signals* composed with configurable weights.
3. **Serves** a read-only, cache-first API over the index — including an
   unofficial AO3-compatible surface — and **publishes** a periodically
   rebuilt, anonymised, signed snapshot of the corpus over a Tor onion
   service so peers can pull it without learning the operator's IP.

Nothing else. No accounts, no sessions, no writes to the source, no
scraping on the request path, no Node, no Postgres, no Redis, no search
daemon, no ML runtime.

### 1.1 Explicitly not shipping

| Cut | Why | Re-entry |
|---|---|---|
| User accounts, sessions, auth | the recommender does not need identity; the dump is better without it | only if a private-taste feature is ever required |
| Web UI | a JSON API is what kindling's consumers and any peer actually use; a UI is a separate artifact | M8, optional |
| Live AO3 scraping | 525-with-valid-body makes latency unbounded; it also puts the operator's IP against AO3's terms | a background `ingest` command, opt-in, never on the request path |
| `implicit`/MF recommender | the tag graph plus SVD embeddings cover the same ground; MF needs a second heavy dependency and a training loop | if the eval harness shows a measurable gap |
| Click/training feedback loops | needs accounts; the arena already models pairwise taste without them | — |
| Federation/ActivityPub | out of scope for a recommender | — |

---

## 2. The general abstraction

### 2.1 Entity

Anything with identity, a human-readable label, and a bag of weighted tags.

```
Entity { id, kind, title, url, summary, stats{}, tags[{name, type, weight}] }
```

`kind` is a free string, not an enum: `ao3_work`, `ao3_user`, `book`,
`film`, `boardgame`, `person`, `publisher`. Two entities of different
kinds may be linked (§2.4).

### 2.2 Source

A plugin with one job: enumerate entities of a kind into the store.
Written once per corpus (AO3, goodreads CSV, a local library, TMDB), never
touched by the engine or the API.

```
type Source interface {
    Kind() string
    Iterate(fn func(Entity) error) error
}
```

Because an entity is the unit, "recommend books" and "recommend AO3 works"
are the same code path with a different `kind` filter.

### 2.3 Signal

A named, independently testable scoring dimension producing `0..1` for a
(candidate, seed-set) pair, with a human-readable reason string that ships
in the response's `evidence`. Kindling's implicit dimensions become these:

| Signal | Kindling equivalent |
|---|---|
| `tag_overlap` | direct tag agreement, Jaccard-style |
| `embedding` | TruncatedSVD over the sparse PMI matrix (the `TagEmbedder`) |
| `neighbourhood` | PMI-weighted tag graph neighbours of the seed tags |
| `quality` | kudos per 1k words, completion, length band |
| `recency` | `update_date` / `first_seen` |
| `popularity` | log-scaled hits + kudos |
| `peer_rating` | kindling's arena — pairwise crowd ratings, kind-agnostic |
| `taste` | reader-bookmark Jaccard against the seed's author (opt-in) |

A **tune** is a named weight vector over signals. `kindred tune tune --set
embedding=0.4` changes ranking without a code change — the property
kindling's `tune.py` already provides, generalised and made per-kind.

### 2.4 Link

`entity_links(from_kind, from_id, to_kind, to_id, relation, weight)` —
"this author wrote that work", "this work is in that collection". Links are
a first-class traversal used for both recommendation (follow `author_of`
two hops from a seed) and API expansion (`/authors/{name}/works`). They are
how a cross-kind seed works without the engine knowing what either kind is.

### 2.5 Diversity

MMR over the ranked list with per-`kind` and per-group caps, mirroring
kindling's `cap_diverse`. **The engine reports the shortfall instead of
padding** — a list of 90 that says "90 of 100: 10 exceeded the cap" is
honest; a list of 100 that ignored the cap is not. This was an explicit
correction in kindling's own spec (§0.3) and is preserved deliberately.

---

## 3. The unofficial AO3 API

Unofficial, unaffiliated, and deliberately incapable of harming AO3.

### 3.1 Surface

AO3-shaped, so existing clients work against a mirror:

```
GET /v1/healthz
GET /v1/version
GET /v1/works?page=&per_page=&sort=kudos|updated|word_count
GET /v1/works/{id}                 → work, tags, authors, stats
GET /v1/works/{id}/related         → ranked by the engine
GET /v1/users/{username}           → pseudonymised profile, counts only
GET /v1/users/{username}/works
GET /v1/tags?q=                    → tags with counts
GET /v1/tags/{name}/works
GET /v1/search?q=&tag=&author=&min_words=&max_words=&complete=&rating=
```

Plus the engine-native surface, which is where the generality shows:

```
GET /v1/kinds
GET /v1/entities/{kind}/{id}
GET /v1/recommend?kind=&seed={kind}:{id}[,{kind}:{id}]&n=&tune=
POST /v1/recommend                 → same, body form, up to 50 seeds
GET /v1/entities/{kind}/{id}/neighbors?relation=
GET /v1/lineage/{kind}/{id}        → the kinds reachable by link
GET /v1/stats                      → corpus counts, index size, build stamp
```

### 3.2 The rules that keep it defensible

1. **Read-only and cache-only.** No request handler can make a network
   call (§0.3). Serving is a pure function of the local index.
2. **Staleness is visible.** Every response carries `X-Kindred-Index-Age`
   and `X-Kindred-Index-Version`. A mirror that is three days stale says
   so in a header rather than pretending otherwise.
3. **It is labelled.** `Server: kindred` plus a JSON `unofficial: true` on
   every document, and a human-visible line on the root page.
4. **Kill switch.** `KINDRED_PUBLIC_API=0` disables every `/v1` route
   except `/v1/healthz`, live, without a redeploy or restart — the routes
   check an atomic flag. The operator can turn off the unofficial surface
   in one config change if AO3, a user, or a takedown asks.
5. **Rate limited** per client key, honouring `CF-Connecting-IP` only from
   loopback (the same trust argument kindling got right: the listener is
   loopback behind a local proxy, so a forwarded header from loopback is
   trustworthy and one from anywhere else is not).
6. **Attribution and terms are an owner action, not a build step.** AO3's
   ToS governs an unofficial API. The spec cannot resolve that; §12 lists
   it as the decision the owner has to make before the port is exposed.

---

## 4. The anonymised peer snapshot

### 4.1 Goal

Let other operators run Kindred over this corpus without either party
learning the other's IP, and without publishing anything about individual
readers.

### 4.2 Anonymisation, decided per table

The corpus has user data. `users` (6,261 rows) carries `url` and
`username`; the interaction tables carry bookmark sets. Publishing those is
publishing what named individuals read, which is the one genuinely
sensitive thing here.

| Table | Published? | Treatment |
|---|---|---|
| `works`, `tags`, `work_tags` | yes, whole | all public AO3 facts |
| `users` | **dropped** | replaced by `tag_affinity` (below) |
| `sessions`, `session_seeds`, `session_cache` | dropped | request-scoped state |
| `taste_profiles`, `taste_profile_signals`, `seed_weights`, `user_work_interactions` | dropped | per-reader history |
| `cooccurrence_edges` | pruned | see §4.4 |
| `works.summary` | kept | public text; 0 rows are private |

`tag_affinity` replaces the dropped user tables:

- one row per user **with ≥ K bookmarks** (K = 20, `--k-anon`),
- a per-dump **random salt**, so pseudonyms cannot be linked across dumps
  by default,
- a normalised vector of `tag → weight`, with **no work IDs** — so a row
  says "reads a lot of `slow burn` and `enemies to lovers`", never "read
  this specific work",
- the `>= K` filter is applied *before* the salt, so the row count itself
  does not leak a small account.

**Trade-off, stated plainly:** a per-dump salt means two peers cannot merge
the same reader across dumps, because the pseudonym differs. `--stable-salt`
keeps linkage possible across dumps, which is a real research affordance and
a real re-identification risk. Default is per-dump; the stable mode is
opt-in, and enabling it is a logged decision in the manifest.

### 4.3 Distribution

- Content-addressed **shards**: rows bucketed by `hash(entity_id) % 256`,
  zstd-compressed, named by the sha256 of their contents.
- **Signed manifest**: `manifest.json` lists version, base version, build
  time, row counts, `k`, salt mode, and every shard's hash; `manifest.minisig`
  signs it. A peer verifies signature then hashes, and refuses on either.
- **Deltas**: a delta manifest lists only shards whose contents changed
  against its `base`, so a peer pulls a few hundred KB per day instead of
  600 MB. A full snapshot every 20 versions.
- **Retention**: the three most recent versions, then delete. A snapshot
  older than that is reproducible from a newer one plus the delta log, but
  the log itself is pruned, so retention is 3.

**Implemented 2026-10-05** (`internal/dump/version.go`, `ForceInterval = 20`,
`RetentionVersions = 3`). The measured result, against the real 1.7 GB mirror
with `--stable-salt` and an unchanged corpus:

| | |
|---|---|
| v0 (full) | 6.7 MB, 256 shards |
| v1 (delta against v0) | **614 B, 0 shards** |

That is the "few hundred KB per day" claim, and it only holds with a stable
salt: with the default per-dump salt every pseudonym changes, so every shard
changes and the delta is the full snapshot. `dump` says so on stdout rather
than shipping 7 MB under a delta's name.

**Three things §4.3 did not say, which the implementation had to decide:**

1. **A full snapshot names `base_version: -1`, and a delta names its base
   explicitly.** `base_version` was `omitempty`, which meant a delta against
   **v0** published no `base_version` at all — v0 being both "the first
   version" and "unset". A peer could not tell those apart in the signed
   bytes. `-1` is not a valid version, so "is this a delta" is now
   answerable from the field alone.
2. **The manifest carries `snapshot_version`, which it never did.** It used
   to exist only as the directory name `v12`, which is not signed. `fetch`
   consequently reported "fetched snapshot **v1**" for every peer, and
   `Manifest.Version` — the manifest *format* version — is 1 for every
   snapshot this build has ever written.
3. **Retention refuses to delete a version that a retained delta names as its
   base.** A delta whose base is gone verifies its own signature perfectly
   and then cannot be applied by anyone who did not already hold the base, so
   nothing downstream would ever report it. Prune prints what it kept and
   why, since "retention is 3" and four directories exist should not be a
   surprise.

**Rejected: delta-on-delta chaining.** A delta whose own base is a delta is
refused and the next version is written full. §4.3 says deltas are listed
against a *base*; chaining makes the size claim depend on every link the peer
happens to hold. Measured consequence of refusing: an unchanged corpus
alternates full/delta, because a delta's successor cannot be a delta.

### 4.4 No local IP leak — threat model

| Threat | Mitigation | Test |
|---|---|---|
| Peer learns my IP | snapshot is served **only** on a Tor onion service; no clearnet listener exists for it | `TestNoClearnetRouteIsRegisteredForASnapshot` (internal/dump) asserts that any snapshot-serving declaration carries onion-only evidence **in its own body**, and `TestTheClearnetListenerWouldHaveToBeDeliberate` asserts internal/dump opens no listener at all |
| Fetch client falls back to clearnet | the fetcher accepts **only** `.onion` hosts, refuses redirects off `.onion`, has no DNS path and no proxy-less dialer | unit test points it at `127.0.0.1` and at a redirect-to-clearnet, asserts refusal |
| A bug makes an outbound request | the serving binary's request path links no HTTP client; the fetch path dials only through the Tor SOCKS proxy | **NOT IMPLEMENTED** — no `go list -deps` assertion exists. Partially covered by `TestTransportHasNoPlainDialFallback` (internal/onion), which checks the transport has no clearnet dialer but does not check the serving binary's link graph |
| Onion service is silently disabled | `KINDRED_DUMP_REQUIRE_ONION=1` (default) makes the dump task exit non-zero if no onion service is up | **NOT IMPLEMENTED** — the env var does not exist and there is no such integration test. Currently unreachable because nothing serves snapshots at all (see the note below) |
| Timing/pattern reveals scale | fixed rebuild cadence, no "new dump" notification push — peers poll | design property |
| Salt leaks and pseudonymises | salt is stored in the manifest, which is public — so a stable salt is *not* a secret, and is treated as a privacy trade-off rather than a security control | §4.2 |

That last row is the one worth pausing on: the stable salt is not
protection, it is linkage. Calling it protection would be wrong.

**Revised 2026-10-04, after auditing this table against the tree.** Three of
the six rows named a test that did not exist, and the two marked
NOT IMPLEMENTED above were the ones found. Row 1 is now implemented, and the
way it is implemented is worth stating because it is not what the table
originally described.

`kindred dump verify --no-clearnet` does not exist and, on inspection, could
not usefully: **this project has no snapshot-serving code at all.** `dump`
writes files to a directory and returns; `internal/onion` only *fetches*.
There is no listener anywhere that a peer could reach, so "no clearnet
listener exists for a snapshot" is a property of the codebase rather than of
a running process — which is why the gate is a source-level assertion over the
AST rather than a flag on a subcommand.

The gate is deliberately a POSITIVE obligation: if a serving symbol
(`ServeSnapshot`, `ServeDump`, `ServeOnion`, …) ever appears, the gate demands
onion-only evidence inside that same declaration. A test that merely greps for
the absence of a listener would pass forever on a codebase that has no
listener for unrelated reasons, and would keep passing right up to the moment
someone added one.

The evidence check is scoped to the declaration, not the file. The first
version scanned the whole file, and `internal/dump/dump.go` already declares
`IsOnionHost` for the fetcher — so a bare `http.ListenAndServe` in a new
function passed, because the file contained the evidence string even though
the function never used it. That mutation survived one round of
mutation-checking before the scope was corrected.

### 4.5 Peers pull; nobody pushes

The producer only ever **serves** over onion. There is no inbound
connection to the host from anywhere, no push protocol, no peer
announcement. A peer wanting data asks; the operator's exposure is exactly
"an onion service exists and serves hashed snapshots", which is the
strongest anonymity property available without running a hidden-service
auth system.

---

## 5. Architecture

```
cmd/kindred/            main: subcommands (serve, ingest, index, dump, fetch, tune, verify)
internal/config/        env + flags, atomics for the kill switch
internal/store/         SQLite. Read-only on the corpus, read-write on kindred.db
internal/corpus/        entity/link model, mirror reader, source adapters
internal/graph/         CSR co-occurrence index, PMI, pruning, mmap'd load
internal/embed/         randomized SVD (Lanczos) over sparse PMI -> float32 embeddings
internal/signal/        one file per signal; each independently testable
internal/diversify/     MMR + caps, with honest shortfall reporting
internal/api/           chi router, /v1 routes, headers, rate limit
internal/dump/          anonymiser, sharder, manifest, signer, delta, retention
internal/onion/         hidden-service supervision + .onion-only fetch client
internal/budget/        RSS sampling and the memory gate
web/                    assets/ and templates/ (go:embed), served, no build step
```

**Layering rule:** handlers never compute a signal; signals never touch
SQL; the store translates. A signal is a pure function over
`Candidate, Seeds, Store` returning `(score, reason)`.

**Storage.** Two files. The corpus is an **existing read-only SQLite
mirror** attached at query time — it is never rewritten and never copied
(1.7 GB on NFS measured at 45.8 MB/s versus 2.7 GB/s on local NVMe, and a
copy is what made kindling workable). Kindred's own state — the CSR index,
embeddings, kerned.db tables, dumps — lives in `kindred.db` on **local
disk, never the pool**.

**Why SQLite, again.** Kindling's spec reached the same conclusion against
the same evidence: the corpus is 1.7 GB and read-mostly, the concurrency
that would justify Postgres is absent, and the alternative is a dialect
layer over 573 tests' worth of SQL. The seam for a later port stays
`internal/store`.

**Concurrency.** One writer (the indexer) and N readers, which is exactly
`MaxOpenConns(1)` for writes plus a read-only pool. WAL, `mmap_size=0`
(the 1.7 GB file must not be mapped into a 512 MB address space),
`cache_size=-1024`, `wal_autocheckpoint=256`, `busy_timeout=5000`.

---

## 6. Memory budget

The budget is a **test**, not an aspiration. `make budget` boots the server
against the real corpus, walks every route, samples `/proc/self/status`
`VmHWM`, and exits non-zero over cap. §PLAN M0.4.

| Mode | Steady RSS | VmHWM peak | Where |
|---|---|---|---|
| Pi lite (top-24 neighbours, no embeddings) | ≤ 60 MB | ≤ 120 MB | Pi 3/4, 512 MB free |
| Pi full | ≤ 140 MB | ≤ 260 MB | Pi 5, 1 GB free |
| thinkcentre | ≤ 220 MB | ≤ 400 MB | alongside 20 services |

### 6.1 Measured, against the real corpus

The table above is the budget. These are the numbers the build actually
produced on thinkcentre against the 1.7 GB / 112,935-work / 634,231-tag
mirror, and they differ from the estimates that produced the table in two
places worth recording.

| Path | Measured peak RSS | Cap | Verdict |
|---|---|---|---|
| `ingest` (full) | **181.4 MB** | 220 MB | pass, 1m24s |
| `embed` (full, dim=32) | **203.2 MB** | 220 MB | pass, 3m10s |
| `serve` (lite) | **26 MB** | 60 MB | pass |
| `serve` (full) | **34 MB** | 220 MB | pass |

The embed step is a **separate command**, not a flag on ingest. The index
build peaks at 181 MB — SQLite holding a 216 MB state database — and the
eigensolver adds 127 MB of its own, so in one process the peak is their
sum, 308 MB, and no amount of freeing inside the process lowers a peak
that belongs to something still open. The two phases share nothing except
files on disk. `kindred embed` reads its node count from `graph_meta`, so
the matrix is always the size of the index it will be used with.

Recommendation latency: 500 candidates scored in 0.93 s, two seeds.

Embedding quality, measured on the stored table: cosine 0.998 between
neighbouring tag ids, −0.24 between distant ones, all 634,232 rows at
dim 32 and 128 bytes each.

**Estimate 1 was wrong by 5×: the adjacency is 26.8 MB, not 124 MB.**
The 124 MB figure sized the *unpruned* adjacency — 15,500,668 directed
entries at 8 B — which is what you get at `top_n = 0`. The shipped build
caps during construction rather than pruning afterwards, so it never
materialises the unpruned arrays at all: 2,884,447 directed entries,
26.8 MB. Building then pruning held both alive at once and measured 352 MB.

**Estimate 2 was wrong in the other direction: the loader matters more
than the graph.** The first loader read the 28 MB index with `os.ReadFile`
and then copied it into the offsets/neighbours/weights arrays: 58.9 MB of
heap for a 27 MB graph, over the 60 MB lite cap. Streaming the arrays off
the file through a 256 KiB buffer brought it to 32.3 MB. The graph was
never the problem; holding the file that describes it was.

Both of these were found by measurement, not by reading the code: the
ingest spike by sampling `VmHWM` per build stage, and the loader by the
health check reporting `over_budget` and naming the number. Three theories
about the ingest spike were wrong before the right one — the Go heap, the
corpus mmap, and a `GROUP BY` sorter — so the trace hook that located it
(`Builder.Trace`) is part of the build, not scaffolding to be removed.

Basis, all measured not estimated: CSR adjacency 2,884,447 directed
entries × 8 B = 26.8 MB at `top_n = 24`; unpruned 15,500,668 × 8 B =
118.3 MB, which is what the cap avoids; SQLite page cache 1 MB; Go runtime
8–15 MB.

### 6.2 The request path is where the budget is actually spent

An idle server is 39.7 MB in lite mode. The requests take it to 58. The
`make budget` gate reports the peak after each step precisely so this is
visible; the four allocations it found were all per-request and all scaled
with work nobody asked for.

| Allocation | Before | After | Fix |
|---|---|---|---|
| candidate pool | +33 MiB | +0 | LIMIT and ORDER BY pushed into SQL; it was selecting every work sharing a seed tag and truncating in Go |
| candidate summaries | (inside the above) | small | `CandidateRowsSlim` omits `summary`; summaries load for the rows that survive ranking |
| `TagOverlap` seed sets | (inside the above) | small | one map per seed, built once per request instead of per candidate |
| `Neighbourhood` vote map | (inside the above) | small | memoised, in a bounded 8-entry LRU |

The last one is worth stating as a rule rather than a fix: memoising a
per-request invariant needs a **bounded** cache. The obvious
implementation — a package-level `map` keyed by the seed set — is the
unbounded `_neighborhood_cache` the previous deployment shipped, and it
is a slow leak rather than a fix. The test asserts the bound, not just
the hit rate.

Pool defaults differ by mode for the same reason: 200 in lite, 1000 in
full. The candidate set is the largest per-request allocation there is,
so a default chosen for quality on a 16 GB host is the wrong default on
a 512 MB Pi. `?pool=` raises it, and the full-mode gate step walks a
1000-strong pool to prove the ceiling still holds.

Where the caps come from, concretely:

- **No hash maps in the hot path.** The graph is arrays. Kindling's
  3.0 GB dict graph becomes a 124 MB array graph; that single
  representation change is most of the win and it is not an algorithm
  change.
- **float32, not float64, throughout** the embeddings and PMI values.
- **Graph stays on disk.** The CSR is mmap'd and only the seed's
  neighbourhood is faulted in per request. The 124 MB figure is address
  space, not resident; resident is what the seed actually touched.
- **No ML runtime.** `implicit` + scikit-learn + scipy + pandas + numpy
  are replaced by a randomised SVD in ~150 lines of Go over the same
  sparse matrix. `TruncatedSVD(n_components=32, random_state=42)` is
  reproduced with Lanczos to a stated tolerance, and the parity report
  (§PLAN M2.5) is a report, not a gate — the algorithm changed, so output
  differs, and pretending parity is required would be a false promise.
- **Bounded caches.** Rate limiter is an LRU capped at 4,096 keys. Tag
  neighbourhood cache is an LRU capped at 2,048 entries. kindling's
  unbounded `_neighborhood_cache` is a leak on a long-lived process.

**Explicitly rejected:** running the recommender in a subprocess that gets
OOM-killed and restarts. A service that answers with wrong results when
memory runs out is worse than one that answers slowly.

---

## 7. Scoring, kept honest

```
score(candidate, seeds) = Σ w_signal · signal(candidate, seeds)
priority                  = score · freshness_decay(candidate)
```

- Weights come from a named tune, default identical to kindling's shipped
  weights, overridable per kind.
- Every response carries `evidence[]`: one `{signal, value, reason}` per
  signal that contributed, so a ranking can be argued with. This is the
  spec's "transparent algorithms" principle (§2.4 of kindling's spec) and
  it is a requirement, not a nice-to-have.
- MMR diversity and caps applied last, with the shortfall reported in
  `meta` (§2.5).
- The failure mode is explicit: **if a signal cannot be computed it is
  omitted from `evidence` and logged as degraded, and `meta.degraded[]`
  names it.** A ranking that silently loses a dimension is the exact bug
  kindling's `9c9287b` fix was about — arena signals were inert in
  production and silent about it. The rule is that every signal's
  degradation is observable from the response.

---

## 8. Migration from kindling

1. `kindred ingest --corpus ~/kindling-data/ao3_metadata.db` reads
   `ao3_metadata.db` read-only and populates
   `kindred.db`. Idempotent, resumable, reports rows in/rows kept/rows
   dropped per table — the "silent no-op beats crashes" rule, applied to
   the migrator.
   **Corrected 2026-10-05.** This said `kindred ingest ao3`. There is no
   positional argument: `ingest` takes `--corpus`, and the mirror is passed by
   path. The wrong form does NOT error on the bad subcommand -- the CLI reads
   `ingest`, treats `ao3` as a stray argument, and fails with `--corpus is
   required`, which reads as "you forgot a flag" rather than "that command does
   not exist".

   Found by `scripts/check-doc-commands.sh`. The silent fall-through is the
   general hazard: a document naming a non-existent subcommand of a real one
   produces an error about something else entirely, so the doc looks broken
   rather than wrong.

2. The index build writes the CSR + embeddings.
3. Kindling's own eval fixtures (`tests/test_scoring_shape.py`,
   `test_taste.py`, `test_rerank_parity.py`) become Kindred's parity
   fixtures where they assert a shape rather than a machine-specific
   number.
4. Kindling keeps working until Kindred's parity report is read and
   accepted. Nothing is deleted as part of this project.

**No data is lost.** The corpus is only read. The only kindling state
worth carrying is the arena's pairwise ratings, which the new `peer_rating`
signal reads through an adapter if the operator wants it.

---

## 9. Deployment

systemd user units, `MemoryMax` set on both hosts, one binary, no runtime
deps:

- thinkcentre: `MemoryMax=512M`, port 8010, DB on local NVMe.
- Pi: `MemoryMax=256M`, port 8010, lite mode by default.
- Cross-compile is plain `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build`
  — `modernc.org/sqlite` is pure Go, so there is no C toolchain
  requirement on the Pi and no reason to ever build there.

Both units carry the same `ExecStartPre` health gate kindling lacked: the
unit is not considered started until `/v1/healthz` answers.

---

## 10. Verification

Every claim in this document is checkable, and the check is named:

| Claim | Check |
|---|---|
| Runs in the budget | `make budget` fails over cap on both modes |
| Works on a Pi | an arm64 run under `systemd-run --user -p MemoryMax=256M` on real hardware |
| Ranks sanely | fixture parity report, read by a human, output inspected not just asserted |
| SVD reproduction is right | Lanczos vs a dense reference on a 500-tag fixture, singular values within 1e-6 |
| Never touches the network on the request path | a test that fails if the handler path links an HTTP client |
| Dump leaks no IP | the four tests in §4.4's table |
| Dump leaks no reader | a test asserting zero rows from every dropped table and that `tag_affinity` has no work-id column |
| Doesn't slow AO3 down | outbound request counter is structurally zero, not merely low |

---

## 11. The parts that will bite

1. **A stale metadata row will lie to you.** `cooccurrence_graph_meta` says
   20,262 nodes; the edges reference 123,047. Measure, never read the meta.
2. **`works.bookmarks` is NULL for 112,890 of 112,935 rows.** Sorting by
   it sorts by NULL. The new schema has `stats` with an explicit
   `has_bookmarks` flag, and a test asserts NULL never wins a sort.
3. **MMR is O(k×n).** Bounding k to 500 and saying so in the log; passing
   `k=len(records)` squares it. This was measured in kindling.
4. **A cold CSR load is a latency cliff.** The index is mmap'd, so the
   cliff is paid per page fault, not at startup. `/v1/healthz` reports
   index residency so a load balancer can tell a warming node from a cold
   one.
5. **Onion does not exist yet on either host.** `tor` is not installed on
   cachyos and I did not check thinkcentre. §PLAN M5.0 installs and
   configures it, and that is a prerequisite, not an assumption.
6. **The dump's salt is public.** A stable salt is linkage, not secrecy
   (§4.4, last row).

---

## 12. Owner decisions, deliberately not made here

1. **AO3's terms and an unofficial API.** Whether to expose the public
   surface at all is the owner's call, informed by AO3's ToS. The kill
   switch (§3.2.4) exists so that call is one config change, reversible in
   seconds.
2. **Whether to keep kindling.** Kindred replaces it functionally; whether
   the old tree is deleted, archived, or left stopped is the owner's.
3. **Stable salt for cross-dump linkage.** Default is off (§4.2).
4. **Where the project lives** — I propose
   `/mnt/disk-important/personal/documents/code/projects/kindred` on the
   thinkcentre pool, with `KINDRED_DB` and all dumps on local disk, built
   on thinkcentre and cross-compiled for arm64.

---

## Connections

- [[concord-collaboration-ideas]] — the same "computed priority, never
  hand-assigned, and show the evidence" principle applied to governance
- [[ao3-recommender]] — the engine Kindling sat on, 573 tests, unmodified
- [[lorehaven]] — the other Rust service on thinkcentre, 13 MB resident:
  the counter-example that a self-hosted service does not have to be
  expensive


---

## STATE CORRECTION (2026-10-04) — this line said "Nothing built" and was wrong

Written 2026-09-29 and never revisited. As of today the repo is **20,744 lines of Go
across 63 files**, last committed 5 hours before I looked, and `docs/goal-check.py`
reports **COMPLETE on all 5 clauses**. The spec's own framing ("drafted, nothing built")
is six days stale.

What exists now, by commit:

    32c1f66  the /block page
    263174a  arena: peer_rating reaches the recommender, batch on a timer
    f56473e  the recommendation query, and a cursor bug that duplicated posts
    e367586  splice the feed's own safety terms, with two mutants
    ffd5743  GET /api/recommended, four dead mutants
    dcfd414  the mutation harness, after fixing two bugs in the harness
    fb681c2  docs/goal-check.py — an executable definition of done
    206d3ff  CI runs the goal-check predicate on every push

Also built and not in this spec at all: the **arena** (peer rating, Glicko,
pairing), **onion** routing, **block**, **leaderboard**, **my-ranking**, and
**embed**. The spec's §0 reasoning about replacing Kindling still holds as the
rationale; only the "nothing built" status is wrong.

### What I did about it (commit `35363b5`, tag `v0.1-web-tested`)

`goal-check.py` was red on one clause: `internal/web` had no test file — 2,029 lines,
the largest untested surface. Now 47 cases, and the first two pinned real defects that
had shipped:

- **`shorten()` counted every `.` in a summary.** Anything opening with an abbreviation,
  a decimal or a URL rendered as the abbreviation: `"Dr. Smith has 3.5k words. Then
  more."` → `"Dr."`, on four pages. Also indexed by byte, so it could cut inside a
  multibyte rune — a hazard `trim()` sixty lines away explicitly guards against.
- **`trim()` cut to `max` bytes then appended `"..."`**, overrunning its own budget by
  three whenever the cut landed just under.

Both mutations verified. The lesson is the same one this file's §0.1 argues about
Python dicts: **a suite proves code works and is blind to absence.** 2,029 lines of
display logic had been correct-by-inspection for its whole life.

### Why the status line went stale, and the fix

Nothing here is anyone's fault — the spec was accurate when written and the build simply
outran the document. But this is now the **fourth** stale "remaining work" claim this
session, and the fifth time the error was in the same direction: **underestimating what
exists**. The pattern across all of them:

| claim | reality |
|---|---|
| `EntityRecommender` "unwritten, 40-60h" | 5 entity kinds built, registered, tested |
| estate survey: whitebois "15 rows in scope" | a live session had just finished item 75 |
| ficnexus `CURATOR_MIN_TRUST` | 4 more gates disagreed with it, unnoticed for months |
| this file: "Nothing built" | 20,744 lines, goal-check COMPLETE |

**So: a status line in a document is a claim with an expiry date, and the only way to
know which are live is to run the repo.** Every `docs/` status I read this session was
wrong in the direction of under-reporting, and the cost of checking was one `ls`.
