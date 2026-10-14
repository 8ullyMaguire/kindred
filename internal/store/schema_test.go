package store

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// The schema is executed with CREATE ... IF NOT EXISTS, which makes a
// duplicate declaration a silent no-op rather than an error: the second
// statement runs, matches the existing object, reports success, and the two
// texts are free to drift apart while every test stays green.
//
// That is not hypothetical here -- arena_ratings was declared twice, with
// identical bodies, and the duplication survived long enough for one copy to
// be reformatted (tabs vs spaces) with a different comment, which is how a
// reader finds out there are two.
//
// The failure mode this pins is the drift, not the duplication: the next
// person to add a column edits whichever copy they found first, and the
// other copy keeps describing the old table -- so a fresh database and a
// migrated one can disagree about the same table, which is exactly the class
// of bug that is invisible until a query fails in production and not in
// tests.
func TestSchemaDeclaresEachObjectOnce(t *testing.T) {
	if strings.TrimSpace(schema) == "" {
		t.Fatal("the schema constant is empty")
	}

	// Names alone, so the pattern does not depend on quoting, IF NOT EXISTS
	// being present, or the whitespace inside the statement.
	decl := regexp.MustCompile(`(?i)CREATE\s+(TABLE|INDEX)\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`)
	seen := map[string][]string{}
	for _, m := range decl.FindAllStringSubmatch(schema, -1) {
		key := strings.ToUpper(m[1]) + " " + strings.ToLower(m[2])
		seen[key] = append(seen[key], key)
	}
	if len(seen) == 0 {
		t.Fatal("no CREATE statements matched: the pattern or the constant changed, " +
			"and a scan that matches nothing passes every duplicate")
	}

	var dupes []string
	for key, hits := range seen {
		if len(hits) > 1 {
			dupes = append(dupes, key)
		}
	}
	if len(dupes) > 0 {
		sort.Strings(dupes)
		t.Errorf("the schema declares %d object(s) more than once: %s. CREATE ... IF "+
			"NOT EXISTS makes the extra declarations silent no-ops, so the copies can "+
			"drift without any test noticing", len(dupes), strings.Join(dupes, ", "))
	}
}

// TestSchemaIsExecutableOnAFreshDatabase guards the other half: the schema
// is one string of SQL with no build step, so a typo in it is only found when
// an install runs. Opening a store executes it, and it is done against a
// deliberately fresh directory -- every other store test opens a database
// that setup() or a previous fixture may already have shaped.
func TestSchemaIsExecutableOnAFreshDatabase(t *testing.T) {
	dir := t.TempDir()
	corpusPath, err := testcorpus.New(4).Write(filepath.Join(dir, "corpus.db"))
	if err != nil {
		t.Fatalf("writing a corpus fixture: %v", err)
	}
	s, err := Open(t.Context(), filepath.Join(dir, "state.db"), corpusPath)
	if err != nil {
		t.Fatalf("opening a fresh store failed, so the schema does not execute "+
			"cleanly: %v", err)
	}
	defer s.Close()

	// And the duplicate this test's sibling exists for is gone: ONE row in
	// sqlite_master, not two.
	var n int
	if err := s.DB.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'arena_ratings'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("arena_ratings exists %d times in a fresh database", n)
	}
}
