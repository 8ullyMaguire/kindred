package harvest

// QA is the check that "22,614 saved" means "22,614 correct". Its fixture is the REAL work page in
// internal/crawl's testdata, parsed once and turned back into a stored row: the row and the page
// then agree by construction, so any mismatch a test produces was put there on purpose. A fixture
// written by hand would agree with a broken comparison just as happily.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
)

func realWorkPage(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("../crawl/testdata/ao3_work_page.html")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return body
}

func storedFromPage(t *testing.T, w *crawl.ParsedWork) StoredWork {
	t.Helper()
	return StoredWork{
		ID:         w.ID,
		Title:      w.Title,
		Authors:    w.Authors,
		WordCount:  w.WordCount,
		Kudos:      w.Kudos,
		Bookmarks:  deref(w.Bookmarks),
		Rating:     w.Rating,
		Complete:   w.Complete,
		UpdateDate: w.UpdateDate,
		Language:   w.Language,
		Chapters:   w.Chapters,
	}
}

// fixedFetcher returns one body for every URL.
type fixedFetcher struct{ body []byte }

func (f fixedFetcher) Fetch(context.Context, string) ([]byte, error) { return f.body, nil }

func TestQAAgreesWithThePageItStoredFrom(t *testing.T) {
	body := realWorkPage(t)
	w, err := crawl.ParseWork(body, fmt.Sprintf("%s/works/%d", "https://archiveofourown.org", 1))
	if err != nil {
		t.Fatal(err)
	}
	if w.Title == "" || w.WordCount == 0 {
		t.Fatalf("premise: the fixture parsed to an empty work: %+v", w)
	}
	res, err := QA(context.Background(), fixedFetcher{body}, "https://archiveofourown.org",
		[]StoredWork{storedFromPage(t, w)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Compared != 1 {
		t.Fatalf("compared %d of %d (parse failures %d)", res.Compared, res.Sampled, res.ParseFailed)
	}
	if !res.OK() {
		t.Fatalf("a row built from the page disagrees with the page: %v %v", res.Mismatch, res.Examples)
	}
}

// The defect QA exists to find: the stored title is wrong. A check that only counted "rows" would
// call this a success.
func TestQACatchesAWrongTitle(t *testing.T) {
	body := realWorkPage(t)
	w, err := crawl.ParseWork(body, "https://archiveofourown.org/works/1")
	if err != nil {
		t.Fatal(err)
	}
	row := storedFromPage(t, w)
	row.Title = "A Different Work Entirely"
	res, err := QA(context.Background(), fixedFetcher{body}, "https://archiveofourown.org", []StoredWork{row})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mismatch["title"] != 1 {
		t.Fatalf("title mismatch not counted: %v", res.Mismatch)
	}
	if res.OK() {
		t.Fatal("a wrong title was reported as OK")
	}
	if len(res.Examples) == 0 || !strings.Contains(res.Examples[0], "A Different Work Entirely") {
		t.Fatalf("the example does not name the disagreement: %v", res.Examples)
	}
}

// Counters move between a crawl and a check. That is life, not a defect -- and a report that says
// otherwise is ignored after its first red run.
func TestQATreatsCounterMovementAsDriftNotError(t *testing.T) {
	body := realWorkPage(t)
	w, err := crawl.ParseWork(body, "https://archiveofourown.org/works/1")
	if err != nil {
		t.Fatal(err)
	}
	row := storedFromPage(t, w)
	row.Kudos = w.Kudos - 7 // seven more kudos arrived after the crawl
	res, err := QA(context.Background(), fixedFetcher{body}, "https://archiveofourown.org", []StoredWork{row})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mismatch["kudos"] != 0 {
		t.Fatalf("counter movement counted as a mismatch: %v", res.Mismatch)
	}
	if res.Drift["kudos"] != 1 {
		t.Fatalf("drift not recorded: %v", res.Drift)
	}
	if !res.OK() {
		t.Fatalf("drift made the sample fail: %v", res.Mismatch)
	}
}

// But a column that never got filled is a defect, however alive the counter is.
func TestQACallsAStoredZeroAnError(t *testing.T) {
	body := realWorkPage(t)
	w, err := crawl.ParseWork(body, "https://archiveofourown.org/works/1")
	if err != nil {
		t.Fatal(err)
	}
	row := storedFromPage(t, w)
	row.Kudos = 0
	res, err := QA(context.Background(), fixedFetcher{body}, "https://archiveofourown.org", []StoredWork{row})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mismatch["kudos"] != 1 {
		t.Fatalf("a stored zero was not a defect: %v", res.Mismatch)
	}
}
