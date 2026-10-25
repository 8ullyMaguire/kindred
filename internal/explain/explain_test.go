package explain

import (
	"strings"
	"testing"
)

// The tests that matter are the two refusals: an item with no shared tag, and an item with ONE
// shared reader. Both must produce no duel at all, because a page that renders one sentence
// against an empty column asks a reader to judge nothing.

func TestBothSentencesNameTheSeed(t *testing.T) {
	d, ok := Build(7, "The Long Way", "A Long Winter", []string{"whump", "sickfic"}, 12)
	if !ok {
		t.Fatal("a work with two shared tags and twelve shared readers must be duelable")
	}
	if d.WorkID != 7 || d.Title != "The Long Way" {
		t.Fatalf("duel identity = %d %q", d.WorkID, d.Title)
	}
	for _, s := range d.Sentence {
		if !strings.Contains(s.Text, "A Long Winter") {
			t.Fatalf("%s sentence does not name the seed: %q", s.Kind, s.Text)
		}
		if s.Text == "" {
			t.Fatalf("%s sentence is empty", s.Kind)
		}
	}
	if d.Sentence[0].Kind != KindTags || d.Sentence[1].Kind != KindReaders {
		t.Fatalf("kinds = %s, %s; want tags then readers", d.Sentence[0].Kind, d.Sentence[1].Kind)
	}
}

func TestNoSharedTagIsNoDuel(t *testing.T) {
	if _, ok := Build(1, "w", "seed", nil, 40); ok {
		t.Fatal("a work with no shared tag was duelable")
	}
}

// ONE shared reader is a coincidence. The first version of this feature would have printed "1
// reader bookmarked both" as a competing explanation.
func TestOneSharedReaderIsNotAPattern(t *testing.T) {
	if _, ok := Build(1, "w", "seed", []string{"angst"}, 1); ok {
		t.Fatal("a single shared reader was treated as an explanation")
	}
	if _, ok := Build(1, "w", "seed", []string{"angst"}, 2); !ok {
		t.Fatal("two shared readers must be duelable")
	}
}

func TestTagSentenceCapsNamesAndCountsTheRest(t *testing.T) {
	shared := []string{"a", "b", "c", "d", "e"}
	e, ok := TagsExplanation("seed", shared)
	if !ok {
		t.Fatal("five shared tags must explain")
	}
	if !strings.Contains(e.Text, "a, b, c and 2 more") {
		t.Fatalf("text = %q, want three names and a remainder", e.Text)
	}
	// The sentence counts what it is NOT showing: "5 tags" with three names would read as a
	// contradiction.
	if !strings.Contains(e.Text, "5 tags") {
		t.Fatalf("text = %q, want the total count", e.Text)
	}
	if e.Support != 5 {
		t.Fatalf("support = %d, want 5", e.Support)
	}
	if len(e.Tags) != 5 {
		t.Fatalf("all shared tags must survive in Tags (the page links them), got %d", len(e.Tags))
	}
}

func TestOneSharedTagReadsAsSingular(t *testing.T) {
	e, ok := TagsExplanation("seed", []string{"whump"})
	if !ok {
		t.Fatal("one shared tag must explain")
	}
	if strings.Contains(e.Text, "1 tags") {
		t.Fatalf("text = %q: singular must not be plural", e.Text)
	}
}

func TestEmptySeedTitleStillReads(t *testing.T) {
	// A seed with a blank title is possible in a malformed mirror; "with “”" would be worse than
	// a phrase.
	e, ok := ReadersExplanation("  ", 3)
	if !ok {
		t.Fatal("a blank seed title must still explain")
	}
	if strings.Contains(e.Text, "\u201c\u201d") {
		t.Fatalf("text = %q: empty quotes", e.Text)
	}
	if !strings.Contains(e.Text, "your work") {
		t.Fatalf("text = %q, want a fallback phrase", e.Text)
	}
}
