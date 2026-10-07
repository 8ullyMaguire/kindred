# Kindred — everything to do

Single source of truth for the full request list. Check items off as they land.

## 0. Current blocker (do first)

- [ ] `go build ./...` is BROKEN (pre-existing, not caused by recent edits):
  - `handlers.go:469` — `FandomsPage.Gate` is `string`, handler passes `int` gate.
  - `handlers.go:499` — `FandomsPage.Rows` is `[]rank.Candidate`, handler assigns `[]corpusquery.Row`.
  - `handlers.go:550` — `UnderratedPage` lacks `Rows`, `Notes`, `Truncated`, `MinWords`, `Complete`
    (templates `underrated.html` already use them).
  - `handlers.go:595/613/614` — `SurprisePage` lacks `Notes`, `Row`, `Seed`.
  - Fix direction: the HANDLERS and TEMPLATES agree with each other; only the struct
    definitions are stale. Update the structs (Gate → int, Rows → []corpusquery.Row,
    add the missing fields), do not butcher the handlers.
  - Never restore `handlers.go.bak` blindly again — it does not compile either.
- [ ] Verify with `go build ./... && go test ./...`, then deploy via `./scripts/deploy.sh`
  (the ONLY deployment path; report failures honestly).

## 1. Recommendations: ?n= everywhere

Goal: `?n=` lets the reader ask for more results on every page that has a limited
list. Default stays at least 20 on tag pages. State lives in the URL only.

- [ ] Tag pages `/tag/{id}` — `?n=` (already parsed, clamp 1..100 → raise cap, keep
      default 20, ensure "Showing the first N. Raise ?n= for more." is accurate).
- [ ] Work pages — `?n=` for the "more like this" / recommendations block on `/work/{id}`.
- [ ] Recommend page `/recommend` — confirm `?n=` works (test uses `n=20`), raise cap.
- [ ] Fandoms `/fandoms` — `?n=` already parsed (1..500); make sure UI exposes it.
- [ ] Underrated `/underrated` — `?n=` already parsed; expose in UI.
- [ ] Search `/search` — `?n=` for result count (currently fixed limit).
- [ ] Neighbours `/neighbours` — `?n=` for the neighbours list.
- [ ] Author pages (see §3) — `?n=` for the author's works list.
- [ ] Every page that truncates must say so and link to the same URL with a bigger `?n=`.

## 2. Tag page taste recommendations (the original ask)

When visiting a tag page the reader currently only sees works WITH that exact tag.
They want works appreciated by people who would like that tag even without it,
with tagged works boosted.

- [ ] Default view: at least 20 works, `?n=` picks the count.
- [ ] Collaborative approach (already sketched in this session, re-apply it —
      it was lost in a backup restore):
  - Take top ~5 works by kudos for the tag as seeds.
  - `engine.Recommend` with those seeds → neighbours from CF + embeddings.
  - Boost works that actually carry the tag (e.g. ×2 score) before sorting.
  - Apply existing filters (words/complete/rating/lang) as post-filters.
  - Render through the normal work-card template with evidence text.
- [ ] Keep `sort=taste` as default on tag pages; `?n=` works together with it.
- [ ] Honest evidence line on every card (taste overlap %, bookmark counts).

## 3. Search improvements

- [ ] Search by work URL: paste `https://archiveofourown.org/works/12345` → hit.
- [ ] Search by bare numeric ID: `12345` → that work.
- [ ] Search by AO3 work URL also accepts `/works/12345/...` trailing segments
      and `?view_adult=true` noise.
- [ ] Title+author combined query in one box: `harry potter snape` matches
      title AND author (already partially works — verify and fix ranking so a
      title+author match outranks a title-only match).
- [ ] Author name search should surface an author link (see §4), not just works.
- [ ] URL/ID hits should appear at the top of results with a clear label
      ("exact work match").

## 4. Author pages

- [ ] Every work card author name links to `/author/{author-slug}` (or an
      equivalent URL-parameter route) showing ALL works by that author.
- [ ] Author page: name, work count, list of works using the standard work card,
      sorted by kudos by default, `?sort=` and `?n=` supported.
- [ ] Multiple authors on a work: each name gets its own link.
- [ ] Works table stores authors as a string — parse with the existing split
      convention (`- ` and `(` separators, comma fallback), build a reverse map,
      no schema change unless unavoidable.
