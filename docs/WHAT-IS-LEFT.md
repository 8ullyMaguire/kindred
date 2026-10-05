# kindred — what is left

Written 2026-10-05, from measurement. Every claim below was checked against the
tree or the running service, not against a plan.

## Where it stands

| | |
|---|---|
| Go | 21 packages, `go test ./... -count=1` exit 0, `go vet` clean, `gofmt` clean |
| One command | `make verify` — gofmt, vet, test, spec, docs, deploy + both gates, memory budget |
| Browser | 79 Playwright tests pass (69 pre-existing + 10 new for the filters) |
| Spec gate | `docs/goal-check.py` — all 7 clauses pass |
| Deployment | live on thinkcentre `:8010`, binary sha256 matches the build, unit restarted 2026-10-05 01:02 |
| Deploy gate | `scripts/check-deploy.sh` — **32 behaviour checks, all pass** |
| Provenance gate | `scripts/check-provenance.sh` — **IN SYNC**, deployed binary built from HEAD |
| Doc gate | `scripts/check-doc-commands.sh` — exit 0, 30 invocations, 3 exits (0 clean / 1 docs wrong / 2 stale binary) |
| Index age | **~130h, and correct.** The index was built 2026-09-29; the header reports the age of the *data*. A deploy does not rebuild an index — `kindred ingest` does. |

## Done this session

- **Deployed.** The instance had been serving a Sep 29 binary for six days.
  Rebuilt, deployed behind a pre-flight gate on a scratch port, verified on the
  wire.
- **A deploy-drift gate**, because every other gate runs against the repo and a
  deploy is the one step where the code under test and the code being run are
  different objects. Mutation-checked twice by building the pre-headers
  revision and serving it: 20 of 22 checks fail, then 28 of 32.
- **SPEC §4.4 row 1 implemented**, as a source-level AST gate rather than the
  `dump verify --no-clearnet` flag the spec named — this project has no
  snapshot-serving code at all, so the guarantee is a property of the codebase.
  Rows 3 and 4 are marked **NOT IMPLEMENTED** rather than left aspirational.
- **`complete` / `rating` / `lang` on the tag page.** Three real bugs fixed
  (see below), and the fixture that had been hiding one of them rewritten.
- **PLAN and SPEC corrected** where they named commands and files that do not
  exist — `dump verify --in` is `verify --dir`, and all eight filenames in
  PLAN §6.1 are fiction.

## Still open, in the order I would do it

### 1. ~~SPEC §4.2 retention and §4.3 delta log~~ — DONE

`internal/dump/version.go`. `ForceInterval = 20`, `RetentionVersions = 3`.
`--version 0` now means next-after-newest (it always claimed to and always wrote
`v0`), `--full` forces a full snapshot, `--keep N` sets retention, and a delta
carries only the shards whose content hash changed.

Measured on the real 1.7 GB mirror, `--stable-salt`, unchanged corpus:

| | |
|---|---|
| v0 full | 6.7 MB, 256 shards |
| v1 delta against v0 | **614 B, 0 shards** |

**Three bugs, all of which passed every unit test**, because they were in the
writer rather than the reader:

1. **The delta never omitted anything.** The rule looked up
   `base.Shards["shard-042.json"]`; the map is keyed by shard NUMBER (`"42"`).
   Every lookup missed, so every shard was written. The snapshot verified, called
   itself a delta, printed "carries 256 of 256, consider --stable-salt" to an
   operator already using `--stable-salt`, and was the size of a full snapshot.
   Caught by running eight real dumps. No test could have caught it: the unit
   tests build manifests by hand and cannot see how `dump` writes one.
2. **`base_version` was `omitempty`**, so a delta against **v0** published no
   base at all — v0 being both "the first version" and "unset". A peer could not
   tell those apart in the signed bytes. A full snapshot now names `-1`, which is
   not a valid version, so "is this a delta" is decidable from the field alone.
3. **The manifest never recorded its own snapshot version.** It existed only as
   the directory name `v12`, which is not signed. `fetch` consequently reported
   "fetched snapshot **v1**" for every peer, and `Manifest.Version` — the
   *format* version — is 1 for every snapshot this build has ever written.

