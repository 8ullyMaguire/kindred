# HANDOFF 2026-10-23e — the mirror is subscribable (#61, #62)

Committed `fc21e12` (feeds + descriptor + discovery), `8eea88a` (the descriptor
advertises only what the server answers), `e06b92d` (the seed feed's own seed,
and an absent date is an absent element). Deployed: provenance IN SYNC,
behaviour 48/48.

## What shipped

    GET /feed/tag/{id}          works under a tag, newest first, RSS 2.0
    GET /feed/seed/{kind}/{id}  the ranking for one seed work
    GET /opensearch.xml         the browser search-engine descriptor
    /tag/{id}                   rel=alternate discovery + "Follow this tag"

All three are served by the API mux, and `guardAPI` now sends `/feed/` and
`/opensearch.xml` there: before that they fell through to the PAGE router, which
answered a working feed with an HTML 404.

## The decisions worth keeping

**Ordering.** `corpus.ChronologicalUpdateDateDesc`, never `update_date DESC`. The
mirror stores AO3's "31 Oct 2025", so a string sort compares the day of month
first: on the live corpus it names 31 Oct 2025 the newest work while the true
newest is 26 Jun 2026. The fixture is a MIX of that form and ISO on purpose — a
pure-ISO fixture validates whichever order it was written for.

**Absolute links, derived per request.** RSS requires them, and this deployment
has no configured public URL (it sits behind a tunnel). `X-Forwarded-Proto` /
`X-Forwarded-Host`, falling back to the request's own scheme and `Host`. A
relative guid is invalid: clients mint their own identity for it, so every
refresh looks like new works.

**A feed entry is dated by the WORK.** The tag feed is therefore honest about
the only question a subscriber has. The seed feed recomputes per request and
says so in its channel description: inventing an arrival time is an assertion
the mirror cannot support.

**An unknown tag or seed is a 404 with a body.** An empty 200 reads as "nothing
new", which is a claim about the corpus rather than about the URL.

**`corpus.ParseDate` is exported** and the epoch-days `parseDate` now goes
through it. A second copy of the layout list would drift silently, and only the
surface built on the copy would show it.

## Three defects the deployment found that the tests did not

1. **The seed was its own first recommendation.** `engine.Request.Exclude` is
   false by zero value; `/api/v1/recommend` sets it true, and the feed handler
   did not. The test meant to catch it compared
   `strings.HasSuffix(link, "/work/11")` against a corpus url that reads
   `.../works/11` — plural — so it matched NEITHER shape and passed whatever
   the feed contained. It parses the trailing id now.
2. **`<pubDate></pubDate>` for a work with an empty `update_date`.** An empty
   element is not "no date": readers parse it and drop the entry or sort it to
   the epoch. `omitempty` now.
3. **The descriptor advertised `?format=rss`.** `/search` has no format
   parameter, so a browser offered a subscription that returned HTML. Removed;
   the test counts the Urls (one) and a mutation re-adding the line reddens it.

## Mutation ledger

| mutation | verdict |
|---|---|
| `ORDER BY w.update_date DESC` | killed — order test names the wrong first item |
| relative guid in the minted-link fallback | SURVIVED, then killed once a fixture work had no url |
| hand-rolled (unescaped) XML | killed — "invalid character entity &" |
| unknown tag answered 200 | killed — the 404 test |
| second Url in the descriptor | killed — "advertises 2 URLs, want 1" |
| `Exclude: true` removed from the seed feed | SURVIVED until the id comparison replaced the suffix check |

The two survivors are the session's lesson: a fixture where every row carries a
url cannot reach the code path that mints one, and a suffix check written for
one URL shape silently matches neither.

## Route audit

`TestEveryAPIRouteIsAccountedFor` failed on all three routes until they were
recorded. They now say why they have no page: a feed is read by a subscriber's
client, the descriptor by a browser's search settings, and `/tag/{id}` carries
the page half.

## #78 — the work-pair report (commit 1fb7a69)

    GET /api/v1/works/{id}/similarity/{other}
    /work/{id}?compare={other}

