// Package testcorpus builds a small AO3-shaped corpus for tests.
//
// ## Why this exists
//
// Before this, every package that needed a corpus wrote its own inline
// `CREATE TABLE` string. That is how four different schemas ended up in the
// tree, and they disagreed: `internal/corpus` and `internal/api` both declare
// `work_tags`, one with `PRIMARY KEY(work_id, tag_id)` and one without, and
// `internal/api`'s lacks the `users` table the other tests assume. A test
// passes against the schema it wrote itself and says nothing about the schema
// production opens.
//
// So the schema lives here, once, and matches the REAL mirror's shape --
// including the parts that only bite at scale:
//
//   - `work_tags` PK is `(work_id, tag_id, tag_type)`, so one tag NAME can sit
//     on one work twice under two tag types. Any count that assumes one row per
//     (work, tag) is wrong, and was: kindred's own corpus queries aggregate
//     with `COUNT(DISTINCT work_id)` for exactly this reason.
//   - `works.bookmarks` is NULLable and really is NULL for uncrawled rows, so
//     anything reading it must tolerate NULL rather than assume 0.
//
// ## The corpus is deterministic
//
// Fixed seed, no time.Now(), no map iteration order in the output. A fixture
// that reshuffles per run makes a ranking test flaky in a way that looks like
// a ranking bug, and `go test -count=1` will not save you because each run is
// a new process.
package testcorpus

import (
	"database/sql"
	"fmt"
	"math"
	"path/filepath"

	// Registered here, not only in tests: `Write` opens a real database, so a
	// caller that has not imported a driver gets `sql: unknown driver "sqlite"`
	// from a package that looks like it needs no dependencies. Every other
	// package in this tree imports it in its own file for the same reason.
	_ "modernc.org/sqlite"
)

// Schema is the corpus mirror's DDL, verbatim in shape.
//
// It is deliberately NOT `IF NOT EXISTS`: a fixture that silently reuses an
// existing file tests nothing, and this is how "the test passed" and "the test
// ran against last week's data" look identical.
const Schema = `
CREATE TABLE works(
	id INTEGER PRIMARY KEY, url TEXT NOT NULL, title TEXT NOT NULL, authors TEXT NOT NULL,
	summary TEXT, rating TEXT, word_count INTEGER DEFAULT 0, hits INTEGER DEFAULT 0,
	kudos INTEGER DEFAULT 0, bookmarks INTEGER, chapters TEXT, language TEXT,
	complete INTEGER DEFAULT 0, update_date TEXT, first_seen TEXT, last_updated TEXT);
CREATE TABLE tags(id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL COLLATE NOCASE);
CREATE TABLE work_tags(
	work_id INTEGER NOT NULL, tag_id INTEGER NOT NULL, tag_type TEXT NOT NULL,
	PRIMARY KEY(work_id, tag_id, tag_type));
CREATE TABLE cooccurrence_edges(
	tag_a_id INTEGER NOT NULL, tag_b_id INTEGER NOT NULL, cooccur_count INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(tag_a_id, tag_b_id));
CREATE TABLE users(
	id INTEGER PRIMARY KEY, url TEXT NOT NULL, username TEXT NOT NULL,
	bookmark_count INTEGER DEFAULT 0, first_seen TEXT, last_updated TEXT);
CREATE TABLE user_work_interactions(
	user_id INTEGER NOT NULL, work_id INTEGER NOT NULL,
	interaction_type TEXT NOT NULL DEFAULT 'bookmarked');
`

