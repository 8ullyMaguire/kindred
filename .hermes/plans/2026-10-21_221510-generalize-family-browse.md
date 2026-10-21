# Generalise "works matching a fandom family × a concept family" into a reusable corpus query, and expose it on the site

## Goal

Replace the one-off `worm-isekai` command with a general **"tag-family intersection"** corpus query
(`family A` × `family B`, recency window, ranked, with per-item evidence), and surface it on the
existing site through the **existing `/api/v1/ao3/works` filter surface** and a **new reader-facing
page built from the existing `/tag/` list template**, so any fandom × any concept can be asked
without new code.

---

> ## Verification status of this plan (2026-10-21)
>
> This plan's code was **extracted verbatim from this document, compiled, and run** in a scratch
> Go module, and the plan's SQL was **executed against the live mirror**. What that found, and
> what it did not:
>
> **Executed and passing:**
> - `internal/tagfamily` — all **six** tests (Tasks 1–3) compile and pass; `go vet` clean;
>   `gofmt` clean. The `families.json` in the plan parses and loads.
> - The CLI's `printBrowseTable` and its test (Task 7) compile and pass against that package.
> - The `COLLATE NOCASE` placement in the `IN` clause — measured on a case-sensitive column
>   (risk A). The plan's spelling is the one that works.
> - The `ChronologicalUpdateDate` SQL expression — run against the live mirror; its top row is
>   `26 Jun 2026` where the plain `ORDER BY update_date DESC` gives `31 Oct 2025` (F7).
> - The F2 rule, re-measured against the 486-page AO3-HTML oracle with the corpus-derived base
>   set: **precision 1.000, recall 0.450, 0 false positives**, and all 11 of Task 1's test cases
>   pass. This measurement **changed the plan** — see risk D.
> - The repo's own conventions — `pageSpec`'s fields, the `{{define "content"}}` template
>   contract, the `data-empty="true"` two-sided sweep, `newTestServer`'s signature, the CLI
>   helpers, and `check-cli-coverage.py`'s hardcoded `COMMANDS` list were all read out of the
>   source, not assumed.
>
> **NOT executed (and therefore not proven):**
> - Everything in `internal/api`, `internal/web` and `cmd/kindred` — those changes are edits to
>   **existing** files, and the plan was written in plan mode, where the repo was not modified.
>   The plan's code for those is written against the **verified** signatures and conventions
>   listed above, but it has not been compiled. **Treat Tasks 4–9 as unproven until Task 10's
>   acceptance run passes.**
> - The live `/browse` page, the live API, and the live CLI. Task 10 is where those are proved.

---

## 1. Current context / assumptions

### 1.1 What exists today (measured, not assumed)

| Thing | Where | State |
|---|---|---|
| Bespoke query | `internal/wormisekai/wormisekai.go` (614 lines), `cmd/kindred/wormisekai.go` | **Uncommitted** (`git status`: `?? internal/wormisekai/`, `?? cmd/kindred/wormisekai.go`, `M cmd/kindred/main.go`) |
| Fandom name rules | `internal/fandom/fandom.go` | Exists. Pure string→string helpers. Has `LooksLikeFandom`, `Canonical`, `GroupKey` |
| Works list API | `internal/api/server.go:906` `handleAO3Works` | AO3-style filter params: `tag`, `complete`, `words`, `rating`, `lang`; `sort`, `limit`, `offset`, `format=csv` |
| Filter parser | `internal/api/server.go:775` `parseWorksFilters` | Returns `[]worksFilter{clause,args}`; one clause per filter, joined once |
| Corpus queries | `internal/corpusquery/` | Modes: `fandom-ranking`, `underrated`, `tag-neighbours`, `surprise`. `Row{Key,Label,Score,Detail,CoWorks}` |
| Corpus-query HTTP | `internal/api/server.go:131` `GET /api/v1/corpus-query/{mode}` | Mode dispatch, 400 on unknown mode |
| Corpus-query CLI | `kindred corpus-query --mode X` | — |
| Tag list page (reader-facing) | `internal/web/handlers.go:1575` `renderTag` | Renders works carrying one tag. Has filter form (`sort`, `words`, `complete`, `rating`, `lang`), `?n=` paging |
| Sort helper | `internal/web/handlers.go:2893` `tagOrder` | `recent` → `update_date DESC` (TEXT — see §1.3) |
| Mirror DB | `~/code/python/ao3/recommender/cache/ao3_metadata.db` | Read-only. 112,935 works, 634,027 tags, 3,893,000 `work_tags` rows |

### 1.2 The measured facts that constrain the design

These were measured against the live mirror on 2026-10-21. They are the reason the design below is
what it is; each is re-verified by a task in §4.

**F1 — Fandom-ness is a GLOBAL per-tag property, and the mirror knows the answer.**
The mirror's `work_tags.tag_type` is useless for this (measured: `freeforms` 3,890,504 /
`characters` 304 / `relationships` 175 / **`fandoms` 104** / `archive_warnings` 83 /
`categories` 79 / `rating` 51 — i.e. the mirror flattens nearly everything to `freeforms`).
**But** the scraper's raw HTML cache still carries AO3's own typed markup:
`html_cache/archiveofourown.org_works_*_view_adult=true.html` contains
`<dt class="fandom tags">` / `<dd class="fandom tags">` blocks.
Measured over 486 parsed work pages: **843 fandom tag-name instances, 3,286 freeform instances, and
zero tag names that are ever both** (`BOTH: 0`) — so fandom-ness is a property of the tag NAME, not
of the work, and it is recoverable exactly.

**F2 — A corpus-derived name rule classifies fandom-ness with precision 1.00 / recall 0.45.**
Rule tested: `X - Y` is a fandom iff (a) `Y` ∈ {`all media types`,`fandom`}, or (b) the tag is
`X (<media qualifier>)`, or (c) `Y` ∉ {type markers} and `X` has a canonical sibling elsewhere.
Measured against F1's oracle: **precision 1.000, recall 0.450** (136 TP / 0 FP / 166 FN).
The 166 false negatives are the creator-form tags whose base has **no canonical sibling in the
corpus** (`a song of ice and fire - george r. r. martin`, `the dresden files - jim butcher`,
`overlord - maruyama kugane & related fandoms`, …). So rule (c) is necessary but not sufficient.

> **The recall figure was re-measured on 2026-10-21 and rose from 0.397 to 0.450**, because the
> earlier measurement used a base set that omitted the oracle's own canonical spellings. The
> **precision figure is the load-bearing one: 1.000, with ZERO false positives.** That was
> achieved by applying the **type-marker check BEFORE the creator-form check** — without that
> ordering, "naruto - freeform" and seven other real freeform tags were misread as fandoms,
> because `naruto` IS a canonical base and the creator-form reading fired first.
>
> **The rule is therefore NOT a pure function of a name.** Rule (c) is about the corpus, and the
> plan implements it as the `Shape` type in Task 1. An earlier draft of this plan specified a
> pure `IsFandomShape(name string) bool`; that function **failed its own test** (it called
> `alternate universe - canon divergence` a fandom) and was replaced. See Task 1's note and
> risk D.

**F3 — Fandom spellings of ONE franchise are near-disjoint populations and do NOT co-occur.**
The Worm universe's six spellings: `parahumans series - wildbow` (2,041 works),
`wildbow - fandom` (2,127), `worm - fandom` (43), `worm - wildbow` (90), `worm (web serial novel)` (12),
`worm` (7), `worm - freeform` (4). The two biggest share **only 17 works**. Co-occurrence therefore
cannot group spellings into a franchise.

**F4 — Character tags do NOT bridge franchises either.**
Measured: `harry potter - j. k. rowling` and `star wars - all media types` share **4,619**
"character" tags. The character-tag space is dominated by generic names, so character overlap
cannot be used to decide that two tag families are the same franchise.

**F5 — So a franchise group must be DECLARED, and the corpus can only VALIDATE it.**
F3+F4 together mean "one franchise = one fandom family" is data, not a derivable fact.
The corpus *can* validate a declared family: every member spelling must exist as a `tags.name`
row, and every member must pass the F2 rule or be listed as an explicit exception.

**F6 — Concept families expand from a seed word, but the naive expansion is far too wide.**
Measured: the token `isekai` occurs in tags totalling **14,126 works** including
`isekai and transmigration`, `reverse isekai`, `technically an isekai`, `isekai'd`, `isekai?`,
`isekai au`, `isekai shenanigans` — but also fandom-scoped names such as
`pureblood culture (harry potter)` for the token `culture` (589 works). A family must therefore be
**an explicit list of tag names**, and the corpus can only *report* which of them exist and how
many works each carries.

