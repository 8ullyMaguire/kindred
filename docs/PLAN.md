# Kindred — PLAN

> Implementable from this document alone: exact file paths, complete
> copy-pasteable code, and per step the exact verification command with its
> expected output. Read `SPEC.md` first; every "why" is there.
>
> **This plan is a living contract.** When a step is found wrong, fix the
> plan in the same commit as the code fix and say in the commit message
> what the plan got wrong and how it was caught.

**Status:** 2026-09-29, written before any code. M0 and M1 are detailed
in full. M2–M6 carry their ordering constraints and rationale; each gets a
detailed section written immediately before it starts (a detailed section
for a distant milestone is written from imagination).

**Repo (proposed, owner to confirm — SPEC §12.4):**
`/mnt/disk-important/personal/documents/code/projects/kindred`
on the thinkcentre pool, remotes `forgejo` + `github`, module
`git.polarisocial.xyz/kindred/kindred`.

---

## 0. Ground rules

1. **`make verify` green before every commit.** `gofmt -l` empty, `go vet`
   clean, `go test ./...` green, `CGO_ENABLED=0 go build` clean.
2. **Every step below ends in a command and its expected output.** If the
   output differs, stop and fix the plan, not the symptom.
3. **Hermetic tests.** `:memory:` DB per test via `setup(t)`, never a
   shared temp file — shared state across tests produces
   order-dependent failures that look like real bugs.
4. **Reference real symbols, not plausible ones.** Before writing a code
   block, confirm the function/type exists. A plan that invents an API
   produces work that does not compile and teaches the implementer to
   trust the plan.
5. **Domain errors map to status codes**, carried over from concord's
   proven pattern: `ErrNotFound`→404, `ErrDuplicate`→409, `ErrInvalid`→400,
   `ErrAuth`→401, `ErrPerm`→403.
6. **Corpus is read-only and never on NFS writes.** Attach the mirror with
   `mode=ro`; all writes go to `kindred.db` on local disk.
7. **Never report a number you did not measure.** The "silent no-op beats
   crashes" rule, applied to this plan: a stage that logs `rows kept: 0`
   is broken even when the test suite is green.

### Dependency versions (verified present in the local module cache)

```
github.com/go-chi/chi/v5   v5.3.2
modernc.org/sqlite         v1.59.0
golang.org/x/crypto        v0.54.0   (minisign, scrypt)
```

All three are in `$(go env GOMODCACHE)` on cachyos, which means the build
works without network. `modernc.org/sqlite` is pure Go — **CGO_ENABLED=0
and no C toolchain on the Pi**, which is what makes §5.2's arm64 build
trivial.

---

## 1. Order, and why

```
M0  skeleton + the memory gate   ← the gate comes first, on purpose
M1  corpus read + ingest + CSR index
M2  signals + embedder + ranking  (needs the graph from M1)
M3  HTTP API + the unofficial AO3 surface
M4  anonymised dump + onion
M5  budgets, deploy, parity report
M6  (optional) web UI — only if wanted
```

**Why the memory gate is M0 and not M6.** The entire reason this project
exists is that kindling used 6.4 GB. A test suite that proves the budget
runs on every commit is the mechanism that stops the same failure from
reappearing quietly as someone adds a cache. Writing it first means the
gate is never retrofitted around a body of code that assumed it away.
This is the one ordering choice here that is genuinely load-bearing.

**Why ingest before signals.** Every signal reads candidate rows out of the
store. Ingest defines the entity shape (`SPEC.md` §2.1) that all of them
consume; building signals against a guessed shape is how a rewrite ends up
with two incompatible notions of "a candidate".

**Why the API after ranking, not before.** The AO3-shaped surface is a thin
projection of the engine's output. Building it first means either writing it
twice or guessing the response shape. `GET /v1/works/{id}/related` is
literally the recommend endpoint with a fixed kind.

