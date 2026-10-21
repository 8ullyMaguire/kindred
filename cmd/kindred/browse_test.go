package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/tagfamily"
)

func TestPrintBrowseTableNamesBothFamilies(t *testing.T) {
	res := tagfamily.Result{
		Items: []tagfamily.Item{{
			ID: 7, Title: "a work", Kudos: 12, Words: 3400, AgeDays: 30,
			MatchedTags: []string{"isekai"}, OtherFandom: "harry potter",
		}},
		Meta: tagfamily.Meta{UniverseWorks: 10, ConceptWorks: 5, Matched: 1, Returned: 1, RecentDays: 365},
	}
	out := captureStdout(t, func() { printBrowseTable(res) })
	if !strings.Contains(out, "universe=10") || !strings.Contains(out, "concept=5") {
		t.Errorf("table does not report both pools: %s", out)
	}
	if !strings.Contains(out, "harry potter") {
		t.Errorf("table does not report the other fandom: %s", out)
	}
}

// captureStdout runs f with os.Stdout redirected to a pipe and returns
// what f printed.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	f()
	w.Close()
	os.Stdout = old
	return <-done
}

func TestPrintBrowseTextWritesOneWorkPerLine(t *testing.T) {
	res := tagfamily.Result{
		Items: []tagfamily.Item{
			{ID: 7, Title: "a work", URL: "https://archiveofourown.org/works/7", AgeDays: 30, Kudos: 12, Words: 3400},
			{ID: 8, Title: "another work", URL: "https://archiveofourown.org/works/8", AgeDays: 40, Kudos: 3, Words: 900},
		},
		Meta: tagfamily.Meta{UniverseWorks: 10, ConceptWorks: 5, Matched: 2, Returned: 2, RecentDays: 365},
	}
	out := captureStdout(t, func() { printBrowseText(res) })
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// Every work is ONE line, and the line carries the work's identity.
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, one per work, got %d: %q", len(lines), out)
	}
	for i, want := range []string{"a work", "another work"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d does not name the work %q: %q", i, want, lines[i])
		}
	}
	// The URL is what a reader can open, so it must be on the line.
	if !strings.Contains(lines[0], "https://archiveofourown.org/works/7") {
		t.Errorf("line does not carry the work URL: %q", lines[0])
	}
}

func TestPrintBrowseTextSaysNothingWhenThereAreNoWorks(t *testing.T) {
	out := captureStdout(t, func() {
		printBrowseText(tagfamily.Result{Meta: tagfamily.Meta{Matched: 0, Returned: 0}})
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("an empty result must print nothing, got %q", out)
	}
}

// TestBrowseTextFileHoldsOneWorkPerLine covers the FILE path, not just
// the printer. The printer and the file writer used to be two copies of
// the same loop, and a heading line added to one of them was invisible
// to the other's test -- so the mutation below was not caught.
func TestBrowseTextFileHoldsOneWorkPerLine(t *testing.T) {
	res := tagfamily.Result{
		Items: []tagfamily.Item{
			{ID: 7, Title: "a work", URL: "https://archiveofourown.org/works/7"},
			{ID: 8, Title: "another work", URL: "https://archiveofourown.org/works/8"},
			{ID: 9, Title: "a third work", URL: "https://archiveofourown.org/works/9"},
		},
		Meta: tagfamily.Meta{Matched: 3, Returned: 3},
	}
	got := browseText(res)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want one line per work (3), got %d lines: %q", len(lines), got)
	}
	for i, want := range []string{"a work", "another work", "a third work"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d does not name the work %q: %q", i, want, lines[i])
		}
	}
}