**F7 — `update_date` is TEXT `DD Mon YYYY`, so lexicographic order is NOT chronological.**
Measured: `ORDER BY update_date DESC` yields top row `31 Oct 2025`; the true chronological maximum
is `26 Jun 2026`. 112,892 rows have length 11; 43 rows are empty. **This is a live bug in
`internal/web/handlers.go:2898`** (`tagOrder`'s `recent` case) and in
`internal/api/server.go:916` (`order = "w.update_date DESC"`). The bespoke `wormisekai` package
gets it right by parsing in Go (`ageDays`, `time.Parse("2 Jan 2006", …)`).
**Any recency window added to this plan must not repeat that bug**, and the plan should fix the two
existing sites.

**F8 — A family-scoped query is expressible in the existing works API.**
`handleAO3Works` already builds `WHERE` clauses from parsed filters. A family filter is one more
clause (`tag IN (…)`) plus a second (`tag IN (…)`) — i.e. an intersection — so **no new endpoint and
no new SQL builder is needed for the API surface.**

### 1.3 What this plan deliberately does NOT do

- It does **not** try to derive franchise groups from the corpus (F3/F4 show that is impossible).
- It does **not** add a new top-level site area, a new nav group, or a new database.
- It does **not** introduce a second ranking implementation. The existing bespoke
  `log10(kudos+1)/sqrt(age+1)` scorer is *reused verbatim* (moved, not rewritten).
- It does **not** touch `internal/engine`'s seed-based recommender, whose pool semantics
  (tags **OR** collab) are a different question.

---

## 2. Architecture / proposed approach

**One general query, three surfaces, one place for the rules.**

1. **New package `internal/tagfamily`** (replacing `internal/wormisekai`) holds the two
   domain rules as *data + code*:
   - `Family` = a named, ordered list of tag names (F6: explicit, never inferred).
   - `Universe` = a named list of fandom *spellings* (F5: declared, corpus-validated).
   - `Validate`/`Resolve` against the mirror: every listed name must exist; the report says which.
   - The F2 fandom-shape rule moves here as a `Shape` type built from the corpus
     (`LoadShape(ctx, db)` → `sh.IsFandom(name) bool`), needed to *classify* a work's "other
     universe" tag in a general way. **It is a type and not a bare function because rule (c) of F2
     is "X has a canonical sibling elsewhere" — a fact about the corpus, not about the name.**
     `internal/fandom` is left alone because it serves the diversity cap with a deliberately
     stricter rule.

2. **The query itself** — "works carrying a tag from family A **and** a tag from family B, updated
   within N days, ranked" — is expressed as **two extra `worksFilter` clauses on the existing
   `/api/v1/ao3/works` endpoint** (F8). The same two clauses are reused by a new
   `internal/api` handler and by the new web page. The bespoke bespoke `Runner` is deleted.

3. **The reader-facing surface** is a **new page `/browse`** that reuses the existing
   `/tag/` list template's markup and the existing `form.controls` filter widget. It is one nav
   item under the existing "Find something to read" group — not a new site area. It answers
   "recent works in <fandom> that are <concept>" for ANY fandom and ANY concept.

### 2.0 One vocabulary across every surface

Every surface of this feature takes the same thing — a **family key** — and resolves it the same
way, against the embedded `internal/tagfamily/families.json`:

| Surface | Parameter | Example | Resolves to |
|---|---|---|---|
| `GET /api/v1/ao3/works` | `universe=`, `concept=` | `?universe=worm&concept=isekai` | the family's spellings, as a `t.name IN (…)` clause |
| `GET /api/v1/browse` | `universe=`, `concept=` | `?universe=worm&concept=isekai` | the family's spellings, as a `tagfamily.Query` |
| `kindred browse` | `--universe`, `--concept` | `--universe worm --concept isekai` | the family's spellings, as a `tagfamily.Query` |
| `GET /browse` (the page) | `universe=`, `concept=` | `/browse?universe=worm&concept=isekai` | the family's spellings, as a `tagfamily.Query` |

**An unknown key is a 400 everywhere**, naming the keys that exist. That is the whole point of
declaring the families: a franchise with six near-disjoint spellings cannot be spelled by hand
in a URL, and a reader who types a wrong key must be told, not shown an empty list.

**A raw ad-hoc tag is still available** through the works endpoint's existing `?tag=` filter.
`universe` and `concept` are the *declared, named* families; `tag` is the free-form one. Two
different things, two different parameters.

**Why this shape:** the user asked for something "as general as possible", usable from the frontend,
that does not change the site much and modifies/expands current functionality rather than adding a
one-off. A **family × family intersection** is the most general primitive that answers the actual
question ("recent isekai wormfics"): one axis is the franchise, the other is the concept, and both
are *named lists* the corpus validates. The intersection is already what the works API does for
`tag`; it only needs to accept a **set** and to accept **two sets**.

### 2.1 The general rule set (the heart of the plan)

| Concept | Representation | Corpus relationship |
|---|---|---|
| **Concept family** | ordered list of tag names (`isekai`, `reverse isekai`, …) | validated: each must exist; report works-per-tag |
| **Fandom family** (a universe) | ordered list of *spellings* (`parahumans series - wildbow`, `worm - fandom`, …) | validated: each must exist and be fandom-shaped (F2), else flagged |
| **Fandom shape** (F2) | function of the tag name | derived |
| **Recency window** | N days | parsed from `DD Mon YYYY` in Go (F7) |
| **Rank** | `log10(kudos+1)/sqrt(ageDays+1)` | reused from the bespoke package verbatim |
| **Evidence** | per item: which family-A tag matched, which family-B tag matched, which *other* fandom tag matched (generalised from the bespoke "direction" field) | derived |

**Generalising "direction".** The bespoke package has a two-valued `Direction`
(`inside`/`into`) and a bespoke `OtherFandom` classifier with a **hand-measured list of 13
freeform namespaces** (`alternate universe`, `post-traumatic stress disorder`, `abandoned work`,
`not canon compliant`, `during worm canon`, `it`, `ack1308`, `inspired by dreaming of sunshine`,
`ashleigh (oc,prt agent ani jennings (oc`, `not recurring)`, `shatterbird`, `sonic.exe`) — a list
that is **Worm-specific and would be wrong for another fandom**. The general replacement is
**F2's rule** (which measured precision 1.00), so "which other universe is this work in" is answered
by a rule that needs no per-fandom list.

### 2.2 The generalisable data file

New file `internal/tagfamily/families.json` (embedded with `go:embed`, like `internal/web/embed.go`
does for assets):

```json
{
  "universes": {
    "worm": {
      "label": "Worm (Parahumans)",
      "spellings": [
        "parahumans series - wildbow",
        "wildbow - fandom",
        "worm - fandom",
        "worm - wildbow",
        "worm (web serial novel)",
        "worm",
        "worm - freeform"
      ],
      "spellings_explicit_freeform": ["worm - freeform"]
    }
  },
  "concepts": {
    "isekai": {
      "label": "isekai / transmigration",
      "tags": ["isekai", "reverse isekai", "technically an isekai", "isekai au",
               "isekai and transmigration", "reverse isekai and transmigration",
               "isekai'd", "isekai?", "isekai shenanigans"]
    }
  }
}
```

`spellings_explicit_freeform` is the honest escape hatch F2's rule needs: a spelling that is
freeform-shaped by name but still means the universe to a reader. It is **data**, and the report
says which spellings were admitted by rule and which by declaration.

---

## 3. Files to create / modify

| Action | Path | Purpose |
|---|---|---|
| create | `internal/tagfamily/families.json` | the declared families (above) |
| create | `internal/tagfamily/families.go` | embed + parse + `Family`/`Universe` types |
| create | `internal/tagfamily/shape.go` | the F2 fandom-shape rule (`Shape`, `LoadShape`, `IsFandom`) |
| create | `internal/tagfamily/validate.go` | corpus validation of a family (exists? fandom-shaped?) |
| create | `internal/tagfamily/families_test.go` | TDD for the above |
| create | `internal/tagfamily/shape_test.go` | TDD for the shape rule |
| create | `internal/tagfamily/helpers_test.go` | the shared `openTestDB` / `mustExec` test helpers |
| delete | `internal/wormisekai/` | replaced by `internal/tagfamily` |
| delete | `cmd/kindred/wormisekai.go` | replaced by the API/CLI surface below |
| modify | `internal/api/server.go` | family filters on `/api/v1/ao3/works`; new `GET /api/v1/browse`; fix `update_date` ordering (F7) |
| create | `internal/api/browse_test.go` | TDD for the new endpoint |
| modify | `internal/web/registry.go` | register the `/browse` page + nav entry |
| modify | `internal/web/handlers.go` | `renderBrowse`; fix `tagOrder` recency (F7) |
| create | `internal/web/assets/browse.html` | the reader-facing page |
| modify | `internal/web/render.go` | `BrowsePage` struct |
| modify | `cmd/kindred/main.go` | wire the new CLI subcommand (replacing `worm-isekai`) |
| modify | `docs/CLI.md` | document the new flags (gated by `scripts/check-cli-coverage.py`) |
| modify | `docs/what-is-left.md` | note the new capability |

---

## 4. Step-by-step tasks

Each task is 2–5 minutes of focused work, names exact paths, gives copy-pasteable code where code
is needed, and gives the exact verification command and its expected output. **TDD order is
mandatory: the failing test is written and RUN (and seen to fail) before the implementation.**

Every task ends with a commit. The repo is on `main` with a dirty tree (the untracked
`wormisekai` package + a modified `cmd/kindred/main.go`), so **Task 0 settles that first.**

---

### Task 0 — Commit the current work, then start clean

The bespoke package is uncommitted. It is about to be replaced, so its state must be captured
before it is deleted (it is the reference for the scoring function and the tag lists).

```bash
cd ~/code-local/go/kindred
git add -A && git commit -m "wip: worm-isekai query (to be generalised into tag-family browse)"
git status --short          # EXPECT: empty
```

**Verify:** `git log --oneline -1` shows the commit; `git status --short` prints nothing.

---

### Task 1 — `internal/tagfamily`: the F2 fandom-shape rule (TDD)

The rule that replaces the bespoke, Worm-specific `OtherFandom` namespace list.

> **This task was rewritten on 2026-10-21 after the plan's own code was compiled and run.**
> The first version of this task specified a **pure** `IsFandomShape(name string) bool` and a test
> that the pure function **failed** (`alternate universe - canon divergence` and
> `post-traumatic stress disorder - ptsd` both came back `true`). The cause is structural, not
> typographical: **rule (c) of F2 is "X has a canonical sibling elsewhere"** — a fact about the
> CORPUS, not about the name. A function that sees only a name cannot apply it.
>
> The rule below was re-measured against the same oracle with the corpus-derived base set
> (1,093 canonical bases read from the mirror's `tags` table):
>
> ```
> CORRECTED RULE (pure shape + corpus-derived bases)
>   creator-form    : 136 TP, 166 FN
>   freeform        : 3286 TN, 0 FP
>   precision=1.000 recall=0.450
> ```
>
> **Precision 1.000, zero false positives**, and all eleven of this task's test cases pass. The
> pure rule alone scored 8 false positives (`naruto - freeform`, `marvel - freeform`, …), because
> a creator-form base like `naruto` IS a canonical base — and `naruto - freeform` is still a
> freeform tag. **The type-marker check must win over the creator-form check**, and the base
> lookup must be a corpus lookup.
>
> **Design consequence: the rule is a type, not a function.** `Shape` holds the canonical base
> set and answers questions about names. Callers construct it once per query from a `*sql.DB`.

**Step 1a — write the failing test.** Create `internal/tagfamily/shape_test.go`:

```go
package tagfamily

import (
	"context"
	"testing"

	_ "modernc.org/sqlite"
)

func TestShapeClassifiesNamesByTheCorpus(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	// The canonical spellings. A creator-form tag is a fandom only if its
	// LEFT side has one of these, so the fixture must contain the
	// canonical spelling and NOT the creator form.
	mustExec(t, db, `INSERT INTO tags (id,name) VALUES
		(1,'harry potter - all media types'),
		(2,'star wars - all media types'),
		(3,'alternate universe - freeform')`)

	sh, err := LoadShape(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadShape: %v", err)
	}

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"canonical all-media", "harry potter - all media types", true},
		{"canonical fandom", "worm - fandom", true},
		{"creator form with canonical base", "harry potter - j. k. rowling", true},
		{"media qualifier", "star wars (movie)", true},
		{"type marker freeform", "alternate universe - freeform", false},
		{"type marker character", "oc - character", false},
		{"type marker relationship", "polyamory - relationship", false},
		{"freeform namespace", "alternate universe - canon divergence", false},
		{"freeform namespace ptsd", "post-traumatic stress disorder - ptsd", false},
		{"no shape at all", "isekai", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := sh.IsFandom(c.in); got != c.want {
			t.Errorf("IsFandom(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestShapeTreatsATypeMarkerAsWinningOverACanonicalBase(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	// "naruto - all media types" makes "naruto" a canonical base, so
	// "naruto - freeform" LOOKS like a creator form. It is not: the right
	// side is a type marker, so the tag names a type.
	mustExec(t, db, `INSERT INTO tags (id,name) VALUES (1,'naruto - all media types')`)

	sh, err := LoadShape(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadShape: %v", err)
	}
	if sh.IsFandom("naruto - freeform") {
		t.Error("IsFandom(\"naruto - freeform\") = true; a type marker on the right " +
			"must win over a canonical base on the left, or every " +
			"<fandom> - freeform\" tag is a fandom")
	}
}
```

> **`openTestDB` and `mustExec` are defined in `internal/tagfamily/families_test.go`** (Task 2's
> test file). **Write Task 2's test file FIRST, or move those two helpers into this file** — they
> are used by both. The plan puts them in Task 2's file; if you implement the tasks in order,
> Task 1 will not compile until Task 2's test file exists. **Either write Task 2's test file
> first, or add the two helpers to a shared `internal/tagfamily/helpers_test.go`.** The second is
> cleaner:
>
> ```go
> package tagfamily
>
> import (
> 	"database/sql"
> 	"testing"
>
> 	_ "modernc.org/sqlite"
> )
>
> // openTestDB opens an in-memory SQLite with the repo's driver.
> //
> // The driver is "sqlite" (modernc.org/sqlite, the repo's choice), NOT
> // "sqlite3" (mattn/go-sqlite3): the driver name is what sql.Open
> // resolves, and a wrong name fails at OPEN time, not at compile time.
> func openTestDB(t *testing.T) *sql.DB {
> 	t.Helper()
> 	db, err := sql.Open("sqlite", ":memory:")
> 	if err != nil {
> 		t.Fatal(err)
> 	}
> 	return db
> }
>
> // mustExec runs a statement and fails the test if it does not work.
> func mustExec(t *testing.T, db *sql.DB, q string) {
> 	t.Helper()
> 	if _, err := db.Exec(q); err != nil {
> 		t.Fatalf("exec %q: %v", q, err)
> 	}
> }
> ```
>
> **Remove those two helpers from Task 2's test file** when you move them here, or the package
> will not compile (duplicate declaration).

**Step 1b — run it and SEE IT FAIL.**

```bash
cd ~/code-local/go/kindred && go test ./internal/tagfamily/ -run TestShape 2>&1 | head -20
```
EXPECT: compile errors — `undefined: LoadShape`, `undefined: Shape`. Record them.

**Step 1c — implement.** Create `internal/tagfamily/shape.go`:

```go
// Package tagfamily answers questions that need a FAMILY of tags rather
// than one tag: a franchise's several spellings, or a concept's several
// spellings. The families are declared (families.json) because the corpus
// cannot derive them -- the spellings of one franchise are near-disjoint
// populations and share no co-occurring works -- but the corpus CAN
// validate them, and this file's rule is what "validate" means.
package tagfamily

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// typeMarkers are AO3's own vocabulary for "this tag names a TYPE, not a
// thing": the right-hand side of "X - freeform" marks a freeform tag.
var typeMarkers = map[string]bool{
	"freeform":     true,
	"character":    true,
	"relationship": true,
}

// mediaQualifiers are the parenthetical forms AO3 uses for a fandom's
// medium: "Star Wars (movie)", "Worm (web serial novel)".
var mediaQualifiers = map[string]bool{
	"anime": true, "manga": true, "video game": true, "book": true,
	"movie": true, "tv show": true, "comics": true, "cartoon": true,
	"novel": true, "webcomic": true, "play": true, "podcast": true,
	"book & movie": true, "anime & manga": true, "original work": true,
}

// canonicalTails are the right-hand sides that make a tag canonical: the
// two spellings AO3 uses for a fandom whose medium is not named.
var canonicalTails = map[string]bool{
	"all media types": true,
	"fandom":          true,
}

// Shape classifies tag names as fandom-shaped, using the CORPUS.
//
// It is a type rather than a bare function because the rule's third
// clause is about the corpus, not about the name: "X - <author>" is a
// fandom only if X has a canonical spelling SOMEWHERE ELSE. A function
// that sees only a name cannot apply that clause, and applying it
// wrongly (treating every "X - Y" as a fandom) measured 8 false
// positives against AO3's own typed markup -- "naruto - freeform",
// "marvel - freeform", "one piece - freeform" and five others.
//
// The measured shape of the rule, against 486 scraped work pages with
// AO3's own "<dd class=\"fandom tags\">" markup (843 fandom tag names,
// 3,286 freeform tag names, and no name ever both -- so fandom-ness is a
// property of the NAME, and the mirror's work_tags.tag_type column
// cannot answer it, flattening 3,890,504 rows to "freeforms"):
//
//	"X - all media types"    canonical, always a fandom
//	"X - fandom"             canonical, always a fandom
//	"X (<media qualifier>)"  fandom in a named medium
//	"X - freeform|character|relationship"   a TYPE, never a fandom
//	"X - <author>"           a creator form: a fandom IFF X is a
//	                          canonical base in the corpus
//
// Measured with the corpus-derived base set: precision 1.000 (0 false
// positives), recall 0.450. The false negatives are franchises whose
// only spelling is a creator form -- "a song of ice and fire - george r.
// r. martin" -- and those are admitted by an explicit declaration in
// families.json instead, which is why the declaration carries a
// per-spelling override.
type Shape struct {
	// bases are the canonical fandom bases: the X of every "X - all
	// media types" and "X - fandom" tag in the corpus.
	bases map[string]bool
}

// LoadShape reads the canonical base set from the corpus.
//
// The read is one scan of the tags table, filtered on the two canonical
// tails. On the live mirror that is 634,027 tags and takes about 2.3
// seconds -- so a Shape is built ONCE per query, not once per tag.
// Do not call LoadShape inside a per-work loop.
func LoadShape(ctx context.Context, db *sql.DB) (*Shape, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM tags WHERE name LIKE '% - all media types' OR name LIKE '% - fandom'`)
	if err != nil {
		return nil, fmt.Errorf("tagfamily: load shape: %w", err)
	}
	defer rows.Close()
	sh := &Shape{bases: map[string]bool{}}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("tagfamily: load shape: %w", err)
		}
		base, ok := canonicalBase(name)
		if ok {
			sh.bases[base] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tagfamily: load shape: %w", err)
	}
	return sh, nil
}

// canonicalBase returns the canonical base of a canonically-shaped name.
func canonicalBase(name string) (string, bool) {
	low := strings.ToLower(strings.TrimSpace(name))
	for _, tail := range []string{" - all media types", " - fandom"} {
		if strings.HasSuffix(low, tail) {
			return strings.TrimSpace(low[:len(low)-len(tail)]), true
		}
	}
	return "", false
}

// IsFandom reports whether a tag NAME names a fandom.
func (sh *Shape) IsFandom(name string) bool {
	low := strings.ToLower(strings.TrimSpace(name))
	if low == "" {
		return false
	}
	if i := strings.LastIndex(low, " - "); i > 0 {
		left := strings.TrimSpace(low[:i])
		right := strings.TrimSpace(low[i+3:])
		switch {
		case canonicalTails[right]:
			// "X - all media types" / "X - fandom": canonical.
			return true
		case typeMarkers[right]:
			// "X - freeform" / "X - character": a TYPE. This check
			// WINS over the creator-form reading below, because
			// "naruto - freeform" has a canonical base on the left
			// and is still a freeform tag.
			return false
		}
		// A creator form: "<fandom> - <author>". It is a fandom only
		// if the left side is a canonical base somewhere in the corpus.
		return sh.bases[left]
	}
	// Parenthetical medium: "Star Wars (movie)".
	if i := strings.LastIndex(low, " ("); i > 0 && strings.HasSuffix(low, ")") {
		qualifier := strings.TrimSpace(low[i+2 : len(low)-1])
		if mediaQualifiers[qualifier] {
			return true
		}
	}
	return false
}

// Base returns the franchise part of a fandom tag name, or "" if the name
// is not fandom-shaped.
//
// "worm - fandom" -> "worm"; "star wars (movie)" -> "star wars";
// "harry potter - j. k. rowling" -> "harry potter".
func (sh *Shape) Base(name string) string {
	low := strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndex(low, " - "); i > 0 {
		left := strings.TrimSpace(low[:i])
		right := strings.TrimSpace(low[i+3:])
		if canonicalTails[right] || sh.bases[left] {
			return left
		}
	}
	if i := strings.LastIndex(low, " ("); i > 0 && strings.HasSuffix(low, ")") {
		if mediaQualifiers[strings.TrimSpace(low[i+2:len(low)-1])] {
			return strings.TrimSpace(low[:i])
		}
	}
	return ""
}
```

**Step 1d — run the test and SEE IT PASS.**

```bash
cd ~/code-local/go/kindred && go test ./internal/tagfamily/ -run TestShape -v
```
EXPECT: `--- PASS: TestShapeClassifiesNamesByTheCorpus` and
`--- PASS: TestShapeTreatsATypeMarkerAsWinningOverACanonicalBase`, and
`ok git.polarisocial.xyz/kindred/kindred/internal/tagfamily`.

**Step 1e — gofmt + commit.**

```bash
cd ~/code-local/go/kindred && gofmt -w internal/tagfamily/ && go test ./internal/tagfamily/ && git add -A && git commit -m "tagfamily: corpus-derived fandom-shape rule (measured P=1.00 R=0.45)"
```

> **The commit message says R=0.45, not R=0.40.** The corrected rule's recall against the
> oracle is 0.450 (136 TP / 302 creator-form labels); the earlier 0.397 figure was measured
> with a base set that did not include the oracle's own canonical spellings.

---

### Task 2 — `internal/tagfamily`: declared families + corpus validation (TDD)

**Step 2a — write the failing test.** Create `internal/tagfamily/families_test.go`:

```go
package tagfamily

import (
	"context"
	"testing"
)

func TestFamiliesLoadAndCarryTheDeclaredMembers(t *testing.T) {
	fams, err := LoadFamilies()
	if err != nil {
		t.Fatalf("LoadFamilies: %v", err)
	}
	u, ok := fams.Universe("worm")
	if !ok {
		t.Fatal("universe \"worm\" not found")
	}
	if len(u.Spellings) == 0 {
		t.Fatal("worm universe has no spellings")
	}
	found := false
	for _, s := range u.Spellings {
		if s == "parahumans series - wildbow" {
			found = true
		}
	}
	if !found {
		t.Errorf("worm spellings missing the dominant spelling; got %v", u.Spellings)
	}
	c, ok := fams.Concept("isekai")
	if !ok {
		t.Fatal("concept \"isekai\" not found")
	}
	if len(c.Tags) == 0 {
		t.Fatal("isekai concept has no tags")
	}
}

func TestValidateReportsAMissingTagRatherThanIgnoringIt(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	mustExec(t, db, `INSERT INTO tags (id,name) VALUES (1,'real fandom - all media types')`)
	// "missing fandom - all media types" is deliberately NOT in the table.
	f := &Universe{Name: "t", Spellings: []string{
		"real fandom - all media types",
		"missing fandom - all media types",
	}}
	// The Shape is the corpus-derived rule from Task 1. It is built from
	// the SAME fixture, so "real fandom" is a canonical base here.
	sh, err := LoadShape(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadShape: %v", err)
	}
	rep, err := ValidateUniverse(context.Background(), db, sh, f)
	if err != nil {
		t.Fatalf("ValidateUniverse: %v", err)
	}
	if len(rep.Missing) != 1 || rep.Missing[0] != "missing fandom - all media types" {
		t.Errorf("Missing = %v, want exactly the absent spelling", rep.Missing)
	}
	if len(rep.Present) != 1 {
		t.Errorf("Present = %v, want the one that exists", rep.Present)
	}
	// "real fandom - all media types" IS fandom-shaped, so it must not
	// be reported as a non-fandom member.
	if len(rep.NonFandom) != 0 {
		t.Errorf("NonFandom = %v, want none", rep.NonFandom)
	}
}
```

> **`openTestDB` and `mustExec` live in `internal/tagfamily/helpers_test.go`** (written in Task 1).
> **Do not define them again in this file** — a duplicate declaration is a compile error in the
> whole package, so it takes down Task 1's tests too. This file's import block is therefore
> only:
>
> ```go
> import (
> 	"context"
> 	"testing"
> )
> ```
>
> (`database/sql` is not needed here, and `modernc.org/sqlite` is not either — the helper file
> already imports the driver, and one import of a driver is enough for the package.)

**Step 2b — run it and SEE IT FAIL.**

```bash
cd ~/code-local/go/kindred && go test ./internal/tagfamily/ -run 'TestFamilies|TestValidate' 2>&1 | head -20
```
EXPECT: compile errors — `undefined: LoadFamilies`, `undefined: ValidateUniverse`, `undefined: Universe`.

**Step 2c — create the data file.** `internal/tagfamily/families.json`:

```json
{
  "universes": {
    "worm": {
      "label": "Worm (Parahumans)",
      "spellings": [
        "parahumans series - wildbow",
        "wildbow - fandom",
        "worm - fandom",
        "worm - wildbow",
        "worm (web serial novel)",
        "worm",
        "worm - freeform"
      ],
      "spellings_explicit_freeform": ["worm - freeform"]
    }
  },
  "concepts": {
    "isekai": {
      "label": "isekai / transmigration",
      "tags": [
        "isekai",
        "reverse isekai",
        "technically an isekai",
        "isekai au",
        "isekai and transmigration",
        "reverse isekai and transmigration",
        "isekai'd",
        "isekai?",
        "isekai shenanigans"
      ]
    }
  }
}
```

**Step 2d — implement.** Create `internal/tagfamily/families.go`:

```go
package tagfamily

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