- [ ] No-JS: links are plain `<a href>`, pages render server-side.

## 5. Auto-crawl AO3 (metadata improves over time)

Kindred should crawl AO3 BY DEFAULT whenever it runs, so metadata keeps growing.

- [ ] `kindred serve` starts a background crawler automatically (opt-out flag,
      e.g. `--no-crawl`).
- [ ] Checkpoint/resume: crawl state persisted (per-work granularity):
  - skip works whose metadata already exists in the DB,
  - a checkpoint file records position/queue so restarts continue where they left off,
  - different checkpoints per work kind if needed (works vs bookmarks vs series),
  - interrupted crawl resumes, never restarts from zero.
- [ ] Input-driven: seed queue from whatever the instance uses (existing works,
      bookmarked works, BFS over tag graph — richest-seed-first ordering like the
      current 24h BFS run, partial runs must still buy the most metadata).
- [ ] Politeness: AO3-appropriate rate limiting, retries with backoff, respect 429/503.
- [ ] Skip-on-hit: metadata presence check BEFORE fetching (works, tags, authors,
      summaries, kudos/hits/bookmarks, series).
- [ ] After crawl: rebuild/refresh the index so recommendations get better
      automatically (or schedule it) — and the footer "index built" timestamp
      reflects it.
- [ ] Log progress to a file, judge by DB growth not the log (lesson from the
      current BFS run).

## 6. Recommendation engine quality (spec §2.5 + §3 changes — partially applied)

- [ ] De-biased collaborative filtering: normalized cosine similarity
      `score(A,B) = co-bookmarks / sqrt(bookmarks_A * bookmarks_B)` instead of raw
      co-occurrence (SPEC updated; verify `internal/collab` actually computes this
      and `internal/engine` uses it).
- [ ] Candidates scored against the active profile/seed set, not a global
      popularity pool.
- [ ] Popularity dampening: `FinalScore = PersonalizedSignals / GlobalKudos^K`,
      K default 0.4, tunable per profile.
- [ ] UI: default sort renamed to "Taste Match" (done in template; re-verify after
      struct fixes), applied on `/recommend` and profile pages too.
- [ ] UI: popularity slider `[ Niche Gems ───•─── Global Hits ]` in the filter
      sidebar (template written; wire the actual parameter through the handler).
- [ ] Evidence string: "Highly aligned with your reader cohort (94% taste overlap,
      12 bookmarks)" style (template written; verify it receives the fields).
- [ ] Default blocked tags list (M/M, Slash, Gay, Romance, Hurt/Comfort, Fluff,
      Angst, Slow Burn, RPF, Real Person Fiction, Reader, Omega, Needs a Hug,
      Bestiality) — stored in config, shown in UI, on by default.

## 7. General quality pass ("made by a small LLM, make it as good as possible")

- [ ] Every handler: consistent `?n=`, `?sort=` handling; clamp ranges documented.
- [ ] Error pages: no raw internal errors leaked, friendly messages.
- [ ] Empty states: every list page explains WHY it is empty.
- [ ] Templates: fix stale struct/field mismatches (that is what broke the build);
      add template-level smoke tests so a renamed struct field fails `go test`
      instead of deploy.
- [ ] Run `go vet ./... && go test ./...` green, then e2e suite.
- [ ] Accessibility/no-JS audit: every action reachable with plain links/forms.
- [ ] Remove dead code, duplicated helpers, leftover `.bak` files after green build.

## 8. Hard constraints (never violate)

- [ ] Works fully with JavaScript disabled; all state in the URL (no cookies,
      no localStorage beyond arena sessions).
- [ ] Deploy ONLY via `./scripts/deploy.sh`; verify after deploy.
- [ ] No credentials/tokens/passwords in any file — `[REDACTED]` if ever needed;
      credentials come from the private env dotfile via shell only.
- [ ] Sanitized filenames everywhere: lowercase kebab-case ASCII, no spaces or
      em dashes (applies to this repo's docs too).
- [ ] Minimum 20 works shown per tag page by default.
- [ ] Evidence must mention taste overlap percentages and bookmark counts.

## Definition of done

`go build ./... && go test ./...` green, deployed with `scripts/deploy.sh`,
then manual verification of: tag page with `?n=50`, search by URL and by ID,
author link → author page, `?n=` on work/fandoms/underrated/search pages,
crawler resumed from a checkpoint skipping existing metadata.
