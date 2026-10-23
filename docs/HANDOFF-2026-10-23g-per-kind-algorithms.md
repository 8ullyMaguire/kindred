# HANDOFF 2026-10-23g — per-kind algorithms, and the live A/B

Commit range: `8161b37` → `027e8fd` → (this doc).
Deployed: provenance IN SYNC, behaviour **73/73**, 27/27 packages green.

## What shipped

**The mechanism is now choosable per kind.** Until today "algorithm" meant one
of two things, both works-only: a TUNE (a named weight vector, compared in
`algo_comparisons`) or the pool mode (`tags+collab+embed`). A tune is one
mechanism's knobs. This adds the question above it — for a seed of kind K, WHICH
mechanism produces the list — and lets the reader answer it.

| kind | live algorithms | default |
|---|---|---|
| works | hybrid, collab, adamic, popular | hybrid |
| tags | embed, cooccur, popular | embed |
| characters / relationships / fandoms | same as tags | embed |
| authors | cobook, tagidf, popular | popular→cobook |

Characters, ships and fandoms share the tag mechanisms deliberately: 3,890,504
of the mirror's 3,891,300 `work_tags` rows say `freeforms`, so a per-`tag_type`
mechanism is the same query behind a filter that matches nothing. That dead
column has now cost two features; it is named in the code at both call sites.

### The disabled set is published, with reasons

Eight algorithms from the reading list are listed and unrunnable, each with the
reason and, where one exists, the measurement:

ppr, lightgcn, als, tagmf, work2vec, fm, userknn, liftrules.

`TagIDF` and `BFS2` are disabled **for works** with a different reason: alone
they are tunes, not algorithms, and `/algo` is where weight vectors are
compared. `TagIDF` is live for authors, where no blend exists to put it in.

The rule applied: an algorithm ships only if one request can produce a list from
the data on disk, with a bounded cost, on this host. Everything training-based
fails that rule, and a page that omits them tells a reader nothing.

### Live A/B

- Arm = `fnv64a("kind|owner") % 2` → default or challenger. Stable per reader
  across requests and deploys; a counter would reassign readers mid-experiment.
- Challenger = the first live non-default in registry order.
- No session and no challenger both mean "no experiment to be in": the default.
- Every served list writes `algo_impressions(owner, kind, algo, arm, seed,
  shown, shown_at)`.
- `arm` is recorded, not reconstructed: `default` / `challenger` / `choice` /
  `request`.
- A read credits the newest impression of that kind whose list **contained** that
  work, once. Crediting "the reader's most recent request" would pay an algorithm
  for a click that came from search.

Measured live: 10 fresh readers on the works kind split **5 hybrid / 5 collab**;
`arm_readers` per kind: `ao3_work/default 5`, `ao3_work/challenger 5`,
`ao3_tag/default 7`, `ao3_tag/challenger 3`.

## Two defects the live walk found (both green in every test)

**1. `seen_works` stores `ao3_work:<id>`, and three queries parsed a bare int.**
`CAST('ao3_work:3' AS INTEGER)` is 0, so #65's re-offer exclusion excluded
nothing — it was inert on the deployment while looking correct in tests, because
the fixture inserted bare numbers. My own live verification inserted a bare id
too, which proved nothing about production rows. Fixed with `store.WorkEntityID`
and `store.EntityNumericID`; the fixture now writes the production shape.

**2. Adamic-Adar answered "no shared readers" for a work with thousands.**
Same cause: `entity_id = '11623035'` matched no row. After the fix:
`11623035` returns candidates at score 2.13, "4 shared reader(s), weighted
1/(1+log(their bookmarks))".

## Where the implementations live

`internal/algo` — registry, statuses, reasons, default per kind, arm assignment.
`internal/engine/related.go` — one function per (kind, algorithm).
`internal/store/algochoice.go` — choices, impressions, credits, standings.
Routes: `GET /api/v1/algo/kinds`, `POST /api/v1/algo/choice`,
`GET /api/v1/related`; pages `/choose`, `/related`.

`GET /api/v1/related` mints a session, unlike every other GET in the API: a list
served to an unassigned reader is not in the experiment.

## Explicitly NOT built

- No offline model is trained anywhere. `ppr`, `als`, `lightgcn`, `tagmf`,
  `work2vec`, `fm`, `userknn`, `liftrules` have no code, only a registry entry
  with a reason.
- The Adamic walk caps at the 4,000 most recent readers of a seed and says so.
- `cobook` walks the seed author's 200 most-read works, then 400 of the
  resulting works, and says so.
- No per-kind challenger rotation: one challenger per kind (the strongest live
  alternative in registry order), because every extra arm divides the evidence.
