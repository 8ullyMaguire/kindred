# PLAN — 2026-10-23d: the mirror becomes subscribable (#61, #62)

Two roadmap rows, one unit: a reader can follow a tag (or a seed) without
opening the site, and a browser can send searches to it.

    #61  RSS feed of recommendations   — no /feed; the tree serves no XML at all
    #62  OpenSearch descriptor         — no /opensearch.xml route

## Decisions

**RSS 2.0, not Atom.** The row says RSS and every reader that matters speaks it.
Atom's advantages (typed dates, stable ids) are worth little here: the corpus
has no per-work revision history, so there is nothing to be precise about.

**Two feeds, and no third.**

    /feed/tag/{id}          works carrying a tag, most recently updated first
    /feed/seed/{kind}/{id}  the recommendations for one seed work

The tag feed is the one a reader subscribes to: its entries are dated by the
work, so "new since you last looked" is a real question. The seed feed answers
"what is like this one" and its entries are dated the same way — there is no
"when was this recommended" to report, because it is computed per request, and
inventing a timestamp would be an assertion the mirror cannot support.

**Ordering uses `corpus.ChronologicalUpdateDateDesc`, never `update_date DESC`.**
That constant exists because the mirror stores AO3's "31 Oct 2025" form and a
string sort is dominated by the day-of-month: measured on the live mirror,
`update_date DESC` returns 31 Oct 2025 as the most recent work while the true
most recent is 26 Jun 2026. A feed that claims "recent" with the wrong order is
worse than no feed.

**Absolute links, derived per request.** RSS requires absolute URLs for `link`
and for a `guid` with `isPermaLink="true"`, and this deployment has no
configured public URL (it sits behind a tunnel whose hostname is not in the
config). So the base comes from `X-Forwarded-Proto` / `X-Forwarded-Host` /
`Host`, defaulting to the request's own scheme and host. A relative guid is
silently invalid: clients mint their own identity for an entry whose guid they
cannot resolve, so every refresh looks like new works.

**An unknown tag or seed is a 404 with a body, not an empty feed.** A 200 with
zero items reads as "nothing new", which is a statement about the corpus rather
than about the URL.

**Feed items carry the work's own fields**, not the ranking's: title, link,
`pubDate` from `update_date`, and a description with the author, word count,
kudos and rating. A feed entry that scored 0.87 in some tune is not information
the reader asked for, and it is not stable between requests.

**n is 50 by default, capped at 200**, like every other list surface.

**Discovery.** The tag page gains
`<link rel="alternate" type="application/rss+xml" href="/feed/tag/{id}">`, so a
reader's browser offers the feed without the URL being documented anywhere. The
page also carries a visible link with the copy "Follow this tag".

**OpenSearch 1.1**, one entry, pointing at `/search?q={searchTerms}`. The
descriptor is what makes the site appear in a browser's search-engine list, and
it advertises the HTML page rather than a JSON API because that is what the
reader ends up on.

## Files

    internal/corpus/ao3.go      ParseDate exported (one parser for epoch days and times)
    internal/api/feed.go        RSS document, both handlers, opensearch.xml
    internal/api/server.go      three routes
    internal/web/handlers.go    guardAPI sends /feed/ and /opensearch.xml to the API
    internal/web/assets/tag.html  the discovery link
    internal/api/feed_test.go   XML parse, order, escaping, 404, bounds
    internal/web/router_test.go /feed/ belongs to the API, not the page router

## Verification

    go build ./... && go test ./... -count=1
    curl -sS http://127.0.0.1:8019/feed/tag/29 | python3 -c 'import sys,xml.dom.minidom as m; m.parseString(sys.stdin.read()); print("well formed")'
    curl -sSi http://127.0.0.1:8019/feed/tag/29 | head -3          # content type
    curl -sS http://127.0.0.1:8019/feed/tag/99999999                # 404, not an empty feed
    curl -sS http://127.0.0.1:8019/opensearch.xml                   # includes {searchTerms}
    curl -sS http://127.0.0.1:8019/tag/29 | grep -c 'application/rss+xml'

Mutations to run: drop the chronological ordering (use `w.update_date DESC`) →
the order test must redden with the mixed-format fixture; drop the escaping →
the hostile-title test must redden; return 200 on an unknown tag → the 404 test
must redden.