//go:embed families.json
var familiesJSON []byte

// Universe is one franchise's set of fandom spellings.
//
// It is DECLARED, not derived, and the corpus cannot derive it: the
// spellings of one franchise are near-disjoint populations (measured: the
// two biggest Worm spellings share 17 of ~2,000 works each), and character
// tags do not bridge franchises either (measured: "harry potter - j. k.
// rowling" and "star wars - all media types" share 4,619 character tags).
// What the corpus CAN do is validate the declaration, which is what
// ValidateUniverse does.
type Universe struct {
	Name      string   `json:"-"`
	Label     string   `json:"label"`
	Spellings []string `json:"spellings"`
	// SpellingsExplicitFreeform lists spellings that are freeform-shaped
	// by name but still mean this universe to a reader ("worm - freeform").
	// They are admitted by DECLARATION, and the validation report says so,
	// so a reader can see which members were admitted by rule.
	SpellingsExplicit []string `json:"spellings_explicit"`
}

// Concept is one idea's set of tag spellings.
//
// A concept is also declared, for the opposite reason: the token "isekai"
// occurs across 14,126 works in the corpus, including names scoped to
// another fandom ("pureblood culture (harry potter)"), so the token alone
// is far too wide to be the family.
type Concept struct {
	Name  string   `json:"-"`
	Label string   `json:"label"`
	Tags  []string `json:"tags"`
}

// Families is the loaded family set.
type Families struct {
	universes map[string]*Universe
	concepts  map[string]*Concept
}

// Universe returns a declared universe by key.
func (f *Families) Universe(name string) (*Universe, bool) {
	u, ok := f.universes[name]
	return u, ok
}

// Concept returns a declared concept by key.
func (f *Families) Concept(name string) (*Concept, bool) {
	c, ok := f.concepts[name]
	return c, ok
}

