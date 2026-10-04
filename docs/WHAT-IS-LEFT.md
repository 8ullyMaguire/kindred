# kindred — what is left

**Created 2026-10-04.** Written after measuring the tree, not after reading
the plan. `docs/PLAN.md` says "Status: 2026-09-29, written before any code"
and never says which of M0–M6 shipped, so every row below was checked against
the filesystem, `go test`, and the running instance.

Rule for this file: a row moves to DONE only with the command that proves it
and the output that was observed. A row that says "shipped" because a plan
says so is not done.

---

## Current state

| | |
|---|---|
| `go build ./...` + `go vet` + `gofmt -l` | clean |
| `go test ./...` | 21 packages, exit 0 |
| Playwright | 69 passed |
| `python3 docs/goal-check.py` | all 7 clauses pass |
| Real corpus | 112,935 works / 6,261 users / 180,677 interactions, on thinkcentre |
| Deployed instance | thinkcentre `127.0.0.1:8010`, systemd **system** unit `kindred.service` |
| **Deployed binary is from 2026-09-29** | **six days stale — 14 commits undeployed** |

### Deploy is the single largest gap

The live instance runs a Sep 29 binary. Verified absent from the live wire:

- no `X-Kindred-Index-Age` / `-Version` headers (SPEC §3.2.2 requires them on
  *every* response)
- no `Server: kindred`
- no sort/length controls on `/tag/{id}`
- no dark mode in the served stylesheet

Every gate this project has is green **against the repo, not against the
running service**. Nothing in the test suite checks the deployed binary, so
this drift was invisible.

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

### 1. Deploy the current binary — everything else is invisible until this

- [ ] rebuild for the deploy host, replace `/usr/local/bin/kindred`
- [ ] `systemctl restart kindred`, confirm `active (running)`
- [ ] verify **on the live wire**, not in the repo:
      `X-Kindred-Index-Age`, `Server: kindred`, `/tag/29?sort=recent` reorders,
      `prefers-color-scheme` present in the served CSS
- [ ] add a gate so this cannot drift again — see item 2

### 2. A deploy-drift gate (the missing check that let item 1 happen)

- [ ] a test that asserts the running instance's headers, from outside the
      repo. Nothing currently compares the deployed binary to HEAD.

### 3. SPEC §4.4's headline privacy assertion has no implementation

SPEC §4.4 states, as the mitigation for "peer learns my IP":

> `kindred dump verify --no-clearnet` asserts no non-onion route is registered

There is no `--no-clearnet` flag anywhere (`grep -rn 'no-clearnet' --include='*.go'`
→ nothing) and no test. What exists is `TestTransportHasNoPlainDialFallback`
in `internal/onion`, which is a different and weaker claim: it checks the
transport has no clearnet dialer, not that the *service* registers only an
onion listener. PLAN §6.2 names the stronger test as `TestNoClearnetRouteForDump`;
it does not exist under that name or any other.

- [ ] decide: implement `dump verify --no-clearnet`, or amend SPEC §4.4 to
      describe the guarantee that is actually tested
- [ ] whichever it is, the test must be mutation-checked

### 4. `docs/PLAN.md` §6.4 and `docs/SPEC.md` §4.4 document commands that do not exist

Both say `kindred dump verify --in <dir>`. The real command is
`kindred verify --dir <dir>` — `verify` is top-level, not a `dump` subcommand.

- [ ] correct both documents, or add a `dump verify` alias
- [ ] this is the second time a doc's command was wrong in a way that made the
      feature look broken; worth a CI check that every command in the docs
      parses

### 5. M5 — budgets and parity

- [ ] `make budget` cannot run on the repo host (it refuses without a corpus);
      it should run against the thinkcentre mirror. The measurement was done
      by hand instead: 0.06 s / 6.2 MB for the works search, 144 MB peak RSS
      against a 220 MB cap.
- [ ] `scripts/budget-pi.sh` is named in PLAN §7 and does not exist
- [ ] arm64 run under `MemoryMax` on real hardware (SPEC §12)

### 6. Filters in the API but not on the pages

`complete=`, `rating=`, `lang=` work in `/api/v1/ao3/works`. The tag page
accepts `?sort=` and `?words=` only. A reader who learns the filters from the
API docs cannot use them in the browser.

- [ ] add `complete` / `rating` / `lang` selects to the tag page controls
- [ ] the rating control must send the AO3 letters (`G`/`T`/`M`/`E`); the
      mirror stores full names and the mapping is `ao3RatingNames`

### 7. Unbuilt, genuinely worth it

From `IDEA-AUDIT.md`, in order:

- [ ] **surprise me** (#22) — one random well-tagged seed; engine is done
- [ ] **CSV export** (#20) — `?format=` does not exist at all
- [ ] **author page** (#25) — verify `/v1/users/{username}/works` exists first
- [ ] **index version in the footer** (#85) — `/stats` does not render it either
- [ ] **tag autocomplete** (#4) — *not* the "10 minutes, endpoint exists" the
      ideas list claims. SPEC §1.1 forbids JS; a `<datalist>` needs static
      options or JS, so this is a design decision before it is code

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
