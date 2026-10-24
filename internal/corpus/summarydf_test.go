package corpus

// The summary term table (#52). Two properties matter and both have caught this project
// before: the tokeniser decides what a "word" is, and the file must be identical between two
// ingests of the same corpus or a diff between mirrors means nothing.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSummaryTokens(t *testing.T) {
	got := SummaryTokens("The Dragon's Hoard: 5+1 times Harry & Draco fought (2019). WARNINGS apply.")
	// "warnings" is deliberately absent: it is AO3 boilerplate and the stop list drops it,
	// because a word on a fifth of the archive's summaries says nothing about any one work.
	want := []string{"dragon", "hoard", "times", "harry", "draco", "fought", "apply"}
	if len(got) != len(want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	// A term repeated in the same summary is emitted twice here; the BUILD dedupes per
	// document, because this is a tokeniser and that is a document-frequency rule.
	if n := len(SummaryTokens("angst angst angst")); n != 3 {
		t.Fatalf("repeated term emitted %d times, want 3 from the tokeniser", n)
	}
	if got := SummaryTokens(""); got != nil {
		t.Fatalf("empty summary tokenised to %v", got)
	}
}

func TestBuildSummaryDocFreqKeepsOnlyTheMeasuredBand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE works(id INTEGER PRIMARY KEY, summary TEXT)`); err != nil {
		t.Fatal(err)
	}
	// 20 documents, so the band is df in [3, 4]: "ubiquitous" on 10 (50%, above the ceiling),
	// "twice" on 2 (below the floor), "hapax" on 1 (below the floor), "filler" on all 20
	// (above the ceiling), and "middling" on exactly 4 -- the only term inside the band.
	summaries := make([]string, 20)
	for i := range summaries {
		summaries[i] = "filler"
	}
	for i := 0; i < 10; i++ {
		summaries[i] += " ubiquitous"
	}
	summaries[0] += " twice"
	summaries[1] += " twice"
	summaries[2] += " hapax"
	for i := 0; i < 4; i++ {
		summaries[i] += " middling"
	}
	for i, s := range summaries {
		if _, err := db.Exec(`INSERT INTO works (id, summary) VALUES (?, ?)`, i+1, s); err != nil {
			t.Fatal(err)
		}
	}

	terms, docs, err := BuildSummaryDocFreq(context.Background(), db, path)
	if err != nil {
		t.Fatal(err)
	}
	if docs != 20 {
		t.Fatalf("docs = %d, want 20", docs)
	}
	if terms != 1 {
		t.Fatalf("kept %d terms, want 1 (middling): the ceiling and the floor both bite", terms)
	}
	table, err := LoadSummaryDocFreq(path)
	if err != nil {
		t.Fatal(err)
	}
	if table.Total != 20 {
		t.Fatalf("total = %v, want 20", table.Total)
	}
	if w := table.IDF("middling"); w <= 0 {
		t.Fatalf("middling has idf %v, want positive", w)
	}
	if w := table.IDF("ubiquitous"); w != 0 {
		t.Fatalf("a term on half the corpus has idf %v, want 0 (dropped by the ceiling)", w)
	}
	if w := table.IDF("hapax"); w != 0 {
		t.Fatalf("a term on one document has idf %v, want 0 (below the floor)", w)
	}
}

// Two ingests of the same corpus must produce the same bytes: the vocabulary is sorted, so a
// diff between two mirrors is a diff in the data rather than in map iteration order.
func TestBuildSummaryDocFreqIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE works(id INTEGER PRIMARY KEY, summary TEXT)`); err != nil {
		t.Fatal(err)
	}
	// 40 documents so the band is df in [3, 8] and several terms survive it. With the first
	// version of this fixture -- 8 identical summaries -- the ceiling was 1.6, NOTHING was
	// kept, and two empty files are identical: the determinism test passed while proving
	// nothing, and the mutant that deleted the sort survived it.
	// Four words on eight documents each (the ceiling at 40 documents is 8, and the check is
	// "more than", so 8 is inside the band), and eight documents carrying only a word on
	// everything (dropped). Ordered so the writer's own order differs from sorted order.
	words := []string{"sorrows", "dragons", "kingdoms", "castles"}
	for i := 1; i <= 40; i++ {
		summary := "nothing notable happens"
		if i <= 32 {
			summary = fmt.Sprintf("a story of %s", words[(i-1)%4])
		}
		if _, err := db.Exec(`INSERT INTO works (id, summary) VALUES (?, ?)`, i, summary); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := BuildSummaryDocFreq(context.Background(), db, path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(SummaryDocFreqPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := BuildSummaryDocFreq(context.Background(), db, path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(SummaryDocFreqPath(path))
	if err != nil {
		t.Fatal(err)
	}
	table, err := LoadSummaryDocFreq(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(table.ByHash) < 2 {
		t.Fatalf("the fixture kept %d terms: determinism over an empty table proves nothing", len(table.ByHash))
	}
	if len(first) != len(second) {
		t.Fatalf("two builds differ in length: %d then %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("two builds differ at byte %d", i)
		}
	}
}

// A corpus with no summaries is an ERROR for the builder: an empty table would silently make
// the signal inert on every request, which is the failure mode this project has shipped once.
func TestBuildSummaryDocFreqRefusesAnEmptyCorpus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE works(id INTEGER PRIMARY KEY, summary TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BuildSummaryDocFreq(context.Background(), db, path); err == nil {
		t.Fatal("an empty corpus produced a table with no error")
	}
}
