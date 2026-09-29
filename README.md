# kindred

A general entity recommender over a read-only local mirror, sized to run on
a Raspberry Pi with 512 MB free and on a workstation with twenty other
services resident.

Static Go binary, no CGO, no Python, no ML runtime. 12 MB.

This is a lightweight replacement for [Concord](https://concord.polarisocial.xyz),
which was taken off the host it ran on for using 800 MB–1.2 GB. kindred
does the same job — a general entity recommender plus an anonymised,
signed peer-snapshot exchange — inside 220 MB, or 60 MB in lite mode.

```
kindred serve      run the HTTP API (recommend + the unofficial AO3 read surface)
kindred ingest     measure the corpus and build the index
kindred embed      compute the tag embeddings from an ingested corpus
kindred recommend  recommend from seeds, from the shell
kindred dump       write an anonymised, signed snapshot for peers
kindred verify     verify a snapshot's signature and shard hashes
kindred fetch      pull a snapshot from a peer's onion service
kindred stats      print corpus and index statistics
kindred tune       inspect or set signal weights
```

`ingest` and `embed` are separate commands on purpose. In one process the
peak is the sum of the two phases, not the max, and the sum does not fit
the budget. See [Memory](#memory).

## Licence

AGPL-3.0. See [LICENSE](LICENSE).

## Measured

Every figure below was produced by running the real binary against a real
1.7 GB / 112,935-work / 634,231-tag / 7,750,334-edge AO3 mirror on the
target hardware, not estimated. `make budget` is the gate that checks
them and exits non-zero if a change breaks one.

| Path | Peak RSS | Cap | Time |
|---|---|---|---|
| `ingest` (full) | 181.6 MB | 220 MB | 1 m 31 s |
| `embed` (full, dim=32) | 203.2 MB | 220 MB | 3 m 10 s |
| `serve` (full) | 35 MB | 220 MB | — |
| `serve` (lite, Pi) | 25 MB | 60 MB | — |

500 candidates ranked in 0.93 s. The embedding is 634,232 rows at dim 32;
cosine similarity is 0.998 between neighbouring tag ids and −0.24 between
distant ones, so the signal is real and not a plausible-looking matrix.

### Why the numbers came down this far

Three allocations accounted for most of it, and all three are the same
mistake at different scales — holding something twice:

- The adjacency is **capped during construction**, not pruned afterwards.
  Build-then-prune holds both forms at once and measured 352 MB.
- The embedding block is **float32** (81 MB) rather than float64 (162 MB),
  with float64 accumulation kept for the parts that need it.
- `debug.SetMemoryLimit` is set from the memory budget, so the Go heap is
  a function of the budget rather than of how long a loop is. Memory was
  growing by exactly the size of each item being processed — 2.4 MB per
  component, 116 MB across a bulk insert — which is allocation without
  collection, not a cache.

A run that is over budget is reported as a **failure**, not a warning,
even when the output is correct.

## Quick start

```sh
CORPUS=/path/to/ao3_metadata.db
DB=~/.local/share/kindred/kindred.db

# build the index, then the embeddings. Two commands, two processes: in
# one process the peak is the sum of both phases and does not fit.
kindred ingest --corpus $CORPUS --db $DB --mode full
kindred embed  --corpus $CORPUS --db $DB --dim 32

# serve it
kindred serve --corpus $CORPUS --db $DB --listen 127.0.0.1:8010 --mode full

# ask it something
curl 'http://127.0.0.1:8010/api/v1/recommend?seed=ao3_work:17249318&n=5'
```

`embed` refuses in lite mode — lite does not read the embedding signal,
and the table is 216 MB of something nothing queries. It also refuses a
second build without `--force`, and checks the memory budget between
components so a host that is too small is told which flag to lower rather
than being OOM-killed three minutes in.

On a Pi, use `--mode lite` throughout and skip `embed` entirely: no
embeddings, top-24 neighbours, and the weights are renormalised so the
ranking keeps its balance without the dimension that was dropped.

To run it as a service, `deploy/kindred.service` is a hardened unit that
binds to loopback only. Adjust the three `Environment=` lines at the top
and install it.

## The API

```
POST|GET /api/v1/recommend     seeds -> ranked entities with evidence
GET     /api/v1/tags/{id}/similar
GET     /api/v1/ao3/works            list, ?tag= &sort=kudos|words|date|hits
GET     /api/v1/ao3/works/{id}
GET     /api/v1/ao3/works/{id}/recommend
GET     /api/v1/ao3/tags
GET     /api/v1/ao3/tags/{id}
GET     /api/v1/ao3/tags/{id}/works
GET     /healthz                 503 when over the memory budget
GET     /stats
```

Seeds are `kind:id`, repeatable or comma-separated. `n` caps at 100,
`limit` at 100, `pool` bounds the work per request.

## What a response tells you

```json
{
  "items": [{"id": 24943012, "title": "...", "score": 0.34,
             "evidence": [{"signal": "tag_overlap", "value": 0.42,
                           "weight": 0.28, "reason": "shares 12 tags with seed 1 (jaccard 0.19)"}]}],
  "meta": {"seeds": 2, "candidates": 500, "tune": "default",
           "degraded": ["recency"], "shortfall": {"requested": 100, "returned": 90,
           "reason": "per-group cap of 3 reached"}}
}
```

Three fields exist because the previous deployment's failure mode was a
plausible-looking response built from signals that were quietly doing
nothing:

- **`degraded`** names every signal that had no opinion on this input. A
  signal returns a skip, never a zero.
- **`shortfall`** says how many results you got against how many you asked
  for, and why. The list is never topped up from below a cap — doing that
  restores exactly the concentration the cap removed.
- **`evidence`** is on every item. A ranking you cannot argue with is not
  one you can tune.

## Memory

The whole project exists to keep this small. The rules that got it there,
each learned from a measurement that contradicted the obvious answer:

- **Arrays, not maps.** The graph is a CSR. The previous deployment's dict
  graph was 3.0 GB resident.
- **Cap during construction.** The adjacency is allocated at its final
  size, 26.8 MB, and never materialised unpruned at 118.3 MB first.
- **Stream the index both ways.** Writing it through a length-tracked
  buffer rather than an `Encode()` copy; reading it through a 256 KiB
  buffer rather than `os.ReadFile` + `Decode`. Holding the file that
  describes the graph cost more than the graph.
- **No ML runtime.** A block power-iteration eigensolver, ~150 lines,
  over the same sparse matrix.
- **`/healthz` returns 503 when over budget.** A health check that reports
  ok at 2× the cap is worse than none.

`Builder.Trace` reports the peak RSS at each build stage. It is not
scaffolding: the ingest's 571 MB spike survived three wrong theories
(Go heap, corpus mmap, a `GROUP BY` sorter) and was only located by
sampling per stage.

## Peer snapshots

Anonymised, content-addressed, signed, and served only over a Tor onion
service.

```sh
kindred dump --corpus ... --db ... --out /srv/snapshots --k-anon 20
kindred verify --dir /srv/snapshots/v0
kindred fetch --from http://<56-char>.onion --into ./peersnap
```

What is published is decided per table, and the decisions are the point:
`works`, `tags` and `work_tags` whole; `users` dropped and replaced by a
`tag_affinity` table carrying tag weights and no work ids; every
session and taste table dropped; `cooccurrence_edges` pruned. Rows exist
only for readers with ≥ K distinct bookmarked works, filtered **before**
pseudonymisation so the row count cannot reveal who clears the bar.

`onion/` has no clearnet path. The dialer is SOCKS-only with no fallback,
so a stopped Tor is a failure rather than a leak; `Proxy` is nil so an
`HTTP_PROXY` in the environment cannot undo it; and a redirect from an
onion to a clearnet host is refused, which is the vector a peer would
otherwise use to observe the operator's IP.

`--stable-salt` makes a reader linkable across dumps. It is a research
affordance and a re-identification risk, and the manifest says which mode
produced it. A salt in a published document is linkage, not secrecy.

## Data facts this is built around

All measured against the real mirror, not assumed:

- `works.bookmarks` is NULL for 112,890 of 112,935 rows. It is scanned into
  a `*int64` and a `has_bookmarks` stat is written, so a later sort cannot
  mistake NULL for zero and rank the emptiest works highest. Nullable text
  is emitted as `null`, not `""`.
- `user_work_interactions` stores the same work more than once per user
  (measured: 7 rows for 4 distinct works), so the k-anon threshold counts
  `DISTINCT work_id`. Counting rows lets a user who bookmarked four works
  seven times clear a threshold of seven alone.
- `interaction_type` is `'bookmarked'` for 173,768 rows and `'bookmarker'`
  for 9,982. The second is a row *about* a work carrying a user id;
  counting it attributes one reader's history from another's action.
- `cooccurrence_graph_meta` claims 20,262 nodes while the edges reference
  123,047. It is stale, and a stale metadata row is a lie you inherit if
  you read it. The builder measures the node space from the edges.

## Tests

```sh
go test ./...
```

The ones that matter are refusal tests and golden-byte tests. A
write-then-read round trip passed while the index writer and reader agreed
on a layout that was wrong — the file was 101,032 bytes longer than its
own header implied, and it was the loader's own invariant check that
caught it. So the format tests pin the file size and the bytes at each
array boundary against the documented layout, with the expected bytes
built by hand rather than by the other implementation.

## Licence

AGPL-3.0, matching the corpus tooling it sits beside.
