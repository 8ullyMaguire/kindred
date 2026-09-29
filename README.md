# kindred

A general entity recommender over a local mirror. One static Go binary, no
runtime services. Recommends entities of any kind from a pluggable source,
serves a read-only API over the index, and publishes anonymised signed
snapshots for peers over Tor.

Replaces **kindling**, an AO3 fanfiction recommender that needed 6.4 GB
across two gunicorn workers and was removed from the 16 GB host on
2026-09-29 for memory. Same corpus, same shape of answer, a fraction of
the footprint.

## Status

Milestones M0–M2 are implemented and tested. M3 (API), M4 (dumps) and M5
(deploy, parity) are not yet written — see `docs/PLAN.md` for the
ordering and the reasoning.

## Why the memory is 24x smaller

The previous deployment held the tag co-occurrence graph as a Python
dict-of-dicts: **3.0 GB resident for 7,750,334 edges over 123,047 tags**.
A dict entry costs roughly 100 bytes of overhead against 8 bytes of
payload. The same graph as three arrays — `offsets`, `neighbors`,
`weights` — is **124 MB**. No algorithm changed; only the representation.

| | kindling | kindred |
|---|---|---|
| runtime | Python 3.12, 2 gunicorn workers | one Go binary |
| graph | dict-of-dicts, 3.0 GB | CSR arrays, 124 MB |
| ML stack | numpy, scipy, scikit-learn, pandas, `implicit` | Go, no ML runtime |
| binary | — | 10.5 MB, `CGO_ENABLED=0` |

## Build and run

```bash
make verify            # gofmt, vet, test, build — must be green
make build
./bin/kindred ingest --corpus /path/to/mirror.db --db ~/.local/share/kindred/kindred.db
./bin/kindred stats  --corpus /path/to/mirror.db --db ~/.local/share/kindred/kindred.db
make budget            # boots the server, walks every route, fails over the RSS cap
```

Cross-compile for a Pi — no C toolchain needed on the Pi at all, because
`modernc.org/sqlite` is pure Go:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '-s -w' -o bin/kindred-arm64 ./cmd/kindred
```

## The corpus is read-only

kindred never writes to the mirror. It is attached with `mode=ro`, and
`store.Open` fails fast with an actionable message if the file is not
shaped like a corpus. All of kindred's own state goes to `kindred.db` on
local disk — never the NFS pool, which is an 8-way mergerfs mount.

## Offline by construction

There is no code path from a request handler to an outbound HTTP client.
Every request served is a request the upstream project never sees. This is
the point, not a limitation: "take load off" is best served by not making
the call.

## Layout

```
cmd/kindred/        subcommands: serve, ingest, recommend, stats, dump, verify, fetch, tune
internal/config/    env + flags, atomic kill switch
internal/store/     two SQLite handles: read-only corpus, read-write kindred.db
internal/corpus/    the kind-agnostic Entity and the AO3 source adapter
internal/graph/     the CSR co-occurrence index — the memory story
internal/embed/     block power iteration with Rayleigh-Ritz projection
internal/budget/    RSS sampling; the cap the build fails over
docs/PLAN.md        ordering, milestones, and what this plan got wrong
```

## Documentation

`docs/SPEC.md` (product and architecture) and `docs/PLAN.md`
(implementation order and verification) are the design documents. The
README describes the code.
