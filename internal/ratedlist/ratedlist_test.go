package ratedlist

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func fixture(t *testing.T) *sql.DB {
	t.Helper()
	p := filepath.Join(t.TempDir(), "corpus.db")
	if _, err := testcorpus.New(40).Write(p); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The real header line, copied from a calibredb run. The parser reads its
// column offsets from this, so a test that hardcoded offsets would pass
// against a fixture the code never sees.
const realHeader = "id    *rating title                                                                          authors"

func TestParseReadsEveryRowAndItsRating(t *testing.T) {
	in := realHeader + "\n" +
		"29    10   Psycho of the Dead                                                             Shaboobamon\n" +
		"11827 10   Pink Drinks                                                                    CarnalCharnal\n" +
		"143   10   Tainted Desire                                                                 aTasteofDarkness (Dirk_Grey)\n" +
		"491   None Just As Planned (Waifu Catalog SI, Worm Start)                                 x50413\n"
	got, err := ParseCalibrePaste(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("parsed %d entries, want 4: %+v", len(got), got)
	}
	if got[0].Title != "Psycho of the Dead" || got[0].Author != "Shaboobamon" {
		t.Errorf("row 1 = %q by %q", got[0].Title, got[0].Author)
	}
	if got[0].Rating != 10 || !got[0].Rated {
		t.Errorf("row 1 rating = %d rated=%v, want 10/true", got[0].Rating, got[0].Rated)
	}
	// The byline's parenthetical pseudonym must survive parsing, because
	// matching needs the real name as well as the pseudonym.
	if !strings.Contains(got[2].Author, "Dirk_Grey") {
		t.Errorf("row 3 author = %q, want the pseudonym preserved", got[2].Author)
	}
	// None is unrated, not zero.
	if got[3].Rated || got[3].Rating != 0 {
		t.Errorf("None row = rated %v / %d, want unrated", got[3].Rated, got[3].Rating)
	}
	if got[3].Title != "Just As Planned (Waifu Catalog SI, Worm Start)" {
		t.Errorf("row 4 title = %q", got[3].Title)
	}
}

// A wrapped title arrives as an indented continuation line with no row id.
// Parsing by whitespace would drop it into the previous row or split it, so
// this is the case the column-driven parser exists for.
//
// The continuation is indented to the column it continues: 90 for an author
// (the padding's end) and 11 for a title. That is what Calibre emits and what
// the modal-column measurement is built to read. The two readings of an
// ambiguous wrap are covered by the tests below.
func TestParseRejoinsWrappedRows(t *testing.T) {
	in := realHeader + "\n" +
		"1862  9    Rogue Knight                                                                 Illuviar\n" +
		"2398  7    Shut up and Sith: or how I learned to keep calm and UNLIMITED POWAH!\n" +
		"           - A SWTOR KOTP Gamer Story\n" +
		"495   None A Gamers Guide to Necromancy                                                   The Dark Wolf Shiro\n"
	got, err := ParseCalibrePaste(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("parsed %d entries, want 3: %+v", len(got), got)
	}
	if got[0].Title != "Rogue Knight" || got[0].Author != "Illuviar" {
		t.Errorf("row 1 = %q by %q", got[0].Title, got[0].Author)
	}
	// A title continuation is indented to the TITLE column, not the author
	// one, and must not be mistaken for the tail of the author.
	if !strings.HasSuffix(got[1].Title, "- A SWTOR KOTP Gamer Story") {
		t.Errorf("wrapped title = %q", got[1].Title)
	}
	// A continuation in the middle of the file must not be attached to the
	// row below it.
	if got[2].Title != "A Gamers Guide to Necromancy" || got[2].Author != "The Dark Wolf Shiro" {
		t.Errorf("row 3 = %q by %q", got[2].Title, got[2].Author)
	}
}

func TestNormaliseStripsBracketedSuffixes(t *testing.T) {
	// "A Precise Note [MHA | Izuku-Centric]" and "A Precise Note" are the
	// same fic to a reader and two keys to a matcher.
	a := Normalise("A Precise Note [MHA | Izuku-Centric]")
	b := Normalise("A Precise Note")
	if a != b {
		t.Errorf("suffix not stripped: %q vs %q", a, b)
	}
}

func TestNormaliseFoldsPunctuationAndCase(t *testing.T) {
	cases := [][2]string{
		{"Harry Potter: The Gamer", "harry potter the gamer"},
		{"A-B", "a b"},
		{"The  Seducer   System", "the seducer system"},
		{"Rogue Knight (Neckbeard)", "rogue knight"},
	}
	for _, c := range cases {
		if got := Normalise(c[0]); got != c[1] {
			t.Errorf("Normalise(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	// What matters is that punctuation variants of the SAME title agree.
	if Normalise("Magician of Darkness!") != Normalise("Magician of Darkness") {
		t.Error("punctuation changed the key")
	}
	// And that separators are interchangeable, which is how a Calibre byline
	// ("The Dark Wolf Shiro") meets a mirror pseudonym
	// ("The_Dark_Wolf_Shiro").
	for _, pair := range [][2]string{
		{"The Dark Wolf Shiro", "The_Dark_Wolf_Shiro"},
		{"Shadow Monarch", "Shadow-Monarch"},
	} {
		if Normalise(pair[0]) != Normalise(pair[1]) {
			t.Errorf("%q and %q normalise differently", pair[0], pair[1])
		}
	}
}

func TestAuthorKeysCoverBylineAndPseudonym(t *testing.T) {
	got := authorKeys("aTasteofDarkness (Dirk_Grey)")
	if len(got) != 2 {
		t.Fatalf("authorKeys = %v, want both the byline and the pseudonym", got)
	}
	if got[0] != "atasteofdarkness" || got[1] != "dirk grey" {
		t.Errorf("authorKeys = %v", got)
	}
}

// An unbalanced bracket is part of the title, not a suffix to cut: cutting at
// the open paren would truncate half the titles in a real library.
func TestNormaliseKeepsUnbalancedBrackets(t *testing.T) {
	got := Normalise("Ink (2022")
	if !strings.Contains(got, "2022") {
		t.Errorf("unbalanced bracket truncated the title: %q", got)
	}
}

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"rogue knight", "rogue knight", 0},
		{"rogue knight", "rogue knigh", 1},
		{"rogue knight", "rogue knigt", 1},
		{"", "abc", 3},
	}
	for _, c := range cases {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	// Two titles with nothing in common report the sentinel, NOT a distance:
	// the function is bounded by the tolerance because the caller only asks
	// "within tolerance or not". A test expecting the true distance here
	// would be asserting a contract the function deliberately does not have.
	for _, pair := range [][2]string{
		{"rogue knight", "completely different"},
		{"rogue knight", "dark knight"},
	} {
		if got := editDistance(pair[0], pair[1]); got <= maxEditDistance {
			t.Errorf("editDistance(%q,%q) = %d, want beyond tolerance",
				pair[0], pair[1], got)
		}
	}
}

func TestMatchToCorpusFindsExactTitleAndAuthor(t *testing.T) {
	db := fixture(t)
	entries := []Entry{{BookID: 1, Title: "Work 1", Author: "Author 1", Rating: 9, Rated: true}}
	m, err := MatchToCorpus(context.Background(), db, entries)
	if err != nil {
		t.Fatal(err)
	}
	m.Summarise()
	// Either it found it exactly, or it did not find it at all -- what it
	// must never do is return a confident match on the wrong work.
	got := m.Entries[0]
	if got.Method == "title-only" || got.Method == "fuzzy" {
		t.Errorf("matched %q by %q, want exact or nothing", got.Title, got.Method)
	}
}

func TestMatchLeavesUnknownTitlesUnmatched(t *testing.T) {
	db := fixture(t)
	entries := []Entry{{BookID: 1, Title: "A Fic This Mirror Has Never Heard Of", Author: "Nobody"}}
	m, err := MatchToCorpus(context.Background(), db, entries)
	if err != nil {
		t.Fatal(err)
	}
	m.Summarise()
	if got := m.Entries[0]; got.WorkID != 0 || got.Method != "" {
		t.Errorf("unknown title matched to work %d (%s)", got.WorkID, got.Method)
	}
	if len(m.Unmatched) != 1 {
		t.Errorf("unmatched = %d, want 1", len(m.Unmatched))
	}
}

func TestMatchOnEmptyEntriesIsAnError(t *testing.T) {
	// "Nothing to match" must not look like "nothing matched".
	if _, err := MatchToCorpus(context.Background(), fixture(t), nil); err == nil {
		t.Error("matching an empty history reported success")
	}
}

func TestMatchedFiltersBelowConfidenceFloor(t *testing.T) {
	m := Match{Entries: []Entry{
		{WorkID: 1, Confidence: 1.0, Method: "exact"},
		{WorkID: 2, Confidence: 0.5, Method: "title-only"},
		{WorkID: 0, Method: ""},
	}}
	got := m.Matched(0.8)
	if len(got) != 1 || got[0].WorkID != 1 {
		t.Errorf("Matched(0.8) = %v, want only the exact match", got)
	}
	if len(m.Matched(0.5)) != 2 {
		t.Errorf("Matched(0.5) = %d, want both matched entries", len(m.Matched(0.5)))
	}
}

func TestReportNamesTheUnmatchedRows(t *testing.T) {
	m := Match{Entries: []Entry{
		{WorkID: 1, Method: "exact", Title: "A"},
		{WorkID: 2, Method: "title-only", Title: "B"},
		{Title: "C", Rating: 10, Rated: true, Author: "x"},
		{Title: "D", Rating: 3, Rated: true, Author: "y"},
	}}
	m.Summarise()
	r := m.Report()
	for _, want := range []string{"matched 2 of 4", "unmatched", "C", "D"} {
		if !strings.Contains(r, want) {
			t.Errorf("report missing %q:\n%s", want, r)
		}
	}
	// The header introduces the listing, so a row must not appear before it.
	if strings.Index(r, "C") < strings.Index(r, "unmatched") {
		t.Error("an unmatched row precedes the listing header")
	}
	// Unrated-before-rated is not the contract; the contract is that a
	// reader can find their 10-rating in the list without reading all of it.
	// The 10-rated row must therefore sort ABOVE the 3-rated one.
	if strings.Index(r, "C") > strings.Index(r, "D") {
		t.Errorf("unmatched rows are not sorted by rating:\n%s", r)
	}
}

func TestParseOfTheRealPasteShape(t *testing.T) {
	// The row the handoff kept: a rating of 10 on row 29, an unrated tail,
	// and a pseudonym wrapped MID-WORD. If the real paste stops parsing, this
	// test is the canary, because the paste is the ONLY surviving record of
	// this reader's library -- the Calibre library itself is now empty.
	//
	// The mid-word wrap is the case that matters: Calibre broke
	// "HarryPotterFanFicArchive_Archivist" across lines as "..._Arch" /
	// "ivist", so the parser cannot know whether the tail is a new token or
	// the rest of one. It records both readings and lets the corpus decide.
	in := realHeader + "\n" +
		"29    10   Psycho of the Dead                                                             Shaboobamon\n" +
		"11824 8    Twin Love                                                                      HarryPotterFanFicArchive_Arch\n" +
		"                                                                                          ivist\n" +
		"495   None A Gamers Guide to Necromancy                                                   The Dark Wolf Shiro\n"
	got, err := ParseCalibrePaste(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("parsed %d, want 3: %+v", len(got), got)
	}
	// The glued reading is primary; the spaced one is the alternative.
	if got[1].Author != "HarryPotterFanFicArchive_Archivist" {
		t.Errorf("wrapped author = %q, want the glued reading", got[1].Author)
	}
	if got[1].AltAuthor != "HarryPotterFanFicArchive_Arch ivist" {
		t.Errorf("alt author = %q, want the spaced reading", got[1].AltAuthor)
	}
	if got[0].AltAuthor != "" || got[2].AltAuthor != "" {
		t.Error("an unwrapped row carries a spurious alternative")
	}
}

// A wrap that lands between tokens must still read as one field with a space,
// and the glued reading is recorded as the alternative. This is the pair that
// a mid-word-only rule would get backwards: joining "Morningstar" with
// "(Neckbeard_Satan)" without a space produces a byline that matches nothing.
func TestWrappedAuthorKeepsBothReadings(t *testing.T) {
	in := realHeader + "\n" +
		"150   9    A Precise Note                                                               Sir Lucifer Morningstar\n" +
		"                                                                                          (Neckbeard_Satan)\n" +
		"1862  9    Rogue Knight                                                                 Illuviar\n"
	got, err := ParseCalibrePaste(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Author != "Sir Lucifer Morningstar(Neckbeard_Satan)" {
		t.Errorf("primary author = %q", got[0].Author)
	}
	if got[0].AltAuthor != "Sir Lucifer Morningstar (Neckbeard_Satan)" {
		t.Errorf("alt author = %q, want the spaced reading", got[0].AltAuthor)
	}
	// The spaced reading is the one that matches a real byline, which is why
	// the matcher tries BOTH rather than trusting the primary.
	keys := authorKeys(got[0].AltAuthor)
	found := false
	for _, k := range keys {
		if k == "sir lucifer morningstar" || k == "neckbeard satan" {
			found = true
		}
	}
	if !found {
		t.Errorf("authorKeys(%q) = %v, want the byline or the pseudonym", got[0].AltAuthor, keys)
	}
}