`internal/dump/delta_integration_test.go` exists because of the first one: it runs
the whole dump → delta → retain → verify cycle over a real corpus file, and it
builds its fixture from the queries in `dump.go` rather than an invented schema
(my first fixture used a `bookmarks` table and failed on "no such column", which
is the correct outcome for a fixture that does not match the code it feeds).

Retention refuses to delete a version a retained delta names as its base: such a
delta verifies its own signature perfectly and then cannot be applied by anyone
who did not already hold the base, so nothing downstream would ever report it.

**Rejected: delta-on-delta chaining.** A delta whose base is a delta is refused
and the next version is written full. Measured consequence: an unchanged corpus
alternates full/delta.

20 mutations over the retention/delta logic, all killed, all verdicts verified to
build first — two were aimed at the wrong expression and two did not compile, so
counting them would have been flattering the suite.

### 2. ~~SPEC §4.4 row 3 — the `go list -deps` assertion~~ — DONE
**Implemented 2026-10-05** (`internal/api/linkgraph_test.go`), mutation-verified
3/3 by adding each forbidden import and confirming the assertion fires.

The assertion the spec implies is FALSE as written. "The serving binary's
request path links no HTTP client" cannot be tested, because `internal/api` *is*
an HTTP server and imports `net/http` to do its job — a gate forbidding that
would be forbidding the product. So the claim became REACHABILITY:

| Path | may link `net/http` | must not link |
|---|---|---|
| `internal/api` (serves) | **yes** | `internal/onion`, `internal/crawl` |
| `internal/signal` (computes) | no | any outbound client package |
| `internal/onion` (fetches) | yes | a plain dialer |

`internal/crawl` really does fetch `archiveofourown.org` over clearnet. It is a
deliberate corpus-seeding tool, nothing imports it, and
`TestCrawlStaysUnreachableFromTheServingBinary` pins that. A gate that scanned
the whole repo for outbound clients would flag the crawler and be wrong — this
assertion is what makes the gate honest rather than satisfiable by deleting it.

A fourth test asserts the harness itself returns a non-empty dependency set with
the stdlib flag correct, because a harness returning an empty set for every
package would make the other three pass vacuously.


### 3b. ~~The doc gate was reading a stale binary~~ — DONE

`check-doc-commands.sh` asks "does the CLI know this command", so its verdict
depends entirely on which binary it asks. `bin/kindred` was three days old, and
the gate confidently reported `--full` and `--keep` as "flag provided but not
defined" — flags the tree defines, and flags it had just been shown.

It now exits 2 if any source file under `internal/` or `cmd/` is newer than the
binary. Three exits, three meanings: 0 clean, 1 the documents are wrong, 2 the
binary is stale and the verdict would describe a build that no longer exists.

That is the third stale-binary-gives-a-confident-wrong-answer instance here,
after a six-day-old deployed binary and a fixture corpus whose uniform ratings
made every filter test pass unconditionally. The rule: **a gate whose verdict
depends on a build artefact must check that the artefact is current.**

### 3. ~~A CI check that every command in the docs parses~~ — DONE

`scripts/check-doc-commands.sh`, exit 0 today. It found one more error than the
two I already knew about:

    SPEC 8.1   kindred ingest ao3  ->  kindred ingest --corpus <mirror>

That one is the interesting shape. The wrong form does not error on the bad
subcommand — the CLI reads `ingest`, treats `ao3` as a stray argument, and
fails with `--corpus is required`, which is an error about something else. So
the exit code says the command works, and only `kindred ingest -h` reveals
that `ingest ao3` is not a thing. The verdict is now made that way.

Five versions of the extractor failed before one worked, and three of them
reported SUCCESS while matching nothing at all:

  v1 matched prose anywhere    -> "43 of 43 broken", every one fake
  v2 grep -oE with \s          -> 0 matches, reported CLEAN
  v3 array read in a subshell  -> 0 matches, reported CLEAN
  v4 `local` in a subshell     -> 0 matches, reported CLEAN
  v5 `*"  "*` skip pattern     -> 21 real commands skipped, reported CLEAN

All three clean-looking failures were found by reading the output rather than
trusting it. The script now refuses to run if it cannot read the subcommand
list from the binary, and runs a self-test against a fixture it writes itself
before it says anything about the documents.

### 4. Docs-command drift, root cause — lower priority than it looks