**Why onion (M4) after the API (M3).** The dump is a producer of files; the
onion service is a transport for those files. Separating them means the
anonymisation and its tests can be verified without a Tor dependency —
and the four leak tests in `SPEC.md` §4.4 are all testable before Tor
exists.

**Why parity report last.** It needs both engines to run. Until M5 the
answer to "is Kindred as good as Kindling" is unmeasurable, and an
unmeasured rewrite is a guess.

---

## 2. M0 — skeleton and the memory gate

**Goal:** a binary that starts, serves a healthz, and a `make budget`
target that will fail the build if that binary ever exceeds a cap.

**Why first:** see §1.

### 2.1 Files

```
go.mod
Makefile
cmd/kindred/main.go
internal/config/config.go
internal/config/config_test.go
internal/httpapi/server.go
internal/httpapi/health_test.go
internal/budget/budget.go
internal/budget/budget_test.go
```

### 2.2 `go.mod` — exact content

```
module git.polarisocial.xyz/kindred/kindred

go 1.27.1

require (
	github.com/go-chi/chi/v5 v5.3.2
	golang.org/x/crypto v0.54.0
	modernc.org/sqlite v1.59.0
)
```

`go mod tidy` after the first build fills the indirect block. Verify:

```bash
go mod tidy && go mod verify
```
Expected: `all modules verified`.

### 2.3 `internal/config/config.go` — complete file

```go
// Package config resolves kindred's settings from env and flags.
//
// Every setting has an env form so one tree runs in dev and prod. The
// public-API kill switch is an atomic rather than a field because it must
// be flippable while requests are in flight (SPEC §3.2.4).
package config

import (
	"flag"
	"os"
	"strconv"
	"sync/atomic"
)

type Config struct {
	Listen    string
	DB        string
	CorpusDB  string
	Version   string
	Mode      string // "lite" or "full"
	TopN      int
	EmbedDim  int
	KAnon     int
	StableSalt bool
	PublicAPI  atomic.Bool
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func Load() *Config {
	c := &Config{
		Listen:   env("KINDRED_LISTEN", "127.0.0.1:8010"),
		DB:       env("KINDRED_DB", ""),
		CorpusDB: env("KINDRED_CORPUS_DB", ""),
		Version:  env("KINDRED_VERSION", "dev"),
		Mode:     env("KINDRED_MODE", "full"),
		TopN:     envInt("KINDRED_TOP_N", 24),
		EmbedDim: envInt("KINDRED_EMBED_DIM", 32),
		KAnon:    envInt("KINDRED_K_ANON", 20),
		StableSalt: os.Getenv("KINDRED_STABLE_SALT") == "1",
	}
	c.PublicAPI.Store(env("KINDRED_PUBLIC_API", "1") == "1")
	return c
}

// Bind attaches flags to a FlagSet, returning it so main can Parse.
func (c *Config) Bind(fs *flag.FlagSet) *flag.FlagSet {
	fs.StringVar(&c.Listen, "listen", c.Listen, "listen address")
	fs.StringVar(&c.DB, "db", c.DB, "kindred.db path (local disk)")
	fs.StringVar(&c.CorpusDB, "corpus", c.CorpusDB, "read-only corpus SQLite path")
	fs.StringVar(&c.Mode, "mode", c.Mode, "lite|full")
	fs.IntVar(&c.TopN, "top-n", c.TopN, "neighbours retained per tag")
	fs.IntVar(&c.EmbedDim, "embed-dim", c.EmbedDim, "SVD dimensions")
	fs.IntVar(&c.KAnon, "k-anon", c.KAnon, "min bookmarks for a tag_affinity row")
	return fs
}
```

Note the `StableSalt` field is only settable by env, not by flag. That is
deliberate: it is a privacy trade-off and the spec says enabling it is "a
logged decision" (`SPEC.md` §4.2), so it must leave a trace. Do not add a
flag for it without also writing the manifest entry.

### 2.4 `internal/budget/budget.go` — complete file

