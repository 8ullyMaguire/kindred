package api

// The taste blend's positive case: a work that does NOT carry the tag must
// appear on the tag page anyway, because readers of the tag appreciated it,
// and it must say so. The negative/degraded cases are covered by
// tagsort_test.go's default arm (fixture has nothing to blend -> the page
// says so and lists the exact matches); this file proves the blend can
// actually blend, which a fixture of only-tagged works can never show.

import (
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	_ "modernc.org/sqlite"
)

// newTasteServer: eight works carrying BOTH tag 1 ("dark") and tag 2
// ("frost"), plus work 9 carrying ONLY "frost".
//
// That shape is the whole test: the tag page for "dark" pools works
// sharing the seeds' tags, so work 9 enters the pool through "frost" while
// carrying no "dark" of its own -- the exact gap the blend exists to close.
// Work 9's score must be non-zero for the blend to run (PMI of the
// dark<->frost pair, which co-occurs on eight works), which makes the
// positive case falsifiable rather than structural.
func newTasteServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	corpusPath := filepath.Join(dir, "corpus.db")
	dbPath := filepath.Join(dir, "state.db")

	{
		f, err := sql.Open("sqlite", corpusPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Exec(corpusSchema); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	seed, err := sql.Open("sqlite", corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	if _, err := seed.Exec(`INSERT INTO tags(id,name) VALUES(1,'dark'),(2,'frost')`); err != nil {
		t.Fatal(err)
	}

	for id := 1; id <= 8; id++ {
		if _, err := seed.Exec(
			`INSERT INTO works(id,url,title,authors,word_count,kudos,hits,rating,language,complete,update_date,first_seen)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			id,
			"https://example.invalid/"+strconv.Itoa(id),
			"Dark "+strconv.Itoa(id),
			"Author"+strconv.Itoa(id%2+1), // two authors, so bylines differ
			1000+id*100, 300-id*10, 1000,
			"General Audiences", "English", 1,
			"2026-01-01", "2026-01-01"); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Exec(`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(?,1,'freeforms')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Exec(`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(?,2,'freeforms')`, id); err != nil {
			t.Fatal(err)
		}
	}
	// The neighbour: shares "frost" with every seed, carries no "dark".
	if _, err := seed.Exec(
		`INSERT INTO works(id,url,title,authors,word_count,kudos,hits,rating,language,complete,update_date,first_seen)
		 VALUES(9,'https://example.invalid/9','Frost only','Author2',9000,5,50,
		        'General Audiences','English',1,'2026-02-01','2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(9,2,'freeforms')`); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(t.Context(), dbPath, corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	srv := &Server{
		Engine: &engine.Engine{
			Store: s, Corpus: corpus.NewAO3(s.Corpus),
			PoolSize: 50, TopN: 4, Lite: true,
		},
		Store:   s,
		Version: "test",
		Lite:    true,
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts
}

// TestTagTasteBlendBringsUntaggedWorks: the headline behaviour.
func TestTagTasteBlendBringsUntaggedWorks(t *testing.T) {
	ts := newTasteServer(t)
	defer ts.Close()

	body := getPage(t, ts, "/tag/1")

	// 1. The blend ran and says it did.
	if !strings.Contains(body, `data-testid="taste-note"`) {
		i := strings.Index(body, `data-testid="taste-error"`)
		var msg string
		if i >= 0 {
			end := strings.Index(body[i:], "</p>")
			msg = body[i:min(i+end, len(body))]
		}
		t.Fatalf("no taste-note; taste-error = %q", msg)
	}

	// 2. Work 9, which carries NO dark tag, is on the page anyway.
	if !strings.Contains(body, ">Frost only<") {
		t.Error("untagged-by-dark work 9 did not appear in the blend")
	}

	// 3. It says which half it came from, with a score to check it by.
	if !strings.Contains(body, `data-testid="taste-score"`) {
		t.Error("blended rows carry no taste score")
	}
	if !strings.Contains(body, "appreciated by this tag's readers") {
		t.Error("untagged blended row does not say it came from the tag's readers")
	}

	// 4. The tagged works are still there and labelled as exact matches.
	if !strings.Contains(body, "carries this tag") {
		t.Error("tagged blended rows are not labelled as carrying the tag")
	}

	// 5. The heading tells the truth about the ordering.
	if !strings.Contains(body, "Taste match") {
		t.Error("blend ran but the heading does not say Taste match")
	}

	// 6. Work 9 is absent from the exact list: sort=kudos must not contain
	//    it, proving the two views are really different queries.
	exact := getPage(t, ts, "/tag/1?sort=kudos")
	if strings.Contains(exact, ">Frost only<") {
		t.Error("exact list contains a work that does not carry the tag")
	}
}

// TestTagTasteBlendHonoursFilters: the filters are the reader's and apply
// to BOTH halves. A blend that quietly dropped them would return filtered-
// out works through the back door.
func TestTagTasteBlendHonoursFilters(t *testing.T) {
	ts := newTasteServer(t)
	defer ts.Close()

	// work 9 is 9,000 words: under:5000 must remove it from the blend
	// exactly as it removes it from the exact list.
	body := getPage(t, ts, "/tag/1?words=under:5000")
	if strings.Contains(body, ">Frost only<") {
		t.Error("blend returned a 9,000-word work under a 5,000-word bound")
	}
	for id := 1; id <= 8; id++ {
		words := 1000 + id*100
		if words < 5000 && !strings.Contains(body, ">Dark "+strconv.Itoa(id)+"<") {
			t.Errorf("exact half lost work %d under the bound", id)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
