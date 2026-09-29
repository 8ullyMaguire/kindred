# PLAN — kindred web UI

Implements `docs/SPEC-web-ui.md`. Every symbol referenced here was checked
against the tree before being written down; where a symbol is new it says
so. Every step ends with a command and its expected output.

Verified to exist before this plan was written:

- `func writeJSON(w http.ResponseWriter, ...)` — `internal/api/server.go:572`
- `func writeErr(w http.ResponseWriter, ...)` — `internal/api/server.go:585`
- `func pathInt64(w, r, name) (int64, error)` — `internal/api/server.go:594`
- `func atoiDefault(s string, def int) int` — `internal/api/server.go:679`
- `func clamp(n, lo, hi int) int` — `internal/api/server.go:695`
- `type Server struct` — `internal/api/server.go:33`
- `corpus.Entity` — `internal/corpus/entity.go:20`
- `corpus.Tag` — `internal/corpus/entity.go:20` (fields Name, Type, Weight)
- `corpus.TagPair` — `internal/corpus/ao3.go:170` (ID int32, Name string)
- `engine.Engine.Corpus` is `*corpus.AO3` — `internal/engine/engine.go`
- `func (a *AO3) Entity(ctx, id int64) (Entity, error)`
- `func (a *AO3) TagPairs(ctx, ids []int64) (map[int64][]TagPair, error)`
- `func (a *AO3) CandidateRowsSlim(ctx, ids []int64) ([]Entity, error)`
- `engine` response field `Items []rank.Candidate` — `internal/engine/engine.go:82`

### Three symbols this plan had wrong, corrected before implementing

A draft of this file referenced `s.Corpus`, `corpus.Work`, and a
`SearchTags` method. None of the three exists. Grepping the tree first is
the entire reason this was cheap to fix; inventing them in the plan would
have produced code that does not compile and a plan that teaches the
implementer to trust it.

- **`api.Server` has no `Corpus` field.** Fields are Engine, Store, Log,
  Version, StartedAt, Lite. The corpus is `s.Engine.Corpus`, typed
  `*corpus.AO3`, and `AO3` has a `DB *sql.DB` field the tag search
  queries directly.
- **Handlers return `corpus.Entity`, not `corpus.Work`.** `Entity` is
  `{ID, Kind, Title, URL, Summary, Stats map[string]float64, Tags []Tag}`.
  Note `Stats` is a **map**, not the flat `Kudos`/`Hits`/`Bookmarks`
  fields of the unused `corpus.Work` struct.
- **There is no `SearchTags` method.** `handleAO3Tags` runs
  `SELECT id, name FROM tags WHERE name LIKE ? ORDER BY name LIMIT ?`
  inline. The UI search must either call that same query or reuse the
  handler's logic; the plan says to factor it into one place rather than
  write a second copy of the SQL.

---

## Step 1 — the asset files

**New files, all under `internal/web/assets/`:**

- `style.css` — the whole theme. Values copied from
  `~/code/ruby/otwarchive/public/stylesheets/site/2.0/`:

```css
/* Palette and type taken from the real AO3 2.0 stylesheets.
   Body is SANS with Georgia headings; links are #900 maroon. */
:root {
  --bg: #fff;
  --text: #2a2a2a;
  --text-strong: #111;
  --muted: #999;
  --border: #ddd;
  --hairline: #ccc;
  --alt: #eee;
  --warm: #f3efec;
  --rose: #efd1d1;
  --link: #900;
  --sans: 'Lucida Grande', 'Lucida Sans Unicode', Verdana, Helvetica,
          sans-serif, 'GNU Unifont';
  --serif: Georgia, serif;
}
body {
  background: var(--bg);
  color: var(--text);
  font: 100%/1.125 var(--sans);
  margin: 0;
}
h1, h2, h3, h4, h5, h6, .heading {
  font-family: var(--serif);
  font-weight: 400;
  word-wrap: break-word;
}
h1 { font-size: 2.5em; line-height: 1; margin: 0.5em 0; }
a, a:link, a:visited { color: var(--link); text-decoration: none; }
a:hover { text-decoration: underline; }
#header, #main, #footer { padding: 1em 3em; }
.blurb, fieldset {
  border: 1px solid var(--border);
  padding: 1em;
  overflow: hidden;
}
/* tags: NOT pills. AO3 uses dotted underline, invert on hover. */
a.tag {
  color: var(--text-strong);
  line-height: 1.5;
  text-decoration: none;
  padding: 0;
  border-bottom: 1px dotted;
}
a.tag:hover { background: var(--link); color: #fff; }
.tags li { display: inline; padding-left: 0; padding-right: 0.25em; }
.stats, .meta { background: var(--warm); }
.count { color: var(--muted); font-size: 0.9em; }
.muted { color: var(--muted); }
```

