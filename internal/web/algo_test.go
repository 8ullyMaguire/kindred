package web

import (
	"context"
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

// algoDeps is a Deps wired for /algo: a fixture corpus, a state store, and
// two tunes whose weights deliberately differ so the page has something to
// say about why the columns differ.
func algoDeps(t *testing.T) (Deps, *store.Store) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	corpusPath, err := testcorpus.New(120).Write(filepath.Join(dir, "corpus.db"))
	if err != nil {
		t.Fatalf("fixture corpus: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "state.db"), corpusPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Two algorithms that disagree in a readable way.
	if err := st.SaveTune(ctx, "quality", map[string]float64{
		"tag_overlap": 0.10, "quality": 0.70, "recency": 0.05,
	}); err != nil {
		t.Fatalf("saving quality tune: %v", err)
	}
	if err := st.SaveTune(ctx, "fresh", map[string]float64{
		"tag_overlap": 0.10, "quality": 0.05, "recency": 0.70,
	}); err != nil {
		t.Fatalf("saving fresh tune: %v", err)
	}

	return Deps{
		Version: "test",
		Engine: &engine.Engine{
			Store:  st,
			Corpus: corpus.NewAO3(st.Corpus),
		},
		Algo: st,
	}, st
}

// TestTheAlgoPageRanksOneSeedThroughTwoAlgorithms is the page's reason to
// exist: the SAME seed, twice, with the weights named.
func TestTheAlgoPageRanksOneSeedThroughTwoAlgorithms(t *testing.T) {
	d, _ := algoDeps(t)
	code, body := arenaBody(t, d, "/algo?seed=ao3_work:1&a=default&b=quality")
	if code != http.StatusOK {
		t.Fatalf("GET /algo = %d: %s", code, body)
	}
	if n := strings.Count(body, "algo-column"); n != 2 {
		t.Fatalf("rendered %d columns, want 2", n)
	}
	// The delta line is load-bearing: without it the reader is asked to
	// judge two lists without being told what changed.
	if !strings.Contains(body, "Signals that differ") {
		t.Fatalf("no delta line: the page asks for a judgement without naming what differs")
	}
	if !strings.Contains(body, "quality") {
		t.Fatalf("the differing signal is not named")
	}
	if n := strings.Count(body, "/work/"); n == 0 {
		t.Fatalf("neither column links a work")
	}
}

// TestTheAlgoPageNamesAnUnknownAlgorithmInItsOwnVoice pins that a typo is a
// readable page, not a 500 and not a silently default-ranked column.
func TestTheAlgoPageNamesAnUnknownAlgorithmInItsOwnVoice(t *testing.T) {
	d, _ := algoDeps(t)
	code, body := arenaBody(t, d, "/algo?seed=ao3_work:1&a=default&b=nosuchtune")
	if code != http.StatusBadRequest {
		t.Fatalf("GET /algo with an unknown algorithm = %d, want 400", code)
	}
	if !strings.Contains(body, "nosuchtune") {
		t.Fatalf("the page does not name the unknown algorithm")
	}
	if strings.Contains(body, "algo-column") {
		t.Fatalf("the page rendered columns for a comparison that could not be made")
	}
}

func TestTheAlgoPageRefusesAnAlgorithmAgainstItself(t *testing.T) {
	d, _ := algoDeps(t)
	code, body := arenaBody(t, d, "/algo?seed=ao3_work:1&a=quality&b=quality")
	if code != http.StatusBadRequest {
		t.Fatalf("GET /algo comparing a tune with itself = %d, want 400", code)
	}
	if !strings.Contains(body, "itself") {
		t.Fatalf("the refusal does not say what was wrong: %s", body)
	}
}

func TestTheAlgoPageWithoutASeedIsTheUntouchedForm(t *testing.T) {
	d, _ := algoDeps(t)
	code, body := arenaBody(t, d, "/algo")
	if code != http.StatusOK {
		t.Fatalf("GET /algo = %d, want 200 (the form, not a 404)", code)
	}
	if !strings.Contains(body, "Name a work and pick two algorithms") {
		t.Fatalf("the untouched page does not say what to do")
	}
	// The registry must populate the selects, or the two names on offer are
	// a constant that can drift from what actually exists.
	if !strings.Contains(body, `value="quality"`) || !strings.Contains(body, `value="fresh"`) {
		t.Fatalf("the algorithm selects are not populated from the tune registry")
	}
}

// TestJudgingFromTheFormRecordsTheVote is the end-to-end page path: the
// control is a plain form, and its vote lands in algo_comparisons.
func TestJudgingFromTheFormRecordsTheVote(t *testing.T) {
	ctx := context.Background()
	d, st := algoDeps(t)

	form := url.Values{
		"seed":   {"ao3_work:1"},
		"algo_a": {"default"},
		"algo_b": {"quality"},
		"choice": {"b"},
	}
	post := httptest.NewRequest(http.MethodPost, "/algo/judge", strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	d.route(rec, post)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /algo/judge = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "judged=b") {
		t.Fatalf("redirect %q does not confirm the recorded choice", loc)
	}

	rows, err := st.AlgoStandings(ctx, "")
	if err != nil {
		t.Fatalf("standings: %v", err)
	}
	var wins int
	for _, r := range rows {
		if r.Algo == "quality" {
			wins = r.Wins
		}
	}
	if wins != 1 {
		t.Fatalf("quality has %d wins after a vote for b, want 1 (standings: %+v)", wins, rows)
	}

	// And the work arena's own tables must be untouched: a vote between
	// weight vectors is not evidence about two works' tags.
	var arenaRows int
	if err := st.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM arena_comparisons").Scan(&arenaRows); err != nil {
		t.Fatal(err)
	}
	if arenaRows != 0 {
		t.Fatalf("arena_comparisons holds %d rows after an algorithm vote", arenaRows)
	}
}

func TestJudgingFromTheFormRefusesAnUnknownChoice(t *testing.T) {
	d, _ := algoDeps(t)
	form := url.Values{
		"seed":   {"ao3_work:1"},
		"algo_a": {"default"},
		"algo_b": {"quality"},
		"choice": {"left"},
	}
	post := httptest.NewRequest(http.MethodPost, "/algo/judge", strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	d.route(rec, post)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /algo/judge with choice \"left\" = %d, want 400", rec.Code)
	}
}
