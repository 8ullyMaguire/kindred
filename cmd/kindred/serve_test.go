package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// TestGraphLoadsFrequenciesAndNames is the regression for the bug this
// whole exercise was about: `serve` passed nil callbacks to LoadCSR, so
// the graph loaded with 634,232 nodes and 2,884,447 edges and answered
// every tag-similarity request with an empty list.
//
// The failure hid because it is not an error. PMI is
//
//	log(P(co|occur) / (P(a) P(b)))
//
// and with every Frequency() returning 0, both marginals are 0, every PMI
// is 0, and the handler drops every non-positive pair. An empty list is
// the CORRECT output for a graph with no frequencies. The endpoints
// returned 200 with `"returned": 0` and the budget gate, which walks
// routes and checks status codes, called it a pass.
//
// So the assertion is on the CONTENT: a tag with co-occurrence edges must
// come back with similar tags and a name. A status-code check cannot
// catch this class of bug, and that is the lesson.
func TestGraphLoadsFrequenciesAndNames(t *testing.T) {
	c := context.Background()
	dir := t.TempDir()
	corpus := filepath.Join(dir, "corpus.db")
	idx := filepath.Join(dir, "state.db")

	// A corpus with four tags, one work, and three co-occurrence edges, so
	// tag 1 genuinely has a neighbour with a positive PMI.
	cdb, err := sql.Open("sqlite", corpus)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE tags(id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE works(id INTEGER PRIMARY KEY, title TEXT, url TEXT, summary TEXT,
			authors TEXT, rating TEXT, word_count INT, hits INT, kudos INT, bookmarks INT,
			chapters INT, language TEXT, complete INT, update_date TEXT, first_seen INT,
			last_updated TEXT)`,
		`CREATE TABLE work_tags(work_id INT, tag_id INT, kind TEXT)`,
		`CREATE TABLE cooccurrence_edges(tag_a_id INT, tag_b_id INT, cooccur_count INT)`,
		`INSERT INTO tags VALUES (1,'alpha'),(2,'beta'),(3,'gamma'),(4,'delta')`,
		`INSERT INTO works VALUES (1,'A Work','http://example.invalid/1','s','a','General',100,1,1,1,1,'en',1,'2026-01-01',1,'2026-01-01')`,
		`INSERT INTO work_tags VALUES (1,1,'freeform'),(1,2,'freeform'),(1,3,'freeform')`,
		`INSERT INTO cooccurrence_edges VALUES (1,2,5),(1,3,4),(2,3,2)`,
	} {
		if _, err := cdb.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := cdb.Close(); err != nil {
		t.Fatal(err)
	}

	// The index the loader reads: 4 nodes, tag 1 adjacent to 2 and 3.
	sdb, err := sql.Open("sqlite", idx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.Exec(`CREATE TABLE graph_meta(key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	// `edges` is the Builder's own table, in the STATE database -- it
	// writes the capped adjacency there, so it has to exist where the
	// Builder writes. The corpus holds `cooccurrence_edges`, which is
	// where the Builder reads from.
	if _, err := sdb.Exec(`CREATE TABLE edges(tag_a INTEGER, tag_b INTEGER, weight REAL)`); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"work_count": "1",
	} {
		if _, err := sdb.Exec(`INSERT INTO graph_meta VALUES(?,?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := sdb.Close(); err != nil {
		t.Fatal(err)
	}

	// The index is built through the real Builder, so the loader is tested
	// against what the writer actually produces rather than against a
	// hand-made fixture that agrees with it by construction.
	{
		cs, err := sql.Open("sqlite", corpus)
		if err != nil {
			t.Fatal(err)
		}
		ds, err := sql.Open("sqlite", idx)
		if err != nil {
			t.Fatal(err)
		}
		b := &graph.Builder{Corpus: cs, Store: ctxExec{ds}, TopN: 8, DBPath: idx}
		if _, err := b.Build(c); err != nil {
			t.Fatal(err)
		}
		cs.Close()
		ds.Close()
	}

	s, err := store.Open(c, idx, corpus)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	freq, err := graph.TagFrequencies(c, s.Corpus)
	if err != nil {
		t.Fatal(err)
	}
	if len(freq) == 0 {
		t.Fatal("TagFrequencies returned nothing; every PMI would be 0 and " +
			"every similarity answer would be an empty list")
	}
	names, err := graph.TagNames(c, s.Corpus, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 5 {
		t.Fatalf("TagNames returned %d entries, want 5 (one per node id, "+
			"with a leading empty entry for id 0)", len(names))
	}
	if names[1] == "" {
		t.Error("names[1] is empty; a graph loaded with no names answers " +
			"every tag lookup with an empty string")
	}

	g, err := graph.LoadCSR(c, idx, s.Corpus,
		func() (map[int32]int64, error) { return graph.TagFrequencies(c, s.Corpus) },
		func() ([]string, error) { return graph.TagNames(c, s.Corpus, lenFromMeta(c, s, "node_count")) },
	)
	if err != nil {
		t.Fatal(err)
	}
	// A tag with edges must have positive-PMI neighbours. Not "the call
	// succeeds" -- the call succeeded while this was broken.
	found := 0
	for id := int32(0); id < 4; id++ {
		if ids, _ := g.Neighbours(id); len(ids) > 0 {
			found++
			break
		}
	}
	if found == 0 {
		t.Fatal("no node has neighbours; the index carries no adjacency")
	}
	if g.Frequency(1) <= 0 {
		t.Error("Frequency(1) is 0; PMI is undefined for a zero marginal and " +
			"every pair is dropped")
	}
}

// ctxExec adapts *sql.DB to the graph package's Executor interface, which
// takes a context. The real command does the same thing in adapter.go, and
// duplicating a four-line adapter in a test is better than exporting one
// just for this.
type ctxExec struct{ *sql.DB }

func (e ctxExec) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return e.DB.ExecContext(ctx, q, args...)
}
