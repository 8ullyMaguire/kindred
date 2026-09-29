# SPEC — kindred web UI

## What this is

A web frontend for kindred, served by the kindred binary itself on the same
port as the API. No second process, no second port, no build step, no
`node_modules`. The target is a Raspberry Pi with 512 MB, so "serve a static
bundle from disk" is a thing to be suspicious of, and "12 MB single binary
that also serves its own HTML" is the property worth having.

The user asked for "the same theme as ao3". This document defines what that
means precisely, because "make it look like AO3" is not a specification.

## Why a frontend at all

kindred is currently API-only. `GET /` returns
`404 {"error":"no route for GET /"}`. Every other site on this host
(gravity :8007, lorehaven :8008, Concord :8006) serves HTML at `/`. A
browser pointed at kindred showed nothing, and the reason was not DNS — it
was that there is no page.

This was missed on the first pass. The symptom was reported as "still
doesn't show anything" and attributed entirely to a missing DNS record,
which was also true, and was not the whole cause. A plan that had looked
at what the other sites return at `/` would have caught it.

## Scope

Shipping in this milestone:

- `GET /` — search page. A query box, and results.
- `GET /work/{id}` — one work: metadata, tags, and recommended neighbours.
- `GET /tag/{id}` — one tag: metadata, works that carry it.
- `GET /search?q=` — tag search, because a work needs a tag to seed from
  and a bare id is not a discovery surface.
- `GET /static/*` — CSS and JS, embedded in the binary.
- A 404 page that is HTML, not JSON, for unmatched browser paths. The API
  404 stays JSON; a browser asking for a page should not receive one.

Not shipping, deliberately:

- **No build step, no bundler, no framework.** One CSS file and one JS
  file, hand-written, embedded with `go:embed`. A React build for four
  routes on a 512 MB Pi trades a real constraint for a convenience.
- **No pagination beyond `per_page`.** The API paginates; the UI passes
  the parameter through and renders what comes back. Infinite scroll is
  not in scope.
- **No user accounts, comments, or anything that writes.** kindred's HTTP
  surface is read-only and stays that way. A login here would be a lie
  about a system that has no users.
- **No search over works by title.** The API has no such endpoint. Adding
  one is a real feature against a 1.7 GB read-only corpus, and is a
  separate milestone. The UI searches *tags*, which the API does support.
- **No AO3 account integration, no "works by this author" page.** Same
  reason.

## Design — what "the AO3 theme" means

Taken from the real stylesheets at
`~/code/ruby/otwarchive/public/stylesheets/site/2.0/`, not from memory.
The first draft of this section was written from memory and was wrong on
both of the two values that decide whether the result reads as AO3.

### Palette (by frequency across the 2.0 set)

| hex | count | role |
|---|---|---|
| `#fff` | 68 | page background |
| `#ddd` | 36 | borders, 1px |
| `#900` | 35 | **all links** |
| `#bbb` | 27 | muted text, disabled |
| `#eee` | 18 | alt background |
| `#ccc` | 15 | hairlines |
| `#111` | 13 | strongest text |
| `#999` | 9 | counts, labels |
| `#2a2a2a` | 9 | **body text** |

Plus `#efd1d1` (a pale rose, used for warnings) and `#f3efec` (a warm
off-white, for a metadata block) — both present in the real sheets.

### Typography

```css
/* body — 01-core.css */
font: 100%/1.125 'Lucida Grande', 'Lucida Sans Unicode', Verdana,
      Helvetica, sans-serif, 'GNU Unifont';

/* headings — 02-elements.css */
h1, h2, h3, h4, h5, h6, .heading {
  font-family: Georgia, serif;
  font-weight: 400;
  word-wrap: break-word;
}
h1 { font-size: 2.5em; line-height: 1; margin: 0.5em 0; }
```

### What my from-memory draft got wrong

- **Body is sans-serif, not serif.** Only headings are Georgia. I had
  "serif throughout" and "the single strongest signal" — that is
  backwards, and it is the difference between looking like AO3 and looking
  like a blog.
- **Links are `#900`, a dark maroon — not `#1f7ec2` blue.** I invented a
  blue from the pre-2023 site. 35 uses, it is the most distinctive colour
  in the whole stylesheet.
- **Body text is `#2a2a2a`, not `#333`.** Minor, but free to be right.

### Rules that make it read as AO3

- **Sans body, serif headings.** Not the reverse.
- **`#900` links**, no underline by default, everywhere including visited.
- **Borders `#ddd`, backgrounds `#eee` / `#f3efec`.** No shadows anywhere.
- **`padding: 1em 3em`** on the page regions — the generous horizontal
  gutter is a real signature.
- **`#efd1d1` pale rose** for the "this is a recommendation, not a fact"
  framing if a page needs one.
- **No dark mode.** AO3 has one; light only is a deliberate first
  milestone and `prefers-color-scheme` is next.

Honest limits: an imitation, not a clone. No AO3 logo or wordmark, no
pixel parity, and no claim of affiliation. The theme is recognisably in
that family and clearly not AO3.

## Architecture

```
internal/api/server.go     add 4 page routes + /static/*
internal/web/embed.go      //go:embed assets/*
internal/web/assets/       index.html, work.html, tag.html, search.html
                            style.css, app.js
internal/web/render.go     template execution, page structs
```

The pages are Go `html/template` files, embedded at compile time. Templates
are executed against a struct per page, with a shared base layout.

`html/template` and not `text/template`, specifically: kindred renders tag
names and work titles from a corpus, and a work called
`<script>alert(1)</script>` is a real title in a real dataset. Contextual
auto-escaping is the whole point of choosing it.

## Constraints inherited from the rest of the project

- **No new dependencies.** Three on purpose: chi, modernc sqlite,
  x/crypto. The frontend uses `html/template` and `embed` from stdlib.
- **The 220 MB RSS cap holds.** Serving HTML costs a few KB of templates
  plus whatever the response buffers are. The `make budget` gate must
  still pass, and `kindred serve` must still report `budget_ok: true`.
- **The API does not change.** This milestone adds routes and a static
  handler. Every existing endpoint keeps its shape, and the existing
  JSON 404 for unmatched `/api/*` paths is preserved.
- **The binary stays static and ~12 MB.** Embedded assets add kilobytes.

## Verification

The automated gate is necessary and not sufficient. The deliverable is
"a person opens a page and sees a work", which no unit test proves.

Automated:

1. `go build ./...` — compiles.
2. `go test ./...` — all green, including new tests for: each page
   returning 200 and `text/html`; `/` not returning JSON; the API 404 for
   `/api/v1/nonexistent` still returning JSON; XSS escaping of a hostile
   tag name; assets served with a correct content type.
3. `make budget` — RSS cap still respected with a page request in the mix.
4. `go vet ./...` — clean.

Manual, on the host:

5. `curl -s localhost:8010/ | head` — HTML, not JSON.
6. A browser at `https://kindred.polarisocial.xyz/` renders the search page
   with the serif stack and the accent rule.
7. A tag id from a real search leads to a work page, and that work's page
   lists recommendations that came from the recommender, not from a
   fixture.

## Known gaps after this milestone

- Work search by title (no API support yet).
- Dark mode.
- The DNS record for `kindred.polarisocial.xyz`, which is a zone change in
  the Cloudflare dashboard and outside this repository entirely.