// Work is one row of the corpus.
type Work struct {
	ID        int64
	Title     string
	Authors   string
	Summary   string
	WordCount int
	Hits      int
	Kudos     int
	// Bookmarks is a pointer because the real column is NULLable and NULL is
	// the normal state for a work nobody has crawled bookmarks for. A plain
	// int cannot express that, so a fixture would quietly normalise away the
	// one case that breaks NULL-unsafe reads.
	Bookmarks *int
	Rating    string
	// Language and Complete are present because the tag page filters on both,
	// and a fixture that leaves them unset makes those filters untestable: the
	// column reads as empty for every row, so "the filter returns nothing" and
	// "the filter ignores the parameter" produce the same answer.
	//
	// Complete is a bool because the column is NOT NULL in the schema; the
	// mirror's real distribution is roughly 76/24 complete/in-progress.
	Language   string
	Complete   bool
	UpdateDate string
}

// Tag is one row of the tag table.
type Tag struct {
	ID   int64
	Name string
}

// WorkTag attaches a tag to a work under a type.
//
// The type is a parameter rather than a constant because the (work_id, tag_id,
// tag_type) PK exists precisely so the same tag name can be attached twice
// under different types, and a fixture cannot reproduce that bug if it cannot
// express the case.
type WorkTag struct {
	WorkID  int64
	TagID   int64
	TagType string
}

// Edge is one co-occurrence edge.
type Edge struct {
	A, B    int64
	Cooccur int
}

// User is one row of the users table.
type User struct {
	ID       int64
	Username string
	// BookmarkCount is the user's own total, which is NOT necessarily the
	// number of interaction rows this fixture writes. The real mirror's
	// interaction table carries duplicates (measured: 7 rows for 4 distinct
	// works), and a fixture that cannot express a duplicate row cannot test
	// the DISTINCT that every bookmark count in this project depends on.
	BookmarkCount int
}

// Interaction is one row of user_work_interactions.
//
// InteractionType exists so a fixture can reproduce the trap that makes
// 'bookmarker' different from 'bookmarked': the first is a row ABOUT a work
// carrying a user id, and counting it attributes one reader's history from
// another's action. Empty means 'bookmarked'.
type Interaction struct {
	UserID          int64
	WorkID          int64
	InteractionType string
}

// Corpus is a fixture waiting to be written.
type Corpus struct {
	Works    []Work
	Tags     []Tag
	WorkTags []WorkTag
	Edges    []Edge
	Users    []User
	// Interactions are written verbatim, duplicates and non-'bookmarked'
	// types included, because that is the shape the real table has.
	Interactions []Interaction
}

// The rating, language and completion vocabulary below is the REAL mirror's,
// not invented. Measured across all 112,935 rows: Explicit 42,968, Teen And Up
// Audiences 27,562, Mature 25,190, Not Rated 8,796, General Audiences 8,419.
//
// That matters because this fixture used to set EVERY work to "Explicit" and
// set no language or completion at all. A filter test written against it
// passes whether or not the filter is wired to the right column -- `rating=E`
// returns all forty works, and an implementation that ignores the parameter
// entirely returns the same forty. It is the tautology shape, and it is the
// reason the tag page shipped a rating control that could not be tested.
//
// Splitting the vocabulary across works makes each filter falsifiable: a
// filter that drops the wrong rows, or ignores the parameter, now returns a
// different set.
//
// The distribution is deliberately uneven and NOT uniform, so a test cannot
// pass by accident on a half-and-half split.
func fixtureRating(i int) string {
	switch i % 5 {
	case 0, 1:
		return "General Audiences"
	case 2:
		return "Teen And Up Audiences"
	case 3:
		return "Mature"
	default:
		return "Explicit"
	}
}

// fixtureLanguage gives three languages in a 5:2:1 ratio. A two-way split
// would let "the first language" and "the other one" pass for each other.
func fixtureLanguage(i int) string {
	switch i % 8 {
	case 5, 6:
		return "Spanish"
	case 7:
		return "French"
	default:
		return "English"
	}
}

// fixtureComplete alternates with a period of 3, so both values occur and
// neither is a rounding error. A 50/50 split would make a test that
// accidentally drops the last row look correct.
func fixtureComplete(i int) bool { return i%3 != 2 }

