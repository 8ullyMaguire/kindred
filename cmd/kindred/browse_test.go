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