```go
// Package budget samples this process's own memory and decides pass/fail
// against a cap. It is a package rather than a shell script because the
// numbers it asserts (VmHWM, Go runtime stats) are only meaningful in
// the process that allocated them.
package budget

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// PeakRSSKiB returns VmHWM — the high-water mark, in KiB. Sampling
// current RSS instead would let a spike between samples go unnoticed,
// and a memory regression is exactly a spike.
func PeakRSSKiB() (int, error) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		return strconv.Atoi(fields[1])
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("VmHWM not found in /proc/self/status")
}

// Report is one sampled point of a budget run.
type Report struct {
	Phase        string  `json:"phase"`
	PeakRSSKiB   int     `json:"peak_rss_kib"`
	HeapAllocMiB float64 `json:"heap_alloc_mib"`
	Goroutines   int     `json:"goroutines"`
}

func Snapshot(phase string) Report {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	peak, _ := PeakRSSKiB()
	return Report{
		Phase:        phase,
		PeakRSSKiB:   peak,
		HeapAllocMiB: float64(ms.HeapAlloc) / (1 << 20),
		Goroutines:   runtime.NumGoroutine(),
	}
}
```

The `PeakRSSKiB` error is deliberately **not** fatal at the call site — a
non-Linux target should still report heap numbers. The budget target
itself checks for the error explicitly.

### 2.5 `internal/budget/budget_test.go` — complete file

```go
package budget

import "testing"

func TestPeakRSSKiBReadsARealNumber(t *testing.T) {
	kiB, err := PeakRSSKiB()
	if err != nil {
		t.Fatalf("PeakRSSKiB: %v", err)
	}
	if kiB <= 0 {
		t.Fatalf("VmHWM = %d KiB, want a positive number for a live process", kiB)
	}
}

func TestSnapshotReportsHeapAndGoroutines(t *testing.T) {
	r := Snapshot("test")
	if r.HeapAllocMiB <= 0 {
		t.Fatalf("heap alloc = %f MiB, want > 0", r.HeapAllocMiB)
	}
	if r.Goroutines <= 0 {
		t.Fatalf("goroutines = %d, want > 0", r.Goroutines)
	}
}
```

A test that cannot fail tests nothing — this one would fail if `Snapshot`
returned a zero struct, which is the failure mode it exists to catch.

### 2.6 `internal/httpapi/server.go` + `health_test.go`

Server struct holding `*config.Config` and a `*chi.Mux`; one route
`GET /v1/healthz` returning
`{"status":"ok","version":"...","mode":"...","index_age_hours":0}`.

The health handler must report index residency (`SPEC.md` §11.4) — start
it as a field the indexer fills, so the field exists from M0 and the
response shape never changes later.

### 2.7 `Makefile` — complete file

```make
GO      ?= go
BIN     ?= bin/kindred
PORT    ?= 8010
BUDGET  ?= 262144        # KiB; 256 MiB, the Pi cap from SPEC §6

.PHONY: verify fmt vet test build budget run clean

verify: fmt vet test build

fmt:
	@test -z "$$($(GO) run cmd/tools/fmtcheck 2>/dev/null || gofmt -l . | tee /dev/stderr)" || \
		( echo "gofmt: files above need formatting" && exit 1 )

vet:
	$(GO) vet ./...

test:
	$(GO) test -timeout 300s ./...

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w' -o $(BIN) ./cmd/kindred

# The gate this whole project exists to satisfy (SPEC §1, §6).
budget: build
	./scripts/budget.sh

run: build
	./$(BIN) serve

clean:
	rm -rf bin
```

Simplify the `fmt` target to `gofmt -l . | (! grep .)` if the `cmd/tools`
path does not exist yet — do not create a tool just to print a file list.

### 2.8 `scripts/budget.sh` — complete file