Two axes, published separately: `tag_overlap` (IDF-weighted Jaccard) and
`embed_cosine` (cosine of the works' tag centroids), with the shared tags
rarest-first and every axis that could not run named in `missing`. The score is a
renormalised mean of what ran. Popularity and recency are deliberately NOT
components.

The test that gives this teeth: a pair sharing a RARE tag versus a pair sharing a
COMMON one, unions the same size. A plain Jaccard index scores them equal, so an
implementation that forgot the IDF fails. Its sibling: a pair with nothing in
common must score exactly 0, or the positive case passes on something that scores
everything alike.

The fixture had to LOAD the document-frequency table. Without it the engine
weighs every tag the same, names `tag_idf` in `missing`, and the weighting test
fails for a reason that has nothing to do with the code under test — which is
exactly how the first run of it failed.

## The reserve question: answered, no change

Measured on the same reader sample (38 readers, 495 held-out bookmarks):

| reserve | pool recall@1000 | recall@10 | nDCG@10 |
|---|---|---|---|
| 1/8 (run #13) | 0.2113 | 0.1050 | 0.1959 |
| 1/4 (run #15) | 0.2115 | 0.1040 | 0.1913 |

Two held-out works across the whole sample: the embedding source is saturated at
1/8, so the constant stays and now carries the numbers.

**The measurement trap.** The first attempt (run #14) read pool recall 0.2024 and
looked like a real regression. It was run with `--users 38` rather than 40, and
`--users` chooses the SAMPLE: 36 usable readers instead of 38, 463 held-out
bookmarks instead of 495. Two eval runs are only comparable at the same
`--users`. The eval's own split is deterministic per reader (PCG seeded by user
id) — the sampling above it is not.

## #40 — reading paths (commits 3297257 + f28f02f)

    GET /api/v1/works/{id}/path?steps=N
    /path/{id}?steps=N

Each step is chosen against the PREVIOUS work, so the list drifts. Every step
publishes `sim_to_prev` AND `sim_to_seed`; `drift_from_seed` names the first step
that shares no tag with the seed.

**The finding this session will be remembered for.** The first version passed
every test and produced, on the live mirror, eight steps sitting 0.93-0.96 from
the SEED at every step: "more like this" in sequence, which is the exact thing
the feature exists not to be. The fixture was too small for the defect — its
nearest work was the right one. The fix is a selection rule,
`sim_to_prev - 0.5*sim_to_seed`, on the same seed:

    weight 0    seed distance 0.966 0.966 0.966 0.966
    weight 0.5  seed distance 0.966 0.896 0.798 0.701   (hops stay 0.89-0.97)

The rule lives in `pathRank` so it has a test stated as a PREFERENCE (a 0.85/0.40
work must outrank a 0.90/0.95 one) rather than as a constant.

**No fandom cap, and that is a finding.** The obvious rule — no fandom more than
twice — cannot fire on this corpus: `work_tags` is 3,890,504 `freeforms` rows of
3,891,300, so a cap keyed on `tag_type='fandoms'` would match nothing, exclude
nothing and report success. That is the dead-column defect the diversify package
already documents. The path measures drift instead of forcing it.

Also: a work never appears twice, and no embeddings is a zero-step path with the
axis named and a note saying where to go instead.

## #65 — new since your last visit (commit 90941c5)

    /new

`seen_works` is a SET with no time order, so nothing could answer "what arrived
since I last looked". `reader_visits` stores three instants: `previous_at` (the
BOUNDARY the list is measured from), `current_at` (the visit's start) and
`last_at` (when we last heard from them).

`last_at` exists because the gap that decides "a new visit" must be measured from
ACTIVITY, not from the visit's start: a reader who browses for an hour is one
visit. The mutation that measures from `current_at` splits one visit into two and
the test says so by name ("an hour of browsing in 20-minute steps counted as 2
visits, want 1").

Both halves of `seen_works` are excluded, read AND shown: a work the reader
scrolled past was still put in front of them.

The exclusion is bound as an IN list because `seen_works` lives in the STATE
database and `works` in the CORPUS database — SQLite cannot join across two
files, so the shorter code would not have run at all.

Live: backdating the boundary to 1 May 2026 returned 40 works newest-first
(90553521 "Bad Moon Rising" first, matching the tag feed's newest), and marking
that work seen removed it from the list and refilled from below. A boundary of
20 Oct returned zero — the corpus's newest `first_seen` is 15 Oct, so zero was
the right answer and the page printed the boundary that made it checkable.

## The gate now covers today's surfaces (f52f7d2)

48 → 63 checks. Two helpers were needed and neither existed:

  status NAME URL EXPECTED     a REFUSAL (404 unknown tag, 400 work-with-itself)
  check_absent NAME URL PATTERN a promise the server must NOT make

Measured that the section bites by re-adding the `format=rss` descriptor URL and
deploying it: the gate said `FAIL opensearch advertises no url it cannot answer`
and `DRIFTED: 1 of 63`. A gate that has never been observed to fail is a gate
nobody knows the shape of.

Two attempts at that mutation were worthless before the third worked, and both
shapes recur:

  - the first did not COMPILE (a `%s` with two verbs and one argument) — a build
    failure is not a survivor;
  - the second ran against a stale `/tmp/check-deploy.sh` from an earlier deploy
    while a `git stash` I had used to get a clean tree had silently removed the
    new section from the tree. The deploy still reported "all 48 checks pass",
    which was true of the script it ran and false of the tree.

## Next

Untouched rows with a real input behind them: 60 (`interleave_impressions` now
records what was shown), 65 (`seen_works.shown_at` is a per-reader timestamp),
43 (nothing negates the taste vector), 40 (no sequential walk). Then 76 (embedding
drift) now that the index is resident and its rows are in one slab.