The docs were written before the CLI settled, and the check in item 3 now
catches the drift. What the check cannot do is notice a command that exists and
is described wrongly — the verdict is "does the CLI know this invocation", not
"does the CLI do what the prose says". Fixing that means either generating the
command sections from `--help` or accepting the limit.

- [ ] decide: generate the CLI reference from `--help`, or document the limit
      in the script header (it is already stated there)

## Three bugs worth remembering

1. **`?rating=Z` returned Mature works.** The mapping had P/S/D/Z from other
   systems' age-rating tables. Those letters mean different things elsewhere. A
   filter that confidently answers a different question is worse than one that
   admits ignorance.

2. **"The tag has 0 works in total" whenever a filter matched nothing.** The
   message read `Tag.WorkCount`, which is the *filtered* count. Found by a
   Playwright assertion on the sentence — the Go test for that branch asserted
   the filters were NAMED and never looked at the number beside them.

3. **Work 1 listed twice, "of 29" over 28 distinct works.** `work_tags` is
   keyed on `(work_id, tag_id, tag_type)`. `internal/api` had a private copy of
   the corpus schema with the PK as `(work_id, tag_id)` — without `tag_type` —
   so the copy could not express the bug and every test passed. That private
   copy also lacked `users` and the `NOT NULL`s on `works.title`/`authors`,
   which is why three fixtures were writing rows production would reject.

## Two lessons about gates, from this session

**A gate that cannot discriminate produces false failures, and a false failure
gets a gate ignored.** The deploy gate twice reported working code as broken —
once by counting rendered cards against a 100-row page limit, once by picking
the "explicit" tag where all 42,796 works are Explicit so `rating=E` returns
everything and `rating=G` returns nothing. Both are *correct*. It now requires
a tag that discriminates on every axis it checks, and says which tag it chose.

**A fixture can remove a bug from the tests while leaving it in the product.**
The old `testcorpus` made every work "Explicit" with no language or completion,
so `rating=G` and `lang=English` each returned either everything or nothing —
and a missing filter produces the same two answers. Every filter test written
against it passed unconditionally.

## Current state

| | |
|---|---|
| `go build ./...` + `go vet` + `gofmt -l` | clean |
| `go test ./...` | 21 packages, exit 0 |
| Playwright | 69 passed |
| `python3 docs/goal-check.py` | all 7 clauses pass |
| Real corpus | 112,935 works / 6,261 users / 180,677 interactions, on thinkcentre |
| Deployed instance | thinkcentre `127.0.0.1:8010`, systemd **system** unit `kindred.service` |
| **Deployed binary** | **rebuilt from 11bcaed and deployed 2026-10-04, verified live** |
| Deploy-drift gate | `scripts/check-deploy.sh`, 32 behaviour checks, mutation-checked |
| Index age on the live instance | **127h — honest.** The index was built 2026-10-01, not by a stale binary |

### Deploy: DONE, and the drift that caused it is now gated

The live instance ran a Sep 29 binary for six days. It was up the whole time —
no `X-Kindred-Index-Age`, no `Server: kindred`, no sort controls, no dark mode.
Rebuilt, deployed with a pre-flight gate on a scratch port, and verified **on
the live wire** rather than in the repo. `scripts/check-deploy.sh` now runs 32
checks against any deployment URL and is mutation-checked by building the
pre-headers revision and watching it fail 20 of 22.

One number on the live instance is worth not misreading: the index age reads
**127 hours**. That is correct — the index was built 2026-10-01, and the header
reports the age of the *data*, not the age of the binary. A deploy does not
rebuild the index; `kindred ingest` does.

---

## Milestone status (measured)

| M | Scope | State |
|---|---|---|
| M0 | skeleton, memory gate | done |
| M1 | corpus read, ingest, CSR index | done |
| M2 | signals, embedder, ranking | done |
| M3 | HTTP API, unofficial AO3 surface | done |
| M4 | anonymised signed snapshots over Tor | **done, under different filenames than PLAN says** |
| M5 | budgets, deploy, parity | **partial — see below** |
| M6 | web UI | done |

M4's files are `internal/dump/dump.go` and `internal/onion/onion.go`, not the
six `anonymise.go`/`shard.go`/`manifest.go`/`sign.go`/`delta.go`/`retention.go`
PLAN §6.1 names, and the subcommand is `dumpcmd.go`. The *obligations* are met
— verified by running them, see below — but PLAN §6.1 is wrong and should be
corrected so the next reader is not misled.

