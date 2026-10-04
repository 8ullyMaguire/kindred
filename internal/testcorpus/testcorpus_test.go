package testcorpus

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestWriteProducesTheRealMirrorShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corpus.db")
	if _, err := New(24).Write(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var works int
	if err := db.QueryRow(`SELECT COUNT(*) FROM works`).Scan(&works); err != nil {
		t.Fatal(err)
	}
	if works != 24 {
		t.Errorf("works = %d, want 24", works)
	}

	// The NULL-bookmarks case must actually be written as NULL, not 0. This is
	// the whole reason Bookmarks is a pointer: a fixture that normalised NULL
	// to 0 would let a NULL-unsafe read pass here and fail on the real mirror.
	var nulls int
	if err := db.QueryRow(`SELECT COUNT(*) FROM works WHERE bookmarks IS NULL`).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls == 0 {
		t.Error("no NULL bookmarks written; the NULL path is untested")
	}
}

func TestOneTagNameCanSitOnAWorkTwiceUnderTwoTypes(t *testing.T) {
	// The (work_id, tag_id, tag_type) PK exists so this is representable, and
	// the real mirror does it: `highschool dxd (anime)` is on 7 works as
	// `fandoms` and 773 as `freeforms` in the production corpus. Any aggregate
	// that assumes one row per (work, tag) is wrong on real data, and a fixture
	// that cannot express this cannot catch it.
	path := filepath.Join(t.TempDir(), "corpus.db")
	if _, err := New(24).Write(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var rows int
	err = db.QueryRow(`SELECT COUNT(*) FROM work_tags
		WHERE work_id=1 AND tag_id=1`).Scan(&rows)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("work 1 tag 1 has %d rows, want 2 (one per tag_type)", rows)
	}

	// And the DISTINCT count differs from the row count -- which is the whole
	// reason corpus queries must use COUNT(DISTINCT work_id).
	var distinct int
	if err := db.QueryRow(`SELECT COUNT(DISTINCT work_id) FROM work_tags
		WHERE tag_id=1`).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct == 0 {
		t.Error("tag 1 attached to no works")
	}
}

func TestTwoFandomsBothLargeEnoughToCap(t *testing.T) {
	// A diversity cap is only falsifiable if more than one group has more
	// members than the cap. With one fandom the cap is a no-op and a test
	// asserting "capped" passes against an implementation that caps nothing.
	path := filepath.Join(t.TempDir(), "corpus.db")
	if _, err := New(24).Write(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT tag_id, COUNT(DISTINCT work_id) n FROM work_tags
		WHERE tag_type='fandoms' AND tag_id IN (1,2)
		GROUP BY tag_id ORDER BY tag_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int64]int{}
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		got[id] = int(n)
	}
	if got[1] < 5 || got[2] < 5 {
		t.Errorf("fandom sizes %v; both need >=5 works for a cap of 3 to bind", got)
	}
}

func TestDeterministicAcrossCalls(t *testing.T) {
	// A fixture that reshuffles per run makes a ranking test flaky in a way
	// that reads as a ranking bug. go test -count=1 does not help: each run is
	// a new process with a new fixture.
	a, err := New(30).Write(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(30).Write(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	readAll := func(path string) string {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var s string
		rows, err := db.Query(`SELECT id||'|'||title||'|'||COALESCE(bookmarks,'NULL')
			FROM works ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			s += line + "\n"
		}
		return s
	}
	if readAll(a) != readAll(b) {
		t.Error("two New(30) calls produced different corpora")
	}
}

func TestSchemaIsNotIfNotExists(t *testing.T) {
	// A fixture that reuses an existing file tests nothing. Writing twice to
	// the same path must fail, not silently no-op.
	path := filepath.Join(t.TempDir(), "corpus.db")
	if _, err := New(4).Write(path); err != nil {
		t.Fatal(err)
	}
	if _, err := New(4).Write(path); err == nil {
		t.Error("second Write to the same path succeeded; Schema should reject it")
	}
}
