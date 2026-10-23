# Handoff — 2026-10-23 (third session): the default moved, and the deploy gate was lying

Follows `docs/HANDOFF-2026-10-23b-embedding-enumeration.md`, whose first item
was "measure on the host, then decide the default". Done, with two detours that
mattered more than the measurement.

## State

| | |
|---|---|
| repo | `~/code-local/go/kindred`, branch `main` |
| code | `9dec664` (engine + api + web + gates) |
| tests | `go test ./... -count=1` — 26 packages green |
| deployed | `163078c`, both gates green (provenance IN SYNC, 48/48 behaviour) |
| default pool mode | **`tags+collab+embed`** (was `tags` via the zero value) |

## The measurement

Same host, same corpus, same 38 readers, same 495 held-out bookmarks, one flag
apart:

    pool_mode                 pool recall@1000   recall@10   nDCG@10   coverage
    tags+collab                       0.1895        0.1031    0.1947     0.6579
    tags+collab+embed                 0.2113        0.1050    0.1959     0.6579

Pool recall +11.5% relative, recall@10 up 1.8%, coverage identical. Request cost
did not move (3.3-3.8 s for every mode; the embedding source adds ~97 candidates
per 1000-item pool). The rule was written down before the numbers existed, so
the default moved.

Re-run after the top-up, and again on the final deployed binary (runs #12 and
#13, the new default with no flag at all) is **identical to the row above** — recall@10 0.1050, pool recall@1000 0.2113,
coverage 0.6579. That is worth stating plainly: the top-up fills the pool from
848 to 1000 candidates and does NOT move recall, because the 152 tag candidates
it adds did not contain a held-out work for these readers. Its value is a pool
that is as full as its sources can make it, not a better number.

Note the baseline is much higher than the 0.0284 / 0.0056 in the 2026-10-23
handoff. That difference is the embedding-signal repair from 2026-10-23b (the
signal had been weighted at zero), not a different measurement.

## Detour 1: the service was being throttled by its own cgroup

The first deploy shipped, provenance passed, and then every page took 19 s while
`/healthz` answered 200 and the behaviour gate hung on its first request.

The cause was not the corpus and not the query:

    systemctl show kindred -p MemoryHigh   ->  400M
    /sys/fs/cgroup/.../memory.events       ->  high 594411
    /sys/fs/cgroup/.../memory.stat         ->  anon 429233152

The 81 MiB tag index (2026-10-23b) took anonymous memory to 409 MB, permanently
above `MemoryHigh=400M`, which was sized for the 220 MB self-cap deleted on
2026-10-22. Above `memory.high` the kernel does not kill the process — it stalls
its allocations while reclaiming, so the service stayed "healthy" and unusable.
`scripts/deploy.sh` now sets `MemoryHigh=768M MemoryMax=1G` as part of the
deploy, so a fresh host does not inherit the mismatch.

**Rule this establishes:** a deploy that ships a binary the cgroup cannot hold
must raise the cgroup, or it ships a service that is up and unusable. Nothing in
`go test` can see this, and `/healthz` says `ok` throughout.

## Detour 2: the behaviour gate could not finish, and did not fail

`scripts/check-deploy.sh` discovered its test tag by walking `/search` results.
On the live mirror `/search?q=general` returns 100 DISTINCT tag links, nearly
all freeform sentence tags ("a lot of coercion in general really") carrying one
work each. The walk therefore made ~400 requests at 3.4 s apiece and then
asserted the filters against a 1-work tag — the exact fixture the script's own
comment warns about ("a tag with one work cannot prove a filter works").

It now asks the CORPUS one SQL query for a tag with ≥50 works, ≥2 ratings and
both completion states (tag 29: 55,810 works, 25,293 complete / 30,517 in
progress). The HTTP walk stays as a capped fallback for fixture instances. 48
checks pass.

Both detours are the same shape: **a gate that cannot run is indistinguishable
from a gate that passed**, which is the failure `deploy.sh` was written to end.

## What shipped beyond the default

- **The pool is topped up, from ONE query.** The reserves worked and left the
  pool short: measured live, `pool_mode=all` gave 848 candidates against 1000 for
  tags-only (tag half capped at 750, collab found 1, embedding 97, 152 slots
  unused). The first fix ran a second query for the tail with the admitted ids
  excluded; measured, each query costs ~2.2 s on a 55k-work seed, so the pool
  answered in 4.6 s against 2.45 s for tags-only — for a byte-identical pool.
  The tag half is now fetched once for the whole budget and admitted in two
  parts: the reserved part before the extra sources, the tail after them.
  That tail then overran the pool — measured live, a 1000-item request returned
  1061 candidates, because the outer `len(out) < limit` test was evaluated
  before the extras and the tail was admitted in full after them. The cap now
  lives in `admit()`, where every source passes through it.
- **Seeds are excluded in SQL too.** Exclusive pools dropped the seed after the
  LIMIT, so a 20-item pool came back as 19 whenever a seed ranked in its own tag
  results.
- **`GET /api/v1/tags/{id}/similar?space=embed`** — #11's surface. Names
  resolved in one query, similarity on every row, `min_sim` validated as 0..1,
  503 (not an empty 200) on a mirror with no embeddings, 400 that names the
  parameter for an unknown space. Default space stays `cooccur`.
- **`center_sim`, and a note when the answer is uninformative.** Curling the
  deployment found two defects no test here could: the response named the
  neighbours and left `"tag"` empty, and tag 29 (55,810 works) answered with
  eight character tags from four unrelated fandoms at cosine 0.60 — the shape
  you get when a vector sits near the middle of the space. Measured over all
  634,232 tags, the cosine to the corpus centroid has median 0.027, p90 0.22,
  max 0.61, and tag 29 sits at 0.38; so the reporting floor is 0.30 (above p90)
  rather than a guess. A character tag answers sharply — jasmine -> aurora,
  aladdin, phillip, belle at 0.88-0.94 — which is why the fix is to qualify the
  answer, not to distrust the space.
- **The tag page grows "Tags that sit near it"** — rendered only when the index
  answers, with the similarity beside each link.
- **`ParsePoolMode("")` is the default**, and the API now parses it even when
  the parameter is absent. Before this, "no opinion" and "tags only" were the
  same request at both layers, so a measured default could never take effect.

## Verification

    go test ./... -count=1        26 packages green
    go vet ./...                  clean
    bash scripts/deploy.sh        build, install, restart, both gates

Mutations, each restored afterwards:

| mutation | caught by |
|---|---|
| `DefaultPoolMode` back to `PoolTags` | `TestRecommendDefaultsToTheMeasuredPoolMode` |
| tail not admitted after the extras | `TestTheReserveDoesNotShortenThePool` (pool 6 of 10) |
| no cap in `admit` | `TestThePoolNeverExceedsItsLimit` (11 candidates for a limit of 10) |
| `embedCandidates` returns nothing | `TestEmbedPoolReachesAWorkTheTagPoolCannot` |
| `filterSQL` dropped from the embed query | `TestEmbedPoolHonoursFiltersInTheQuery` |
| old renormalise branch | `TestEmbeddingSignalContributesOnEveryItem` |

One honest gap: the SQL-level seed exclusion has no mutation that reddens a
test, because no fixture in the tree gives the seed the top position in its own
tag ordering (the exclusion is observable only as "the pool is one short"). The
code carries the comment; a fixture that ranks the seed first would close it.

## Live checks after the final deploy

    GET /api/v1/recommend?seed=ao3_work:17249318&n=20
      pool_sources {tags: 902, collab: 1, embedding: 97, overlap: 180}
      candidates 1000                              (the cap holds)
      latency 2.8 s                                (tags-only: 2.5 s)
    GET /api/v1/tags/29/similar?space=embed          tag "f/m", center_sim 0.38, note
    GET /api/v1/tags/61461/similar?space=embed       tag "jasmine (disney)", 0.005, no note
      -> aurora 0.938, aladdin 0.928, phillip 0.926
    GET /tag/29?sort=kudos                           0.36 s, section + central note
    GET /tag/61461?sort=kudos                        section, no note
    kindred eval (deployed binary)                   recall@10 0.1050, pool@1000 0.2113

The `/tag/{id}` page's 3.7 s in the earlier session was the cgroup throttle, not
the page: it answers in 0.36 s now.

## Next

1. `docs/PLAN-recsurfaces-31-100.md` rows 59 (2-D projection of the tag space),
   76 (drift) and 72 (width search) all read the same resident index and are now
   cheap. #11 is BUILT and walked.
2. The embed pool source's reserve is an eighth of the pool; with the tail
   filling the rest, the reserve is now only about ORDER, not capacity. Worth
   re-measuring whether a larger reserve (more embed-only works ranked) helps —
   the current +11.5% came from 97 candidates in a 1000-item pool.
3. `idf_overlap` and `seed_agreement` still name themselves degraded on a fresh
   mirror until `kindred ingest` builds the doc-freq table; that is by design and
   is reported, not silent.
