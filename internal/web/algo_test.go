package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
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
	if n := strings.Count(body, "/works/"); n == 0 {
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

// columnScores reads the printed scores out of one /algo column, in document
// order.
//
// The score is what a reader judges: each item prints `printf "%.2f" .Score`.
// Reading it back from the rendered HTML rather than from the engine's result
// is deliberate — the defect this pins lived entirely in the wiring (the page
// asked the engine for a diversified ranking and then promised the reader that
// only the weights differed), and a test of the handler's inputs would have
// seen nothing wrong.
func columnScores(t *testing.T, body, marker string) []float64 {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no column marked %q in the page", marker)
	}
	rest := body[i:]
	// The column ends where the next one begins.
	if j := strings.Index(rest[1:], `class="algo-column"`); j >= 0 {
		rest = rest[:j+1]
	}
	var out []float64
	for _, field := range strings.Split(rest, `class="score">`)[1:] {
		end := strings.IndexByte(field, '<')
		if end < 0 {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(field[:end]), 64)
		if err != nil {
			t.Fatalf("score %q is not a number: %v", field[:end], err)
		}
		out = append(out, v)
	}
	return out
}

// TestTheAlgoColumnsAreOrderedByTheirWeights pins the page's central promise.
//
// The page says "both columns rank this same work, from the same candidate
// pool: only the weights change". That was false: the engine ran the MMR
// diversifier on both columns, and MMR subtracts a novelty penalty measured in
// the score's own units. Two tunes producing different score magnitudes were
// therefore penalised unequally, and the printed scores were not descending —
// measured on the fixture, `default` and an extreme quality-only tune returned
// the same five works in the same order, so the arena was asking a reader to
// judge the diversifier.
//
// A column ordered by its weights has non-increasing scores by construction.
// Anything else means something re-ordered the list after scoring, and the
// reader is no longer looking at the algorithm.
func TestTheAlgoColumnsAreOrderedByTheirWeights(t *testing.T) {
	d, _ := algoDeps(t)

	for _, pair := range []string{"a=default&b=quality", "a=quality&b=fresh", "a=fresh&b=default"} {
		code, body := arenaBody(t, d, "/algo?seed=ao3_work:1&n=6&"+pair)
		if code != http.StatusOK {
			t.Fatalf("GET /algo?%s = %d: %s", pair, code, body)
		}

		for _, marker := range []string{`data-testid="algo-a"`, `data-testid="algo-b"`} {
			scores := columnScores(t, body, marker)
			if len(scores) < 3 {
				t.Fatalf("%s&%s: %s has %d scored items; an ordering assertion would be vacuous",
					pair, marker, marker, len(scores))
			}
			for i := 1; i < len(scores); i++ {
				if scores[i] > scores[i-1] {
					t.Fatalf("%s %s prints %v: item %d scores above item %d, so something "+
						"re-ordered the column after scoring. The page promises only the weights differ.",
						pair, marker, scores, i, i-1)
				}
			}
		}
	}
}

// The merged view: one list, both arms in it, and the impressions recorded so
// a read mark can be attributed to the arm that showed the work.
//
// The page is the ONLY place a reader can vote for an interleaved list, so a
// merged view that renders but records nothing is a feature that looks
// finished and produces no evidence at all. That is what this asserts against.
func TestTheMergedAlgoPageShowsBothArmsAndRecordsThem(t *testing.T) {
	d, st := algoDeps(t)
	d.Interleave = st

	code, body := arenaBody(t, d, "/algo?seed=ao3_work:1&a=default&b=quality&mode=interleave&n=10")
	if code != http.StatusOK {
		t.Fatalf("GET /algo?mode=interleave = %d: %s", code, body)
	}
	if n := strings.Count(body, `data-testid="interleaved-item"`); n == 0 {
		t.Fatalf("the merged list rendered no items: %s", body)
	}
	// Both arms are in the one list, each item naming its contributor.
	if !strings.Contains(body, `data-arm="default"`) || !strings.Contains(body, `data-arm="quality"`) {
		t.Fatal("the merged list does not name both arms on its items")
	}
	// The side-by-side columns must NOT render: a reader shown two lists
	// plus a merged one has been asked the question twice.
	if strings.Contains(body, "algo-column") {
		t.Fatal("the merged view rendered the two-column comparison as well")
	}

	arms, _, err := st.InterleaveStats(context.Background())
	if err != nil {
		t.Fatalf("InterleaveStats: %v", err)
	}
	if len(arms) == 0 {
		t.Fatal("the page rendered a merged list and recorded no impressions; " +
			"no read mark could ever be attributed to it")
	}
	for _, a := range arms {
		if a.Impressions == 0 {
			t.Fatalf("arm %q has an empty row in the standings", a.Arm)
		}
	}
}

// An odd slot count is refused in the page's own voice, because the reader can
// fix it in the URL -- and because the merge's asymmetry is real.
func TestTheMergedAlgoPageRefusesAnOddSlotCount(t *testing.T) {
	d, st := algoDeps(t)
	d.Interleave = st
	code, body := arenaBody(t, d, "/algo?seed=ao3_work:1&a=default&b=quality&mode=interleave&n=9")
	if code != http.StatusBadRequest {
		t.Fatalf("GET with n=9 = %d, want 400", code)
	}
	if !strings.Contains(body, "even") {
		t.Fatalf("the page does not say why an odd slot count is refused: %s", body)
	}
	if strings.Contains(body, `data-testid="interleaved-item"`) {
		t.Fatal("the page rendered a merged list it says it cannot build")
	}
}

// Without a state database there is nowhere to record an impression, so the
// page refuses rather than showing a list whose result nobody could read.
func TestTheMergedAlgoPageRefusesWithoutAStore(t *testing.T) {
	d, _ := algoDeps(t)
	code, body := arenaBody(t, d, "/algo?seed=ao3_work:1&a=default&b=quality&mode=interleave&n=10")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("GET without an interleave store = %d, want 503", code)
	}
	if !strings.Contains(body, "attribute") {
		t.Fatalf("the page does not explain that the list cannot be attributed: %s", body)
	}
}
