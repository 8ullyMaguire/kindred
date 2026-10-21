package tagfamily

import (
	"context"
	"testing"

	_ "modernc.org/sqlite"
)

func TestShapeClassifiesNamesByTheCorpus(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	// The canonical spellings. A creator-form tag is a fandom only if its
	// LEFT side has one of these, so the fixture must contain the
	// canonical spelling and NOT the creator form.
	mustExec(t, db, `INSERT INTO tags (id,name) VALUES
		(1,'harry potter - all media types'),
		(2,'star wars - all media types'),
		(3,'alternate universe - freeform')`)

	sh, err := LoadShape(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadShape: %v", err)
	}

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"canonical all-media", "harry potter - all media types", true},
		{"canonical fandom", "worm - fandom", true},
		{"creator form with canonical base", "harry potter - j. k. rowling", true},
		{"media qualifier", "star wars (movie)", true},
		{"type marker freeform", "alternate universe - freeform", false},
		{"type marker character", "oc - character", false},
		{"type marker relationship", "polyamory - relationship", false},
		{"freeform namespace", "alternate universe - canon divergence", false},
		{"freeform namespace ptsd", "post-traumatic stress disorder - ptsd", false},
		{"no shape at all", "isekai", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := sh.IsFandom(c.in); got != c.want {
			t.Errorf("IsFandom(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestShapeTreatsATypeMarkerAsWinningOverACanonicalBase(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	// "naruto - all media types" makes "naruto" a canonical base, so
	// "naruto - freeform" LOOKS like a creator form. It is not: the right
	// side is a type marker, so the tag names a type.
	mustExec(t, db, `INSERT INTO tags (id,name) VALUES (1,'naruto - all media types')`)

	sh, err := LoadShape(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadShape: %v", err)
	}
	if sh.IsFandom("naruto - freeform") {
		t.Error("IsFandom(\"naruto - freeform\") = true; a type marker on the right " +
			"must win over a canonical base on the left, or every " +
			"<fandom> - freeform\" tag is a fandom")
	}
}