### M4 verified for real, on the 1.7 GB mirror

PLAN §6.4 says "verify with the tool, not by eye". That had never been run
against real data. Now it has:

```
kindred dump --corpus <1.7GB mirror> --out /tmp/kdumps --k-anon 20
  tag_affinity rows : 4065 (k=20, salt=per-dump)
  shards            : 256
  public key        : 0dc13c64...
  real 19.1s / user 12.0s / sys 1.8s

kindred verify --dir /tmp/kdumps/v0
  manifest v1 verified (k=20, salt=per-dump, full=true)
  256 shards verified by content hash          exit 0

  same, after editing one float in shard-000.json:
  BAD  shard-000.json: hash f19a2ff6... does not match manifest 97fa8320...
  1 of 256 shards failed their hash             exit 1
```

The signature detects tampering and the exit code is non-zero. That is the
property the whole snapshot feature rests on, and it had never been observed.

Privacy, checked directly on the artefact because the named test does not
exist: shards contain exactly two keys, `p` (a salted pseudonym) and `w` (tag
weights). No user id, no username. 200 sampled usernames → 0 occurrences.
4,103 users are above k=20 and pseudonymised; 2,158 are below and absent.

**One scare worth recording.** A first leak-check reported 125 raw user ids in
the dump. All 125 were *shard keys* — `"149":{"hash":…,"path":"shard-149.json"}`
— colliding with user id 149 by coincidence, because shard numbers and user
ids are both small integers. A bare `grep -E "\b$id\b"` cannot tell a leak from
a collision. Anyone repeating this check must look at *where* the token
appears before reporting it.

---

## Open work, highest value first

### 1. ~~Deploy the current binary~~ — DONE

- [x] cross-compiled static binary (the deploy host has Go 1.22.2, the module
      needs 1.27.1, so the build happens on the repo host)
- [x] pre-flight gate on a scratch port with its own state db — the live WAL is
      never opened by a second process
- [x] atomic replace (`mv`, not `cp`), `systemctl restart kindred`
- [x] verified on the live wire: headers present, sort reorders, dark mode
      served, peak RSS 144 MB against the 220 MB cap

### 2b. ~~The behaviour gate could not see the BUILD~~ — DONE

`check-deploy.sh` makes 32 checks and every one is about behaviour. Behaviour
cannot distinguish "correct" from "correct, from six days ago", which is the
failure it was written to catch — and on 2026-10-05 it reported
"all 32 checks pass" against a binary from 2026-09-29 while the tree carried the
delta/retention work. It was green, and wrong in the most expensive way
available.

`scripts/check-provenance.sh` asks instead whether the deployed binary was built
from HEAD. It reads the commit out of the binary's own build metadata.

**It compares COMMITS, not binary hashes, after getting that wrong three times.**
Hashing the live binary against a local build cannot work:

- `go build` stamps the binary with the vcs revision, so **committing changes the
  bytes without changing a line of source**, and an uncommitted tree stamps
  `+dirty` — a different binary from identical source.
- `CGO_ENABLED` must match exactly. Without it the local build is a cgo build,
  45 KB larger, and can never match, so the gate was permanently red. **A gate
  that is always red gets ignored, which is worse than having no gate.**
- `git log -1 --format=%h` is 7 hex characters and the stamp is 12, so the
  equality test was false on a *perfect* deployment. It reported DRIFTED for a
  binary installed from HEAD minutes earlier. The previous commit claimed this
  gate was "verified in both directions"; the IN SYNC case had never run against
  a correct deployment.

`+dirty` is its own verdict: a binary built from uncommitted source is not
reproducible, cannot be rolled back, and cannot be reviewed.

The gate also deleted its own evidence on the way: it stripped the `+dirty`
suffix off the stamp and then asked whether the stripped result contained
`+dirty`. A real problem reported under the wrong name, in a gate meant to be
believed.

All three verdicts verified against the live host: dirty → DRIFTED, HEAD~1 →
DRIFTED naming the commit and the count, clean HEAD → IN SYNC.

`scripts/deploy.sh` builds, ships, verifies the copy's sha on the far side,
installs, restarts, waits for `/healthz` by observation rather than a sleep, and
then runs BOTH gates, failing if either does.

