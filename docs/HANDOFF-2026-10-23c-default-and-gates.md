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
| deployed | see the deploy log; both gates run by `scripts/deploy.sh` |
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

- **The pool is topped up.** The reserves worked and left the pool short:
  measured live, `pool_mode=all` gave 848 candidates against 1000 for tags-only
  (tag half capped at 750, collab found 1, embedding 97, 152 slots unused). The
  tag half is now a closure called twice — reserved budget, then whatever the
  extras left unused — with admitted ids excluded in SQL, not after the LIMIT.
- **Seeds are excluded in SQL too.** Exclusive pools dropped the seed after the
  LIMIT, so a 20-item pool came back as 19 whenever a seed ranked in its own tag
  results.
- **`GET /api/v1/tags/{id}/similar?space=embed`** — #11's surface. Names
  resolved in one query, similarity on every row, `min_sim` validated as 0..1,
  503 (not an empty 200) on a mirror with no embeddings, 400 that names the
  parameter for an unknown space. Default space stays `cooccur`.
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
| top-up disabled | `TestTheReserveDoesNotShortenThePool` (pool 6 of 10) |
| `embedCandidates` returns nothing | `TestEmbedPoolReachesAWorkTheTagPoolCannot` |
| `filterSQL` dropped from the embed query | `TestEmbedPoolHonoursFiltersInTheQuery` |
| old renormalise branch | `TestEmbeddingSignalContributesOnEveryItem` |

One honest gap: the SQL-level seed exclusion has no mutation that reddens a
test, because no fixture in the tree gives the seed the top position in its own
tag ordering (the exclusion is observable only as "the pool is one short"). The
code carries the comment; a fixture that ranks the seed first would close it.

## Next

1. **`kindred eval` on the new default** (in flight at the time of writing) to
   confirm the top-up did not change the picture — the numbers above were
   measured BEFORE the top-up.
2. `docs/PLAN-recsurfaces-31-100.md` rows 59 (2-D projection of the tag space),
   76 (drift) and 72 (width search) all read the same index and are now cheap.
   #11 is BUILT, not partial, once the page section is deployed and walked.
3. The `/tag/{id}` page still renders in ~3.7 s on a 55k-work tag. Measured but
   not investigated; it is the largest single latency in the product.
