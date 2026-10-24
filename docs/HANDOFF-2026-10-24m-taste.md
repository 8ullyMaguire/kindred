# Handoff — #36 session taste export/import, and the owner key that hashed nobody

2026-10-24. Two rows: #48's gate became a command (it still says no), and #36 shipped the
taste vector's way out of the database.

## #48 — the refusal, mechanised (`d644992`)

The row asks for a reader-velocity signal. A signal needs a weight, a weight needs a
distribution. Re-measured on the live state database: **8 owners, 11 marked works, 8
reader-days, max 3 in one day.** There is nothing there to fit.

The row's own threshold is now code (`internal/store/velocity.go`), so the next session does
not have to re-derive it from a markdown sentence:

* ≥20% of owners with ≥5 reader-days (thickness), and
* p90 ≥ 2× p50 marks/reader-day (spread) — under a factor of two, a velocity weight moves every
  reader by about the same amount, which is a constant wearing a signal's name.

`kindred velocity` prints the distribution, the percentiles, the fastest owners and a verdict
that names **the count that would clear it**:

```
GATE NOT MET: 0 of 8 owners have 5+ reader-days (need 2); the gate fails on THICKNESS --
2 more owner(s) with 5 days would clear it
```

Five mutants killed, including "the gate always returns true" and a percentile off by one (the
pass fixture pins p50 = 1.2 and p90 = 4.8 to its own arithmetic, because `p90 > p50` survives an
off-by-one and ships a distribution no reader has).

## #36 — the vector can leave

    GET /api/v1/taste          the caller's vector: id, NAME, weight, evidence n
    PUT /api/v1/taste          import; MERGE by default, ?replace=1 to replace

Decisions, each with a reason a reader could argue with:

* **The name travels with the id.** "tag 41" is not something a reader can argue with, and a
  name survives a re-ingest while an id does not. The corpus resolves names → ids
  (`TagIDsByName`), taking the LOWEST id when one name is held under two tag types — which the
  mirror really does (the fixture reproduces it: `tag 1` is a fandom and a freeform).
* **The evidence count travels with the weight.** `session_taste` divides by √n. A weight of 0.8
  corroborated once is not the same claim as 0.8 corroborated forty times, so an import that
  dropped `n` would silently inflate every imported tag.
* **Merge is the default.** Replacing on every import means a partial file deletes forty
  comparisons the reader made and never mentioned. `?replace=1` has to be asked for, and the
  response reports `before`, `after` and `replaced` so the caller is told what it overwrote.
* **An unknown tag NAME refuses the whole import** (422, naming the tag) rather than being
  skipped. A portable file that partially lands is worse than one that is refused.
* **Everything is validated before anything is written**, and there is no way to address
  another reader's vector: the owner is the arena cookie, hashed.

## The defect the import found

`Store.OwnerKey(ctx, "")` **hashed the empty string into a real owner key.** HMAC(salt, "") is a
perfectly good hex string, so every anonymous caller shared one owner key — "nobody" had a
vector, and any weights ever written under it would have applied to every visitor.

Every call site was already written as `if key, err := s.OwnerKey(...); err == nil`, which is
correct only if the function refuses. It now returns `ErrNoIdentifier` for an empty identifier,
which fixes the class at the source rather than in the two new handlers.

`GET /api/v1/taste` with no session is 401 and was 200 before this — which is how the defect
announced itself: the test asking for 401 is what noticed.

## Verification

* `go test ./...` — 30 packages, 0 failures (the api package takes 83 s; a 420 s ssh call is not
  enough for the full suite plus a build, so run it locally).
* 6 mutants, 6 killed: replace ignored; unknown name skipped; evidence count dropped; empty
  identifier hashed; unknown JSON fields allowed; (plus the gate's five).
* `scripts/check-deploy.sh` gained three #36 checks: GET 401, PUT 401, and an anonymous export
  that must name no weight. The round trip is the api suite's, where a cookie exists.
* The route audit (`routes_audit_test.go`) refused both new routes until they were accounted
  for — that test caught this before I did. Neither gets a page: the vector belongs on the
  arena's own "what I have learned" panel, and a standalone page would be a page about a table.

## What #36 did NOT do

No UI. The arena shows a reader their vector nowhere yet, so the export is API-only; the panel
in `web/` is the natural home and is not written. Import by id is accepted for a caller coming
from a different mirror, but there is no migration between two mirrors' id spaces — the name
path is the portable one, and the handoff says so instead of pretending otherwise.