```bash
#!/usr/bin/env bash
# Boot the server, walk every route, assert the peak RSS cap.
#
# Reads SPEC §6 for the caps. Fails non-zero over budget — that is the
# entire contract, and it must fail the build, not warn.
set -euo pipefail

PORT="${PORT:-8010}"
CAP_KIB="${BUDGET:-262144}"
DB="${DB:-$(mktemp -d)/kindred.db}"
BASE="http://127.0.0.1:${PORT}"

./bin/kindred serve --db "$DB" --listen "127.0.0.1:${PORT}" &
PID=$!
trap 'kill $PID 2>/dev/null || true; rm -rf "$(dirname "$DB")"' EXIT

for _ in $(seq 1 50); do
  curl -fsS "${BASE}/v1/healthz" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS "${BASE}/v1/healthz" >/dev/null || { echo "server never became healthy"; exit 1; }

# Walk every registered route. Each entry is "METHOD PATH".
while read -r method path; do
  [ -z "$method" ] && continue
  curl -fsS -o /dev/null -X "$method" "${BASE}${path}" || true
done < routes.txt

PEAK_KIB=$(awk '/VmHWM/ {print $2}' "/proc/${PID}/status")
echo "peak RSS: ${PEAK_KIB} KiB (cap ${CAP_KIB} KiB)"
if [ "$PEAK_KIB" -gt "$CAP_KIB" ]; then
  echo "OVER BUDGET by $((PEAK_KIB - CAP_KIB)) KiB" >&2
  exit 1
fi
echo "budget ok"
```

`routes.txt` is a plain list, one `METHOD PATH` per line, and it is the
single source of truth for what the budget run exercises. The API milestone
appends to it; nothing else may.

### 2.9 Verification — M0

```bash
cd /mnt/disk-important/personal/documents/code/projects/kindred
make verify
```
Expected, in order:
```
<no gofmt output>
<no vet output>
ok  	git.polarisocial.xyz/kindred/kindred/internal/budget
ok  	git.polarisocial.xyz/kindred/kindred/internal/config
ok  	git.polarisocial.xyz/kindred/kindred/internal/httpapi
```

```bash
make budget
```
Expected:
```
peak RSS: 1xxxx KiB (cap 262144 KiB)
budget ok
```

Then prove the gate can actually fail — a gate never seen failing is not a
gate:

```bash
BUDGET=1024 make budget; echo "exit=$?"
```
Expected: `OVER BUDGET by ... KiB` and `exit=1`.

Record the real number in `docs/measurements.md` with the date and the
git SHA. It is the baseline every later milestone is compared against.

**Commit:** `m0: skeleton with the memory budget as a build gate`

---

## 3. M1 — corpus read, ingest, CSR index

**Goal:** read the 1.7 GB mirror read-only, model entities, and produce a
compact array co-occurrence index. This is the milestone where the 24×
memory win (`SPEC.md` §0.1) either happens or does not.

### 3.1 Measure before building (this step exists to catch the plan being wrong)

The plan asserts the CSR will be ~124 MB. Measure the real numbers first,
because `SPEC.md` §0.1 already documents one stale metadata row in this
schema.

```bash
DB=~/kindling-data/ao3_metadata.db
sqlite3 "file:$DB?mode=ro" "
  SELECT 'edges',        COUNT(*) FROM cooccurrence_edges
  UNION ALL SELECT 'edge_tags',   COUNT(*) FROM (SELECT tag_a_id FROM cooccurrence_edges UNION SELECT tag_b_id FROM cooccurrence_edges)
  UNION ALL SELECT 'meta_claim',  node_count FROM cooccurrence_graph_meta
  UNION ALL SELECT 'works',      COUNT(*) FROM works
  UNION ALL SELECT 'work_tags',  COUNT(*) FROM work_tags;"
```

Then compute the CSR size exactly as the code will:

```bash
# directed entries = 2 * edges; each is (tag_id int32, count float32) = 8 B
sqlite3 "file:$DB?mode=ro" "SELECT COUNT(*)*2*8/1048576 FROM cooccurrence_edges;"
```

**If `edge_tags` differs materially from `meta_claim`, update `SPEC.md`
§0.1 in this same commit and say so in the message.** That discrepancy is
already known; this step confirms whether it is the only one.