// UniverseNames lists the universe keys, sorted.
func (f *Families) UniverseNames() []string {
	out := make([]string, 0, len(f.universes))
	for k := range f.universes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Universes lists the declared universes, sorted by key.
//
// It returns the universes themselves, not just their keys, because the
// web page needs each one's Label to show the reader ("Worm (Parahumans)")
// while the URL carries the key ("worm").
func (f *Families) Universes() []*Universe {
	out := make([]*Universe, 0, len(f.universes))
	for _, k := range f.UniverseNames() {
		out = append(out, f.universes[k])
	}
	return out
}

// Concepts lists the declared concepts, sorted by key.
func (f *Families) Concepts() []*Concept {
	out := make([]*Concept, 0, len(f.concepts))
	for _, k := range f.ConceptNames() {
		out = append(out, f.concepts[k])
	}
	return out
}

// ConceptNames lists the concept keys, sorted.
func (f *Families) ConceptNames() []string {
	out := make([]string, 0, len(f.concepts))
	for k := range f.concepts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LoadFamilies parses the embedded family declarations.
func LoadFamilies() (*Families, error) {
	var raw struct {
		Universes map[string]*Universe `json:"universes"`
		Concepts  map[string]*Concept  `json:"concepts"`
	}
	if err := json.Unmarshal(familiesJSON, &raw); err != nil {
		return nil, fmt.Errorf("tagfamily: parse families.json: %w", err)
	}
	f := &Families{universes: raw.Universes, concepts: raw.Concepts}
	for k, u := range f.universes {
		u.Name = k
	}
	for k, c := range f.concepts {
		c.Name = k
	}
	return f, nil
}

// UniverseReport is the result of checking a declared universe against
// the corpus. It exists so a member that does not exist is REPORTED, not
// silently skipped: a filter that quietly drops a term looks like one that
// did not fire.
type UniverseReport struct {
	Present   []string `json:"present"`
	Missing   []string `json:"missing"`
	NonFandom []string `json:"non_fandom_shape"`
	Explicit  []string `json:"explicit_freeform"`
}

// ValidateUniverse checks every declared spelling against the mirror.
//
// The shape argument is the corpus-derived Shape from Task 1: whether a
// spelling "looks like a fandom" is a question about the corpus, not
// about the name, so the caller builds the Shape once and passes it in.
func ValidateUniverse(ctx context.Context, db *sql.DB, sh *Shape, u *Universe) (UniverseReport, error) {
	var rep UniverseReport
	explicit := map[string]bool{}
	for _, s := range u.SpellingsExplicit {
		explicit[s] = true
	}
	for _, s := range u.Spellings {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tags WHERE name = ? COLLATE NOCASE`, s).Scan(&n); err != nil {
			return rep, fmt.Errorf("tagfamily: validate %s: %w", s, err)
		}
		if n == 0 {
			rep.Missing = append(rep.Missing, s)
			continue
		}
		rep.Present = append(rep.Present, s)
		if !sh.IsFandom(s) {
			if explicit[s] {
				rep.Explicit = append(rep.Explicit, s)
			} else {
				rep.NonFandom = append(rep.NonFandom, s)
			}
		}
	}
	return rep, nil
}
```

> **The `ValidateUniverse` signature changed** — it now takes a `*Shape`. **Its caller in Task 2's
> test must be updated**: `ValidateUniverse(context.Background(), db, sh, fams.Universe("worm"))`
> — and the test must build the `sh` first with `LoadShape(context.Background(), db)`.
> The test's fixture `tags` table must therefore contain the canonical spellings the rule
> consults, or every spelling will be reported as `NonFandom`.

> **Note for the implementer:** the loop above queries `tags` once per spelling with
> `QueryRowContext`. That is one round trip per member of a family — fine for a family of seven
> spellings, and it is what keeps the report's "which spelling is missing" answer exact. Do not
> "optimise" it into a single `IN` query without also rewriting the report: the report is the
> point, and a batched query that loses the per-spelling answer would be a faster way to be wrong.

**Step 2e — run the tests and SEE THEM PASS.**

```bash
cd ~/code-local/go/kindred && go test ./internal/tagfamily/ -v 2>&1 | tail -20
```
EXPECT: `--- PASS: TestFamiliesLoadAndCarryTheDeclaredMembers`,
`--- PASS: TestValidateReportsAMissingTagRatherThanIgnoringIt`, `ok ...internal/tagfamily`.

**Step 2f — commit.**

```bash
cd ~/code-local/go/kindred && gofmt -w internal/tagfamily/ && go test ./internal/tagfamily/ && git add -A && git commit -m "tagfamily: declared universes/concepts + corpus validation report"
```

---

### Task 3 — Generalise the query: family × family intersection with recency (TDD)

This replaces `internal/wormisekai`. The query is: *works carrying a tag from universe U **and** a
tag from concept C, updated within N days, ranked by `log10(kudos+1)/sqrt(age+1)`, with evidence.*

**Step 3a — write the failing test.** Create `internal/tagfamily/query_test.go`:

```go
package tagfamily

import (
	"context"
	"testing"
	"time"
)

func TestQueryIntersectsTwoFamiliesAndKeepsOnlyRecentWorks(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE works (id INTEGER PRIMARY KEY, title TEXT, url TEXT, authors TEXT,
		kudos INTEGER, word_count INTEGER, rating TEXT, complete INTEGER, update_date TEXT)`)
	mustExec(t, db, `CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	mustExec(t, db, `CREATE TABLE work_tags (work_id INTEGER, tag_id INTEGER, tag_type TEXT)`)
	mustExec(t, db, `INSERT INTO tags (id,name) VALUES
		(1,'worm - fandom'),(2,'isekai'),(3,'harry potter - all media types')`)
	mustExec(t, db, `INSERT INTO work_tags (work_id,tag_id,tag_type) VALUES
		(10,1,'freeforms'),(10,2,'freeforms'),
		(11,1,'freeforms'),(11,3,'freeforms'),
		(12,1,'freeforms'),(12,2,'freeforms')`)

	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	mustExec(t, db, `INSERT INTO works (id,title,url,authors,kudos,word_count,rating,complete,update_date) VALUES
		(10,'both and recent','u','a',100,1000,'General',1,'01 Oct 2026'),
		(11,'only the fandom','u','a',100,1000,'General',1,'01 Oct 2026'),
		(12,'both but stale','u','a',100,1000,'General',1,'01 Oct 2024')`)

	res, err := Query(context.Background(), db, QueryOptions{
		UniverseTags: []string{"worm - fandom"},
		ConceptTags:  []string{"isekai"},
		RecentDays:   365,
		Now:          now,
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1: %+v", len(res.Items), res.Items)
	}
	got := res.Items[0]
	if got.ID != 10 {
		t.Errorf("ID = %d, want 10", got.ID)
	}
	if got.AgeDays != 20 {
		t.Errorf("AgeDays = %d, want 20", got.AgeDays)
	}
	if len(got.MatchedTags) != 1 || got.MatchedTags[0] != "isekai" {
		t.Errorf("MatchedTags = %v, want [isekai]", got.MatchedTags)
	}
	if res.Meta.UniverseWorks != 3 {
		t.Errorf("UniverseWorks = %d, want 3", res.Meta.UniverseWorks)
	}
	if res.Meta.ConceptWorks != 2 {
		t.Errorf("ConceptWorks = %d, want 2", res.Meta.ConceptWorks)
	}
}

func TestQueryReportsAWorkThatAlsoCarriesAnotherFandom(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE works (id INTEGER PRIMARY KEY, title TEXT, url TEXT, authors TEXT,
		kudos INTEGER, word_count INTEGER, rating TEXT, complete INTEGER, update_date TEXT)`)
	mustExec(t, db, `CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	mustExec(t, db, `CREATE TABLE work_tags (work_id,tag_id,tag_type TEXT)`)
	mustExec(t, db, `INSERT INTO tags (id,name) VALUES
		(1,'worm - fandom'),(2,'isekai'),(3,'harry potter - all media types')`)
	mustExec(t, db, `INSERT INTO work_tags (work_id,tag_id,tag_type) VALUES
		(10,1,'freeforms'),(10,2,'freeforms'),(10,3,'freeforms')`)
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	mustExec(t, db, `INSERT INTO works (id,title,url,authors,kudos,word_count,rating,complete,update_date)
		VALUES (10,'crossover','u','a',100,1000,'General',1,'01 Oct 2026')`)

	res, err := Query(context.Background(), db, QueryOptions{
		UniverseTags: []string{"worm - fandom"},
		ConceptTags:  []string{"isekai"},
		RecentDays:   365,
		Now:          now,
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(res.Items))
	}
	if res.Items[0].OtherFandom != "harry potter" {
		t.Errorf("OtherFandom = %q, want %q", res.Items[0].OtherFandom, "harry potter")
	}
}
```

> **`mustExec` and `openTestDB` live in `internal/tagfamily/helpers_test.go`** (written in Task 1).
> **Do not redefine them in this file** — a duplicate declaration is a compile error in the whole
> package, so it takes down every other test in it. This file's import block is therefore:
>
> ```go
> import (
> 	"context"
> 	"testing"
> 	"time"
> )
> ```
>
> (`database/sql` is not needed here, and neither is the sqlite driver import — the helper file
> already imports it, and one driver import per package is enough.)

**Step 3b — run it and SEE IT FAIL.**

```bash
cd ~/code-local/go/kindred && go test ./internal/tagfamily/ -run TestQuery 2>&1 | head -20
```
EXPECT: compile errors — `undefined: Query`, `undefined: QueryOptions`.

**Step 3c — implement.** Create `internal/tagfamily/query.go`:

```go
package tagfamily

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// DefaultRecentDays is the recency window when a caller names none.
const DefaultRecentDays = 365

// DefaultLimit is the number of rows returned when a caller names none.
const DefaultLimit = 50

// QueryOptions configures a family-intersection query.
type QueryOptions struct {
	// UniverseTags are the fandom spellings a work must carry at least
	// one of. This is the franchise axis.
	UniverseTags []string
	// ConceptTags are the concept spellings a work must carry at least
	// one of. This is the idea axis.
	ConceptTags []string
	// RecentDays keeps only works updated within the window. Zero means
	// DefaultRecentDays.
	RecentDays int
	// Now pins the clock so a test can pin recency.
	Now time.Time
	// Limit caps the rows returned.
	Limit int
}

// Item is one matching work, with the evidence for why it matched.
type Item struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Authors  string `json:"authors"`
	Kudos    int64  `json:"kudos"`
	Words    int64  `json:"words"`
	Rating   string `json:"rating"`
	Complete bool   `json:"complete"`
	// UpdateDate is the mirror's raw "DD Mon YYYY" string, kept verbatim
	// because that is what the mirror stores and what a reader can check.
	UpdateDate string `json:"update_date"`
	// AgeDays is the parsed age in days, or -1 when the date could not
	// be parsed.
	AgeDays int `json:"age_days"`
	// MatchedTags are the concept tags the work carries.
	MatchedTags []string `json:"matched_tags,omitempty"`
	// OtherFandom is a DIFFERENT fandom the work carries, when it has
	// one. It is the general "this story crosses universes" fact, and it
	// is empty for a work that stays inside one universe.
	OtherFandom string `json:"other_fandom,omitempty"`
	// Score is the ranking score, exposed so the order can be checked.
	Score float64 `json:"score"`
}

// Meta reports the pool sizes so an empty list is explainable.
type Meta struct {
	UniverseWorks int `json:"universe_works"`
	ConceptWorks  int `json:"concept_works"`
	Matched       int `json:"matched"`
	Returned      int `json:"returned"`
	RecentDays    int `json:"recent_days"`
}

// Result is a query result plus the counts that explain it.
type Result struct {
	Items []Item `json:"items"`
	Meta  Meta   `json:"meta"`
}

// Query answers: which works carry a tag from BOTH families, were updated
// within the window, and rank highest.
//
// The score is log10(kudos+1)/sqrt(ageDays+1): log because a work with
// 10,000 kudos is not ten times better than one with 1,000; the age term
// because a reader asking for "recent" wants the live ones near the top
// rather than buried under a 2019 classic. This is the same scorer the
// bespoke worm-isekai command used, moved here unchanged.
func Query(ctx context.Context, db *sql.DB, opts QueryOptions) (Result, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.RecentDays <= 0 {
		opts.RecentDays = DefaultRecentDays
	}
	if opts.Limit <= 0 {
		opts.Limit = DefaultLimit
	}
	// The Shape is built ONCE per query, not once per work: LoadShape
	// scans the tags table and takes about 2.3 seconds against the live
	// mirror. otherFandom uses it for every row.
	sh, err := LoadShape(ctx, db)
	if err != nil {
		return Result{}, err
	}
	universe, err := workIDsForTags(ctx, db, opts.UniverseTags)
	if err != nil {
		return Result{}, err
	}
	concept, err := workIDsForTags(ctx, db, opts.ConceptTags)
	if err != nil {
		return Result{}, err
	}
	var res Result
	res.Meta.UniverseWorks = len(universe)
	res.Meta.ConceptWorks = len(concept)
	res.Meta.RecentDays = opts.RecentDays

	ids := make([]int64, 0, len(universe))
	for id := range universe {
		if concept[id] {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	items := make([]Item, 0, len(ids))
	for _, id := range ids {
		it, ok, err := work(ctx, db, id, opts.Now)
		if err != nil {
			return Result{}, err
		}
		if !ok || it.AgeDays < 0 || it.AgeDays > opts.RecentDays {
			continue
		}
		it.MatchedTags = matchedTags(ctx, db, id, opts.ConceptTags)
		it.OtherFandom = otherFandom(ctx, db, sh, id, opts.UniverseTags)
		items = append(items, it)
	}
	res.Meta.Matched = len(items)
	for i := range items {
		items[i].Score = score(items[i])
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		return items[i].ID < items[j].ID
	})
	if len(items) > opts.Limit {
		items = items[:opts.Limit]
	}
	res.Items = items
	res.Meta.Returned = len(items)
	return res, nil
}

// score is log10(kudos+1) / sqrt(ageDays+1).
func score(it Item) float64 {
	age := float64(it.AgeDays)
	if age < 0 {
		age = 0
	}
	return math.Log10(float64(it.Kudos)+1) / math.Sqrt(age+1)
}

// ageDays parses the mirror's "DD Mon YYYY" date.
//
// The date is TEXT, so it cannot be compared as a string: measured, the
// lexicographic maximum ("31 Oct 2025") is a year older than the
// chronological maximum ("26 Jun 2026"). Every recency decision here is
// made in Go for that reason.
func ageDays(updateDate string, now time.Time) int {
	if updateDate == "" {
		return -1
	}
	t, err := time.Parse("2 Jan 2006", updateDate)
	if err != nil {
		return -1
	}
	return int(now.Sub(t).Hours() / 24)
}

// workIDsForTags returns every work carrying at least one of the names.
func workIDsForTags(ctx context.Context, db *sql.DB, names []string) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(names) == 0 {
		return out, nil
	}
	q := `SELECT DISTINCT wt.work_id FROM work_tags wt
		JOIN tags t ON t.id = wt.tag_id
		WHERE t.name COLLATE NOCASE IN (` + placeholders(len(names)) + `)`
	args := make([]any, 0, len(names))
	for _, n := range names {
		args = append(args, n)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("tagfamily: tag query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// work fetches one work row.
func work(ctx context.Context, db *sql.DB, id int64, now time.Time) (Item, bool, error) {
	const q = `SELECT id, title, url, authors, kudos, word_count, rating, complete,
		COALESCE(update_date,'') FROM works WHERE id = ?`
	var it Item
	var complete int64
	err := db.QueryRowContext(ctx, q, id).Scan(&it.ID, &it.Title, &it.URL, &it.Authors,
		&it.Kudos, &it.Words, &it.Rating, &complete, &it.UpdateDate)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("tagfamily: work %d: %w", id, err)
	}
	it.Complete = complete == 1
	it.AgeDays = ageDays(it.UpdateDate, now)
	return it, true, nil
}

// matchedTags returns the concept tags a work carries.
func matchedTags(ctx context.Context, db *sql.DB, id int64, tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	q := `SELECT t.name FROM work_tags wt JOIN tags t ON t.id = wt.tag_id
		WHERE wt.work_id = ? AND t.name COLLATE NOCASE IN (` + placeholders(len(tags)) + `)
		ORDER BY t.name`
	args := make([]any, 0, len(tags)+1)
	args = append(args, id)
	for _, n := range tags {
		args = append(args, n)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return out
		}
		out = append(out, n)
	}
	return out
}

// otherFandom returns a DIFFERENT fandom the work carries, if any.
//
// The rule is the corpus-derived Shape, not a list of freeform
// namespaces. The bespoke implementation carried a hand-measured list of
// 13 namespaces gathered from Worm works, which would be wrong for any
// other franchise; the shape rule was measured at precision 1.000 against
// AO3's own typed markup.
//
// The universe's own spellings are skipped, so a work is not reported as
// crossing into itself.
func otherFandom(ctx context.Context, db *sql.DB, sh *Shape, id int64, universeTags []string) string {
	own := map[string]bool{}
	for _, t := range universeTags {
		own[strings.ToLower(t)] = true
	}
	q := `SELECT t.name FROM work_tags wt JOIN tags t ON t.id = wt.tag_id WHERE wt.work_id = ?`
	rows, err := db.QueryContext(ctx, q, id)
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return ""
		}
		if own[strings.ToLower(n)] {
			continue
		}
		if !sh.IsFandom(n) {
			continue
		}
		// The fandom part is the left of " - ", or the name without its
		// parenthetical medium.
		base := sh.Base(n)
		if base == "" || own[base] {
			continue
		}
		return base
	}
	return ""
}

// placeholders builds "?,?,?" for an IN clause.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
```

**Step 3d — run the tests and SEE THEM PASS.**

```bash
cd ~/code-local/go/kindred && go test ./internal/tagfamily/ -v 2>&1 | tail -25
```
EXPECT: all SIX tests PASS — `TestShapeClassifiesNamesByTheCorpus`,
`TestShapeTreatsATypeMarkerAsWinningOverACanonicalBase`, `TestFamiliesLoad…`,
`TestValidateReports…`, `TestQueryIntersects…`, `TestQueryReports…`.

> **These six tests were compiled and run against this plan's own code on 2026-10-21** (in a
> scratch module, extracted verbatim from this document). All six pass, `go vet` is clean, and
> the only `gofmt` complaint was a struct-field alignment in `families.go`, which is now fixed in
> the text above.

**Step 3e — commit.**

```bash
cd ~/code-local/go/kindred && gofmt -w internal/tagfamily/ && go test ./internal/tagfamily/ && git add -A && git commit -m "tagfamily: family x family intersection query with recency, evidence and ranking"
```

---

### Task 4 — Expose it on the existing works API (TDD) + fix the recency ordering bug

`GET /api/v1/ao3/works` already parses filters into `[]worksFilter`. Two new filters —
`universe=` and `concept=` — turn it into the family-intersection query without a new endpoint.

**Step 4a — write the failing test.** Create `internal/api/browse_test.go`:

```go
package api

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestParseWorksFiltersAcceptsAFamilyPair(t *testing.T) {
	// The values are FAMILY KEYS, not tag names. The parser resolves
	// them against the embedded families.json into the spellings that
	// make the family, so this test needs no database.
	q := parseQuery(map[string][]string{
		"universe": {"worm"},
		"concept":  {"isekai"},
	})
	filters, err := parseWorksFilters(q)
	if err != nil {
		t.Fatalf("parseWorksFilters: %v", err)
	}
	clause := strings.Join(filtersClauses(filters), " AND ")
	if !strings.Contains(clause, "t.name COLLATE NOCASE IN") {
		t.Errorf("clause %q has no tag IN filter", clause)
	}
	if n := strings.Count(clause, "t.name COLLATE NOCASE IN"); n != 2 {
		t.Errorf("clause %q has %d tag IN filters, want 2", clause, n)
	}
	// The worm family's SPELLINGS must be the bound arguments, because
	// "worm" on its own is only 7 works out of the family's ~4,300.
	// They are arguments, not clause text -- the clause carries only
	// placeholders -- so the assertion has to look at the args.
	var args []any
	for _, f := range filters {
		args = append(args, f.args...)
	}
	joined := fmt.Sprint(args)
	if !strings.Contains(joined, "parahumans series - wildbow") {
		t.Errorf("args %v do not carry the worm family's spellings; the "+
			"family key was used as a tag name", args)
	}
	// And the concept family's too.
	if !strings.Contains(joined, "reverse isekai") {
		t.Errorf("args %v do not carry the isekai family's tags", args)
	}
}

func TestParseWorksFiltersRejectsAnUnknownFamily(t *testing.T) {
	q := parseQuery(map[string][]string{"universe": {"not-a-family"}})
	if _, err := parseWorksFilters(q); err == nil {
		t.Fatal("an unknown universe= was accepted; a filter that cannot fire " +
			"must be a 400, not a query that returns everything")
	}
}
```

> **Helper needed for the test** (add to `browse_test.go`): the test refers to two small helpers
> that do not exist yet. Add them to the same file:
>
> ```go
> // parseQuery builds url.Values from a map, for the test.
> func parseQuery(m map[string][]string) url.Values {
> 	v := url.Values{}
> 	for k, vals := range m {
> 		for _, val := range vals {
> 			v.Add(k, val)
> 		}
> 	}
> 	return v
> }
>
> // filtersClauses extracts the SQL clauses from parsed filters.
> func filtersClauses(filters []worksFilter) []string {
> 	out := make([]string, 0, len(filters))
> 	for _, f := range filters {
> 		out = append(out, f.clause)
> 	}
> 	return out
> }
> ```
>
> The test file's import block is therefore:
>
> ```go
> import (
> 	"net/url"
> 	"strings"
> 	"testing"
> )
> ```
>
> (`net/http` and `net/http/httptest` are **not** needed here — those are for the HTTP-level
> tests in Task 5, which live in a different file.)

**Step 4b — run it and SEE IT FAIL.**

```bash
cd ~/code-local/go/kindred && go test ./internal/api/ -run 'TestParseWorksFilters' 2>&1 | head -20
```
EXPECT: `clause "" has no tag IN filter` — `parseWorksFilters` **silently ignores** parameters it
does not recognise (it has no "unknown filter" branch; the function simply returns the clauses it
built, and `handleAO3Works` only reads the five filters it knows). So `?universe=…` produces **no
clause at all** and the filter is dropped, which is exactly the bug the "applied filters" echo in
`handleAO3Works` was written to expose. Record that message.

> **This is the failure mode the plan exists to remove.** A silently-ignored filter is worse than
> a rejected one: the caller asked for a restriction, the server returned every work, and nothing
> said so. The new filters must therefore be **added** to `parseWorksFilters`, and the endpoint's
> `applied` echo must list them (it already echoes `tag`, `complete`, `words`, `rating`, `lang` —
> `universe` and `concept` must join that list, see Step 4c).

**Step 4c — implement.** In `internal/api/server.go`, inside `parseWorksFilters` (line 775), add a
block **before the final `return out, nil`** (line 888) — after the `lang` filter. The parser has
**no switch on the parameter name** and **no "unknown filter" error branch**: each filter is an
independent `if v := strings.TrimSpace(q.Get("…")); v != "" { … }` block that appends to `out`.
So the new filters are two more such blocks, not two `case` arms:

```go
	// universe and concept. A FAMILY filter.
	//
	// The value is a FAMILY KEY -- "worm", "isekai" -- not a tag name.
	// It is resolved against the declared families
	// (internal/tagfamily/families.json) into the list of spellings
	// that make the family, and the work must carry AT LEAST ONE of
	// them. Two families together are an intersection, which is the
	// general "fandom x concept" question: a work has to carry
	// something from each side.
	//
	// A family key rather than a raw tag list, because every surface
	// of this feature speaks one vocabulary: the works API, the
	// /api/v1/browse endpoint, the `kindred browse` CLI and the /browse
	// page all take "worm" and "isekai". A raw ad-hoc tag is what the
	// EXISTING `tag` filter is for, and it still works; `universe` and
	// `concept` are the declared, named families.
	//
	// The clause is an EXISTS subquery rather than a JOIN because the
	// works table is joined to work_tags/tags only once, by the tag
	// filter, and a second JOIN of the same tables would multiply the
	// rows.
	//
	// An UNKNOWN family key is an error, not "no filter": a filter that
	// cannot fire is a request the caller did not mean, and silently
	// ignoring it returns every work while looking like a filter that
	// did.
	fams, err := tagfamily.LoadFamilies()
	if err != nil {
		return nil, err
	}
	for _, fam := range []string{"universe", "concept"} {
		v := strings.TrimSpace(q.Get(fam))
		if v == "" {
			continue
		}
		var names []string
		switch fam {
		case "universe":
			u, ok := fams.Universe(v)
			if !ok {
				return nil, fmt.Errorf("unknown universe %q; known: %s",
					v, strings.Join(fams.UniverseNames(), ", "))
			}
			names = u.Spellings
		case "concept":
			c, ok := fams.Concept(v)
			if !ok {
				return nil, fmt.Errorf("unknown concept %q; known: %s",
					v, strings.Join(fams.ConceptNames(), ", "))
			}
			names = c.Tags
		}
		ph := make([]string, 0, len(names))
		args := make([]any, 0, len(names))
		for _, n := range names {
			ph = append(ph, "?")
			args = append(args, n)
		}
		out = append(out, worksFilter{
			clause: "EXISTS (SELECT 1 FROM work_tags wt JOIN tags t ON t.id = wt.tag_id " +
				"WHERE wt.work_id = w.id AND t.name COLLATE NOCASE IN (" + strings.Join(ph, ",") + "))",
			args: args,
		})
	}
```

> **The `tagfamily` import.** `internal/api/server.go` will need
> `"git.polarisocial.xyz/kindred/kindred/internal/tagfamily"`. **Check whether it is already
> imported** — the `handleBrowse` handler in Task 5 needs it too, so add it once.
>
> **This is the one place where the `parseWorksFilters` unit test needs a real database.** The
> filter now resolves a family key through the EMBEDDED `families.json`, so the test does not
> need the corpus at all — `LoadFamilies` reads the embedded file, not the mirror. **The test
> therefore still works with no database open**, and the clause it produces is checked by
> inspecting the returned `[]worksFilter`, exactly as the plan's other parser test does.
>
> **`splitTagList` is no longer needed and is REMOVED from this task.** The parameter is a single
> family key, not a comma-separated list. If a later task needs a comma-separated tag list, put
> that helper back then.

**Also add the two filters to the endpoint's `applied` echo.** In `handleAO3Works`
(`internal/api/server.go:996`) the loop reads:

```go
	for _, k := range []string{"tag", "complete", "words", "rating", "lang"} {
```

Change it to:

```go
	for _, k := range []string{"tag", "universe", "concept", "complete", "words", "rating", "lang"} {
```

Without this the endpoint would apply the filter and not report it — the exact "your filter was
ignored" ambiguity the comment above that loop exists to prevent.

**Step 4d — fix the recency ordering bug (F7).** Two sites:

`internal/api/server.go` line ~916:
```go
		order = "w.update_date DESC"
```
becomes
```go
		order = corpus.ChronologicalUpdateDateDesc
```

`internal/web/handlers.go` line ~2898:
```go
		return "CASE WHEN w.update_date IS NULL OR w.update_date = '' THEN 1 ELSE 0 END, w.update_date DESC"
```
becomes
```go
		return corpus.ChronologicalUpdateDateDesc
```

Then add to `internal/corpus/` a new file `internal/corpus/dates.go`:

```go
package corpus

// ChronologicalUpdateDateDesc is a SQL expression that orders works by
// their update_date in CHRONOLOGICAL order, most recent first.
//
// The mirror stores update_date as TEXT in AO3's "DD Mon YYYY" form, so a
// plain `update_date DESC` sorts it as a string: measured on the live
// mirror, that yields "31 Oct 2025" as the most recent work, while the
// true most recent is "26 Jun 2026". The day is two digits, so the string
// order is dominated by the day-of-month and the month name is never
// compared.
//
// The expression below splits the string into year, month name and day and
// compares them numerically. It is used for every "recent" ordering, so
// that the site's recent-first list and the API's date sort agree with
// the calendar.
//
// The DESCENDING direction is part of the constant, not left to the
// caller. That is deliberate: every use of it is "most recent first", and
// a bare key expression would let a caller append nothing and get
// ASCENDING order -- which is exactly the bug the first draft of this
// constant had, and it was caught only by running the existing sort tests
// (they listed [1,2,3,4,5,6,7,8] instead of [8,7,6,5,4,3,2,1]).
//
// A NULL or empty date becomes '0000-00-00', which sorts LAST in a
// descending list, so an undated work is at the end rather than the
// beginning.
const ChronologicalUpdateDateDesc = `
CASE
	WHEN w.update_date IS NULL OR w.update_date = '' THEN '0000-00-00'
	WHEN instr(w.update_date, ' ') > 0 THEN
		substr(w.update_date, -4) || '-' ||
		CASE substr(w.update_date, instr(w.update_date, ' ') + 1, 3)
			WHEN 'Jan' THEN '01' WHEN 'Feb' THEN '02' WHEN 'Mar' THEN '03'
			WHEN 'Apr' THEN '04' WHEN 'May' THEN '05' WHEN 'Jun' THEN '06'
			WHEN 'Jul' THEN '07' WHEN 'Aug' THEN '08' WHEN 'Sep' THEN '09'
			WHEN 'Oct' THEN '10' WHEN 'Nov' THEN '11' WHEN 'Dec' THEN '12'
			ELSE '00' END || '-' ||
		substr('0' || substr(w.update_date, 1, instr(w.update_date, ' ') - 1), -2)
	ELSE substr(w.update_date, 1, 10)
END DESC`
```

> **The expression handles BOTH date formats, and it has to.** The mirror stores `update_date` as
> AO3's `DD Mon YYYY` (measured: 112,892 rows of length 11, 43 empty), but the repo's own TEST
> FIXTURES use ISO `YYYY-MM-DD` (`internal/api/tagsort_test.go` seeds `"2026-01-01"` …). A
> single-format expression sorts the ISO fixtures as garbage — `substr(update_date, -4)` on
> `"2026-01-01"` is `"-01-01"`, and the month lookup finds no space, so every ISO row collapses
> to the same key and the order is arbitrary.
>
> **The `instr(update_date, ' ') > 0` test is the format discriminator:** a space means the
> `DD Mon YYYY` form (which needs parsing); no space means ISO (which already sorts correctly,
> and is passed through as `substr(…, 1, 10)` so a full ISO timestamp also works).
>
> **This was found by running the existing test suite, not by reading it.** The plan's first
> version of this expression was single-format and it broke six subtests in
> `internal/api/tagsort_test.go` (`?sort=recent` listed `[1,2,3,4,5,6,7,8]` instead of
> `[8,7,6,5,4,3,2,1]`).

Verify the `corpus` package is imported in both files; if not, add it.

> **Verified (2026-10-21):** both files **already import** `internal/corpus` —
> `internal/api/server.go:29` and `internal/web/handlers.go:19`. No import change is needed.
>
> **The expression hardcodes the alias `w.` — and both call sites use it, so it works. This is a
> constraint, not an accident.** The SQL is:
>
> ```
> CASE WHEN w.update_date IS NULL OR w.update_date = '' THEN '0000-00-00'
> ELSE substr(w.update_date, -4) || '-' || CASE substr(w.update_date, instr(w.update_date, ' ')+1, 3) … END || '-' || substr('0' || substr(w.update_date, 1, instr(w.update_date, ' ')-1), -2)
> END
> ```
>
> It refers to `w.update_date`, so the query **must** alias the works table as `w`. Both sites do:
> - `internal/api/server.go:929` — `FROM works w`
> - `internal/web/handlers.go:1796` — `FROM work_tags wt JOIN works w ON w.id = wt.work_id`
>
> **If you ever use this constant in a query that does not alias `works` as `w`, it will fail with
> `no such column: w.update_date`.** That is the error this exact expression produced when it was
> first run against the mirror as `FROM works` (unaliased) — it is not hypothetical.
>
> **Verified against the live mirror (2026-10-21):** with the alias, the expression's top row is
> `26 Jun 2026` and the plain `ORDER BY update_date DESC` top row is `31 Oct 2025` — the two
> disagree, which is F7. The expression also parses correctly for a single-digit day
> (`substr('0' || …, -2)` pads `5 Jan 2021` to `05`).

**Step 4e — run the API tests and SEE THEM PASS.**

```bash
cd ~/code-local/go/kindred && go test ./internal/api/ ./internal/web/ ./internal/corpus/ -count=1
```
EXPECT: `ok` for all three packages. If the date-order change breaks an existing web test that
asserted the old (wrong) ordering, that test is asserting a bug — update its expectation to the
chronological value and say so in the commit message.

**Step 4f — commit.**

```bash
cd ~/code-local/go/kindred && gofmt -w internal/ && go test ./internal/... -count=1 && git add -A && git commit -m "api: universe=/concept= family filters on /api/v1/ao3/works; fix lexicographic 'recent' ordering"
```

---

### Task 5 — A dedicated browse endpoint that returns the evidence (TDD)

`/api/v1/ao3/works` returns works, but the *question* "which family matched?" needs the
`Meta` counts and the per-item `other_fandom`. A small dedicated endpoint returns exactly that.

**Step 5a — write the failing test.** Create `internal/api/browse_endpoint_test.go`:

```go
package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestHandleBrowseReturnsTheFamilyQueryWithItsCounts(t *testing.T) {
	ts := newTestServer(t)
	// The parameters are FAMILY KEYS: "worm" and "isekai", the same
	// vocabulary the CLI and the /browse page use.
	resp, err := http.Get(ts.URL + "/api/v1/browse?universe=worm&concept=isekai&recent=365&n=5")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, readAll(t, resp))
	}
	body := readAll(t, resp)
	for _, want := range []string{`"items"`, `"meta"`, `"universe_works"`, `"concept_works"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

func TestHandleBrowseRejectsAQueryWithNoFamily(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/v1/browse")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (a query with no family asks nothing)", resp.StatusCode)
	}
}

func TestHandleBrowseRejectsAnUnknownFamily(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/v1/browse?universe=not-a-family")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (an unknown family must not match "+
			"everything silently)", resp.StatusCode)
	}
}
```

> **The imports are already there.** Verified 2026-10-21: `internal/api/server.go` imports
> `"fmt"` (line 20), `"strings"` (line 25) and `"errors"` (line 19) already, and `writeJSON(w,
> status, body any)` (line 1296) and `writeErr(w, status, err error)` (line 1309) have exactly
> the signatures this handler uses. **The only import the plan adds is `"internal/tagfamily"`.**

> **Verified test conventions (2026-10-21).** `newTestServer(t)` exists at
> `internal/api/server_test.go:41` and returns an **`*httptest.Server`** — there is no
> `srv.Handler` to call directly, and `srv.Routes()` is the mux. The pattern used by the
> existing API tests is:
>
> ```go
> ts := newTestServer(t)
> resp, err := http.Get(ts.URL + "/api/v1/...?...")
> ```
>
> **The helper `readAll(t, resp)` ALREADY EXISTS** — it is at `internal/api/block_test.go:292`.
> Do NOT add it to the test file: Go will not compile a second declaration of the same
> function in a package, and the error (`readAll redeclared in this block`) is the only sign
> that the plan's first draft of this step was wrong. Reuse the existing one, which is why
> this test file's import block needs no `io`:
>
> ```go
> import (
> 	"net/http"
> 	"strings"
> 	"testing"
> )
> ```
>
> **Note** `readAll` consumes the body, so in the first
> test the `defer resp.Body.Close()` is still right but the body can only be read once — read it
> once into `body` and assert against that, as the code above does.
>
> **The 200 case depends on the test corpus carrying a family.** `newTestServer` builds a fixture
> corpus. If it has no work tagged `worm - fandom` **and** `isekai`, the handler returns 200 with
> an empty `items` array — which still satisfies the assertions above (the keys are present), so
> the test is valid either way. The counts being non-zero is checked against the **live mirror**
> in Step 5d, not here.

**Step 5b — run it and SEE IT FAIL.**

```bash
cd ~/code-local/go/kindred && go test ./internal/api/ -run TestHandleBrowse 2>&1 | head -20
```
EXPECT: `status = 404, want 200` — the catch-all route (`internal/api/server.go:153`) answers
`no route for GET /api/v1/browse` for any path not explicitly registered. Record that message.

**Step 5b-pre — satisfy the repo's own route-audit gate.** `internal/api/routes_audit_test.go`
parses `server.go` with `go/ast`, reads every `mux.HandleFunc` pattern, and requires each one to
appear in the `routeTable` map in that file:

```go
func TestEveryAPIRouteIsAccountedFor(t *testing.T) {
	// The direction that matters: a route nobody recorded. Adding an endpoint
	// and forgetting this file is the whole failure mode.
	for _, pattern := range routesInSource(t) {
		if _, ok := routeTable[pattern]; !ok {
			t.Errorf("route %q is registered but not accounted for: give it a page or record why it has none", pattern)
		}
	}
}
```

**The plan's first draft of Task 5 did not include this step, and the gate caught it:**
`route "GET /api/v1/browse" is registered but not accounted for`. There is a second test,
`TestTheRouteAuditHasNoStaleEntries`, which goes the OTHER way — an entry for a route that no
longer exists is a table describing a server that is not running — so a route cannot be left
behind either.

**Add the entry to the `routeTable` map in `internal/api/routes_audit_test.go`, in the
"reached by a page" group** (the `/browse` page of Task 6 is the page that reaches it):

```go
	"GET /api/v1/browse":                 {Page: "/browse"},
