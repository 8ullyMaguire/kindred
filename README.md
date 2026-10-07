# kindred

A general entity recommender over a read-only local mirror, sized to run on
a Raspberry Pi with 512 MB free and on a workstation with twenty other
services resident.

Static Go binary, no CGO, no Python, no ML runtime. ~220 MB RSS (lite mode: ~60 MB).

This is a lightweight replacement for [Concord](https://concord.polarisocial.xyz),
which was taken off the host it ran on for using 800 MB–1.2 GB. kindred
does the same job — a general entity recommender plus an anonymised,
signed peer-snapshot exchange — inside 220 MB, or 60 MB in lite mode.

## Commands

```bash
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

## Measured

Every figure below was produced by running the real binary against a real
1.7 GB / 112,935-work / 634,231-tag / 7,750,334-edge AO3 mirror on the
target hardware, not estimated. `make budget` is the gate that checks
them and exits non-zero if a change breaks one.

|| Path | Peak RSS | Cap | Time |
||---|---|---|---|
|| `ingest` (full) | 181.6 MB | 220 MB | 1 m 31 s |
|| `embed` (full, dim=32) | 203.2 MB | 220 MB | 3 m 10 s |
|| `serve` (full) | 35 MB | 220 MB | — |
|| `serve` (lite) | 23 MB | 220 MB | — |
|| `recommend` (n=5) | 15 MB | 220 MB | 125 ms avg |

## Licence

AGPL-3.0. See [LICENSE](LICENSE).

## Verification

Run `./scripts/check-doc-commands.sh` to confirm every command the
documents quote exists in this build.

Run `python3 docs/goal-check.py` to validate the project against its
completion predicate.

Run `make budget` to enforce the memory/speed guarantees above.