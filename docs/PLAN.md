# Kindred — PLAN

> Implementable from this document alone: exact file paths, complete
> copy-pasteable code, and per step the exact verification command with its
> expected output. Read `SPEC.md` first; every "why" is there.
>
> **This plan is a living contract.** When a step is found wrong, fix the
> plan in the same commit as the code fix and say in the commit message
> what you changed and why.

---

## 0. Prerequisites

- Go 1.22+ (tested with 1.22.5)
- GNU make
- SQLite 3.35+ (for the corpus database)
- Git
- About 20 GB of free disk space for the corpus mirror
- Raspberry Pi 4 with 4 GB RAM or equivalent (for testing the memory budget)

The corpus mirror (AO3 dump) is assumed to already exist at
`~/kindling-data/ao3_metadata.db`. Instructions for building it are
outside the scope of this plan.

---

## 1. Repository layout

```bash
$ tree -L 2
.
├── cmd
│   ├── e2eserver       # HTTP server for end-to-end tests
│   ├── kcollabverify   # Tool to verify the collaborative filtering index
│   └── kindred         # Main command (kindred serve, ingest, etc.)
├── internal
│   ├── api             # HTTP API handlers and server
│   ├── arena           # Anonymous peer-ranking arena (Glicko-2)
│   ├── budget          # Memory and speed guard (make budget)
│   ├── collab          # Collaborative filtering (co-bookmark index)
│   ├── corpus          # AO3 database schema and accessors
│   ├── corpusquery     # Corpus analysis modes (fandom-ranking, etc.)
│   ├── crawl           # Fetch AO3 work pages (HTTP client)
│   ├── diversify       # Diversity algorithms (MMR, etc.)
│   ├── dump            # Write anonymised, signed snapshots
│   ├── embed           # Compute tag embeddings (SVD)
│   ├── engine          # Recommendation engine (ranking, pooling)
│   ├── fandom          # Fandom-specific utilities
│   ├── graph           # In-memory graph representation (CSR)
│   ├── onion           # Fetch snapshots from peer .onion services
│   ├── profile         # Taste profiles (like/dislike vectors)
│   ├── rank            # Ranking data structures and utilities
│   ├── signal          # Signal weights and tuning
│   ├── store           # Kindred SQLite database (tunes, profiles, arena, snapshots)
│   ├── testcorpus      # Small test corpus (for unit tests)
│   └── web             # Server-rendered HTML UI (templates, handlers)
├── docs
│   ├── SPEC.md         # This spec
│   ├── PLAN.md         # This plan
│   ├── SPEC-web-ui.md  # Web UI specification
│   ├── PLAN-web-ui.md  # Web UI implementation plan
│   ├── WHAT-IS-LEFT.md # Verified completed work
│   ├── what-is-left.md # Same as above, lowercase for linking
│   ├── DECISIONS.md    # Architectural decisions and why
│   ├── HANDOFF.md      # Notes for the next implementer
│   ├── PROVENANCE.md   # Where design ideas came from
│   ├── goal-check.py   # Completion predicate (run to verify)
│   └── specs/
│       └── 2026-10-05-engine-coverage.md # What the engine tests cover
├── go.mod
├── go.sum
├── LICENSE
├── Makefile
├── README.md
├── routes.txt          # Historical checklist (not the source of truth)
└── scripts
    ├── budget.sh       # Enforces memory/speed guarantees
    ├── check-cli-coverage.py     # Verifies every CLI flag is documented
    ├── check-doc-commands.sh     # Verifies every documented command exists
    ├── check-provenance.sh       # Checks PROVENANCE.md against git history
    ├── deploy.sh       # Deploys to a Raspberry Pi
    ├── exp-delta.sh    # Shows changes in memory/speed over time
    ├── exp-retention.sh    # Enforces snapshot retention policy
    └── *.sh            # Other helper scripts
```

---

## 2. Dependencies

All dependencies are vendored via the Go module system. See `go.mod` for the
exact versions. No external services (database, message queue, etc.) are
required; everything runs in-process.

To fetch dependencies:

```bash
go mod download
```

Expected output: a list of downloaded modules, ending with no errors.

---

## 3. Build the binary

```bash
make build
```

or equivalently:

```bash
go build -o bin/kindred ./cmd/kindred
```

Expected output: no errors, and a binary at `bin/kindred`.

Verify:

```bash
bin/kindred --help
```

Expected output: the usage message (see `cmd/kindred/main.go`).

---

## 4. Run the test suite

```bash
make test
```

or equivalently:

```bash
go test ./... -count=1
```

