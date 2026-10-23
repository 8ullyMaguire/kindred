package corpus

// #54: the tag's shelf in numbers.
//
// The tests compute the expected values from the fixture with their own SQL
// rather than restating them as constants: a constant would pass against an
// implementation that returned the fixture's numbers for the wrong reasons, and
// the point of a median is that it is NOT the mean.
//
// Three claims are pinned, because each is a way the block could mislead:
//
//   - the median is the middle work, so a single very long work does not move it
//     (that is why it is not the mean);
//   - "most saved" is the maximum, with NULL bookmarks not winning;
//   - the trend is newest-first and its shares are relative to the busiest year,
//     so a bar cannot be sized against a value it cannot see.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func shelfDB(t *testing.T) *AO3 {
	t.Helper()
	dir := t.TempDir()
	path, err := testcorpus.New(120).Write(filepath.Join(dir, "corpus.db"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewAO3(db)
}

func TestTagShelfMediansAreNotMeans(t *testing.T) {
	ctx := context.Background()
	a := shelfDB(t)

	shelf, err := a.TagShelf(ctx, 1)
	if err != nil {
		t.Fatalf("shelf: %v", err)
	}
	if shelf.Works == 0 {
		t.Fatal("fixture tag 1 has no works, so nothing below can be asserted")
	}

	// The corpus's own answers, computed here.
	//
	// COUNT(DISTINCT work_id), because `work_tags` is keyed by
	// (work_id, tag_id, tag_type) and work 1 carries tag 1 under two types on
	// purpose: the raw row count is 85 for 84 works, and asserting the row count
	// would pin the double-count as correct.
	var wantWorks int
	var wantMedianWords, wantMedianKudos int64
	if err := a.DB.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT work_id) FROM work_tags WHERE tag_id = 1`).Scan(&wantWorks); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.QueryRowContext(ctx, `
		SELECT COALESCE(w.word_count,0) FROM
		  (SELECT DISTINCT work_id FROM work_tags WHERE tag_id = 1) t
		  JOIN works w ON w.id = t.work_id
		ORDER BY COALESCE(w.word_count,0) LIMIT 1 OFFSET ?`,
		wantWorks/2).Scan(&wantMedianWords); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.QueryRowContext(ctx, `
		SELECT COALESCE(w.kudos,0) FROM
		  (SELECT DISTINCT work_id FROM work_tags WHERE tag_id = 1) t
		  JOIN works w ON w.id = t.work_id
		ORDER BY COALESCE(w.kudos,0) LIMIT 1 OFFSET ?`,
		wantWorks/2).Scan(&wantMedianKudos); err != nil {
		t.Fatal(err)
	}

	if shelf.Works != wantWorks {
		t.Fatalf("works = %d, want %d", shelf.Works, wantWorks)
	}
	if shelf.MedianWords != wantMedianWords {
		t.Fatalf("median words = %d, want %d", shelf.MedianWords, wantMedianWords)
	}
	if shelf.MedianKudos != wantMedianKudos {
		t.Fatalf("median kudos = %d, want %d", shelf.MedianKudos, wantMedianKudos)
	}
	// The fixture's kudos span four orders of magnitude, so a mean equal to the
	// median would mean the query is not computing a median at all.
	if shelf.MeanKudos == shelf.MedianKudos && shelf.Works > 20 {
		t.Errorf("mean kudos == median kudos (%d) on a fixture with a wide spread; the "+
			"\"median\" is probably the mean", shelf.MedianKudos)
	}
}

func TestTagShelfFindsTheMostSavedWorkAndIgnoresNulls(t *testing.T) {
	ctx := context.Background()
	a := shelfDB(t)
	shelf, err := a.TagShelf(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var wantID, wantBM int64
	if err := a.DB.QueryRowContext(ctx, `
		SELECT w.id, w.bookmarks FROM work_tags wt JOIN works w ON w.id = wt.work_id
		WHERE wt.tag_id = 1 AND w.bookmarks IS NOT NULL
		ORDER BY w.bookmarks DESC, w.id LIMIT 1`).Scan(&wantID, &wantBM); err != nil {
		t.Fatal(err)
	}
	if shelf.Top.ID != wantID || shelf.Top.Bookmarks != wantBM {
		t.Fatalf("top work = %d with %d bookmarks, want %d with %d",
			shelf.Top.ID, shelf.Top.Bookmarks, wantID, wantBM)
	}
	if shelf.Top.Title == "" {
		t.Error("the most-saved work has no title, so the page would link an id")
	}
}

// spreadYears stamps the fixture's works with DIFFERENT update years.
//
// The fixture writes one fixed date for every work (deliberately: a recency
// assertion must not depend on when the suite runs), so a trend test over it sees
// a single year — and every ordering and share mutation SURVIVES a one-year
// trend, because there is nothing to order and the busiest year is the only year.
// Measured: "oldest-first" and "share of total instead of share of busiest" both
// survived until this fixture had more than one year in it.
func spreadYears(t *testing.T, a *AO3, tagID int64) map[string]int {
	t.Helper()
	ctx := context.Background()
	rows, err := a.DB.QueryContext(ctx,
		`SELECT DISTINCT w.id FROM work_tags wt JOIN works w ON w.id = wt.work_id
		 WHERE wt.tag_id = ? ORDER BY w.id`, tagID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	// 2024 gets few, 2025 the most, 2026 the rest: an uneven shape so a share
	// computed against the total differs from one computed against the busiest
	// year.
	want := map[string]int{}
	for i, id := range ids {
		year := "2026"
		switch {
		case i%6 == 0:
			year = "2024"
		case i%2 == 0:
			year = "2025"
		}
		if _, err := a.DB.ExecContext(ctx,
			`UPDATE works SET update_date = ? WHERE id = ?`, "15 Jan "+year, id); err != nil {
			t.Fatal(err)
		}
		want[year]++
	}
	return want
}

func TestTagShelfTrendIsNewestFirstAndSharesTheBusiestYear(t *testing.T) {
	ctx := context.Background()
	a := shelfDB(t)
	want := spreadYears(t, a, 1)
	if len(want) < 2 {
		t.Fatalf("the fixture spread produced %d year(s); a trend test needs more than one", len(want))
	}
	shelf, err := a.TagShelf(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(shelf.Years) != len(want) {
		t.Fatalf("trend has %d years, want %d (%v)", len(shelf.Years), len(want), want)
	}
	for _, y := range shelf.Years {
		if want[y.Year] != y.Works {
			t.Fatalf("year %s counted %d works, want %d", y.Year, y.Works, want[y.Year])
		}
	}
	for i := 1; i < len(shelf.Years); i++ {
		if shelf.Years[i-1].Year <= shelf.Years[i].Year {
			t.Fatalf("years are not newest-first: %q then %q",
				shelf.Years[i-1].Year, shelf.Years[i].Year)
		}
	}
	busiest := 0
	for _, y := range shelf.Years {
		if y.Works > busiest {
			busiest = y.Works
		}
	}
	for _, y := range shelf.Years {
		want := float64(y.Works) / float64(busiest)
		if y.Share < want-0.001 || y.Share > want+0.001 {
			t.Fatalf("year %s share = %.3f, want %.3f (works %d of busiest %d)",
				y.Year, y.Share, want, y.Works, busiest)
		}
	}
}

func TestTagShelfWithNoWorksIsEmptyNotAnError(t *testing.T) {
	ctx := context.Background()
	a := shelfDB(t)
	shelf, err := a.TagShelf(ctx, 999999)
	if err != nil {
		t.Fatalf("a tag with no works returned an error: %v", err)
	}
	if shelf.Works != 0 || len(shelf.Years) != 0 || shelf.Top.ID != 0 {
		t.Fatalf("shelf for an empty tag = %+v, want the zero value", shelf)
	}
}