- `app.js` — one behaviour only: the search box submits to `/search?q=`,
  plus progressive enhancement for the tag filter. No framework, no
  build. If JS fails, the form still works because it is a real
  `<form method="get" action="/search">`.

**Verify:**

```sh
cd ~/code-local/go/kindred
test -f internal/web/assets/style.css && test -f internal/web/assets/app.js && echo "assets present"
grep -c "900" internal/web/assets/style.css   # expect >= 1
```

---

## Step 2 — the embed and the render helpers

**New file `internal/web/embed.go`:**

```go
// Package web serves kindred's HTML frontend from the binary itself, on
// the same port as the API, with no build step and no runtime asset
// directory. A target with 512 MB of RAM does not need a node_modules.
package web

import "embed"

// assets holds the templates, CSS and JS. go:embed bakes them into the
// binary, so a deployed kindred is one file with no siblings to lose.
//
//go:embed assets
var assets embed.FS
```

**New file `internal/web/render.go`** — template parsing, the FuncMap, and
the page structs. There is no `template.FuncMap` anywhere in the tree yet,
so this is new code; `dict` is the helper for passing several values to
one `{{template}}` call, since Go templates have no multi-arg template.

Templates are parsed once at init, not per request, and a parse failure
is a panic: a template that does not compile is a programming error, and
it should stop the process at startup rather than produce a 500 on the
first request and look like a runtime problem.

```go
func mustParse(name string) *template.Template { /* ... */ }
```

Page structs. `Base` is embedded in each, which is how the shared header,
footer and `<head>` get into every page without a second template:

```go
type Base struct {
    Title   string
    Heading string
    Version string
}

type SearchPage struct {
    Base
    Query   string
    Results []TagHit   // {ID int64, Name string, Count int}
    Total   int
}

type WorkPage struct {
    Base
    Work    corpus.Entity  // NOT corpus.Work -- Entity is what Entity() returns
    Tags    []corpus.TagPair
    Similar []SimilarHit   // {ID int64, Title string, Score float64}
}

type TagPage struct {
    Base
    Tag   corpus.Tag
    Works []WorkHit       // {ID, Title, Author, Kudos, Hits}
    Total int
}
```

**Verify:**

```sh
go build ./internal/web/ && echo "web package compiles"
```

---

## Step 3 — the four pages

**New files in `internal/web/assets/`:**

- `layout.html` — the base. `{{define "layout"}}`, the `<head>`, the
  header, `{{template "content" .}}`, the footer, and the `/static/style.css`
  link. Note `{{` and `}}` are Go template syntax; AO3's own ERB-ish
  `{{ }}` habits do not apply.
- `search.html` — the form and the result list.
- `work.html` — title, author, summary, stats, tags, recommendations.
- `tag.html` — the tag, the works carrying it.

Stats are a `map[string]float64`, and the AO3 work stats include keys
that are NULL in the corpus. A missing key reads as the zero value, which
would print `0` for a bookmark count nobody has. The template must
distinguish absent from zero via a helper, and the helper must be tested
with a key that is not in the map:

```go
// In render.go.
func stat(m map[string]float64, key string) (string, bool) {
    v, ok := m[key]
    if !ok || v == 0 {
        return "\u2014", false   // an em dash, not a zero
    }
    return strconv.FormatFloat(v, 'f', -1, 64), true
}
```

```html
{{template "stat" dict "Label" "Bookmarks" "Value" (stat .Work.Stats "bookmarks")}}
```

**Verify:**

```sh
go test ./internal/web/ -run TestTemplatesParse -v
```

