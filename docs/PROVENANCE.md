# kindred — provenance

How the numbers in SPEC §6 and the README were produced, so they can be
re-derived rather than believed.

## Environment

- thinkcentre, Linux ARM64, the 1.7 GB mirror at
  `~/kindling-data/ao3_metadata.db`.
- Go 1.27.1, `CGO_ENABLED=0`, `-trimpath -ldflags '-s -w'`.
- The measurement host is the same machine the service would run on. A
  memory figure taken elsewhere is a different fact.

## The corpus

| Table | Rows | Note |
|---|---|---|
| `works` | 112,935 | 42 have `word_count = 0` |
| `tags` | 634,231 | max id 634,231 |
| `work_tags` | 3,891,300 | 3,890,504 are `freeforms` |
| `cooccurrence_edges` | 7,750,334 | max tag id 634,231 |
| `users` | 6,261 | dropped from snapshots |
| `user_work_interactions` | 183,750 | 173,768 `bookmarked`, 9,982 `bookmarker` |

Facts that are not derivable from the schema and had to be measured:

- `works.bookmarks` is NULL for 112,890 of 112,935 rows.
- `user_work_interactions` holds the same work more than once per user:
  user 1 has 7 rows for 4 distinct works.
- Distinct bookmarked works per user: 1 → 109 users, 2-3 → 220, 4-9 → 513,
  10-19 → 1,272, 20-49 → 3,165, 50-99 → 683, 100+ → 26. This is why
  k=20 publishes 3,874 readers rather than none — an earlier query that
  counted rows and filtered on the wrong `interaction_type` returned zero
  and looked like a privacy win.
- `cooccurrence_graph_meta` claims 20,262 nodes while the edges reference
  123,047 distinct tags. The metadata row is stale.

## Re-running the budget gate

```sh
make build
scp bin/kindred thinkcentre:/tmp/kindred
ssh thinkcentre 'bash scripts/budget.sh /tmp/kindred   ~/kindling-data/ao3_metadata.db /tmp/kr-final/k.db 8488 lite'
```

The gate boots the server, walks every route, and reads `VmHWM` from
`/proc/<pid>/status` after each step. It exits non-zero over the cap, and
prints the delta per step so a failure names the request.

`/healthz` returns 503 when the process is over its own cap, so the same
figure is observable in production without the gate.

## What "measured" means here

Every figure in the README and SPEC §6 came from a run of the gate or the
ingest against the real corpus, on the target host. Where a figure is
computed rather than observed — the 26.8 MB adjacency, the 118.3 MB
unpruned form — it is arithmetic over the measured node and edge counts,
and both inputs are printed by the ingest.

Estimates that turned out wrong, kept because the SPEC's table is derived
from them:

- **Adjacency 124 MB → 26.8 MB.** The estimate sized the unpruned
  adjacency. The shipped build caps during construction, so the unpruned
  form is never allocated.
- **Loader memory not considered at all.** The first loader held the
  28 MB index and the arrays it described, which was more memory than the
  graph. This is the estimate the spec was missing entirely.

## Data-fidelity checks

- Embedding accuracy: `TestEmbedConvergesToFloat32Precision` asserts the
  settled error is at float32 epsilon (1.19e-7), against a dense
  reference. A looser bound would let a numerical bug pass.
- Index format: `internal/graph/persist_test.go` pins the file size and
  the bytes at each array boundary against the documented layout, with
  expected bytes built by hand. A round trip could not catch the padding
  bug because the writer and reader agreed with each other.
- Snapshot privacy: `kindred dump` then a scan of all 6,261 real
  usernames and URLs across every byte of the output. Zero appear as a
  complete published value; zero work ids; row keys are `p` and `w` only.
