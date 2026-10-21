package tagfamily

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "modernc.org/sqlite"
)

// TestLiveDeclaredFamiliesValidateAgainstTheMirror is a LIVE check, not a
// hermetic one: it opens the real read-only mirror and validates every
// declared family against it.
//
// It is skipped unless KINDRED_LIVE_CORPUS names the mirror, because the
// hermetic suite must not depend on a 1.7 GB file.
//
// The point is that a family is DECLARED, so a declaration can be WRONG
// (a spelling that does not exist, or one that is not fandom-shaped) and
// only the corpus can say so.
func TestLiveDeclaredFamiliesValidateAgainstTheMirror(t *testing.T) {
	path := os.Getenv("KINDRED_LIVE_CORPUS")
	if path == "" {
		t.Skip("KINDRED_LIVE_CORPUS not set")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	sh, err := LoadShape(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	fams, err := LoadFamilies()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range fams.Universes() {
		rep, err := ValidateUniverse(ctx, db, sh, u)
		if err != nil {
			t.Fatalf("%s: %v", u.Name, err)
		}
		if len(rep.Missing) > 0 {
			t.Errorf("universe %q declares spellings the mirror does not have: %v", u.Name, rep.Missing)
		}
		if len(rep.NonFandom) > 0 {
			t.Errorf("universe %q declares spellings that are not fandom-shaped: %v", u.Name, rep.NonFandom)
		}
		t.Logf("universe %-16s present=%d explicit=%d", u.Name, len(rep.Present), len(rep.Explicit))
	}
	// A concept's members are checked for EXISTENCE only: a concept is an
	// idea, and "isekai" is not a fandom-shaped name by design.
	for _, c := range fams.Concepts() {
		for _, tag := range c.Tags {
			var n int
			if err := db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM tags WHERE name = ? COLLATE NOCASE`, tag).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Errorf("concept %q declares a tag the mirror does not have: %q", c.Name, tag)
			}
		}
		t.Logf("concept %-20s tags=%d", c.Name, len(c.Tags))
	}
}