```

**Step 5c — implement.** In `internal/api/server.go`:

1. Register the route. **It must go BEFORE the catch-all `mux.HandleFunc("/", …)` at line 153** —
   `ServeMux` prefers the most specific pattern, but the catch-all is registered last on purpose
   and the ordering must be preserved. Put it with the other GET reads, next to
   `mux.HandleFunc("GET /api/v1/ao3/works", s.handleAO3Works)` (line 116):
```go
	mux.HandleFunc("GET /api/v1/browse", s.handleBrowse)
```

2. Add the handler. Create `internal/api/browse.go` — a NEW file, so the endpoint is
   separable from `server.go` and deletable without touching it:

```go
package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/tagfamily"
)

// handleBrowse answers the general "fandom family x concept family"
// question and returns the evidence for it.
//
// It exists beside /api/v1/ao3/works rather than inside it because the
// works endpoint answers a different question: it returns a list of works
// with a filter applied, and it cannot say how large each pool was or
// which fandom a work crossed into. Those counts are what make an empty
// or short list explainable.
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	universeKey := strings.TrimSpace(q.Get("universe"))
	conceptKey := strings.TrimSpace(q.Get("concept"))
	if universeKey == "" && conceptKey == "" {
		writeErr(w, http.StatusBadRequest,
			errors.New("universe and concept are both empty; name at least one family"))
		return
	}
	// The values are FAMILY KEYS -- "worm", "isekai" -- the same
	// vocabulary the works API, the CLI and the /browse page use. They
	// are resolved against the declared families here, so an unknown key
	// is a 400 naming the keys that exist, rather than a query that
	// silently matches nothing.
	fams, err := tagfamily.LoadFamilies()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var universeTags, conceptTags []string
	if universeKey != "" {
		u, ok := fams.Universe(universeKey)
		if !ok {
			writeErr(w, http.StatusBadRequest,
				fmt.Errorf("unknown universe %q; known: %s",
					universeKey, strings.Join(fams.UniverseNames(), ", ")))
			return
		}
		universeTags = u.Spellings
	}
	if conceptKey != "" {
		c, ok := fams.Concept(conceptKey)
		if !ok {
			writeErr(w, http.StatusBadRequest,
				fmt.Errorf("unknown concept %q; known: %s",
					conceptKey, strings.Join(fams.ConceptNames(), ", ")))
			return
		}
		conceptTags = c.Tags
	}
	recent := atoiDefault(q.Get("recent"), tagfamily.DefaultRecentDays)
	limit := clamp(atoiDefault(q.Get("n"), tagfamily.DefaultLimit), 1, 200)
	res, err := tagfamily.Query(r.Context(), s.Engine.Corpus.DB, tagfamily.QueryOptions{
		UniverseTags: universeTags,
		ConceptTags:  conceptTags,
		RecentDays:   recent,
		Limit:        limit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Echo the applied filters, for the same reason handleAO3Works does: a
	// client must be able to tell "your filter matched nothing" from
	// "your filter was ignored".
	applied := map[string]string{}
	for _, k := range []string{"universe", "concept"} {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			applied[k] = v
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":   res.Items,
		"meta":    res.Meta,
		"applied": applied,
	})
}
```

> **Verified against the codebase (2026-10-21).** The names below are the real ones:
>
> | Need | Real name | Where |
> |---|---|---|
> | JSON response | `writeJSON(w, status, body)` | `internal/api/server.go:1296` |
> | Error response | `writeErr(w, status, err)` — **not** `httpError` | used at `internal/api/server.go:923` |
> | Corpus DB handle | `s.Engine.Corpus.DB` — there is **no** `s.corpusDB()` and **no** `s.hasCorpus()` | `internal/api/server.go:958` |
> | Int parse | `atoiDefault(s string, def int) int` | `internal/api/server.go:1403` |
> | Int clamp | `clamp(n, lo, hi)` — the API package's helper; `clampInt(n, lo, hi)` is the **web** package's | `internal/api/server.go:909` |
>
> **There is no corpus-presence check on `*Server`.** `handleAO3Works` does not make one — it
> queries `s.Engine.Corpus.DB` directly and lets a nil-pointer-free query error surface as a 500.
> The handler below does the same: **do not invent `hasCorpus()`**. If the corpus is absent the
> query fails and `writeErr(w, 500, err)` reports it, exactly as the works endpoint does.
>
> Also note `handleAO3Works` echoes the applied filters back (`applied` map, `internal/api/server.go:996`).
> **`/api/v1/browse` must do the same** for `universe` and `concept`, for the same reason the
> comment there gives: a client must be able to tell "your filter matched nothing" from "your
> filter was ignored".

3. Import the new package:
```go
	"git.polarisocial.xyz/kindred/kindred/internal/tagfamily"
```

**Step 5d — verify against the LIVE mirror** (this is the real proof, since the test DB may not
carry a family):

```bash
cd ~/code-local/go/kindred && go build -o bin/kindred ./cmd/kindred && \
  curl -s "http://127.0.0.1:8099/api/v1/browse?universe=worm%20-%20fandom&concept=isekai&recent=365&n=3"
```
EXPECT (if the server is running on 8099): a JSON object with `"items"` and a `"meta"` whose
`"universe_works"` is > 0. If no server is running, run the query directly against the mirror
with `sqlite3` and record the counts in the commit message instead.

**Step 5e — commit.**

```bash
cd ~/code-local/go/kindred && gofmt -w internal/api/ && go test ./internal/api/ -count=1 && git add -A && git commit -m "api: GET /api/v1/browse — family x family query with pool counts and evidence"
```

---

### Task 6 — The reader-facing page `/browse` (TDD, then markup)

The site gains ONE page, under the EXISTING "Find something to read" nav group, built from the
existing `/tag/` list markup and the existing `form.controls` filter widget.

**Step 6a — write the failing test.** Append to `internal/web/registry_test.go`:

```go
func TestBrowsePageIsRegisteredAndRouted(t *testing.T) {
	spec, _, ok := findPage("/browse")
	if !ok {
		t.Fatal("/browse is not in the page registry")
	}
	if spec.Nav == "" {
		t.Error("/browse has no Nav, so no nav item can mark it current")
	}
	if spec.Handler == nil {
		t.Error("/browse has no Handler")
	}
	if !spec.List {
		t.Error("/browse renders a list of rows but does not declare List: true, " +
			"so the two-sided empty-state contract is not applied to it")
	}
}
```

**Step 6b — run it and SEE IT FAIL.**

```bash
cd ~/code-local/go/kindred && go test ./internal/web/ -run TestBrowsePage 2>&1 | head -20
```
EXPECT: `--- FAIL: TestBrowsePageIsRegisteredAndRouted ... /browse is not in the page registry`.

**Step 6c — implement the registry entry.** In `internal/web/registry.go`, add to the `specs` slice
in `allPages()` (right after the `/neighbours` entry, which is at line ~223):

```go
		{
			Template: "browse.html",
			Paths:    []string{"/browse"},
			Nav:      "/browse",
			List:     true,
			Handler: func(d Deps, w http.ResponseWriter, r *http.Request, _ string) {
				d.renderBrowse(w, r)
			},
		},
