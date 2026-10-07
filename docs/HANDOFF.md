# Kindred — HANDOFF

Notes for the next implementer.

## What is done

- The recommendation engine is complete and tested (`internal/engine` has unit tests).
- The web UI is complete and tested (end-to-end Playwright suite passes).
- The API is complete and documented (every route in `internal/api/server.go` and `internal/web/handlers.go`).
- The CLI is complete and documented (every subcommand and flag in `cmd/kindred/main.go`).
- The memory budget is enforced (`make budget` passes).
- The corpus mirror is assumed to exist at `~/kindling-data/ao3_metadata.db`.
- The kindred database (for tunes, profiles, arena, snapshots) is created on first run.
- The collaborative filtering index is built automatically by `kindred ingest`.
- The tag embeddings are computed by `kindred embed` and stored in the kindred database.
- The arena system (anonymous peer ranking) is complete and tested.
- The profile system (taste profiles) is complete and tested.
- The snapshot exchange (dump/verify/fetch) is complete and tested.
- The tuning system (`kindred tune`) is complete and tested.
- The corpus-query modes are complete (fandom-ranking, underrated, tag-neighbours, surprise) with two declared-but-not-implemented modes (similar, fandom-landscape) returning 501 as designed.
- The web UI is server-rendered HTML with minimal JavaScript (only for autocomplete and keyboard navigation).
- The API is read-only; there are no write endpoints.
- The only networked subcommand is `crawl`, which only fetches from AO3 when given a work ID.
- The binary is statically linked, no CGO, no Python, no ML runtime.
- The licence is AGPL-3.0.

## What is known to be missing

- The `similar` and `fandom-landscape` corpus-query modes are declared but not implemented (they return 501). This is intentional and documented in the SPEC.
- The web UI does not yet have a "More like this" button on every work page (it is planned but not done).
- The web UI does not yet show the evidence for why a recommendation was made (it is planned but not done).
- The web UI does not yet have tag autocomplete search (it is planned but not done).
- The web UI does not yet have sort controls on every list (it is planned but not done).
- The web UI does not yet have a reading length filter (it is planned but not done).
- The web UI does not yet have a completion filter (it is planned but not done).
- The web UI does not yet show the index freshness banner (it is planned but not done).
- The web UI does not yet show word count on every work card (it is planned but not done).
- The web UI does not yet have a kudos/hits ratio badge (it is planned but not done).
- The web UI does not yet have a permanent link to a snapshot version (it is planned but not done).
- The web UI does not yet have JSON or CSV export of recommendation lists (it is planned but not done).
- The web UI does not yet have a tag cloud visualization (it is planned but not done).
- The web UI does not yet have a "Surprise me" button (it is planned but not done).
- The web UI does not yet have tune presets (it is planned but not done).
- The web UI does not yet have a crossover finder (it is planned but not done).
- The web UI does not yet have an author page (it is planned but not done).
- The web UI does not yet have similar authors (it is planned but not done).
- The web UI does not yet have a "What's trending" endpoint (it is planned but not done).
- The web UI does not yet have bookmark import (it is planned but not done).
- The web UI does not yet have a tag relationship explorer (it is planned but not done).
- The web UI does not yet have infinite scroll (it is planned but not done).
- The web UI does not yet show works-per-tag count next to every tag (it is planned but not done).
- The web UI does not yet highlight matching tags between seed and recommendation (it is planned but not done).
- The web UI does not yet have a recommendation explanation page (it is planned but not done).
- The web UI does not yet have series grouping (it is planned but not done).
- The web UI does not yet have a "Not interested" per-session exclusion (it is planned but not done).
- The web UI does not yet have an RSS feed of recommendations (it is planned but not done).
- The web UI does not yet have an OpenSearch descriptor (it is planned but not done).
- The web UI does not yet have a status page (it is planned but not done).
- The web UI does not yet have a print-friendly recommendation list (it is planned but not done).
- The web UI does not yet have fuzzy tag search (it is planned but not done).
- The web UI does not yet have tag aliases (it is planned but not done).
- The web UI does not yet have an embeddable widget (it is planned but not done).
- The web UI does not yet show the diff between two recommendation runs (it is planned but not done).
- The web UI does not yet have multi-fandom recommendations discoverable in the UI (it is planned but not done).
- The web UI does not yet have "Works like this but longer/shorter" (it is planned but not done).
- The web UI does not yet have a language filter (it is planned but not done).
- The web UI does not yet have an API rate limit dashboard (it is planned but not done).
- The web UI does not yet have progressive loading (it is planned but not done).
- The web UI does not yet have canonical URLs for SEO (it is planned but not done).
- The web UI does not yet have had an accessibility audit (it is planned but not done).

## What is likely to change

