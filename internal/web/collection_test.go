package web

// #50 "Complete your collection": the author page tells a reader how far
// through a shelf they are, and can show them only what is left.
//
// The unit is about a NUMBER THE PAGE MAKES ABOUT A PERSON, so the tests pin the
// ways that number can be wrong rather than the markup:
//
//   - a work the site merely SHOWED is not read (a flattering count);
//   - the count is over the whole byline match, not the page (pagination
//     dressed as progress);
//   - no session means no claim at all (an anonymous reader is not "0 of 20");
//   - the ?unread=1 list must actually omit the read works, not just relabel
//     itself.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// collectionDeps builds an author page over the fixture corpus, with a state
// database whose session "s" belongs to owner "o".
func collectionDeps(t *testing.T) (Deps, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	path, err := testcorpus.New(120).Write(filepath.Join(dir, "corpus.db"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.TouchSession(context.Background(), "s", "o"); err != nil {
		t.Fatalf("session: %v", err)
	}
	return Deps{Version: "test", Engine: &engine.Engine{
		Store: st, Corpus: corpus.NewAO3(db),
	}}, st
}

// authorWorks returns the fixture ids whose byline matches, ordered as the page
// orders them (kudos desc).
func authorWorks(t *testing.T, d Deps, needle string) []int64 {
	t.Helper()
	rows, err := d.Engine.Corpus.DB.Query(
		`SELECT id FROM works WHERE authors LIKE ? ORDER BY kudos DESC, id`, "%"+needle+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestTheAuthorPageSaysHowMuchOfTheShelfIsRead(t *testing.T) {
	ctx := context.Background()
	d, st := collectionDeps(t)
	ids := authorWorks(t, d, "author1")
	if len(ids) < 8 {
		t.Fatalf("fixture gives %d works for author1; the test needs a shelf to make progress on", len(ids))
	}
	// Four read, and one only SHOWN: the shown one must not count.
	for _, id := range ids[:4] {
		if err := st.MarkRead(ctx, "o", store.WorkEntityID(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkShown(ctx, "o", []string{store.WorkEntityID(ids[4])}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/author?q=author1", nil)
	req.AddCookie(&http.Cookie{Name: ArenaCookieName, Value: "s"})
	rr := httptest.NewRecorder()
	d.renderAuthor(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, headOf(rr.Body.String(), 400))
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-testid="collection-progress"`) {
		t.Fatalf("no progress line for a reader with a session:\n%s", headOf(body, 800))
	}
	if !strings.Contains(body, `data-read="4"`) {
		t.Errorf("progress says read=%s, want 4 (a work that was only shown is not read):\n%s",
			attr(body, "data-read"), headOf(body, 800))
	}
	if !strings.Contains(body, "data-total=\""+itoaInt(int64(len(ids)))+"\"") {
		t.Errorf("progress says total=%s, want the whole shelf %d and not the page size",
			attr(body, "data-total"), len(ids))
	}
	if !strings.Contains(body, "to go") {
		t.Errorf("the line does not say how many are left")
	}
	// The read works must still be IN the unfiltered list: the page reports
	// progress, it does not silently hide what you have read.
	for _, id := range ids[:4] {
		if !strings.Contains(body, "/works/"+itoaInt(id)+`"`) {
			t.Errorf("work %d is missing from the unfiltered list: reporting progress must not "+
				"be a filter in disguise", id)
		}
	}
}

func TestTheUnreadFilterShowsOnlyWhatIsLeft(t *testing.T) {
	ctx := context.Background()
	d, st := collectionDeps(t)
	ids := authorWorks(t, d, "author1")
	for _, id := range ids[:4] {
		if err := st.MarkRead(ctx, "o", store.WorkEntityID(id)); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/author?q=author1&unread=1", nil)
	req.AddCookie(&http.Cookie{Name: ArenaCookieName, Value: "s"})
	rr := httptest.NewRecorder()
	d.renderAuthor(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, `data-testid="unread-filter-on"`) {
		t.Fatalf("the list is filtered but does not say so:\n%s", headOf(body, 700))
	}
	for _, id := range ids[:4] {
		if strings.Contains(body, "/works/"+itoaInt(id)+`"`) {
			t.Errorf("work %d is marked read and still listed under ?unread=1", id)
		}
	}
	if !strings.Contains(body, "/works/"+itoaInt(ids[4])+`"`) {
		t.Errorf("an unread work %d is missing from the filtered list", ids[4])
	}
	// Two numbers, two meanings, and confusing them is the defect:
	//
	//   - the PROGRESS line counts the shelf ("read 4 of 24, 20 to go"), which
	//     does not change because a filter is on;
	//   - the LIST count counts what the filter selected ("showing 20 of 20").
	//
	// A filtered list that reported the shelf's total above it, or a progress
	// line that reported the filtered total, both read as arithmetic errors.
	if !strings.Contains(body, "data-total=\""+itoaInt(int64(len(ids)))+"\"") {
		t.Errorf("the progress line stopped reporting the shelf: read=%s of total=%s",
			attr(body, "data-read"), attr(body, "data-total"))
	}
	if !strings.Contains(body, "of "+itoaInt(int64(len(ids)-4))+` matching work`) &&
		!strings.Contains(body, "of "+itoaInt(int64(len(ids)-4))+`&nbsp;`) {
		// The works-count line prints "Showing N of M matching works"; assert
		// the M, which is the filtered total.
		if !strings.Contains(body, "of "+itoaInt(int64(len(ids)-4))) {
			t.Errorf("the list count does not report the filtered total %d; a filtered list that "+
				"prints the shelf's size above it reads as a broken page:\n%s",
				len(ids)-4, headOf(body, 900))
		}
	}
}

func TestAnAuthorPageWithoutASessionClaimsNothing(t *testing.T) {
	d, _ := collectionDeps(t)
	rr := httptest.NewRecorder()
	d.renderAuthor(rr, httptest.NewRequest(http.MethodGet, "/author?q=author1", nil))
	body := rr.Body.String()
	if strings.Contains(body, `data-testid="collection-progress"`) {
		t.Fatalf("an anonymous reader is told how much of the author they have read:\n%s", headOf(body, 800))
	}
}

// attr pulls one attribute's value out of the body, for failure messages that
// name the wrong number rather than just "not found".
func attr(body, name string) string {
	i := strings.Index(body, name+`="`)
	if i < 0 {
		return "<absent>"
	}
	rest := body[i+len(name)+2:]
	if j := strings.Index(rest, `"`); j >= 0 {
		return rest[:j]
	}
	return "<unterminated>"
}

func itoaInt(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
