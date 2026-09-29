package graph

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

const corpusFixture = `
CREATE TABLE works(id INTEGER PRIMARY KEY, title TEXT, url TEXT, summary TEXT, authors TEXT,
	word_count INTEGER, hits INTEGER, kudos INTEGER, bookmarks INTEGER, chapters TEXT, language TEXT,
	complete INTEGER, update_date TEXT, first_seen TEXT, rating TEXT);
CREATE TABLE tags(id INTEGER PRIMARY KEY, name TEXT);
CREATE TABLE work_tags(work_id INTEGER, tag_id INTEGER, tag_type TEXT);
CREATE TABLE cooccurrence_edges(tag_a_id INTEGER, tag_b_id INTEGER, cooccur_count INTEGER);
`

type recorder struct {
	execs  int
	argMax int
}

func (r *recorder) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	r.execs++
	if len(args) > r.argMax {
		r.argMax = len(args)
	}
	return sql.Result(nil), nil
}

func newCorpus(t *testing.T, stmts ...string) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corpus.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(corpusFixture); err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("fixture %q: %v", s, err)
		}
	}
	return db
}

func TestBuildMeasuresInsteadOfTrustingMetadata(t *testing.T) {
	// The real corpus carried a cooccurrence_graph_meta row claiming
	// 20,262 nodes while the edges referenced 123,047. Build must derive
	// the node set from the edges, and must say what it found.
	db := newCorpus(t,
		`INSERT INTO tags(id,name) VALUES(1,'a'),(2,'b'),(3,'c'),(9,'never-in-an-edge')`,
		`INSERT INTO cooccurrence_edges(tag_a_id,tag_b_id,cooccur_count) VALUES(1,2,5),(1,3,3)`,
		`INSERT INTO works(id,title,url,authors) VALUES(1,'w','u','x')`,
		`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(1,1,'freeforms'),(1,2,'freeforms')`,
	)
	rec := &recorder{}
	b := &Builder{Corpus: db, Store: rec, DBPath: filepath.Join(t.TempDir(), "own.db")}
	res, err := b.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Edges != 2 {
		t.Fatalf("edges = %d, want 2", res.Edges)
	}
	if res.TagsIn != 4 {
		t.Fatalf("tags_in = %d, want 4", res.TagsIn)
	}
	// Node ids are the corpus ids, so the array must span 1..3 at least.
	if res.Nodes < 3 {
		t.Fatalf("nodes = %d, want at least 3", res.Nodes)
	}
	if res.CSRBytes <= 0 {
		t.Fatalf("csr_bytes = %d", res.CSRBytes)
	}
}

// TestBuildStaysUnderSQLiteVariableCeiling is the test the real corpus
// demanded. A 7.7M-edge table batched at 20,000 rows binds 60,000
// variables and SQLite refuses with "too many SQL variables" — a failure
// no fixture with three edges could ever produce.
func TestBuildStaysUnderSQLiteVariableCeiling(t *testing.T) {
	const n = 900
	var tagStmts, edgeStmts, wtStmts []string
	tagStmts = append(tagStmts, `INSERT INTO tags(id,name)
		WITH RECURSIVE seq(value) AS (
			SELECT 1 UNION ALL SELECT value + 1 FROM seq WHERE value + 1 <= `+itoa(n)+`
		) SELECT value, 'tag' || value FROM seq`)
	// A ring plus chords: enough edges to need many batches at 300 rows.
	for i := 1; i <= n; i++ {
		j := i%n + 1
		edgeStmts = append(edgeStmts, "INSERT INTO cooccurrence_edges(tag_a_id,tag_b_id,cooccur_count) VALUES("+
			itoa(i)+","+itoa(j)+","+itoa(i)+")")
	}
	db := newCorpus(t, tagStmts...)
	if len(edgeStmts) > 0 {
		if _, err := db.Exec(joinSemi(edgeStmts)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO works(id,title,url,authors) VALUES(1,'w','u','x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(1,1,'freeforms')`); err != nil {
		t.Fatal(err)
	}
	_ = wtStmts

	rec := &recorder{}
	b := &Builder{Corpus: db, Store: rec, DBPath: filepath.Join(t.TempDir(), "own.db")}
	res, err := b.Build(context.Background())
	if err != nil {
		t.Fatalf("Build with %d edges: %v", n, err)
	}
	if res.Edges != int64(n) {
		t.Fatalf("edges = %d, want %d", res.Edges, n)
	}
	if rec.argMax > maxEdgeArgs {
		t.Fatalf("a batch bound %d variables, over the %d ceiling", rec.argMax, maxEdgeArgs)
	}
}

func TestBuildAgainstARealSQLiteStore(t *testing.T) {
	// The recorder cannot catch a constraint violation or a type error;
	// this runs the same path against a real database.
	db := newCorpus(t,
		`INSERT INTO tags(id,name) VALUES(1,'a'),(2,'b')`,
		`INSERT INTO cooccurrence_edges(tag_a_id,tag_b_id,cooccur_count) VALUES(1,2,7)`,
	)
	own, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "own.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	if _, err := own.Exec(`CREATE TABLE edges(tag_a INTEGER, tag_b INTEGER, weight REAL, PRIMARY KEY(tag_a,tag_b))`); err != nil {
		t.Fatal(err)
	}
	b := &Builder{Corpus: db, Store: ownAdapter{own}, DBPath: filepath.Join(t.TempDir(), "own2.db")}
	if _, err := b.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := own.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("edges table has %d rows, want 1 (one undirected edge stored once)", n)
	}
	// The pair must be stored canonically, so a lookup finds it without
	// knowing which way round it arrived.
	var a, bb int
	if err := own.QueryRow(`SELECT tag_a, tag_b FROM edges`).Scan(&a, &bb); err != nil {
		t.Fatal(err)
	}
	if a != 1 || bb != 2 {
		t.Fatalf("stored (%d,%d), want (1,2)", a, bb)
	}
}

func TestBuildWithNoEdgesReportsZero(t *testing.T) {
	db := newCorpus(t, `INSERT INTO tags(id,name) VALUES(1,'a')`)
	rec := &recorder{}
	b := &Builder{Corpus: db, Store: rec, DBPath: filepath.Join(t.TempDir(), "own.db")}
	res, err := b.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Edges != 0 || res.KeptEdges != 0 {
		t.Fatalf("an empty corpus reported %d edges / %d kept", res.Edges, res.KeptEdges)
	}
}

func TestBuildPropagatesStoreErrors(t *testing.T) {
	// A store that refuses must not be reported as a successful build.
	db := newCorpus(t,
		`INSERT INTO tags(id,name) VALUES(1,'a'),(2,'b')`,
		`INSERT INTO cooccurrence_edges(tag_a_id,tag_b_id,cooccur_count) VALUES(1,2,7)`,
	)
	b := &Builder{Corpus: db, Store: failStore{}, DBPath: filepath.Join(t.TempDir(), "own.db")}
	if _, err := b.Build(context.Background()); err == nil {
		t.Fatal("a failing store was reported as success")
	}
}

type failStore struct{}

func (failStore) Exec(context.Context, string, ...any) (sql.Result, error) {
	return nil, errors.New("store refused")
}

type ownAdapter struct{ db *sql.DB }

func (a ownAdapter) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return a.db.ExecContext(ctx, q, args...)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func joinSemi(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ";"
		}
		out += x
	}
	return out
}
