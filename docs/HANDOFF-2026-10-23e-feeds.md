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

## Next

- The embed reserve question is answered by the numbers already in hand: the
  deployed pool was 902 tags + 1 collab + 97 embed with a 125-slot reserve, so
  the embed source under-fills its reserve and raising the reserve cannot help
  until it has more than 125 candidates to offer. The next measurement is why it
  offers 97 — likely the nearest-tag fan-out, not the reserve.
- Then the untouched rows with a real input behind them: 60 (`interleave_
  impressions` exists), 65 (`seen_works.shown_at` exists), 78 (no pair route).
