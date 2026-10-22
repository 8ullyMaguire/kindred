package corpus

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// The document-frequency side file. It is an artefact the server TRUSTS — it
// is loaded at startup and read for every candidate on every ranking — so the
// two failure modes worth pinning are "it round-trips exactly" and "a damaged
// file is refused rather than half-read".

// buildFixtureCorpus writes a small corpus with known frequencies.
//
//   - work 1: tags 10, 11, 12
//   - work 2: tags 10, 11
//   - work 3: tags 10
//
// so tag 10 is on 3 of 3 works, tag 11 on 2, tag 12 on 1 — the shape that
// makes IDF meaningful (a universal tag, a common one, a rare one).
func buildFixtureCorpus(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE works (id INTEGER PRIMARY KEY, title TEXT)`,
		`CREATE TABLE work_tags (work_id INTEGER, tag_id INTEGER, tag_type TEXT)`,
		`INSERT INTO works (id, title) VALUES (1,'a'),(2,'b'),(3,'c')`,
		`INSERT INTO work_tags (work_id, tag_id, tag_type) VALUES
			(1,10,'freeforms'),(1,11,'freeforms'),(1,12,'freeforms'),
			(2,10,'freeforms'),(2,11,'freeforms'),
			(3,10,'freeforms')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	return path
}

func TestBuildDocFreqCountsEveryTagAndTheTotal(t *testing.T) {
	path := buildFixtureCorpus(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tags, total, err := BuildDocFreq(context.Background(), db, path)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tags != 3 {
		t.Fatalf("tags = %d, want 3", tags)
	}
	if total != 3 {
		t.Fatalf("total works = %v, want 3", total)
	}

	df, err := LoadDocFreq(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if df.Total != 3 {
		t.Fatalf("loaded total = %v, want 3", df.Total)
	}
	for tag, want := range map[int32]float64{10: 3, 11: 2, 12: 1} {
		got, ok := df.Count(tag)
		if !ok {
			t.Fatalf("tag %d missing from the loaded table", tag)
		}
		if got != want {
			t.Fatalf("tag %d count = %v, want %v", tag, got, want)
		}
	}
}

// TestDocFreqRoundTripsEveryEntry checks that the file holds EVERY tag, not
// the ones a test happened to look up. A loader that stopped early would pass
// a spot check and leave most tags "maximally rare".
func TestDocFreqRoundTripsEveryEntry(t *testing.T) {
	path := buildFixtureCorpus(t)
	db, _ := sql.Open("sqlite", path)
	defer db.Close()
	if _, _, err := BuildDocFreq(context.Background(), db, path); err != nil {
		t.Fatal(err)
	}
	df, err := LoadDocFreq(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(df.ByTag) != 3 {
		t.Fatalf("loaded %d tags, want 3: %v", len(df.ByTag), df.ByTag)
	}
}

func TestLoadDocFreqRefusesAMissingFile(t *testing.T) {
	_, err := LoadDocFreq(filepath.Join(t.TempDir(), "nope.db"))
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, want os.ErrNotExist so the caller can tell "+
			"\"never ingested\" apart from \"corrupt\"", err)
	}
}

func TestLoadDocFreqRefusesABadMagic(t *testing.T) {
	path := buildFixtureCorpus(t)
	db, _ := sql.Open("sqlite", path)
	defer db.Close()
	if _, _, err := BuildDocFreq(context.Background(), db, path); err != nil {
		t.Fatal(err)
	}
	// Overwrite the magic, keeping the length: a file from another tool must
	// not be read as frequencies.
	b, err := os.ReadFile(DocFreqPath(path))
	if err != nil {
		t.Fatal(err)
	}
	copy(b[0:4], "XXXX")
	if err := os.WriteFile(DocFreqPath(path), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDocFreq(path); err == nil {
		t.Fatal("loaded a file with a bad magic")
	} else if !strings.Contains(err.Error(), "magic") {
		t.Fatalf("err = %v, want it to name the magic", err)
	}
}

// TestLoadDocFreqRefusesATruncatedFile pins the length check.
//
// It matters because the loader pre-sizes its map from the header's count: a
// truncated file would otherwise decode whatever bytes happened to follow and
// install a table of garbage frequencies, which ranks confidently and wrongly.
func TestLoadDocFreqRefusesATruncatedFile(t *testing.T) {
	path := buildFixtureCorpus(t)
	db, _ := sql.Open("sqlite", path)
	defer db.Close()
	if _, _, err := BuildDocFreq(context.Background(), db, path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(DocFreqPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DocFreqPath(path), b[:len(b)-4], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDocFreq(path); err == nil {
		t.Fatal("loaded a truncated file")
	}
}

// TestDocFreqPathSitsBesideTheCorpus pins the side-file convention: not in
// the mirror (which the server opens read-only) and not in the state db
// (which is a different lifecycle — the table is a property of the corpus).
func TestDocFreqPathSitsBesideTheCorpus(t *testing.T) {
	got := DocFreqPath("/x/ao3_metadata.db")
	if got != "/x/ao3_metadata.db.docfreq" {
		t.Fatalf("DocFreqPath = %q", got)
	}
}