### 2. ~~A deploy-drift gate~~ — DONE

- [x] `scripts/check-deploy.sh`, 32 behaviour checks, exits 1 on drift / 2 on unreachable
- [x] mutation-checked by building `a95e224^` and serving it: **20 of 22 fail**

Run it after every deploy:
```bash
scripts/check-deploy.sh http://127.0.0.1:8010
```

### 3. ~~SPEC §4.4's headline privacy assertion~~ — DONE, and rows 3/4 marked NOT IMPLEMENTED

- [x] row 1 implemented as a source-level AST gate (a flag could not work:
      nothing serves snapshots, so the guarantee is about the codebase)
- [x] rows 3 and 4 marked **NOT IMPLEMENTED** in the spec rather than left
      aspirational
- [x] mutation-checked: a bare `net.Listen` fails, a bare `ServeSnapshot`
      fails, a compliant `ServeSnapshot` guarded by `IsOnionHost` passes

SPEC §4.4 states, as the mitigation for "peer learns my IP":

> `kindred dump verify --no-clearnet` asserts no non-onion route is registered

There is no `--no-clearnet` flag anywhere (`grep -rn 'no-clearnet' --include='*.go'`
→ nothing) and no test. What exists is `TestTransportHasNoPlainDialFallback`
in `internal/onion`, which is a different and weaker claim: it checks the
transport has no clearnet dialer, not that the *service* registers only an
onion listener. PLAN §6.2 names the stronger test as `TestNoClearnetRouteForDump`;
it does not exist under that name or any other.

- [x] decided: amend the spec to describe what is actually enforced, and
      implement it as a source gate
- [x] row 3 (`go list -deps` assertion that no HTTP client is linked into the
      serving path) is still genuinely missing — see item 9 below

### 9. ~~SPEC §4.4 row 3: the `go list -deps` assertion~~ — DONE, see item 2

Every row of §4.4 is now either implemented or marked NOT IMPLEMENTED with the
reason. Row 4 (the `KINDRED_DUMP_REQUIRE_ONION` check) remains unimplemented and
remains **unreachable**: nothing serves snapshots, so there is no onion service
to be silent about. Row 1's gate is written to constrain the serving path when
it appears.

The old text below is kept as the record of what was claimed:

### 10. ~~SPEC §4.2 retention and §4.3 delta log~~ — DONE, see item 1 above

- [x] `grep -rn 'retention|delta' internal/dump/` now finds `version.go`
- [x] a delta against an unchanged corpus measures 614 B against a 6.7 MB base

### 4. ~~`docs/PLAN.md` §6.4 documents commands that do not exist~~ — DONE

Both say `kindred dump verify --in <dir>`. The real command is
`kindred verify --dir <dir>` — `verify` is top-level, not a `dump` subcommand.

- [x] both documents corrected, with the observed output pasted in
- [x] PLAN §6.1's eight fictional filenames corrected, and the genuinely
      missing delta/retention called out rather than left implied
- [ ] **still worth doing:** a CI check that every `kindred …` command quoted
      in the docs actually parses. Two documents in a row named a command that
      does not exist, and the failure mode reads as "the feature is broken"

### 5. M5 — budgets and parity (partial)

- [x] ~~`make budget` cannot run on the repo host~~ — DONE, and it was measuring
      nothing

`scripts/budget-remote.sh` builds here, ships the binary and the gate, copies the
state db into a scratch directory, measures on thinkcentre, and brings the
verdict back. `make budget` picks whichever path is possible.

**The first version of it measured a server with no index and reported PASS.**

The index is a **separate file**: `kindred.db` (217 MB of SQLite) and
`kindred.db.graph` (28 MB, the mmapped CSR). The runner copied only the first, so
the booted server had no index beside it — it started, answered every route, and
peaked at **24 MiB**. `budget.sh` accepted the resulting 503 from tag-similarity
as *"a correct answer for a host that has not been ingested"*, so the gate
reported a pass at **47 MiB** against a 220 MiB cap.

The real number, with the index present:

    healthz (index mmapped at startup)        140 MiB
    recommend, wide pool n=50                 144 MiB
    ao3 works list                            159 MiB
    tag similarity (graph-backed)             159 MiB
    arena API (leaderboard + rank)            168 MiB   <- peak
    ------------------------------------------------
    168 MiB against a 220 MiB cap: 76% of budget, not 21%.