### 3.2 Files

```
internal/corpus/entity.go
internal/corpus/entity_test.go
internal/corpus/ao3.go
internal/corpus/ao3_test.go
internal/store/store.go
internal/store/store_test.go
internal/graph/graph.go
internal/graph/graph_test.go
internal/graph/csr.go
internal/graph/csr_test.go
cmd/kindred/ingest.go
```

### 3.3 `internal/corpus/entity.go` — the shape everything else assumes

```go
package corpus

// Entity is the kind-agnostic unit (SPEC §2.1). kind is a free string on
// purpose: the engine must not need a registry edit to learn about a new
// corpus.
type Entity struct {
	ID      int64
	Kind    string
	Title   string
	URL     string
	Summary string
	Stats   map[string]float64
	Tags    []Tag
}

type Tag struct {
	Name   string
	Type   string
	Weight float64
}
```

`Stats` is a map rather than a struct because a signal must be able to read
a corpus-specific metric without a core change; a signal that needs a new
field for every source has defeated the abstraction. The cost is one map
per candidate, which is bounded by the candidate pool cap, not the corpus.

### 3.4 `internal/corpus/ao3.go` — the source adapter

`Iterate(fn func(Entity) error) error` over `works`, joined to `work_tags`
and `tags`, one statement with a LEFT JOIN and no N+1. Two rules from the
measured schema (`SPEC.md` §0.2, §11.2):

- `bookmarks` is NULL for 112,890 of 112,935 rows. Scan into `*int64` and
  record `Stats["has_bookmarks"] = 0` when NULL, so a later sort cannot
  accidentally treat NULL as zero.
- `tag_type` is `freeforms` for 3,890,504 of 3,891,300 rows. Do **not**
  branch on tag type; carry it through as a label and let signals filter.

### 3.5 `internal/graph/csr.go` — the win, in one type

```go
// CSR is a compressed-sparse-row adjacency: offsets has len(nodes)+1
// entries, and node i's neighbours are indices[col[offsets[i]:offsets[i+1]]].
//
// This replaces kindling's dict-of-dicts graph (3.0 GB for 7.75M edges)
// with arrays. It is the single reason the memory budget is achievable
// (SPEC §0.1, §6) and it changes no algorithm.
type CSR struct {
	NodeCount int
	EdgeCount int
	offsets   []int64
	neighbors []int32
	weights   []float32
}
```

`Build(edges []Edge) *CSR` counting-sort by `tag_a_id`. The two-pointer
counting sort is the whole implementation; no maps, no sorting a slice of
structs, no `container/list`.

**Invariant test, in `csr_test.go`:** for every node,
`len(neighbors[offsets[i]:offsets[i+1]])` equals the edge count incident to
it, and summing every degree equals `2*EdgeCount`. A test that only checks
`len(CSR)` would pass on a silently truncated graph — which is exactly the
silent-no-op failure the house rules call out.

### 3.6 Verification — M1

```bash
make verify
go test ./internal/graph/ -run TestCSR -v
```
Expected: subtests for `DegreeSum`, `RoundTrip`, `EmptyGraph`, `SingleNode`,
all `PASS`.

```bash
go run ./cmd/kindred ingest --corpus ~/kindling-data/ao3_metadata.db --db /tmp/k.db
```
Expected output — note it must print the counts, and a `0` is a bug, not a
quiet success:
```
ingest works:     rows_in=112935 rows_kept=112935 rows_dropped=0
ingest tags:      rows_in=634231 rows_kept=123047 rows_dropped=511184
ingest work_tags: rows_in=3891300 rows_kept=3891300 rows_dropped=0
index: nodes=123047 edges=7750334 csr_bytes=118 (MiB)
```

The `tags` line dropping 511k is **expected and correct**: the graph only
references the 123,047 tags that co-occur. The point of printing
`rows_in` alongside is that a silent 0 would look identical to a legitimate
prune without it.

Then verify the index against the source, independently of the code that
built it:

