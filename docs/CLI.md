# kindred — CLI reference

**This document exists because `scripts/check-cli-coverage.py` measured a gap.**
Before it, the project had one docs gate and it asked one direction:

> every `kindred …` command quoted in `docs/` — does the binary know it?

That direction was made green, and it catches a real failure: a document naming a
command that does not exist produces an error message indistinguishable from a
bug in the feature it documents, so the cost lands on whoever debugs it.

The other direction had no gate:

> every flag the binary defines — is it named anywhere in the docs?

The asymmetry is not neutral. The first direction has a *symptom*. The second is
pure absence, so nothing else in this project would ever report it. Measured
before this document existed:

    22 command-specific flags defined by the binary, named in no document
    2 global flags accepted everywhere, named nowhere
    7 of 12 commands had no example invocation with any flag at all

Those seven — `recommend`, `crawl`, `profile`, `rate`, `corpus-query`, `stats`,
`fetch` — were all *named* in prose somewhere. None was shown being *used*. A
reader who knows `kindred crawl` exists still could not learn that `-workers` is
what makes it polite.

**Read this if you want to know what a flag does.** It is complete by
construction: every entry below is a flag the binary lists in `-h`, minus seven
that `-h` already documents adequately (`--json` and six global config flags
named in [Global flags](#global-flags)).

The gate runs in `make verify` and **fails the build when a new flag is added
undocumented**, so this document cannot silently fall behind the binary.

---

## Global flags

Bound for every command by `internal/config.Config.Bind`, so they are accepted
everywhere and documented once. Each also reads an environment variable, which is
what a systemd unit or container should set instead of passing flags.

| flag | env | default | meaning |
|---|---|---|---|
| `--corpus` | `KINDRED_CORPUS` | — | read-only corpus SQLite path |
| `--db` | `KINDRED_DB` | `~/.local/share/kindred/kindred.db` | kindred's own state; local disk, never the pool |
| `--listen` | `KINDRED_LISTEN` | `127.0.0.1:8010` | listen address |
| `--mode` | `KINDRED_MODE` | `full` | `lite` drops the embedder and keeps only top-N neighbours |
| `--top-n` | `KINDRED_TOP_N` | `24` | neighbours retained per tag. Costs memory: this is the flag `lite` mode trades down |
| `--embed-dim` | `KINDRED_EMBED_DIM` | `32` | SVD dimensions for the tag embedding |
| `--k-anon` | `KINDRED_K_ANON` | `20` | min bookmarks before a `tag_affinity` row is emitted |

`--top-n` and `--embed-dim` are accepted by commands that do not use them —
`kindred stats --top-n 99` parses and exits without complaint. They are config,
not per-command options; that is deliberate so a systemd unit can pass one uniform
flag set, and it means an unused one is silently ignored rather than rejected.

## `serve` — the HTTP API

```bash
kindred serve --corpus ~/kindling-data/ao3_metadata.db \
              --db ~/.local/share/kindred/kindred.db \
              --listen 127.0.0.1:8010 --mode full
```

The only long-running command. `full` maps the co-occurrence index and peaks near
**168 MiB**; `lite` peaks near **26 MiB**. Both are measured by
`make budget`, not asserted here.

## `ingest` — build the index

```bash
kindred ingest --corpus ~/kindling-data/ao3_metadata.db --embed
```

`-embed` does **not** build embeddings. It prints the `kindred embed` command
that does, after the index is built. This is deliberate: embedding is a
separate multi-minute step with its own memory profile, and folding it in would
make `ingest` a two-phase command whose peak nobody measured.

## `embed` — build the tag embedding

Not yet documented above because every one of its flags is either global or
listed by `-h`. It is the command `--top-n` and `--embed-dim` exist for.

## `recommend` — recommend entities

```bash
kindred recommend --corpus ~/kindling-data/ao3_metadata.db \
                  --db ~/.local/share/kindred/kindred.db \
                  --seed ao3_work:12345 --pool 400 --n 20
```

| flag | default | meaning |
|---|---|---|
| `--pool` | the mode's default | candidate pool size before ranking. `0` means "let the mode choose". Larger is more accurate and slower; this is the flag the service's own `--top-n` equivalent on the read path |

Seeds are `kind:id` pairs, repeatable by comma: `--seed ao3_work:1,ao3_work:2`.

## `crawl` — fetch AO3 work pages into the mirror

The only networked subcommand, and the only place this project talks to AO3.

```bash
kindred crawl --corpus ~/kindling-data/ao3_metadata.db \
              --seeds ao3_work:12345 \
              --workers 4 \
              --state ~/crawl-state.json \
              --crawl-delay -1
```

| flag | default | meaning |
|---|---|---|
| `--seeds` | — | comma-separated `kind:id` or AO3 work URLs to seed from |
| `--urls` | — | file of work URLs, one per line; `-` reads stdin |
| `--workers` | `1` | concurrent fetchers. **The crawl delay is the binding constraint, not this** — the default is 1 because AO3's `robots.txt` asks for one request per N seconds, and raising `-workers` without raising `-crawl-delay` is how you get your IP blocked |
| `--crawl-delay` | `-1` | seconds between requests. `-1` means "read `robots.txt`", which is the right answer. `0` means no wait and is only for a fixture server |
| `--state` | — | resume file. A crawl interrupted mid-run continues from it |
| `--checkpoint-urls` | `25` | persist resume state every N fetches |
| `--max-retries` | `3` | retries for a transient failure |
| `--retry-backoff` | `2s` | first backoff; doubles per attempt |
| `--base-url` | AO3 | override the base URL, for a fixture server |
| `--parse-only` | off | fetch and parse, print a summary, write nothing |
| `--offline` | off | make no network request at all; report every URL as unfetched |

`--offline` exists so the parser can be tested against real pages without making
requests. It is the flag that makes this command safe to exercise in CI.

## `profile` — taste profiles

```bash
kindred profile list
kindred profile build dark --works 12345,23456
kindred profile show dark
kindred profile rate dark --work 12345 --like
```

**Positional, not flags**: the profile NAME is the argument after the subcommand,
not `--name`. The example above is right and `--name dark` is not.

Two things that are *not* invocations, because they do not work:

- `-h` is not a thing here. The subcommand dispatch runs before any flag parsing,
  so `-h` is reported as an unknown **subcommand**.
- `--name` does not exist on this command. The name is positional.

The `rate` subcommand here is the same operation as the top-level `rate`
command, which is the short form of it.

## `rate` — fold one rating in

```bash
kindred rate --corpus ~/kindling-data/ao3_metadata.db \
             --name dark --work 12345 --like --save
```

| flag | meaning |
|---|---|
| `--work` | work id being rated (**required**) |
| `--like` | this work is a like |
| `--dislike` | this work is a dismissal |
| `--save` | persist the updated profile |

`--save` is separate from the rating on purpose: rating without saving is a dry
run, which is how you check what a rating would do to the weights before doing
it.

## `corpus-query` — ask a question about the corpus

```bash
kindred corpus-query --corpus ~/kindling-data/ao3_metadata.db \
                     --query fandom-ranking --profile dark --limit 25
kindred corpus-query --corpus ~/kindling-data/ao3_metadata.db \
                     --query tag-neighbours --tag "Slow Burn"
```

| flag | default | meaning |
|---|---|---|
| `--query` | `fandom-ranking` | which question. One of `fandom-ranking`, `tag-neighbours`, `underrated` |
| `--limit` | `100` | maximum rows |
| `--min-co-works` | `20` | evidence gate for `fandom-ranking`. A fandom on fewer than this many works is not ranked, because one enthusiastic reader makes a fandom look large. Negative disables the gate |
| `--profile` | — | taste profile to weight by, for `fandom-ranking` |
| `--tag` | — | tag name, for `tag-neighbours` |

`--min-co-works` is the flag that keeps the leaderboard honest, and it is the
reason the web page shows a count beside every fandom rather than a ranking.

## `stats` — corpus and index statistics

```bash
kindred stats --corpus ~/kindling-data/ao3_metadata.db
```

Prints works, tags, edges and embedding counts. Takes no command-specific flags.

## `dump` — write a signed snapshot

```bash
kindred dump --corpus ~/kindling-data/ao3_metadata.db \
             --db ~/.local/share/kindred/kindred.db \
             --version 0 --full --keep 3 --stable-salt
```

Documented in [PROVENANCE.md](PROVENANCE.md), which is the authority on what the
signature covers. `--stable-salt`, `--full`, `--keep` and `--version` are all
explained there.

## `verify` — check a snapshot

```bash
kindred verify --dir ~/dumps/v1
```

## `fetch` — pull a snapshot from a peer

```bash
kindred fetch --from http://<56-characters>.onion \
              --into ~/dumps --socks 127.0.0.1:9050
```

| flag | default | meaning |
|---|---|---|
| `--from` | — | peer's onion base URL (**required**) |
| `--into` | — | local directory to write into |
| `--socks` | `127.0.0.1:9050` | Tor SOCKS address, **loopback only**. Refuses a non-loopback address, because a SOCKS proxy reachable from the network is an open relay |
| `--timeout` | `1m` | overall timeout |

## `tune` — signal weights

```bash
kindred tune --name default --list
kindred tune --name default --set "tag_overlap=0.3,co_occurrence=0.5"
```

`--name` defaults to `default`. `--json` is available and documented by `-h`.