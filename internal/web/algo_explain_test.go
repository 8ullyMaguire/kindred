package web

// #55: one work, two sentences, one vote.
//
// The tests are about the two things that can go wrong and only look fine in a browser: an item
// with ONE sentence must not be offered as a duel, and a vote must reach the standings table
// under a session rather than being recorded against nobody.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// explainDeps builds a corpus whose first forty works are all bookmarked by five readers, so a
// candidate that shares a tag with the seed also has plenty of shared readers. Without the
// interactions no work could ever be explained by readers, and every duel test would be vacuous.
func explainDeps(t *testing.T) Deps {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	c := testcorpus.New(120)
	// A SECOND cohort with exactly ONE reader per work (plus the seed): works 41-60 are
	// bookmarked by user 9200 and by nobody else, so a candidate from that range that shares a
	// tag with the seed has exactly one shared reader -- below the floor, and therefore not a
	// duel.
	c.Users = append(c.Users, testcorpus.User{ID: 9200, Username: "only-one"})
	for w := int64(1); w <= 120; w++ {
		// The seed itself (1) plus every work the five-reader cohort does not touch, so each of
		// 21..120 shares EXACTLY one reader with the seed.
		if w == 1 || w > 20 {
			c.Interactions = append(c.Interactions, testcorpus.Interaction{UserID: 9200, WorkID: w})
		}
	}
	for i := 1; i <= 5; i++ {
		c.Users = append(c.Users, testcorpus.User{ID: int64(9000 + i), Username: "reader" + itoa10(i)})
		for w := int64(1); w <= 20; w++ {
			c.Interactions = append(c.Interactions, testcorpus.Interaction{UserID: int64(9000 + i), WorkID: w})
		}
	}
	corpusPath, err := c.Write(filepath.Join(dir, "corpus.db"))
	if err != nil {
		t.Fatalf("fixture corpus: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "state.db"), corpusPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return Deps{
		Version: "test",
		Engine:  &engine.Engine{Store: st, Corpus: corpus.NewAO3(st.Corpus)},
		Algo:    st,
	}
}

func itoa10(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// countDuels counts rendered duel blocks by their test id.
func countDuels(body string) int { return strings.Count(body, "data-testid=\"explain-duel\"") }

func TestExplainPageShowsBothSentencesForEveryDuel(t *testing.T) {
	d := explainDeps(t)
	code, body := arenaBody(t, d, "/algo?mode=explain&seed=ao3_work:1&n=20")
	if code != http.StatusOK {
		t.Fatalf("GET explain = %d: %s", code, body)
	}
	if countDuels(body) == 0 {
		t.Fatalf("no duels rendered; the page says: %s", note(body))
	}
	// Every duel must carry BOTH kinds. A block with a single sentence is the defect this page
	// exists to avoid, so the counts must agree.
	// Counted on the COLUMN marker, not on data-kind: the standings table carries one
	// data-kind per kind, so counting data-kind counts the standings too and reads as a
	// sentence standing alone in a column.
	tags := strings.Count(body, `class="algo-column" data-kind="tags"`)
	readers := strings.Count(body, `class="algo-column" data-kind="readers"`)
	if tags != countDuels(body) || readers != countDuels(body) {
		t.Fatalf("%d duels but %d tag columns and %d reader columns: a sentence is alone in its column",
			countDuels(body), tags, readers)
	}
	if !strings.Contains(body, "shares ") || !strings.Contains(body, "readers bookmarked both") {
		t.Fatalf("the sentences are not the ones the package writes: %s", note(body))
	}
	if !strings.Contains(body, "can be explained both ways") {
		t.Fatal("the page does not say how many of the ranked works qualified")
	}
	// One h1 per page: the layout owns it, and the duel list must not add a second.
	if n := strings.Count(body, "<h1"); n != 1 {
		t.Fatalf("%d h1 elements, want 1", n)
	}
}

// A work with a shared tag but only ONE shared reader must not be offered: "1 reader bookmarked
// both" is a coincidence, and printing it beside a real sentence asks a reader to judge nothing.
func TestAnItemWithOneSharedReaderIsNotADuel(t *testing.T) {
	d := explainDeps(t)
	ctx := context.Background()

	// The premise, measured rather than assumed: the ranking must contain at least one work
	// that shares a tag with the seed and has EXACTLY ONE shared reader. If the fixture stops
	// producing one, this test says so instead of passing vacuously -- the first version of it
	// asserted a string that never appeared and stayed green with the floor deleted.
	seed, err := engine.ParseSeed("ao3_work:1")
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Engine.Recommend(ctx, engine.Request{
		Kind: seed.Kind, Seeds: []engine.Seed{seed}, N: 20, Exclude: true, Lambda: 1,
	})
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	ao3 := corpus.NewAO3(d.Engine.Store.Corpus)
	seedPairs, err := ao3.TagPairs(ctx, []int64{seed.ID})
	if err != nil {
		t.Fatal(err)
	}
	seedTagIDs := map[int32]bool{}
	for _, p := range seedPairs[seed.ID] {
		seedTagIDs[p.ID] = true
	}
	var oneReader []int64
	for _, c := range res.Items {
		pairs, err := ao3.TagPairs(ctx, []int64{c.ID})
		if err != nil {
			t.Fatal(err)
		}
		shares := false
		for _, p := range pairs[c.ID] {
			if seedTagIDs[p.ID] {
				shares = true
				break
			}
		}
		if !shares {
			continue
		}
		n, err := ao3.SharedReaders(ctx, seed.ID, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			oneReader = append(oneReader, c.ID)
		}
	}
	if len(oneReader) == 0 {
		t.Fatalf("premise: no ranked work shares exactly ONE reader with the seed, so the floor "+
			"cannot be tested (the ranking returned %d works)", len(res.Items))
	}

	code, body := arenaBody(t, d, "/algo?mode=explain&seed=ao3_work:1&n=20")
	if code != http.StatusOK {
		t.Fatalf("GET explain = %d: %s", code, body)
	}
	for _, id := range oneReader {
		if strings.Contains(body, fmt.Sprintf(`data-testid="explain-duel" data-work="%d"`, id)) {
			t.Fatalf("work %d has ONE shared reader and was offered as a duel", id)
		}
	}
	// And no rendered sentence rests on a single person.
	if strings.Contains(body, "readers &middot; 1<") {
		t.Fatalf("a duel was rendered with one shared reader: %s", note(body))
	}
}

// The vote: no session, no row. The POST mints one, records under it, and the standings move.
func TestExplainVoteRecordsUnderASession(t *testing.T) {
	d := explainDeps(t)
	form := url.Values{"seed": {"ao3_work:1"}, "work": {"3"}, "choice": {"tags"}}
	code, body, res := postForm(t, d, "/algo/explain", form)
	if code != http.StatusSeeOther {
		t.Fatalf("POST /algo/explain = %d: %s", code, body)
	}
	if loc := res.Result().Header.Get("Location"); !strings.Contains(loc, "judged=tags") {
		t.Fatalf("redirect = %q, want a judged=tags target", loc)
	}
	rows, err := d.Algo.ExplanationStandings(context.Background())
	if err != nil {
		t.Fatalf("standings: %v", err)
	}
	if rows[0].Wins != 1 || rows[0].Judged != 1 {
		t.Fatalf("tags standings = %+v, want one win", rows[0])
	}
	if rows[1].Losses != 1 {
		t.Fatalf("readers standings = %+v, want the mirror", rows[1])
	}

	// And the page now shows the record rather than an empty table.
	_, page := arenaBody(t, d, "/algo?mode=explain&seed=ao3_work:1")
	if !strings.Contains(page, "explain-standings") {
		t.Fatal("the standings table is not on the page after a vote")
	}
	if !strings.Contains(page, "underpowered") && !strings.Contains(page, "no leader") {
		t.Fatal("one vote rendered a leader: the power floor is not applied on the page")
	}
}

func TestExplainVoteRefusesAnUnknownChoice(t *testing.T) {
	d := explainDeps(t)
	code, body, _ := postForm(t, d, "/algo/explain",
		url.Values{"seed": {"ao3_work:1"}, "work": {"3"}, "choice": {"both"}})
	if code != http.StatusBadRequest {
		t.Fatalf("POST with choice=both = %d, want 400: %s", code, body)
	}
	if !strings.Contains(body, "tags, readers or neither") {
		t.Fatalf("the refusal does not name the allowed answers: %s", body)
	}
}

func TestExplainVoteRefusesANonNumericWork(t *testing.T) {
	d := explainDeps(t)
	code, _, _ := postForm(t, d, "/algo/explain",
		url.Values{"seed": {"ao3_work:1"}, "work": {"the-long-way"}, "choice": {"tags"}})
	if code != http.StatusBadRequest {
		t.Fatalf("POST with a non-numeric work = %d, want 400", code)
	}
}

// An empty duel list must SAY why, or it reads as "nothing here is related".
func TestExplainPageExplainsAnEmptyDuelList(t *testing.T) {
	d := explainDeps(t)
	// Work 1 with a pool of one: the ranking returns nothing to explain.
	code, body := arenaBody(t, d, "/algo?mode=explain&seed=ao3_work:1&n=2")
	if code != http.StatusOK {
		t.Fatalf("GET explain = %d: %s", code, body)
	}
	if countDuels(body) == 0 && !strings.Contains(body, "explain-note") {
		t.Fatalf("an empty duel list with no reason: %s", note(body))
	}
}

// postForm drives a page form the way a browser does: an encoded body, no JSON.
func postForm(t *testing.T, d Deps, target string, values url.Values) (int, string, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	d.route(rec, req)
	return rec.Code, rec.Body.String(), rec
}

// note pulls the page's own explanation out of the body, so a failure says what the page said.
func note(body string) string {
	const marker = "data-testid=\"explain-note\">"
	if i := strings.Index(body, marker); i >= 0 {
		rest := body[i+len(marker):]
		if j := strings.Index(rest, "<"); j > 0 {
			return strings.TrimSpace(rest[:j])
		}
	}
	return "(no note)"
}