```bash
sqlite3 /tmp/k.db "SELECT COUNT(*) FROM graph_meta WHERE edge_count=7750334;"
```
Expected: `1`. And:

```bash
du -h /tmp/k.db
```
Expected: under 200 MB, down from a 1.7 GB corpus. **If this is over
200 MB, the CSR is not being used — find out why before continuing.**

**Commit:** `m1: corpus ingest and a CSR co-occurrence index`

---

## 4. M2 — signals, embedder, ranking

**Ordering constraint, written before this milestone starts:** implement
`tag_overlap` and `neighbourhood` first, because they need only the CSR
(M1) and they are the two dimensions that carry the most weight in kindling.
Add `quality`/`recency`/`popularity` next (pure SQL over the corpus, no
graph). Add `embedding` only after the Lanczos implementation is verified
against a dense reference — it is the most expensive thing in the project
and the easiest to get quietly wrong. `peer_rating` and `taste` are last and
are both optional.

**Files:** `internal/embed/lanczos.go`, `internal/embed/lanczos_test.go`,
`internal/signal/{tag_overlap,neighbourhood,quality,recency,popularity,embedding}.go`,
`internal/signal/signal_test.go`, `internal/diversify/mmr.go`,
`internal/diversify/mmr_test.go`, `internal/rank/rank.go`.

### 4.1 The signals contract

```go
// A Signal is a pure scoring dimension. It never writes, never mutates its
// candidate, and always returns a reason — a ranking that cannot explain
// itself is not a ranking anyone can argue with (SPEC §7).
type Signal interface {
	Name() string
	Score(c Candidate, seeds []Candidate, s Store) (float64, string, error)
}
```

`errSkipSignal` is the degradation path: returning it omits the signal from
the score, records it in `meta.degraded[]`, and continues. Kindling's
`9c9287b` was exactly this bug — the arena signals were inert in production
and silent about it. **A signal that cannot be computed must be visible in
the response.** This is a required behaviour with a required test.

### 4.2 `internal/embed/lanczos_test.go` — the gate

The one test that matters: build a 500×500 sparse matrix with a known
spectrum, run randomised Lanczos for 32 dimensions, compare against a
dense `eigen` reference computed in the test itself.

Expected: top-32 singular values agree to within 1e-6 relative error;
any agreement is a failure, and a test that compares the implementation to
itself proves nothing (see the round-trip lesson in memory — two codec
halves agreeing proves neither is right).

### 4.3 Verification — M2

```bash
go test ./internal/embed/ -run TestLanczosMatchesDenseReference -v
```
Expected: `--- PASS`, and the printed max relative error `< 1e-6`.

```bash
go test ./internal/signal/ -v
```
Expected: one `PASS` per signal, plus `TestDegradedSignalAppearsInMeta` —
which asserts that a signal returning `errSkipSignal` shows up in
`meta.degraded` and is absent from `evidence`. **If that test is hard to
write, the degradation path is not actually observable and §7's
requirement is unmet** — fix the code, not the test.

```bash
go test ./internal/diversify/ -run TestMMR -v
```
Expected: `PASS`, plus `TestShortfallIsReportedNotPadded` — asking for 100
with a cap that admits 90 must return 90 rows and a `meta.shortfall`
explaining the 10, never 100 with the cap ignored (`SPEC.md` §2.5).

**Commit:** `m2: signals, Lanczos embeddings, MMR with honest shortfalls`

---

## 5. M3 — HTTP API and the unofficial AO3 surface

**Ordering constraint:** the `/v1` routes in `SPEC.md` §3.1 are exactly the
contract. Implement `GET /v1/recommend` first, then project it.

**Files:** `internal/api/router.go`, `internal/api/recommend.go`,
`internal/api/ao3.go`, `internal/api/middleware.go`, `internal/api/*_test.go`,
plus `internal/config`'s `PublicAPI` gate checked in one middleware.

### 5.1 The three defences that must exist before any route does