// New returns a deterministic fixture with `nWorks` works over a small tag
// vocabulary, wired so that a real ranking is possible: works share tags, have
// varying popularity, and the seed neighbourhood is genuinely denser than the
// rest of the corpus.
//
// The shape is chosen to make assertions falsifiable. If every work had
// identical tags and stats, "the ranking is correct" would be true of any
// implementation including a random shuffle.
func New(nWorks int) *Corpus {
	if nWorks < 4 {
		nWorks = 4
	}
	c := &Corpus{}

	// Tag vocabulary. Two fandoms so `max_per_fandom` has something to cap,
	// plus trope and freeform tags so tag-overlap signals have structure.
	names := []struct {
		name string
		typ  string
	}{
		{"harry potter - all media types", "fandoms"},
		{"star wars - all media types", "fandoms"},
		{"harry potter - j. k. rowling", "fandoms"},
		{"tagged fandom shape", "fandoms"},
		{"dark", "freeforms"},
		{"explicit", "freeforms"},
		{"villain", "freeforms"},
		{"slow burn", "freeforms"},
		{"enemies to lovers", "freeforms"},
		{"alternate universe - canon divergence", "freeforms"},
		{"first fan", "freeforms"},
	}
	for i, n := range names {
		c.Tags = append(c.Tags, Tag{ID: int64(i + 1), Name: n.name})
	}

	for i := 1; i <= nWorks; i++ {
		id := int64(i)
		// Popularity spread over ~4 orders of magnitude so `popularity` and
		// `quality` signals have a real ordering to get right.
		bm := int(math.Round(float64(50) * math.Pow(3.2, float64((i*7)%11))))
		kd := bm / 3
		if i%5 == 0 {
			kd = bm // a few works with a perfect kudos ratio
		}
		bookmarks := bm
		var bmp *int
		if i%7 == 0 {
			bmp = nil // exercise the NULL path
		} else {
			b := bookmarks
			bmp = &b
		}
		c.Works = append(c.Works, Work{
			ID:        id,
			Title:     fmt.Sprintf("Fixture Work %03d", i),
			Authors:   fmt.Sprintf("author%d", (i%5)+1),
			Summary:   fmt.Sprintf("Summary for fixture work %03d.", i),
			WordCount: 5_000 + i*3_000,
			Hits:      10_000 + i*500,
			Kudos:     kd,
			Bookmarks: bmp,
			Rating:    fixtureRating(i),
			Language:  fixtureLanguage(i),
			Complete:  fixtureComplete(i),
			// A fixed date, NOT time.Now(): a relative recency assertion must
			// not depend on when the suite runs.
			UpdateDate: "2026-01-15",
		})

		// Fandom: a 70/30 split, NOT alternating. `i%6` gives 20/4 and starves
		// star wars to 4 works; a diversity cap over a 4-work fandom is a no-op,
		// so a cap test would pass against an implementation that caps nothing.
		if i%10 < 7 {
			c.WorkTags = append(c.WorkTags,
				WorkTag{id, 1, "fandoms"}, WorkTag{id, 3, "fandoms"})
		} else {
			c.WorkTags = append(c.WorkTags,
				WorkTag{id, 2, "fandoms"}, WorkTag{id, 4, "fandoms"})
		}
		// Trope tags, rotating so overlap varies.
		c.WorkTags = append(c.WorkTags,
			WorkTag{id, int64(5 + (i % 4)), "freeforms"},
			WorkTag{id, int64(9 + (i % 3)), "freeforms"})
		// Work 1 carries tag 1 as BOTH a fandom and a freeform, reproducing the
		// real mirror's duplicate-name-two-types shape on purpose.
		if i == 1 {
			c.WorkTags = append(c.WorkTags, WorkTag{id, 1, "freeforms"})
		}
	}

	// Co-occurrence edges between tags that co-occur, which is what the graph
	// index is built from.
	for w := range c.Works {
		ids := []int64{}
		for _, wt := range c.WorkTags {
			if wt.WorkID == c.Works[w].ID && wt.TagType == "fandoms" {
				ids = append(ids, wt.TagID)
			}
		}
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				c.Edges = append(c.Edges, Edge{A: ids[i], B: ids[j], Cooccur: 3 + w%4})
			}
		}
	}
	return c
}