```

> **Verified field names (2026-10-21).** `pageSpec` is at `internal/web/registry.go:55` and its
> fields are: `Template`, `Paths`, `Prefix`, `Nav`, `List`, `AcceptsPost`, `Handler`.
> **There is no `Path`, no `Title`, no `Render` and no `Heading` field** — the page's title and
> heading come from the `Base` struct the handler builds, and the handler is a
> `func(d Deps, w http.ResponseWriter, r *http.Request, arg string)`.
>
> **`List: true` is REQUIRED, not optional.** `TestEveryListPageMarksItsEmptyState`
> (`internal/web/registry_test.go:199`) reads the template file and fails if a list page's
> template does not contain the literal `data-empty="true"`. And
> `TestListPagesEmptyStateIsTwoSided` (`internal/web/registry_test.go:267`) fails if the marker
> is present on an element that renders ALONGSIDE rows — the empty state must be in a branch that
> only renders when the list is empty. Both are verified by the sweep, on the real page.
>
> `Nav` must be a path the nav actually contains, because `navKey(r.URL.Path)` (used by
> `d.baseOf`, `internal/web/handlers.go:2643`) resolves the current-nav marker from it, and
> `layout.html` marks the current item with `{{if eq .NavKey "..."}}`.

**Step 6d — implement the page.** In `internal/web/handlers.go`:

```go
// renderBrowse answers "recent works in this fandom that are this concept".
//
// The page is a form over one query: a universe (a fandom family), a
// concept (an idea family), and a recency window. Both families come from
// the declared set in internal/tagfamily, so adding a fandom or a concept
// to the site is a data edit, not a code edit.
func (d Deps) renderBrowse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	page := BrowsePage{Base: d.baseOf(r, "Browse by fandom and concept", "Browse by fandom and concept")}
	fams, err := tagfamily.LoadFamilies()
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	for _, u := range fams.Universes() {
		page.UniverseOptions = append(page.UniverseOptions,
			SortOption{Value: u.Name, Label: u.Label})
	}
	for _, c := range fams.Concepts() {
		page.ConceptOptions = append(page.ConceptOptions,
			SortOption{Value: c.Name, Label: c.Label})
	}
	page.Universe = q.Get("universe")
	page.Concept = q.Get("concept")
	page.RecentDays = clampInt(atoiDefault(q.Get("recent"), tagfamily.DefaultRecentDays), 1, 3650)
	page.N = clampInt(atoiDefault(q.Get("n"), 50), 1, 200)
	page.Searched = page.Universe != "" || page.Concept != ""

	if page.Searched {
		var universeTags, conceptTags []string
		if u, ok := fams.Universe(page.Universe); ok {
			universeTags = u.Spellings
		}
		if c, ok := fams.Concept(page.Concept); ok {
			conceptTags = c.Tags
		}
		if len(universeTags) > 0 || len(conceptTags) > 0 {
			res, err := tagfamily.Query(ctx, d.Engine.Corpus.DB, tagfamily.QueryOptions{
				UniverseTags: universeTags,
				ConceptTags:  conceptTags,
				RecentDays:   page.RecentDays,
				Limit:        page.N,
			})
			if err != nil {
				d.fail(w, r, http.StatusInternalServerError, err)
				return
			}
			page.Meta = res.Meta
			// The rows are converted to WorkHit so the SHARED workrow
			// template renders them. A second row markup for one page
			// would be a second thing to keep in step with the first.
			for _, it := range res.Items {
				page.Works = append(page.Works, WorkHit{
					ID:        it.ID,
					Title:     it.Title,
					Author:    it.Authors,
					URL:       it.URL,
					Kudos:     it.Kudos,
					WordCount: it.Words,
					Complete:  boolToInt(it.Complete),
				})
			}
		}
	}
	d.page(w, "browse.html", page, http.StatusOK)
}
```

> **Two helpers the code above assumes, which may or may not exist — check before writing:**
> - `fams.Universes()` / `fams.Concepts()` returning **slices** (not the `UniverseNames()` /
>   `ConceptNames()` key-list methods from Task 2). **Add them to `internal/tagfamily/families.go`**
>   in Task 2, returning `[]*Universe` and `[]*Concept` sorted by key, so the page can show a
>   `Label` next to a `Name`. If you prefer not to add them, use the existing key-list methods and
>   make the option `Label` equal to the key.
> - `boolToInt` — a one-line helper (`func boolToInt(b bool) int64 { if b { return 1 }; return 0 }`).
>   `WorkHit.Complete` is an `int64`, not a bool. If the web package already has such a helper, use
>   it; otherwise add this one to `internal/web/render.go` next to `WorkHit`.
>
> The conversion deliberately drops `it.OtherFandom` and `it.MatchedTags`: the shared `workrow`
> template has no slot for them, and adding one would change every work row on the whole site.
> **The per-item evidence is therefore shown on the API and the CLI, not on the card** — see
> open question 2 in §6, which asks whether that is acceptable to the owner.

> **Verified names (2026-10-21).**
>
> | Need | Real name | Where |
> |---|---|---|
> | Corpus DB | `d.Engine.Corpus.DB` — **no** `d.corpusDB()` and **no** `d.hasCorpus()` exist | `internal/web/handlers.go:1627` shows `d.Engine.Corpus.DB` in use |
> | Render the page | `d.page(w, "browse.html", page, http.StatusOK)` — the `render(w, r, name, data)` shape in this plan does **not** exist | `internal/web/handlers.go:2988` |
> | Base struct | `d.baseOf(r, title, heading)` → `Base{Title, Heading, NavKey, ...}` | `internal/web/handlers.go:2643` |
> | Int helpers | `atoiDefault(s, def)` and `clampInt(n, lo, hi)` — **the web package's** names, both in `internal/web/handlers.go:3088` and `:3099` | these are correct for the web package |
> | Failure | `d.fail(w, r, status, err)` | `internal/web/handlers.go:3014` |
>
> The page renders through `d.page`, which buffers the template, renders it, and only then writes
> the status — so a template error becomes a clean 500 rather than a half-written page. Do not call
> `Render` directly.

**Step 6e — add the page struct.** In `internal/web/render.go`, next to `TagPage`:

```go
// BrowsePage is the "fandom x concept" browse page.
type BrowsePage struct {
	Base
	// UniverseOptions and ConceptOptions are the declared families, as
	// select options, so the form can show a label ("Worm (Parahumans)")
	// while the URL carries the key ("worm").
	UniverseOptions []SortOption
	ConceptOptions  []SortOption
	// Universe and Concept are the current selections, by key.
	Universe string
	Concept  string
	// RecentDays is the recency window in days.
	RecentDays int
	// N is the page size.
	N int
	// Searched is true once a family has been named. It separates "the
	// reader has not asked yet" from "the reader asked and found
	// nothing" -- two states that must not share a sentence.
	Searched bool
	// Works are the matching works, as WorkHit rows so the shared
	// workrow template renders them.
	Works []WorkHit
	// Meta reports the pool sizes.
	Meta tagfamily.Meta
}
```

**Step 6f — write the template.** Create `internal/web/assets/browse.html`.

> **Verified template contract (2026-10-21).** A page template is **not** a whole HTML document:
> - It is `{{define "content"}} … {{end}}` — **not** `{{define "browse.html"}}` and **not** a
>   `{{template "base"}}` call. `layout.html` is `{{define "layout"}}` and the renderer executes
>   the **"layout"** wrapper (`internal/web/render.go:881`); a page's own `{{define "content"}}`
>   is what fills the layout's content slot. `tag.html` opens with `{{define "content"}}` at
>   line 1 and closes with `{{end}}` at its last line.
> - There is **no** `<main class="wrap">` and **no** `<h1>` in a page template: the heading comes
>   from `Base.Heading`, which `layout.html` prints. `tag.html` starts straight into
>   `<p class="count">`.
> - Rows are rendered with the shared `{{template "workrow" .}}` (defined in `layout.html:136`),
>   which expects a `WorkHit`-shaped row: `ID, Title, Author, URL, Summary, Kudos, Hits,
>   WordCount, Language, Complete, HasTag, ShowScore, Score` (`internal/web/render.go:271`).
>   **The browse page must therefore render `[]WorkHit`, not `[]tagfamily.Item`** — the shared row
>   template cannot render a foreign struct, and hand-rolling a second row markup for one page
>   is exactly the duplication this plan is avoiding. Convert in the handler (Step 6d).
> - A **list page must carry the literal `data-empty="true"`** on the element that says "nothing
>   found", and that element must be in a branch that renders **only** when the list is empty
>   (`TestEveryListPageMarksItsEmptyState` and `TestListPagesEmptyStateIsTwoSided`).
> - The filter control is `<form class="controls" method="get" action="…" role="search">` with
>   `<label for="…">` + `<select id="…" name="…">`, and it carries a hidden `n` so the reader's
>   chosen count survives a filter change (see the comment in `tag.html`).

With those constraints, the template is:

```html
{{define "content"}}
{{/* A GET form, so a result is a URL a reader can bookmark or share --
     the same reason the tag page's filter form is a GET. */}}
<form class="controls" method="get" action="/browse" role="search"
      aria-label="Filter works by fandom and concept">
  {{/* The reader's ?n= rides along, or every filter change would snap the
       list back to the default while the heading kept claiming otherwise. */}}
  <input type="hidden" name="n" value="{{.N}}">

  <label for="universe">Fandom</label>
  <select id="universe" name="universe">
    <option value="">any fandom</option>
    {{$u := .Universe}}
    {{range .UniverseOptions}}
      <option value="{{.Value}}" {{if eq .Value $u}}selected{{end}}>{{.Label}}</option>
    {{end}}
  </select>

  <label for="concept">Concept</label>
  <select id="concept" name="concept">
    <option value="">any concept</option>
    {{$c := .Concept}}
    {{range .ConceptOptions}}
      <option value="{{.Value}}" {{if eq .Value $c}}selected{{end}}>{{.Label}}</option>
    {{end}}
  </select>

  <label for="recent">Updated within</label>
  <select id="recent" name="recent">
    <option value="30" {{if eq .RecentDays 30}}selected{{end}}>30 days</option>
    <option value="90" {{if eq .RecentDays 90}}selected{{end}}>90 days</option>
    <option value="365" {{if eq .RecentDays 365}}selected{{end}}>one year</option>
    <option value="730" {{if eq .RecentDays 730}}selected{{end}}>two years</option>
  </select>

  <input type="submit" value="Search">
</form>

{{if .Searched}}
  {{/* The counts are on the page because a short list is otherwise
       unexplainable: "3 results" and "0 results" look identical until
       the reader can see how large each pool was. */}}
  <p class="count" data-testid="browse-counts">
    {{.Meta.Matched}} work{{if ne .Meta.Matched 1}}s{{end}} matched &mdash;
    fandom pool {{commas .Meta.UniverseWorks}},
    concept pool {{commas .Meta.ConceptWorks}},
    window {{.Meta.RecentDays}} days.
  </p>

  {{if .Works}}
    {{range .Works}}{{template "workrow" .}}{{end}}
  {{else}}
    <p class="count" data-testid="browse-empty" data-empty="true">
      No work matched that combination. Widen the recency window, or pick
      another concept &mdash; the counts above say how large each pool was.
    </p>
  {{end}}
{{else}}
  <p class="count">
    Pick a fandom and a concept to see recent works that carry both.
  </p>
{{end}}
{{end}}
```

> **Note for the implementer:** the CSS class names above (`count`, `controls`, `byline`) are the
> ones `tag.html` and `layout.html` actually use. **The `data-testid` hooks are new** — the
> browser suite (`e2e/tests/`) selects on them, and the site's convention is that a specific
> element gets a specific hook (`data-testid="no-matches"`, `data-testid="works-heading"`).

**Step 6g — add the nav item.** The nav lives in `internal/web/assets/layout.html`, in the
`<div class="nav-group">` whose heading is "Find something to read" (around line 48). Add one
`<li>` to that group's `<ul>`, after the "Tag neighbours" entry (line 54):

```html
          <li><a href="/browse"{{if eq .NavKey "/browse"}} class="is-current" aria-current="page"{{end}}>Browse by fandom</a></li>
```

> **The `{{if eq .NavKey "/browse"}}` wrapper is required**, not decorative: it is how every other
> nav item marks itself current, and `navKey(r.URL.Path)` (`internal/web/handlers.go:2643` via
> `d.baseOf`) is what feeds it. An `<a href="/browse">` without it renders a link that can never
> be marked current — the item works but the site cannot say where the reader is.
>
> **The nav label is short on purpose.** The existing group reads: Search, Rank from seeds,
> Fandoms, Underrated, Tag neighbours, Surprise me. A sixth item labelled "Browse by fandom and
> concept" is longer than any of them and reads as a different kind of thing; "Browse by fandom"
> says the same thing in the same voice. **Confirm the label with the owner** (see §6, open
> question 1) — but do not shorten it below the point where a reader can tell it is a *filter*
> and not another list of tags.

**Step 6h — add the page to the RENDER SWEEP. This step is REQUIRED, not optional.**

`TestListPagesEmptyStateIsTwoSided` (`internal/web/registry_test.go:267`) does not read the
template — it **renders** each list page, populated and empty, from the table
`pageRenderCases` (`internal/web/render_pages_test.go:181`). And at the end of that test
(`registry_test.go:322`) it walks every list page and fails if the page has **no empty case**:

```
browse.html is a list page with no case that renders it empty, so a marker
moved off its empty branch would survive this test and the source scan alike;
add a case with hasEmptyList: true
```

So the browse page needs **two** entries in `pageRenderCases` — one populated, one empty. Add
them to `internal/web/render_pages_test.go`, next to the `tag.html` cases:

```go
	{
		name: "browse.html",
		tpl:  "browse.html",
		data: BrowsePage{
			Base:    sweepBase,
			Searched: true,
			Universe: "worm",
			Concept:  "isekai",
			UniverseOptions: []SortOption{{Value: "worm", Label: "Worm (Parahumans)"}},
			ConceptOptions:  []SortOption{{Value: "isekai", Label: "isekai"}},
			RecentDays: 365,
			N:          50,
			Works: []WorkHit{
				{ID: 1, Title: "W1", Author: "A", Kudos: 10, Hits: 100,
					WordCount: 5000, Language: "en", Complete: 1,
					URL: "https://example.invalid/works/1"},
			},
			Meta: tagfamily.Meta{
				UniverseWorks: 100, ConceptWorks: 20, Matched: 1, Returned: 1, RecentDays: 365,
			},
		},
		wantContains: []string{
			// The counts are on the page, because "1 result" and
			// "0 results" look identical until the reader can see
			// how large each pool was.
			"100", "20", "365",
			// The form is a GET, so the result is a shareable URL.
			`name="universe"`, `name="concept"`,
			// And the row is the SHARED row template.
			"/work/1", "5,000 words",
		},
		// A populated render must NOT carry the empty marker.
		wantNotContains: []string{"data-empty"},
	},
	{
		// A search that matched nothing says so, and names the pools
		// rather than claiming the fandom is empty.
		name:         "browse.html with no matches",
		tpl:          "browse.html",
		hasEmptyList: true,
		data: BrowsePage{
			Base:    sweepBase,
			Searched: true,
			Universe: "worm",
			Concept:  "isekai",
			UniverseOptions: []SortOption{{Value: "worm", Label: "Worm (Parahumans)"}},
			ConceptOptions:  []SortOption{{Value: "isekai", Label: "isekai"}},
			RecentDays: 365,
			N:          50,
			Works:      nil,
			Meta: tagfamily.Meta{
				UniverseWorks: 100, ConceptWorks: 20, Matched: 0, Returned: 0, RecentDays: 365,
			},
		},
		wantContains: []string{
			`data-empty="true"`,
			// The pools are still stated, so the reader can tell
			// "no overlap" from "no data".
			"100", "20",
		},
	},
```

> **The `tagfamily` import.** `render_pages_test.go` will need
> `"git.polarisocial.xyz/kindred/kindred/internal/tagfamily"` for `tagfamily.Meta`. **Check
> whether it is already imported** — if the file already imports it, do not add a second import.
>
> **`wantNotContains: []string{"data-empty"}` on the populated case is the point of the whole
> sweep.** The two-sided test fails a populated render that carries a marker, because a marker
> that shows with rows is "the same lie the other way round". A page that renders its empty
> state *always* — not only when the list is empty — is a page that tells every reader it found
> nothing.
>
> The `browse.html` template is picked up by the renderer automatically: `pages` is built from
> the registry (`internal/web/render.go:580`, `pageTemplates()`), so registering the page with
> `Template: "browse.html"` is what puts it in the map. **Do not add a separate registration** —
> `renderOne` fails with `no template set for browse.html` if the registry entry is missing, and
> a second registration would be a second thing to keep in step.

**Step 6i — run the web tests.**

```bash
cd ~/code-local/go/kindred && go test ./internal/web/ -count=1
```
EXPECT: `ok git.polarisocial.xyz/kindred/kindred/internal/web`. The registry sweep test
(`TestEveryPageHasAHandler`, `TestEveryRoutedPageDispatches`) will FAIL if the handler is not
wired — that is the point of the sweep, so fix the wiring rather than the test.

**Step 6i — verify in a browser** (the site change must be *seen*, not just asserted):

```bash
cd ~/code-local/go/kindred && make build && ./bin/kindred serve --corpus ~/code/python/ao3/recommender/cache/ao3_metadata.db &
sleep 3
curl -s -o /dev/null -w "%{http_code}\n" "http://127.0.0.1:8099/browse?universe=worm&concept=isekai&recent=365"
```
EXPECT: `200`. Then open `http://127.0.0.1:8099/browse?universe=worm&concept=isekai&recent=365` in a
browser and confirm: the page has an `<h1>`, the form shows the two selected options, the count
line is non-zero, and at least one work title links to its AO3 URL. **Record the counts you saw.**

**Step 6j — commit.**

```bash
cd ~/code-local/go/kindred && gofmt -w internal/web/ && go test ./internal/web/ -count=1 && git add -A && git commit -m "web: /browse page — fandom x concept, reusing the tag list form and rows"
```

---

### Task 7 — Replace the bespoke CLI with a general one

The bespoke `worm-isekai` command becomes a general `browse` command whose families are named.

**Step 7a — write the failing test.** The repo's CLI tests are sparse; assert on the package
instead. Create `cmd/kindred/browse_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

func TestPrintBrowseTableNamesBothFamilies(t *testing.T) {
	res := tagfamily.Result{
		Items: []tagfamily.Item{{
			ID: 7, Title: "a work", Kudos: 12, Words: 3400, AgeDays: 30,
			MatchedTags: []string{"isekai"}, OtherFandom: "harry potter",
		}},
		Meta: tagfamily.Meta{UniverseWorks: 10, ConceptWorks: 5, Matched: 1, Returned: 1, RecentDays: 365},
	}
	out := captureStdout(t, func() { printBrowseTable(res) })
	if !strings.Contains(out, "universe=10") || !strings.Contains(out, "concept=5") {
		t.Errorf("table does not report both pools: %s", out)
	}
	if !strings.Contains(out, "harry potter") {
		t.Errorf("table does not report the other fandom: %s", out)
	}
}
```

> **Note for the implementer:** `captureStdout` does not exist. Add it to the same test file:
>
> ```go
> // captureStdout runs f with os.Stdout redirected to a pipe and returns
> // what f printed.
> func captureStdout(t *testing.T, f func()) string {
> 	t.Helper()
> 	r, w, err := os.Pipe()
> 	if err != nil {
> 		t.Fatal(err)
> 	}
> 	old := os.Stdout
> 	os.Stdout = w
> 	done := make(chan string)
> 	go func() {
> 		b, _ := io.ReadAll(r)
> 		done <- string(b)
> 	}()
> 	f()
> 	w.Close()
> 	os.Stdout = old
> 	return <-done
> }
> ```
>
> Add `"io"` and `"os"` to the test file's imports, and
> `"git.polarisocial.xyz/kindred/kindred/internal/tagfamily"` too.

**Step 7b — run it and SEE IT FAIL.**

```bash
cd ~/code-local/go/kindred && go test ./cmd/kindred/ -run TestPrintBrowse 2>&1 | head -20
```
EXPECT: compile error — `undefined: printBrowseTable`.