1. `TestRequestPathLinksNoHTTPClient` — a test that walks
   `go list -deps ./internal/api` output and fails if `net/http` appears
   in a *handler's* transitive imports. It is a structural assertion
   (`SPEC.md` §3.2.1), and it is what makes "no request handler can make a
   network call" a guarantee rather than a promise in a docstring.
2. `TestPublicAPIKillSwitch` — with `KINDRED_PUBLIC_API=0`, every `/v1`
   route except `/v1/healthz` returns 404, and flipping the atomic at
   runtime takes effect on the next request with no restart.
3. `TestIndexAgeHeader` — every response carries
   `X-Kindred-Index-Age` and `X-Kindred-Index-Version`, so a stale mirror
   is visible to the client (`SPEC.md` §3.2.2).

### 5.2 The rate limit's trust argument

`CF-Connecting-IP` is honoured **only** when the immediate peer is
loopback. Ported verbatim from concord's fix: the listener is loopback
behind a local proxy, so a forwarded header from loopback is trustworthy
and from anywhere else is not. Keying on `RemoteAddr` alone put every
visitor in one shared bucket and 429'd the whole site — a bug that was
found live, not in review.

LRU cap 4,096 keys (`SPEC.md` §6). Kindling's `pruneIdle` was
time-based only; a long-lived process with many short-lived keys still
grows without bound.

### 5.3 Verification — M3

```bash
go test ./internal/api/ -v
```
Expected: `PASS` per route, plus the three defences above, plus
`TestAO3SurfaceShape` asserting `/v1/works?page=1&per_page=20` returns the
AO3 field names clients expect (`id`, `title`, `authors`, `summary`,
`word_count`, `kudos`, `hits`, `tags[]`).

```bash
make verify && ./bin/kindred serve --db /tmp/k.db &
curl -s localhost:8010/v1/works?per_page=2 | head -20
curl -s -D- -o /dev/null localhost:8010/v1/works/1/related | grep -i x-kindred
```
Expected: a JSON array of works, and both `X-Kindred-Index-Age` and
`X-Kindred-Index-Version` in the headers.

**Commit:** `m3: the API and the unofficial AO3 surface`

---

## 6. M4 — anonymised snapshot over Tor

**Ordering constraint:** anonymiser → sharder → manifest → signer →
retention, then onion. Each stage gets its own test, and the anonymiser's
tests need no Tor at all — which is the reason the onion work is last.

**Files:** `internal/dump/anonymise.go`, `internal/dump/shard.go`,
`internal/dump/manifest.go`, `internal/dump/sign.go`, `internal/dump/delta.go`,
`internal/dump/retention.go`, `internal/onion/service.go`, `internal/onion/fetch.go`,
plus `cmd/kindred/dump.go`.

### 6.1 The dropped tables are a test, not a comment

```go
var droppedTables = []string{
	"users", "sessions", "session_seeds", "session_cache",
	"taste_profiles", "taste_profile_signals", "seed_weights",
	"user_work_interactions",
}
```

`TestDumpContainsNoUserRows` asserts zero of each in the output, and
`TestTagAffinityHasNoWorkIDs` asserts the `tag_affinity` schema has no
`work_id` column at all. Both exist because the failure is a *retained*
identifier, which no snapshot would ever look wrong for.

### 6.2 The four leak tests from `SPEC.md` §4.4

| Test | Asserts |
|---|---|
| `TestFetchRefusesClearnetHost` | pointing the fetcher at `127.0.0.1` refuses |
| `TestFetchRefusesRedirectOffOnion` | an `.onion` → `example.com` redirect refuses |
| `TestNoClearnetRouteForDump` | the service registers only the onion listener |
| `TestKAnonFiltersBeforeSalting` | the `>= K` filter runs before the salt is applied, so the row count cannot leak a small account |

The second one is the real bug risk: a client that follows redirects
happily is a working IP leak.

### 6.3 Pruning the co-occurrence edges — measured, not guessed