Expected output: all tests pass, with no failures. The output should end
with `ok` for every package and no `FAIL` lines.

---

## 5. Verify formatting and vet

```bash
make fmt
```

Expected output: no output (gofmt exits silently if no changes are needed).

```bash
make vet
```

Expected output: no output (govet exits silently if no issues are found).

---

## 6. Run the memory/speed budget guard

```bash
make budget
```

This runs `scripts/budget.sh`, which:

1. Starts a fresh instance of the `kindred serve` command with the corpus
   mirror at `~/kindling-data/ao3_metadata.db`.
2. Walks a fixed set of API routes (the ones listed in `scripts/budget.sh`).
3. Measures peak RSS and latency for each route.
4. Checks that the peak RSS does not exceed the cap (220 MB for full mode,
   60 MB for lite mode).
5. Checks that the latency does not exceed the threshold (defined in the
   script).

Expected output: a table of routes with their measured peak RSS and time,
followed by either `budget OK` or `budget FAILED` and a list of violations.

If the budget fails, the step is not complete until the binary is
modified to meet the budget.

---

## 7. Verify the documentation matches the code

```bash
./scripts/check-doc-commands.sh
```

Expected output: every command quoted in the documents (README.md, SPEC.md,
PLAN.md, etc.) exists in this build, with a count of how many were
extracted and how many were verified.

```bash
./scripts/check-cli-coverage.py
```

Expected output: every flag and subcommand of the `kindred` binary is
documented in the usage message (`cmd/kindred/main.go`).

---

## 8. Run the end-to-end browser suite

```bash
cd e2e && npm test
```

Expected output: all Playwright tests pass, with no failures. The suite
runs a real Chromium instance against the `kindred serve` command and
checks that the web UI behaves as expected.

---

## 9. Verify the engine has tests

```bash
find internal/engine -name '*_test.go' -exec wc -l {} +
```

Expected output: at least one test file (`internal/engine/engine_test.go`)
with a non-zero line count, and the output of `go test ./internal/engine`
should show `ok`.

---

## 10. Run the goal check (final verification)

```bash
python3 docs/goal-check.py
```

Expected output: the script exits with code 0 and prints `COMPLETE -- all 7 clauses pass.` (or similar). The clauses are:

1. [ok  ] builds
2. [ok  ] go vet clean
3. [ok  ] gofmt clean
4. [ok  ] test suite green
5. [ok  ] no untested packages
6. [ok  ] page coverage
7. [ok  ] ao3-recommender parity

If any clause fails, the step is not complete until the failure is
addressed.

---

## 11. (Optional) Deploy to a Raspberry Pi

```bash
./scripts/deploy.sh
```

Expected output: the binary is copied to the target device, the service is
installed and started, and a health check passes.

---

## 12. (Optional) Build the corpus mirror

This step is outside the scope of the kindred project, but for completeness:

1. Follow the instructions in the Kindling project to download the AO3
   data dump.
2. Run the Kindling ingest process to build the SQLite database at
   `~/kindling-data/ao3_metadata.db`.
3. Verify the database contains the expected tables and row counts.

---

## 13. Verify the API endpoints

Once the binary is built and serving (`kindred serve --corpus ~/kindling-data/ao3_metadata.db --db /var/lib/kindred/kindred.db --listen 127.0.0.1:8010`), you can verify the API endpoints manually or with a script.

Example verification script:

```bash
#!/bin/bash
set -euo pipefail

B=http://127.0.0.1:8010

# Health check
curl -s -f "$B/healthz" | grep -q '"status":"ok"'

# Stats
curl -s -f "$B/stats" | grep -q '"corpus_works":112935'

# Recommendations
curl -s -f "$B/api/v1/recommend?seed=ao3_work:1&n=2" | python3 -c "
import json, sys
d=json.load(sys.stdin)
assert len(d['items']) == 2
assert 'score' in d['items'][0]
assert 'evidence' in d['items'][0]
"

# Tag search
curl -s -f "$B/api/v1/ao3/tags?q=hero&limit=1" | python3 -c "
import json, sys
d=json.load(sys.stdin)
assert len(d['items']) == 1
assert 'name' in d['items'][0]
"

# Web UI home page
curl -s -f "$B/" | grep -q '<html'

echo "All checks passed"
```

Expected output: the script runs to completion and prints "All checks passed".

---

## 14. Verify the snapshot exchange

```bash
# Dump a snapshot
kindred dump --dir /tmp/kindred-snap

# Verify the snapshot
kindred verify --dir /tmp/kindred-snap

# Fetch from a peer (requires a peer running kindred serve with onion
# enabled; see the onion package for details)
kindred fetch --onion peer.onion --dir /tmp/kindred-snap-2
```