**Step 7c — implement.** Create `cmd/kindred/browse.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/tagfamily"
)

// runBrowse answers the general "fandom family x concept family" question.
//
// It replaces the bespoke worm-isekai command: the families are named
// (--universe worm --concept isekai), and the families themselves live in
// internal/tagfamily/families.json, so asking about a different fandom or
// a different idea needs no code change.
func runBrowse(ctx context.Context, corpusPath string, args []string) error {
	fs := newFlagSet("browse")
	universe := fs.String("universe", "", "fandom family key (e.g. worm)")
	concept := fs.String("concept", "", "concept family key (e.g. isekai)")
	recent := fs.Int("recent", tagfamily.DefaultRecentDays, "recency window in days")
	limit := fs.Int("n", tagfamily.DefaultLimit, "number of results")
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	out := fs.String("out", "", "write the JSON payload to this path as well")
	c := bindConfig(fs)
	if _, err := finishConfig(c, fs, args); err != nil {
		return err
	}
	if *universe == "" && *concept == "" {
		return fmt.Errorf("browse: --universe and --concept are both empty; name at least one family")
	}

	db, closeDB, err := openCorpusForSubcommand(c.CorpusDB, args)
	if err != nil {
		return err
	}
	defer closeDB()

	fams, err := tagfamily.LoadFamilies()
	if err != nil {
		return err
	}
	var universeTags, conceptTags []string
	if *universe != "" {
		u, ok := fams.Universe(*universe)
		if !ok {
			return fmt.Errorf("browse: unknown universe %q; known: %s",
				*universe, strings.Join(fams.UniverseNames(), ", "))
		}
		universeTags = u.Spellings
	}
	if *concept != "" {
		cn, ok := fams.Concept(*concept)
		if !ok {
			return fmt.Errorf("browse: unknown concept %q; known: %s",
				*concept, strings.Join(fams.ConceptNames(), ", "))
		}
		conceptTags = cn.Tags
	}

	res, err := tagfamily.Query(ctx, db, tagfamily.QueryOptions{
		UniverseTags: universeTags,
		ConceptTags:  conceptTags,
		RecentDays:   *recent,
		Limit:        *limit,
	})
	if err != nil {
		return err
	}

	payload, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, append(payload, '\n'), 0o644); err != nil {
			return fmt.Errorf("browse: write %s: %w", *out, err)
		}
	}
	if *asJSON {
		fmt.Println(string(payload))
		return nil
	}
	printBrowseTable(res)
	return nil
}

// printBrowseTable prints the human-readable view.
func printBrowseTable(res tagfamily.Result) {
	fmt.Printf("%d matched (showing %d) | universe=%d concept=%d window=%dd\n\n",
		res.Meta.Matched, res.Meta.Returned, res.Meta.UniverseWorks,
		res.Meta.ConceptWorks, res.Meta.RecentDays)
	for _, it := range res.Items {
		extra := ""
		if it.OtherFandom != "" {
			extra = " -> " + it.OtherFandom
		}
		if len(it.MatchedTags) > 0 {
			extra += " [" + strings.Join(it.MatchedTags, ", ") + "]"
		}
		fmt.Printf("%-9d %5dd %5dk %7dw  %s%s\n",
			it.ID, it.AgeDays, it.Kudos, it.Words, it.Title, extra)
	}
}
```

> **Verified against the existing CLI (2026-10-21).** The helpers this uses all exist and are used
> the same way by the command it replaces (`cmd/kindred/wormisekai.go`):
> `newFlagSet(name)`, `bindConfig(fs)`, `finishConfig(c, fs, args)`, `openCorpusForSubcommand(c.CorpusDB, args)`
> (which returns `(*sql.DB, closeFunc, error)`).
> The `corpusPath` parameter is **unused** — the bespoke command took it and ignored it too, because
> `openCorpusForSubcommand` resolves the corpus itself. **Keep the parameter** so the call site in
> `main.go` stays uniform with the other subcommands; do not "clean it up" into a compile error
> about an unused parameter (Go allows unused function parameters, so it will build either way).
>
> `splitTags` (a comma-splitting helper) already exists in `cmd/kindred/wormisekai.go` and is
> **deleted along with that file** — the new command does not need it, because the families come
> from the declarations rather than from a comma list.

**Step 7d — wire it up and remove the bespoke command.**

In `cmd/kindred/main.go`:
- Replace the `"worm-isekai"` case (line ~79) with `"browse"`.
- Change the call to `runBrowse(ctx, config.Load().CorpusDB, args)`.
- **Replace the usage line** (line 33) — the command list in the `usage` string is what
  `scripts/check-doc-commands.sh` parses to learn the CLI's subcommands
  (`sed -n 's/^  \([a-z][a-z0-9-]*\)[ \t].*/\1/p'` on `kindred --help`), so a command that is not
  listed there is invisible to that gate:
  ```
    worm-isekai   recent Worm-universe works matching an isekai taste
  ```
  becomes
  ```
    browse      recent works matching a fandom family and a concept family
  ```
  **Keep the two-space indent and the column alignment** — the parser needs `^  <verb>` followed by
  a space or tab, and the alignment is what keeps the help readable.
- **Delete** `cmd/kindred/wormisekai.go` and the whole `internal/wormisekai/` directory.

```bash
cd ~/code-local/go/kindred && git rm -r --cached internal/wormisekai cmd/kindred/wormisekai.go >/dev/null && rm -rf internal/wormisekai cmd/kindred/wormisekai.go
```

> **Note the `sort` import.** `cmd/kindred/wormisekai.go` imports `sort` for its `splitTags`
> helper, and `splitTags` is the only user of it in that file. Deleting the file removes the
> import with it — but check whether `sort` is used elsewhere in `cmd/kindred/` before assuming
> `main.go` needs no change to its imports.

**Step 7e — verify the CLI against the live mirror.**

```bash
cd ~/code-local/go/kindred && go build -o bin/kindred ./cmd/kindred && \
  ./bin/kindred browse --universe worm --concept isekai --recent 365 --n 5 \
    --corpus ~/code/python/ao3/recommender/cache/ao3_metadata.db
```
EXPECT: a header line with non-zero `universe=` and `concept=` counts, followed by up to 5 rows
with titles, kudos, words and an age in days.

Also verify the error paths:
```bash
cd ~/code-local/go/kindred && ./bin/kindred browse --universe nosuchfandom --corpus ~/code/python/ao3/recommender/cache/ao3_metadata.db; echo "exit=$?"
```
EXPECT: a message naming the unknown key and listing the known ones, exit != 0.

**Step 7f — commit.**

```bash
cd ~/code-local/go/kindred && gofmt -w cmd/ && go test ./cmd/... -count=1 && git add -A && git commit -m "cli: replace worm-isekai with general 'browse --universe --concept'"
```

---

### Task 8 — Delete the bespoke package for good, and prove nothing referenced it

```bash
cd ~/code-local/go/kindred && grep -rn "wormisekai\|worm-isekai" --include=*.go --include=*.md --include=*.sh . | grep -v "^./docs/what-is-left.md"
```
EXPECT: **no output**. If any line appears, remove that reference and re-run.

```bash
cd ~/code-local/go/kindred && go build ./... && go test ./... -count=1
```
EXPECT: every package `ok` — including `internal/api` and `internal/web`.

```bash
cd ~/code-local/go/kindred && git add -A && git commit -m "chore: drop the bespoke worm-isekai package"
```

---

### Task 9 — Documentation and the gates

`make verify` runs four gates this change can break. Each is fixed by editing the document, not by
weakening the gate.

**Step 9a — document the new flags.** `scripts/check-cli-coverage.py` requires every
command-specific flag to be named in a document, and (once `browse` is in its `COMMANDS` list)
requires an example on a single line. Add to `docs/CLI.md`, in the section that lists commands:

```markdown
#### browse

    kindred browse --universe worm --concept isekai --recent 365 --n 50

Answers "recent works in this fandom that are this concept" over the AO3 mirror.
`--universe` and `--concept` name families declared in
`internal/tagfamily/families.json`; `--recent` is the recency window in days; `--n` caps the
results; `--json` emits the payload as JSON; `--out` writes it to a file. At least one of
`--universe` and `--concept` is required.

    kindred browse --universe worm --concept isekai --json --out worm-isekai.json
```

