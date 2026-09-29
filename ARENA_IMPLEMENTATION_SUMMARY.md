# Arena — state of the implementation

Verified on thinkcentre against the real 1.7 GB mirror, and live at
`https://kindred.polarisocial.xyz`. Commits `8052070` (UI) and `22b6d29` (batch).

## What exists

| surface | route | notes |
|---|---|---|
| pair + judge | `GET /arena`, `POST /arena/judge` | radio form, no JS, 303 to next pair |
| standings | `GET /leaderboard`, `GET /api/v1/arena/leaderboard` | rating, RD, W/L/D |
| one work | `GET /rank/<id>`, `GET /api/v1/arena/rank/{id}` | rating, tally, period history |
| your profile | `GET /my-ranking`, `GET /api/v1/arena/my-ranking` | liked / disliked tags |
| rating period | `POST /api/v1/arena/batch` | the only writer of ratings |

Engine (`internal/arena`, unchanged and well tested): Glicko-2 pinned to the
paper's iteration table, four pairing strategies (maxinfo, maxuncertainty,
maxexpected, explore), batch update, idle decay, rarity-weighted tag learning.

Store: five tables — comparisons, ratings, rating history, tag weights, sessions.

## Verified behaviour

Judged 5 comparisons over 4 works, ran a batch, and the leaderboard came back
with real ratings: top work 1685.5 ±155.2 on a 3-1-1 record. A second batch
with nothing new correctly skips rather than inventing a period.

Full budget gate on the real corpus: **37 MiB against a 220 MiB cap, PASS**,
with the arena steps walked. Live service after 25 arena page hits: peak RSS
181 MiB of 220, zero panics, zero restarts.

## The bugs, and why the suite was green through all of them

Four defects, all at the boundary between a correct computation and the thing
it talks to. `internal/arena`'s arithmetic was right throughout and its tests
passed through every one.

1. **`comments` column** — the candidate pool ordered by a column that exists
   in neither the real mirror nor the test fixture. The comment claimed it was
   "the field that is actually populated". `hits` is.
2. **`placeholders[:len(allTags)]`** — the tag-name query sliced the *work-ID*
   placeholder list to the tag count. Two works, 78 tags, a two-element slice:
   panic on every arena request.
3. **`RatingRow` as a query arg** — the batch passed a struct where nine
   scalars were expected, so every period 500'd and no rating was ever written.
   The store's own `SaveRating` does the same upsert correctly, so this was a
   second, wrong copy of working SQL in the API layer.
4. **period number read as a timestamp** — `arena_last_period` was written as
   `1, 2, 3…` and read back as `time.Unix(1, 0)` = 1970-01-01T00:00:01Z, so
   every batch re-folded the entire history. The bound was also inclusive, and
   `judged_at` has one-second resolution, so same-second judgements were
   double-counted too.

Each fix is proven by mutation: restoring the bug makes the specific test fail
with the original error, and the source is restored in the same process.

## Known gaps

- `routes.txt` is **dead**. It claims to be the gate's source of truth and
  nothing reads it; `scripts/budget.sh` has its own hardcoded list. Kept as a
  review checklist, with a header saying so. The gate is the script.
- The arena signal is **not** wired into the recommender. `internal/signal`
  has no arena term, so learned preferences do not yet affect `/recommend`.
  Deliberate, and the one open product decision here.
- The batch is manual. Nothing schedules `POST /api/v1/arena/batch`; it runs
  when something calls it.
- No browser-driven test. Coverage is HTTP-level via `internal/api`'s fixture.
- `internal/web` still has no test file of its own; page tests live in
  `internal/api` because that is where the fixture is.

See `HANDOFF.md` for the per-file change list and the extend-it notes.