140 MiB of that is the index being mapped in, which is why `healthz` is already
the largest step. **SPEC §6.1 recorded `serve (full)` as 34 MB**, so the spec's
own table has been claiming a pass for a path the product does not run. Corrected,
with the per-route table and the reason.

Both layers now fail rather than pass: a scratch directory with no `.graph`
beside it is refused by the pre-flight (exit 2), and a 503 from tag-similarity is
a failure inside `budget.sh` (exit 1). Both verified by removing the copy and by
running `budget.sh` with no index at all.

Why this matters more than a wrong number: a memory gate that measures a server
*without its index* would pass a build that OOMs the moment the index is present,
which is the build anyone actually runs.

- [ ] `scripts/budget-pi.sh` is named in PLAN §7 and does not exist. `lite` mode's
      26 MiB is measured and correct, so the Pi question is answerable without a
      Pi — but the *host* is named, and a number from thinkcentre is not a number
      from a Pi 3 with 512 MB.
- [ ] arm64 run under `MemoryMax` on real hardware (SPEC §12)

### 5b. ~~M5: budgets and parity~~ — partially done; script still missing

`complete=`, `rating=`, `lang=` work in `/api/v1/ao3/works`. The tag page
accepts `?sort=` and `?words=` only. A reader who learns the filters from the
API docs cannot use them in the browser.

- [ ] add `complete` / `rating` / `lang` selects to the tag page controls
- [ ] the rating control must send the AO3 letters (`G`/`T`/`M`/`E`); the
      mirror stores full names and the mapping is `ao3RatingNames`

### 7. Unbuilt, genuinely worth it

From `IDEA-AUDIT.md`, in order:

**Every claim below re-verified against the tree on 2026-10-05, not carried
forward from the ideas list.** Two of the five were wrong about what exists.
Checking them turned up two defects that had nothing to do with the ideas, both
in the mode-list tests this item was never about (see item 8).