with the test asserting all four templates parse AND that
`TestBookmarksNilRendersDash` renders a work with a nil `Bookmarks` and
asserts the output contains `—` and not `>0<`.

---

## Step 4 — wire the routes

**Modify `internal/api/server.go`.** Add to `Routes()`, **before** the
catch-all `mux.HandleFunc("/", ...)` at line 72 — `ServeMux` prefers the
most specific pattern, but a bare `/` page handler registered after the
catch-all would still win on specificity, and the ordering must be
obvious to the next reader:

```go
mux.Handle("/", web.Pages(s.Engine, s.Version))
mux.Handle("/static/", http.StripPrefix("/static/", web.Static()))
```

The catch-all at line 72 must keep returning **JSON** for `/api/*`. The
new `mux.Handle("/")` is less specific than `/api/v1/...` so the API is
untouched, but this is exactly the kind of claim that needs a test rather
than reasoning: see step 5.

**Verify:**

```sh
go build ./... && go vet ./... && echo "builds clean"
```

---

## Step 5 — the tests

**New file `internal/api/web_test.go`.**

| test | asserts |
|---|---|
| `TestRootServesHTMLNotJSON` | `GET /` → 200, `Content-Type` contains `text/html`, body starts with `<!doctype` |
| `TestAPI404StillJSON` | `GET /api/v1/nonexistent` → 404 and `Content-Type` is JSON. **This is the regression guard for the new catch-all.** |
| `TestWorkPageRenders` | `GET /work/1` → 200, contains the title |
| `TestTagPageRenders` | `GET /tag/1` → 200 |
| `TestSearchPageRenders` | `GET /search?q=harry` → 200, contains at least one tag |
| `TestStaticCSS` | `GET /static/style.css` → 200, `text/css`, body contains `#900` |
| `TestHostileTagNameIsEscaped` | a tag named `<script>alert(1)</script>` is escaped, and the raw string does not appear |
| `TestUnknownBrowserPathIsHTML` | `GET /nope` → 404 and `text/html` |

The XSS test is the important one. kindred renders strings from a
1.7 GB corpus, and `html/template` is chosen specifically for contextual
auto-escaping. A test that asserts a script tag was escaped is the only
proof that the right template package is in use.

**Verify:**

```sh
go test ./... 2>&1 | grep -v "no test files"
```

expect every package `ok`, and specifically no `FAIL` in
`internal/api` or `internal/web`.

---

## Step 6 — the memory gate

The API was 145 MiB of a 220 MiB budget; HTML rendering adds template
memory and response buffers. This is the step most likely to fail, and
failing it is the point of having the gate.

```sh
make budget
```

expect `budget_ok: true` and peak RSS under 225,280 KiB.

---

## Step 7 — deploy and the manual gate

```sh
make build
scp bin/kindred thinkcentre:/tmp/k
ssh thinkcentre 'sudo cp /tmp/k /usr/local/bin/kindred && sudo systemctl restart kindred'
ssh thinkcentre 'curl -s -o /dev/null -w "%{http_code} %{content_type}\n" localhost:8010/'
```

expect `200 text/html; charset=utf-8`.

**The manual gate that no test replaces:** open
`https://kindred.polarisocial.xyz/` in a browser. It must show the search
page with a sans-serif body, Georgia headings and maroon links. Search
`harry`, click a tag, click a work. The work page must list
recommendations that came out of the recommender.

That chain is five client-side steps through three handlers and a
template. No unit test covers it.

---

## Ordering, and why

Assets before render, because `render.go` embeds the directory and will
not compile without it. Render before routes, because a route that
returns a nil template is a nil-pointer panic at request time. Tests after
routes, because the tests are about the routes. Budget after tests,
because there is no point measuring a build that does not compile.

## If a step fails

- Step 5's `TestAPI404StillJSON` failing means the new `/` handler
  shadowed the API. That is a real regression, not a flaky test.
- Step 6 going over budget means the templates are not the problem —
  it means response buffering is, and the fix is in the handler, not the
  CSS.
- A blank page with a 200 in the browser means a template executed and
  produced nothing. `curl` the page and look at the bytes; do not add a
  retry.