- The memory budget may be tuned as the corpus grows; the `make budget` guard will catch regressions.
- The API may add new endpoints for planned features (e.g., `/api/v1/entities/{kind}/{id}/neighbors`), but the current API is stable and versioned under `/api/v1/`.
- The web UI may add new pages or components for planned features, but the current structure is stable.
- The CLI may add new flags or subcommands for planned features, but the current structure is stable.
- The tuning system may add new signals, but the current seven signals are stable.
- The arena system may adjust the Glicko-2 parameters, but the core design is stable.
- The profile system may adjust the vector size, but the current design is stable.
- The snapshot exchange may adjust the encryption or signing scheme, but the core design is stable.
- The embeddings may adjust the dimensionality or algorithm, but the current design is stable.
- The collaborative filtering index may adjust the weighting or decay, but the core design is stable.

## What is risky

- The corpus mirror is large (1.7 GB) and takes time to build; the `ingest` and `embed` commands are separate to avoid exceeding the memory budget.
- The web UI uses server-rendered HTML, so adding complex client-side interactions may require a rethink.
- The API is read-only, so adding write endpoints would require a major redesign.
- The snapshot exchange assumes the peer is trustworthy; there is no sandboxing of received snapshots.
- The arena system assumes the user is acting in good faith; there is no defence against sybil attacks or collusion.
- The profile system assumes the user is acting in good faith; there is no defence against poisoning the taste vectors.
- The embeddings are computed from the co-occurrence graph, which is static after ingest; there is no online update.
- The collaborative filtering index is built from the mirror's own user-work interactions, which may be sparse or biased.

## What is stable

- The core recommendation engine (tag overlap, neighbourhood, quality, recency, popularity, embedding, collab, peer_rating) is stable and tested.
- The API contract (JSON responses, error handling, headers) is stable and tested.
- The web UI structure (pages, components, templates) is stable and tested.
- The CLI structure (subcommands, flags, help) is stable and tested.
- The memory budget enforcement is stable and tested.
- The corpus mirror format (SQLite schema) is stable and tested.
- The kindred database format (for tunes, profiles, arena, snapshots) is stable and tested.
- The collaborative filtering index format is stable and tested.
- The embeddings format is stable and tested.
- The arena system format (Glicko-2 ratings) is stable and tested.
- The profile system format (like/dislike vectors) is stable and tested.
- The snapshot exchange format (anonymised, signed snapshots) is stable and tested.
- The tuning system format (named tunes with weights) is stable and tested.
- The corpus-query modes interface is stable and tested.
- The licensing (AGPL-3.0) is stable and tested.

## How to verify progress

Run the goal check:

```bash
python3 docs/goal-check.py
```

Expected output: `COMPLETE -- all 7 clauses pass.`

Run the budget guard:

```bash
make budget
```

Expected output: a table of routes with measured peak RSS and time, followed by `budget OK`.

Run the test suite:

```bash
make test
```

Expected output: all tests pass, with no failures.

Run the documentation checks:

```bash
./scripts/check-doc-commands.sh
./scripts/check-cli-coverage.py
```

Expected output: every documented command and flag exists in this build.

Run the end-to-end browser suite:

```bash
cd e2e && npm test
```

Expected output: all tests pass, with no failures.

## How to deploy

The `deploy.sh` script handles deployment to a Raspberry Pi or similar device. It expects:

- The corpus mirror at `~/kindling-data/ao3_metadata.db` on the target.
- A directory for the kindred database (default: `/var/lib/kindred`).
- A systemd service file (provided in `deploy/kindred.service`).
- The binary built for the target architecture (the script cross-compiles if needed).

Run:

```bash
./scripts/deploy.sh
```

Expected output: the binary is copied, the service is installed and started, and a health check passes.

## How to release

1. Tag the release: `git tag v0.3.0 && git push --tags`
2. Build the binary: `GOOS=linux GOARCH=amd64 go build -o dist/kindred-linux-amd64 ./cmd/kindred`
3. Create a checksum: `sha256sum dist/kindred-linux-amd64 > dist/kindred-linux-amd64.sha256`
4. Upload the binary and checksum to the release page.

The binary is statically linked, so it should run on any Linux distribution with the same kernel version or newer.

## Who to ask

- For questions about the recommendation engine: look at the `internal/engine` package and the tests in `internal/engine/engine_test.go`.
- For questions about the API: look at the `internal/api` package and the tests in `internal/api/*_test.go`.
- For questions about the web UI: look at the `internal/web` package and the end-to-end tests in `e2e/tests/`.
- For questions about the CLI: look at the `cmd/kindred/main.go` file.
- For questions about the memory budget: look at the `scripts/budget.sh` script and the `internal/budget` package.
- For questions about the corpus mirror: look at the `internal/corpus` package.
- For questions about the kindred database: look at the `internal/store` package.
- For questions about the collaborative filtering index: look at the `internal/collab` package and the `cmd/kcollabverify` command.
- For questions about the embeddings: look at the `internal/embed` package.
- For questions about the arena system: look at the `internal/arena` package.
- For questions about the profile system: look at the `internal/profile` package.
- For questions about the snapshot exchange: look at the `internal/dump` and `internal/onion` packages.
- For questions about the tuning system: look at the `internal/signal` package.
- For questions about the corpus-query modes: look at the `internal/corpusquery` package.
- For questions about the licensing: look at the `LICENSE` file.

---