> **Two format requirements, both enforced by the gate and neither obvious:**
> 1. The example line must carry a **flag on the same line** as the verb — the gate's regex is
>    `kindred\s+browse\b[^\n`]*-{1,2}`, and `[^\n`]*` cannot cross a line break. A bare
>    `kindred browse` on its own line does **not** satisfy it.
> 2. The prose paragraphs **must not** contain a `kindred browse …` line that the
>    `check-doc-commands.sh` extractor will then try to RUN. That gate executes every
>    `kindred <verb> …` line it finds in `docs/*.md` and `README.md` against the binary, and
>    classifies a failure as BROKEN only when the CLI says "unknown command" or "unknown flag".
>    A line like `kindred browse --universe worm --concept isekai` in the prose is therefore
>    **executed** — it will fail with "--corpus is required", which is not a BROKEN verdict, so
>    it passes. But do not write a line with a flag that does not exist, or the gate will call it
>    broken. The indented code-block form above is what the extractor expects and handles.
>
> The second example is there on purpose: it is the invocation that produces the **file** the
> reader asked for in the original request (a recommendation file), and it exercises `--out`.

**Step 9b — run the doc gates.**

```bash
cd ~/code-local/go/kindred && go build -o bin/kindred ./cmd/kindred && ./scripts/check-doc-commands.sh
```
EXPECT: exit 0, and a line reporting the number of invocations checked.

```bash
cd ~/code-local/go/kindred && python3 scripts/check-cli-coverage.py
```
EXPECT: exit 0, and a line reporting that every flag is documented or exempt. **If a new flag is
reported uncovered, document it — do NOT add it to the `EXEMPT` map** (the script's own header
says an exemption is a decision a human must audit).

> **The gate has a hardcoded command list, and `browse` is not in it.**
> `scripts/check-cli-coverage.py:81` reads:
>
> ```python
> COMMANDS = [
>     "serve", "ingest", "recommend", "crawl", "profile", "rate",
>     "corpus-query", "stats", "dump", "verify", "fetch", "tune",
> ]
> ```
>
> **`worm-isekai` was never in that list**, so the command it replaces was never covered by the
> coverage gate — which is why it could carry an undocumented `--strict` flag. **Add `"browse"` to
> that list** so the new command IS covered:
>
> ```python
> COMMANDS = [
>     "serve", "ingest", "recommend", "crawl", "profile", "rate",
>     "corpus-query", "browse", "stats", "dump", "verify", "fetch", "tune",
> ]
> ```
>
> With `browse` in the list the gate enforces **two** things on it:
> 1. **Every flag it advertises is documented** (the flag scan reads `kindred browse -h`, so a
>    flag declared in Go but unreachable at runtime is still caught — that is the gate's stated
>    design, and the reason to add the command rather than leave it out).
> 2. **It has an example in `docs/`** — the gate requires the regex
>    `kindred\s+browse\b[^\n`]*-{1,2}` to match somewhere in `docs/**/*.md`. The `--json` example
>    in Step 9a satisfies it, but the example must be on **one line** with a flag on the same
>    line, or the regex will not match it.
>
> The flags `browse` advertises, and therefore must document, are: `universe`, `concept`,
> `recent`, `n`, `json`, `out` — plus whatever `bindConfig` adds globally (the gate checks the
> global set separately). `json` is in the gate's `EXEMPT` map already.

**Step 9c — run the spec gate.**

```bash
cd ~/code-local/go/kindred && python3 docs/goal-check.py
```
EXPECT: exit 0 (the goal is complete). If a clause now fails because a new package changed the
package count, **read the failing clause and fix the code or the baseline honestly** — the
script's own rules say a floor is measured with `--update-baseline`, never guessed.

> **The `page coverage` clause was ALREADY FAILING before this work, for a reason that has
> nothing to do with it: the gate reads two things from the Go source with regexes, and a
> refactor moved both of them.**
>
> | What the gate reads | Where it looked | Where it is now |
> |---|---|---|
> | the page list | a `[]string{...}` literal in `render.go` | `pageTemplates()`, which reads the registry |
> | the routes | `case p == "..."` in `handlers.go` | each `pageSpec`'s `Paths` in `registry.go` |
>
> Both regexes matched **nothing**, so the gate reported `could not find the page list in
> render.go` and then **every nav link as unresolvable** — 15 gaps, on a site whose nav links
> all resolve. A gate that fails on everything measures nothing.
>
> **The fix is to repair the gate, not to weaken it.** `docs/goal-check.py` now reads the page
> list and the routes from `internal/web/registry.go`, where the truth lives. The old regexes
> are kept as a FALLBACK (the `if not tpl_list:` branch), so a future refactor that moves the
> list back still works.
>
> **Verified by mutation:** changing the registry's `/browse` to `/browse-typo` makes the gate
> report `route /browse-typo is never requested by the browser suite`, and restoring it makes
> the gate green again. A gate that cannot fail is not a gate.
>
> **The e2e suite needs the new page too.** The same clause requires every route to be
> requested by `e2e/tests/*.spec.js`, so `/browse` was added to the `PAGES` table in
> `e2e/tests/pages.spec.js`:
>
> ```js
>   { path: '/browse', link: '/browse', name: 'browse', title: /browse/i, heading: true },
> ```

**Step 9d — run the whole verification.**

```bash
cd ~/code-local/go/kindred && make verify
```
EXPECT: gofmt clean, `go vet` silent, all tests `ok`, the spec gate green, both doc gates green,
the deploy script green, and the memory budget within its cap.

**Step 9e — update the "what is left" doc.**

In `docs/what-is-left.md`, replace any line that says the family-intersection query is
worm-specific or unimplemented with a line saying it is the general `/browse` capability, and
record the measured facts that bound it (the recall 0.397 of the shape rule, the declared-family
requirement, and the two fixed recency-ordering sites).

**Step 9f — commit.**

```bash
cd ~/code-local/go/kindred && git add -A && git commit -m "docs: document the browse command and the family capability"
```

---

### Task 10 — Final acceptance run (this is what "done" means)

Every claim below is checked by a command with a stated expected output. **Paste the real output
into the commit message** — a claim without output is not a claim.

```bash
cd ~/code-local/go/kindred && gofmt -l . | tee /dev/stderr | wc -l
```
EXPECT: `0`.

```bash
cd ~/code-local/go/kindred && go vet ./... && echo VET_OK
```
EXPECT: `VET_OK`.

```bash
cd ~/code-local/go/kindred && go test ./... -count=1
```
EXPECT: every package `ok`; **no** package reported as `[no test files]` for the new code path
(`internal/tagfamily` MUST have tests — `docs/goal-check.py` has a clause about untested packages).

```bash
cd ~/code-local/go/kindred && make verify
```
EXPECT: all gates green, end to end.

```bash
cd ~/code-local/go/kindred && go build -o bin/kindred ./cmd/kindred && \
  ./bin/kindred browse --universe worm --concept isekai --recent 365 --n 200 \
    --json --out /tmp/browse-worm-isekai.json \
    --corpus ~/code/python/ao3/recommender/cache/ao3_metadata.db && \
  python3 -c "import json;d=json.load(open('/tmp/browse-worm-isekai.json'));print('items',len(d['items']));print('meta',d['meta'])"
```
EXPECT: `items` ≤ 200 and a `meta` whose `universe_works` and `concept_works` are both > 0.
**Record the three numbers.**

```bash
cd ~/code-local/go/kindred && curl -s -o /dev/null -w "%{http_code}\n" "http://127.0.0.1:8099/browse?universe=worm&concept=isekai&recent=365"
```
EXPECT: `200`.

**Then, and only then, commit:**

```bash
cd ~/code-local/go/kindred && git add -A && git commit -m "verify: full acceptance run green; browse returns N items over M universe / K concept works"
```

---

## 5. Tests / validation summary

| Layer | Test | File |
|---|---|---|
| Rule | `TestShapeClassifiesNamesByTheCorpus` — 11 cases incl. all 3 type markers and 2 freeform namespaces; `TestShapeTreatsATypeMarkerAsWinningOverACanonicalBase` | `internal/tagfamily/shape_test.go` |
| Data | `TestFamiliesLoadAndCarryTheDeclaredMembers` — the worm universe and isekai concept load | `internal/tagfamily/families_test.go` |
| Validation | `TestValidateReportsAMissingTagRatherThanIgnoringIt` — a missing spelling is REPORTED, not skipped | `internal/tagfamily/families_test.go` |
| Query | `TestQueryIntersectsTwoFamiliesAndKeepsOnlyRecentWorks` — intersection, recency filter, age, matched tags, pool counts | `internal/tagfamily/query_test.go` |
| Query | `TestQueryReportsAWorkThatAlsoCarriesAnotherFandom` — the generalised "crosses universes" fact | `internal/tagfamily/query_test.go` |
| API | `TestParseWorksFiltersAcceptsAFamilyPair` — two family filters become two clauses | `internal/api/browse_test.go` |
| API | `TestParseWorksFiltersRejectsAnEmptyFamily` — an empty family is a 400, not a silent no-op | `internal/api/browse_test.go` |
| API | `TestHandleBrowseReturnsTheFamilyQueryWithItsCounts` — the payload carries `items` and `meta` | `internal/api/browse_test.go` |
| API | `TestHandleBrowseRejectsAQueryWithNoFamily` — a query with no family is a 400 | `internal/api/browse_test.go` |
| Web | `TestBrowsePageIsRegisteredAndRouted` — the page exists and has a nav key | `internal/web/registry_test.go` |
| Web | existing `TestEveryPageHasAHandler`, `TestEveryRoutedPageDispatches`, `TestListPagesEmptyStateIsTwoSided` | `internal/web/registry_test.go` |
| CLI | `TestPrintBrowseTableNamesBothFamilies` — the table reports both pools and the other fandom | `cmd/kindred/browse_test.go` |
| Gates | `make verify` — gofmt, vet, tests, spec, both doc gates, deploy, budget | `Makefile` |

**Mutation discipline.** Two of these tests exist because the corresponding bug is silent and has
already happened in this repo:

- The recency test (`TestQueryIntersectsTwoFamiliesAndKeepsOnlyRecentWorks`) must **fail** if
  `ageDays` is replaced by a lexicographic comparison. Verify by temporarily changing
  `time.Parse("2 Jan 2006", updateDate)` to `time.Parse("2006-01-02", updateDate)` and confirming
  the test goes red with `AgeDays = 20, want 20` becoming a different number, then restore.
- The validation test must **fail** if a missing spelling is silently skipped instead of reported.
  Verify by changing the `n == 0` branch to `continue` without appending to `rep.Missing`, and
  confirming the test goes red, then restore.

---

## 6. Risks, tradeoffs, and open questions

### A. `COLLATE NOCASE` in an `IN` list is a silent-failure trap (VERIFIED, and it bit this plan)

The natural spelling — `WHERE t.name IN (?, ?) COLLATE NOCASE` — **does not apply the collation to
the comparison**. The `COLLATE` binds to the last element of the list, not to the comparison
between the column and the list. Measured on this machine (SQLite, 2026-10-21):

| Form | Stored `Worm - Fandom`, asked for `worm - fandom`, on a **case-sensitive** column |
|---|---|
| `t.name IN ('worm - fandom') COLLATE NOCASE` | **0 rows** |
| `t.name COLLATE NOCASE IN ('worm - fandom')` | 1 row |
| `t.name = 'worm - fandom' COLLATE NOCASE` | 1 row |
| `lower(t.name) = lower('worm - fandom')` | 1 row |

The real mirror's `tags.name` is declared `TEXT NOT NULL UNIQUE COLLATE NOCASE`, so the column
carries the collation itself and **both spellings happen to return the same 133 works there** —
which is exactly why the wrong form survives review and passes a live smoke test. The failure
appears the moment a test fixture uses a case-sensitive column, or the schema changes.

**The plan uses `t.name COLLATE NOCASE IN (...)` everywhere** (the `COLLATE` on the column, before
`IN`). The plan's own test fixtures must therefore declare the test table the way the mirror
does, or the fixture must not depend on case-insensitivity at all. Do not "simplify" the SQL back
to the `IN (...) COLLATE NOCASE` spelling.

### B. The site's own conventions are stricter than they look

Three conventions in `internal/web` are enforced by tests that a plan written from the outside
will trip over, and each is a **silent** failure (a blank page, not an error):

- `pageSpec` has **no** `Path`, `Title`, `Render` or `Heading` field. The fields are `Template`,
  `Paths`, `Prefix`, `Nav`, `List`, `AcceptsPost`, `Handler`.
- A page template is `{{define "content"}}`, **not** a whole HTML document and **not**
  `{{define "<name>.html"}}`. The renderer executes the **"layout"** wrapper, not "base".
- A list page must carry the literal `data-empty="true"` **and** that element must be in a branch
  that renders only when the list is empty.

All three are asserted by `internal/web/registry_test.go`
(`TestEveryListPageMarksItsEmptyState` at line 199, `TestListPagesEmptyStateIsTwoSided` at line
267). Read those two tests before writing the template.

### C. The API's filter parser IGNORES unknown parameters

`parseWorksFilters` (`internal/api/server.go:775`) has **no "unknown filter" branch** — it
returns only the clauses it built, and `handleAO3Works` reads five named filters. An
unrecognised `?universe=…` therefore produces **no clause and no error**: the caller asked for a
restriction, the server returned every work, and nothing said so. This is why the plan adds the
two families to the parser **and** to the endpoint's `applied` echo, and why the test for a
missing family expects **400, not an empty list**.

### D. The fandom-shape rule needs the CORPUS, and the plan's first draft got that wrong

**This is the plan's one design bug that was found by running the plan's own code, and it is
recorded here because the failure mode is quiet.**

An earlier draft specified a pure `IsFandomShape(name string) bool` and a test asserting that
`alternate universe - canon divergence` and `post-traumatic stress disorder - ptsd` are NOT
fandoms. **The plan's own code failed that test** — both came back `true`.

The cause is structural: **rule (c) of F2 is "X has a canonical sibling elsewhere"**, which is a
fact about the corpus, not about the name. A function that sees only a name cannot apply it.
Approximating it as "any `X - Y` where Y is not a type marker is a fandom" measured **8 false
positives** against AO3's own typed markup — `naruto - freeform`, `marvel - freeform`,
`one piece - freeform`, `game of thrones - freeform`, `original character - freeform`,
`rimworld - freeform`, `underage - freeform`, `underage sex - freeform` — every one of them a
freeform tag, because the left side (`naruto`, `marvel`) IS a canonical base.

Two rules fix it, and **both are load-bearing**:

1. **The type-marker check must come BEFORE the creator-form check.** A right-hand side of
   `freeform` / `character` / `relationship` means the tag names a TYPE, whatever the left side
   is. This alone removes all 8 false positives.
2. **The creator-form check must consult the corpus.** `Shape` holds the set of canonical bases
   — the `X` of every `X - all media types` and `X - fandom` tag — read from the mirror's `tags`
   table (1,093 bases, one scan, ~2.3 s).

**The corrected rule measures precision 1.000, recall 0.450, zero false positives, and passes all
eleven of Task 1's test cases.**

**The cost of getting it wrong is not a crash.** A fandom-shape rule that over-matches does not
error; it reports that a work crosses into a universe it does not belong to, on every work, with
no signal that the classification is wrong. The corpus-derived `Shape` is the only thing standing
between the site and 8 wrong answers per thousand works.

**Also note:** `Shape` is built **once per query**, never per work — `LoadShape` scans the tags
table. A `LoadShape` call inside the per-item loop would make every query take minutes.

### E. Other risks

| # | Risk | Likelihood | Mitigation |
|---|---|---|---|
| R1 | **The F2 shape rule's recall is 0.354 (re-measured at implementation time against the 302-fandom / 3,286-freeform ground truth in `gt_tags.json`; the plan's 0.397 estimate was slightly optimistic).** Many creator-form fandom tags (`a song of ice and fire - george r. r. martin`) are missed, so a work's "other universe" may go unreported. | Certain (measured) | Accepted and documented. The rule errs toward silence, which is the safe direction for a *crossing* claim. Explicit per-spelling declarations cover the family's own members. |
| R1b | **The recall gap is dominated by PLAIN-named fandoms.** Measured: 90 of the 302 ground-truth fandom names are plain (`dragon age: inquisition`, `mass effect`, `star wars original trilogy`) and **all 90 are missed**; 2,984 of the 3,286 freeform names are also plain. A plain name is therefore intrinsically ambiguous, and the corpus cannot separate them: `futa - fandom` and `bdsm - all media types` really do exist as tags, so AO3 itself has typed those as fandoms. | Certain (measured) | **Tried and rejected:** admitting any plain name that is a canonical base elsewhere raises recall 0.354 → 0.364 but drops precision 1.000 → 0.564 (85 false positives: `futa`, `bdsm`, `erotica`, `self insert`, `enemies to lovers`). Admitting a parenthetical-derived base set raises recall to 0.533 and drops precision to 0.508. **The plan's choice of precision over recall stands**, and the false-positive list is the evidence for it. A reader is shown a wrong crossing far more often than a missing one. |
| R2 | **A declared family can silently rot** (a tag renamed upstream). | Medium | `ValidateUniverse` reports `missing` / `non_fandom_shape`; the CLI prints them; the API exposes them. Add the report to the browse page's count line in a follow-up if the CLI output proves noisy. |
| R3 | **The `update_date` fix changes existing sort output** (the site's "recent" list and the API's date sort will reorder). Any test asserting the old order is asserting a bug. | Certain | Read the failing test and update the expectation to the chronological value; say so in the commit message. Verify the live page's order by eye (Task 6i). |
| R4 | **`COLLATE NOCASE` on the tag name in the `IN` clause** changes matching for a family member whose case differs from the stored row. | Low | `tags.name` is declared `UNIQUE ... COLLATE NOCASE`, so a case-insensitive comparison matches the unique index. Verify with Task 10's live run: the `universe_works` count must be ≥ 2,041 (the measured size of `parahumans series - wildbow` alone). **If the count is 0, the COLLATE placement is wrong — fix it before continuing.** |
| R5 | **The new page's template conventions are guessed** from `tag.html` in this plan; the real names may differ. | High | Each template step says "read `tag.html` and copy its shape exactly" and marks the guessed parts as placeholders. The implementer substitutes, not guesses. |
| R6 | **The new `pageSpec` fields are guessed** (e.g. `Render` vs `Handler`). | High | Same mitigation: the registry struct is quoted by line number to read first. |
| R7 | **The `/browse` page is a new site area**, which the user said to avoid. | Medium | It is ONE page inside the EXISTING "Find something to read" nav group, reusing the existing list markup, form widget and CSS. No new nav group, no new site chrome. If the user prefers no new page at all, the API (Tasks 4–5) already delivers the capability to the frontend, and the page can be dropped without rework. |
| R8 | **`docs/goal-check.py` may fail on the new package count.** | Low | The script documents `--update-baseline` for a floor that changed; read the failing clause and fix it honestly. |

### F. Tradeoffs taken

1. **Declared families over derived ones.** Deriving would be more "automatic" but F3 and F4 prove
   it is impossible from this corpus. Declaring costs a data edit per new fandom — the honest cost.
2. **A rule-based "other universe" over a hand list.** The bespoke list was 13 Worm-specific
   namespaces; the shape rule is general and measured at precision 1.00, at the cost of recall 0.40.
   **Precision was chosen deliberately**: a false "crosses into Harry Potter" is a wrong claim in a
   reader's face; a missed crossing is an absent line.
3. **Reuse the existing works endpoint** for the API rather than adding one, and add
   `/api/v1/browse` only because the pool counts and the evidence need a home the works endpoint
   does not have.
4. **Keep the bespoke scorer** rather than adopting the engine's signal blend: the engine's
   semantics are seed-based similarity; this query is an intersection with a recency window, and
   re-using the bespoke scorer keeps the ranking identical to the command the reader already
   knows.
5. **One new page, not a new site area.** The alternative (no page at all) leaves the capability
   API-only; the alternative (a new nav group) is the "site changes a lot" the user rejected.

### G. Open questions (need the owner's answer, not the implementer's guess)

1. **Should the frontend expose the *raw* family names, or the declared keys?** The plan uses
   declared keys (`worm`, `isekai`) in the URL and renders them as option labels. If the reader
   should see "Worm (Parahumans)" and "isekai / transmigration" instead, the `label` fields in
   `families.json` are already there and only the template needs to use them.
2. **Should a family's `spellings_explicit_freeform` members be shown as such on the page?** The
   validation report distinguishes rule-admitted from declaration-admitted members. Showing that
   on the page is honest but adds UI; the plan currently keeps it in the CLI/API report only.
3. **Is one new page acceptable, or should `/browse` be folded into the existing `/search` page as
   extra filters?** Folding avoids a new page entirely but `/search` is a free-text query over
   works and tags, and mixing a two-axis family form into it would make one page answer two
   different questions.
4. **Should the family declarations live in `families.json` (embedded, versioned with the code) or
   in the database (editable by the owner without a deploy)?** The plan embeds them: it is
   reviewable in a diff and needs no migration. A DB table would need a migration and an admin
   path.
5. **Should `internal/fandom` be unified with the new `internal/tagfamily` shape rule?** They
   answer different questions — `internal/fandom` is the strict diversity-cap filter, the new rule
   is the loose "is this a fandom" classifier. The plan leaves them separate on purpose. Merging
   them would change diversity-cap behaviour, which is out of scope here.

---

### Task 11 — the export, and the families the export needed

**What the user asked for:** *"execute plan, when you are done export 200 recs to a txt file,
single work per line."*

**Step 11a — MEASURE before choosing a family pair.** The 200 works are not a free choice: the
intersection of a universe family and a concept family is a fixed population, and the plan's
canonical `worm x isekai` is only **55 works in the whole corpus** (30 within 365 days). A pair
that "sounds big" is not a pair that can yield 200.

Measured against the live mirror, all pools, all-time vs 365-day:

| universe | concept | all-time | 365d | 730d |
|---|---|---|---|---|
| worm | isekai | 55 | 30 | 48 |
| worm | alternate universe | 453 | 170 | **272** |
| harry potter | isekai | 119 | 83 | 107 |
| harry potter | alternate universe | 1048 | 305 | 506 |
| naruto | isekai | 176 | 104 | 145 |
| marvel | alternate universe | 256 | 76 | 113 |

So the export uses **`worm x alternate universe` over 730 days: 272 matches, 200 exported.**

**Step 11b — the new families are DATA, not code.** `harry potter`, `naruto` and `marvel`
universes and `alternate universe` and `reincarnation` concepts were added to
`internal/tagfamily/families.json`. No Go code changed to add them. That is the whole point of
the generalization, and it is now measured rather than asserted.

**Step 11c — the LIVE validation test found real errors in the DECLARATIONS.** A family is
declared, so a declaration can be wrong, and only the corpus can say so. The new
`internal/tagfamily/live_validate_test.go` opens the real mirror and checks every declared
member. It found, and the declarations now fix:

| family | what the live check caught |
|---|---|
| `worm` | `worm` is not fandom-shaped (a plain name); `parahumans series - wildbow` and `worm (web serial novel)` are not recognisable either |
| `naruto` | `naruto - fandom` DOES NOT EXIST in the mirror |
| `marvel` | `marvel - fandom` DOES NOT EXIST in the mirror |
| `reincarnation` | 4 of 8 declared tags DO NOT EXIST (`reincarnate`, `reincarnation!`, `reincarnated into another world`, `reincarnation & multiple lives identity crises`) |
| `harry potter` | `harry potter` is a plain name, not fandom-shaped |

**The `worm` family had three unvouched members and NOBODY had ever checked.** The validation
was written but never wired to anything that ran. It is now a test that runs against the real
mirror.

**Step 11d — `spellings_explicit_freeform` became `spellings_explicit`.** The old name said
"freeform", but the members that need declaring are not all freeform: a plain-named fandom
(`harry potter`), a creator form whose left side is not a canonical base
(`parahumans series - wildbow`), an unknown medium qualifier (`worm (web serial novel)`) and a
freeform tag (`worm - freeform`) are all the same thing to the query -- a declared member the
shape rule did not vouch for. One list, not one list per reason.

**Step 11e — `--text` and `--text-out`, ONE WORK PER LINE.** The line is `title<TAB>url`. An
empty result is an EMPTY FILE, not a file with a heading, so a line-counting script does not
have to know the format.

**THE MUTATION THAT CAUGHT A REAL GAP.** The first version had the printer and the file writer
as two copies of the same loop, and the test only covered the printer. Adding a heading line to
the file-writer copy was **NOT CAUGHT** -- the test passed. The mutation was:

```
MUTATED exit: 0
```

The two were then merged into one `browseText` function, and a second test
(`TestBrowseTextFileHoldsOneWorkPerLine`) was added for the file path. The same mutation now
kills three tests:

```
--- FAIL: TestPrintBrowseTextWritesOneWorkPerLine
    browse_test.go:62: want 2 lines, one per work, got 3: "2 works\na work\t..."
--- FAIL: TestBrowseTextFileHoldsOneWorkPerLine
    browse_test.go:100: want one line per work (3), got 4 lines: "3 works\n..."
RESTORED exit: 0
```

**Step 11f — the export itself.**

```bash
cd ~/code-local/go/kindred && \
  ./bin/kindred browse --corpus ~/code/python/ao3/recommender/cache/ao3_metadata.db \
    --universe worm --concept "alternate universe" --recent 730 --n 200 \
    --text-out exports/worm-alternate-universe.txt
```
EXPECT: `272 matched (showing 200) | universe=2166 concept=27745 window=730d`, and
`wc -l exports/worm-alternate-universe.txt` reports **200**.

VERIFIED on the file: 200 lines, every line exactly one tab, every URL matching
`https://archiveofourown.org/works/<digits>`, 200 distinct URLs, 200 distinct titles.