// Write creates the corpus at path and returns the path.
//
// Written with a plain handle because the corpus is attached READ-ONLY in
// production; a fixture that opened it read-write would not exercise the
// attachment path that has actually broken things.
func (c *Corpus) Write(path string) (string, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if _, err := db.Exec(Schema); err != nil {
		return "", fmt.Errorf("schema: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	for _, t := range c.Tags {
		if _, err := tx.Exec(`INSERT INTO tags(id,name) VALUES(?,?)`, t.ID, t.Name); err != nil {
			return "", fmt.Errorf("tag %q: %w", t.Name, err)
		}
	}
	for _, w := range c.Works {
		var bm any
		if w.Bookmarks != nil {
			bm = *w.Bookmarks
		}
		if _, err := tx.Exec(`INSERT INTO works
			(id,url,title,authors,summary,rating,word_count,hits,kudos,bookmarks,
			 chapters,language,complete,update_date,first_seen,last_updated)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			w.ID, fmt.Sprintf("https://example.invalid/works/%d", w.ID),
			w.Title, w.Authors, w.Summary, w.Rating, w.WordCount, w.Hits, w.Kudos,
			// language and complete were hardcoded to "English" and 1 here,
			// which silently overrode the struct. The tag page filters on both,
			// so both had to come from the fixture.
			bm, "1/?", w.Language, boolToInt(w.Complete),
			w.UpdateDate, "2026-01-01", "2026-01-01"); err != nil {
			return "", fmt.Errorf("work %d: %w", w.ID, err)
		}
	}
	for _, wt := range c.WorkTags {
		// OR IGNORE, because the fixture deliberately attaches one tag twice
		// under two types and that is two DISTINCT primary keys, not a dup.
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO work_tags(work_id,tag_id,tag_type) VALUES(?,?,?)`,
			wt.WorkID, wt.TagID, wt.TagType); err != nil {
			return "", fmt.Errorf("work_tag %d/%d: %w", wt.WorkID, wt.TagID, err)
		}
	}
	for _, e := range c.Edges {
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO cooccurrence_edges(tag_a_id,tag_b_id,cooccur_count) VALUES(?,?,?)`,
			e.A, e.B, e.Cooccur); err != nil {
			return "", fmt.Errorf("edge %d-%d: %w", e.A, e.B, err)
		}
	}
	for _, u := range c.Users {
		if _, err := tx.Exec(
			`INSERT INTO users(id,url,username,bookmark_count,first_seen,last_updated)
			 VALUES(?,?,?,?,?,?)`,
			u.ID, fmt.Sprintf("https://example.invalid/users/%d", u.ID),
			u.Username, u.BookmarkCount, "2026-01-01", "2026-01-01"); err != nil {
			return "", fmt.Errorf("user %d: %w", u.ID, err)
		}
	}
	for _, in := range c.Interactions {
		// Plain INSERT, NOT OR IGNORE: there is no primary key on this
		// table in the real mirror, and the duplicate rows it carries are
		// the reason every count here must be DISTINCT. A fixture that
		// silently deduplicated them would make those counts untestable.
		typ := in.InteractionType
		if typ == "" {
			typ = "bookmarked"
		}
		if _, err := tx.Exec(
			`INSERT INTO user_work_interactions(user_id,work_id,interaction_type) VALUES(?,?,?)`,
			in.UserID, in.WorkID, typ); err != nil {
			return "", fmt.Errorf("interaction %d/%d: %w", in.UserID, in.WorkID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return filepath.Clean(path), nil
}

// boolToInt renders a bool as the 0/1 the SQLite INTEGER column stores. A
// bool does not scan out of SQLite as an int without this, and storing Go's
// true/false makes the column read "1" only by accident of the driver.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