- [ ] **surprise me** (#22) — one random well-tagged seed. The engine is done;
      this is a route plus a template. Genuinely small, and it is the one idea
      whose whole value is that it needs no new logic.
- [ ] **CSV export** (#20) — **confirmed absent.** No `?format=` anywhere; grep
      for it turns up only `fmt.Sprintf` and one prose string.
- [ ] **author page** (#25) — **confirmed absent.** The route table is 17
      routes and none is a user route. `users` exists in the corpus and the
      arena ranks readers, so there is per-reader data to show — but §4.2 drops
      the user tables from snapshots, which is a different surface and does not
      license this.
- [x] ~~**index version in the footer** (#85)~~ — **the claim was wrong: the
      footer already renders the index age**, in three states including "the age
      of this mirror is not recorded", and `X-Kindred-Index-Version` is already on
      every response. What genuinely does not exist is an index *identity*:
      `store` writes no `index_version`, and `Base` carries `IndexBuiltAt` and
      `CorpusBuiltAt` but no build id. Age answers "how stale", not "which
      build". Folded into item 8 below rather than done.
- [ ] **tag autocomplete** (#4) — *not* the "10 minutes, endpoint exists" the
      ideas list claims. SPEC §1.1 forbids JS and a `<datalist>` needs either
      static options (useless at 100k+ tags) or JS (forbidden), so this is a
      design decision before it is code: the honest no-JS answer is a
      "browse popular tags" page, which may already exist.

**Removed from this list:** `SPEC §4.4 row 3` was here twice, once as an open
item and once as a superseded quote. Both are gone; the work is done and recorded
at item 2. A superseded quote with live checkboxes in it is how a tracker
reports work that does not exist.

**The next four are all one route plus one template, and none is started.** They
are listed in the order the ideas file does, not by value. "Surprise me" is the
only one whose value needs no new logic at all: the engine is done, so a random
well-tagged seed is a `RANDOM()` over works with tags, and the corpus layer has
no `RANDOM()` anywhere yet — so it is one new query, not one new page. Author page
is the largest (17 routes, no user route exists, and there is per-reader data to
show).

### 8. Deliberately out of scope

Every idea whose mechanism is JavaScript, because SPEC §1.1 forbids it and
that is the project's selling point: **#11** j/k navigation, **#35/#51/#74/
#92/#99** session-local state, **#88** PWA, **#79** browser extension.

Not "hard", not "later" — impossible as written.

---

## Gates that must pass before every commit

```bash
gofmt -l .                                   # must print nothing
go build ./... && go vet ./...               # must be silent
go test ./... -count=1                       # 21 packages, exit 0
python3 docs/goal-check.py                   # all 7 clauses
cd e2e && CI=1 npx playwright test           # 69 passed
```

And for anything added: **mutation-check it**. A gate that cannot be made to
fail is decoration. Several in this repo have been:

- `make budget` passing is impossible without a corpus, so it always "passes"
  vacuously on the repo host
- the tune-table CSS rule is asserted but its *rendering* is unreachable: the
  e2e corpus (~40 works) produces no recommendations, so the `<details>` is
  never emitted
- two of eight CSS mutations initially "passed" because the edit never
  applied — an anchor assert checked the input, not the result

---

## Ops notes

- **thinkcentre** holds the corpus at `/home/alvaro/kindling-data/ao3_metadata.db`
  and the state DB at `/var/lib/kindred/kindred.db`. Service listens on
  `127.0.0.1:8010` only — not reachable off-host, by design.
- **The repo has no checkout on thinkcentre.** Verification there means
  `scp` the binary plus a script file. Do not `scp` into a git working tree;
  commit scripts instead, or an untracked file blocks the next fast-forward.
- zsh on thinkcentre glob-expands pipes inside `ssh "..."`, so a `grep -cE`
  with alternation returns "no matches found" and the next command reports 127
  for something that never ran. **Put greps in a remote script file.**
- `systemd` units are **system** units under `/etc/systemd/system/`, needing
  root; not `--user`.
- two remotes: `forgejo` (14 behind) and `github` (12 behind). Push both.

### 8. Two tests that could not fail — FOUND AND FIXED, recorded so the shape is recognisable

Found while re-verifying item 7's claims, and in a file item 7 has nothing to do
with. Both are the same defect, and the second is the subtler one.

**`TestEveryWorkingModeHasARunnerMethod` iterated a map literal of `true`:**

```go
for m, hasMethod := range map[Mode]bool{
    ModeFandomRanking: true, ModeTagNeighbours: true, ModeUnderrated: true,
} {
    if !hasMethod { t.Errorf("mode %q is in Modes() but has no Runner method", m) }
}
```

Every value is `true`, so the failure branch is unreachable. It read like a guard
on the mode list and guarded nothing.

**`TestDeclaredButUnimplementedModesAreHonestAboutIt` compared a function with its
own definition.** `IsImplemented(m)` is *defined* as membership in `Modes()`, so
asserting the two agree cannot fail for any edit to `Modes()`.

The survivor, found by mutation and not by reading:

| mutation | before | after |
|---|---|---|
| `ModeUnderrated` dropped from `Modes()` | **13 tests PASS** | FAIL |
| `TagNeighbours` dropped from `Modes()` | pass | FAIL |
| a mode advertised with no method behind it | pass | FAIL |
| a `Runner` method present but unadvertised | pass | FAIL |

Nothing else in the tree noticed either, because the `Runner` method still exists
and still works, so Go compiles. The consequence is a *user-visible* one: the
CLI's `corpus-query: mode %q is declared but not implemented; working modes: %s`
error silently stops naming a mode the product runs, so anyone who typos into
that error, or reads `--help`, is told `underrated` does not exist.

Both tests now check the invariant against the **runtime type** via `reflect`
rather than against a hand-written list. The mode value is kebab-case
(`fandom-ranking`) and the Go identifier CamelCase (`ModeFandomRanking`), and
reflect cannot derive one from the other — so the mapping is one explicit map
(`modeMethods`), not a mangling rule that would hold only by accident.

**Also removed:** `internal/corpusquery/corpusquery_test.go.backup2`, tracked since
`ecd3f3f`, and its `.bak` twin. Identical copies of a test file that no longer
exists; `go test` ignores them because they are not `*.go`, so nothing complained.
`.bak` was invisible because the **user's global** gitignore covers `*.bak` while
`.backup2` was matched by nothing — which is why one was committed and the other
was not. `.gitignore` now carries the rule, so the next harness run cannot do it
again silently.