Expected output: no errors, and the snapshot files are present in the
directory.

---

## 15. Verify the collaborative filtering index

```bash
# Build the collab index (done automatically by ingest unless --no-collab)
kindred ingest --corpus ~/kindling-data/ao3_metadata.db --db /var/lib/kindred/kindred.db

# Verify it
./cmd/kcollabverify/kcollabverify
```

Expected output: the tool prints statistics about the co-bookmark index
and exits with code 0.

---

## 16. Verify the embeddings

```bash
# Compute embeddings
kindred embed --corpus ~/kindling-data/ao3_metadata.db --db /var/lib/kindred/kindred.db

# Verify they exist in the database
sqlite3 /var/lib/kindred/kindred.db "SELECT count(*) FROM embeddings;"
```

Expected output: a row count equal to the number of tags in the corpus
(634,231 as of the 2026-09-29 mirror).

---

## 17. Verify the tuning system

```bash
# List the default tune
kindred tune --name default --list

# Set custom weights
kindred tune --name default --set "tag_overlap=0.5,neighbourhood=0.3"

# Inspect the new weights
kindred tune --name default --list

# Reset to default
kindred tune --name default --reset
```

Expected output: the tune weights are printed, updated, and reset as
expected.

---

## 18. Verify the corpus-query modes

```bash
# Fandom ranking
kindred corpus-query --mode fandom-ranking --tag 42 --limit 5

# Underrated
kindred corpus-query --mode underrated --tag 42 --limit 5

# Tag neighbours
kindred corpus-query --mode tag-neighbours --tag 42 --limit 5

# Surprise
kindred corpus-query --mode surprise --tag 42 --limit 5
```

Expected output: each command returns a JSON list of results with the
expected fields for the mode.

---

## 19. Verify the arena system

```bash
# Start a fresh kindred serve with an empty database for testing
kindred serve --corpus ~/kindling-data/ao3_metadata.db --db /tmp/kindred-test.db --listen 127.0.0.1:8011 &
KINDRED_PID=$!
sleep 2  # let it start

# Get a pair
curl -s "http://127.0.0.1:8011/api/v1/arena/pair" | python3 -c "
import json, sys
d=json.load(sys.stdin)
assert len(d['items']) == 2
assert d['items'][0]['id'] != d['items'][1]['id']
"

# Record a judgment (replace IDs with actual ones from the pair)
curl -s -X POST -H "Content-Type: application/json" \
  -d '{"winner":"ao3_work:1","loser":"ao3_work:2"}' \
  "http://127.0.0.1:8011/api/v1/arena/compare"

# Check the leaderboard
curl -s "http://127.0.0.1:8011/api/v1/arena/leaderboard" | python3 -c "
import json, sys
d=json.load(sys.stdin)
assert len(d['items']) > 0
"

# Clean up
kill $KINDRED_PID
```

Expected output: the commands run without error and return the expected
data structures.

---

## 20. Verify the profile system

```bash
# Build a profile from seeds
kindred profile build --seed ao3_work:1 --seed ao3_work:2 --name test-profile

# List profiles
kindred profile list | grep test-profile

# Show the profile
kindred profile show --name test-profile

# Rate a work (like)
kindred profile rate --name test-profile --seed ao3_work:3 --value 1

# Rate a work (dislike)
kindred profile rate --name test-profile --seed ao3_work:4 --value -1

# Export the profile
kindred profile export --name test-profile > /tmp/test-profile.json

# Import the profile
kindred profile import --name test-profile-copy < /tmp/test-profile.json

# Clean up
kindred profile delete --name test-profile
kindred profile delete --name test-profile-copy
```

Expected output: the commands run without error and the profile data is
preserved across export/import.

---

## 21. Verify the crawl subcommand

```bash
# Fetch a single work page (requires network access to AO3)
kindred crawl --corpus ~/kindling-data/ao3_metadata.db --work-id 12345

# Verify the work was added (optional, requires checking the database)
sqlite3 ~/kindling-data/ao3_metadata.db "SELECT count(*) FROM works WHERE id=12345;"
```

Expected output: the command runs without error (though it may fail if
AO3 is blocking the request or the work ID does not exist).

---

## 22. Final verification

Run the goal check one more time to ensure nothing broke:

```bash
python3 docs/goal-check.py
```

Expected output: `COMPLETE -- all 7 clauses pass.`

If the goal check passes, the implementation is complete and ready for
use.

---