`SPEC.md` §4.2 says `cooccurrence_edges` ships pruned. The threshold is not
known until the dump is built, so **M4 starts by measuring the edge count
distribution** and then choosing a percentile:

```bash
sqlite3 "file:~/kindling-data/ao3_metadata.db?mode=ro" "
  SELECT cooccur_count, COUNT(*) FROM cooccurrence_edges
  GROUP BY 1 ORDER BY 1 LIMIT 20;"
```

Keep the threshold that retains ~60% of edges and ~95% of edge mass, and
record both numbers in the spec. A dump that is 600 MB every day is not
"sharing a dump", it is a denial of service on the operator.

### 6.4 Verification — M4

```bash
go test ./internal/dump/ ./internal/onion/ -v
```
Expected: every test above `PASS`, and `TestDumpContainsNoUserRows`
printing the per-table count it checked (`users: 0`, …) so a reviewer can
see the assertion ran rather than trusting the word `PASS`.

```bash
./bin/kindred dump --corpus ~/kindling-data/ao3_metadata.db \
  --out /tmp/dumps --k-anon 20
```
Expected: a manifest, a signed `manifest.minisig`, N shards, and printed
row counts per table. **Then verify with the tool, not by eye:**

```bash
./bin/kindred dump verify --in /tmp/dumps
```
Expected: `signature ok`, `hashes ok`, `no user rows`, exit 0.

**Commit:** `m4: anonymised signed snapshots over a Tor onion service`

---

## 7. M5 — budgets, deploy, parity

**This is the milestone that answers the question the project exists for:**
does the rewrite cost less and work as well.

**Files:** `scripts/budget-pi.sh`, `deploy/kindred.service`,
`deploy/kindred-pi.service`, `docs/measurements.md`,
`docs/parity/compare.py`, `docs/PARITY.md`.

### 7.1 Deploy

- thinkcentre: `MemoryMax=512M`, port 8010, DB on local NVMe, corpus
  attached read-only from the pool.
- Pi: `MemoryMax=256M`, `--mode lite`, cross-compiled arm64 binary —
  `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build`, no C toolchain needed
  on the Pi at all.
- Both units: `ExecStartPre` gates on `/v1/healthz` answering. Concord's
  `make deploy` never worked because its healthz grep expected compact
  JSON while the handler pretty-prints; a unit that greps for a JSON
  substring is the same bug waiting to happen. Compare status codes, not
  body text.

### 7.2 The parity report is read by a human

`docs/PARITY.md` must contain, for a fixed set of 20 seed works and 5 tunes:

- the top-20 from kindling and from kindred side by side,
- rank correlation (Spearman) per seed,
- RSS and p99 latency for each,
- the recommendation on whether kindred replaces kindling, and the
  disagreements that argue against it.

**It is a report, not a test.** `SPEC.md` §6 says the SVD is a
reimplementation and output will differ; a gate asserting bit-parity would
be a gate that has to be deleted. Report it, read it, decide it.

### 7.3 Verification — M5

```bash
make budget && BUDGET=262144 make budget
```
Expected on thinkcentre: `peak RSS: N KiB (cap 262144 KiB)` with
`N < 225000` (`SPEC.md` §6's 220 MB steady plus headroom).

```bash
ssh <pi> 'systemctl --user restart kindred && curl -fsS localhost:8010/v1/healthz'
```
Expected: `{"status":"ok",...}`. A real arm64 run on real hardware — not a
cross-compile, not a claim.

**Commit:** `m5: budget verification on both hosts and the parity report`

---

## 8. M6 — web UI (optional, only if wanted)

`SPEC.md` §1.1 cuts this deliberately. If it is wanted, the constraint is
that it must not reintroduce a build step or a framework: server-rendered
`html/template` + the embedded CSS, ~2,000 lines of Go templates, no Node,
no client framework. Anything else defeats the project's premise.

---

## 9. What this plan got wrong so far

Recorded as required by the house rules, updated in the same commit as any
code fix. **Empty as of writing, which means nothing has been implemented
yet — the first entry appears with M0's first real correction.**
