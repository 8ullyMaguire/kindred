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
