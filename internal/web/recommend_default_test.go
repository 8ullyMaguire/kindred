package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"database/sql"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"

	_ "modernc.org/sqlite"
)

// TestAnUnseededRecommendStartsFromTheMostKudosedWorks is the idea-67 check:
// a reader who types nothing gets a list, and the page says WHERE the seeds
// came from. Both halves matter — a default that is not named reads as a
// taste match the mirror never measured.
func TestAnUnseededRecommendStartsFromTheMostKudosedWorks(t *testing.T) {
	dir := t.TempDir()
	path, err := testcorpus.New(40).Write(filepath.Join(dir, "corpus.db"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	eng := &engine.Engine{Corpus: corpus.NewAO3(db)}
	d := Deps{Version: "test", Engine: eng}

	rr := httptest.NewRecorder()
	d.renderRecommend(rr, httptest.NewRequest(http.MethodGet, "/recommend", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-testid="seed-origin"`) {
		t.Fatalf("the page ranked from default seeds but does not say so in seed-origin:\n%s", headOf(body, 600))
	}
	if !strings.Contains(body, "most-kudosed") {
		t.Errorf("the seed-origin note does not name popularity as its source:\n%s", headOf(body, 600))
	}
	// The seeds themselves must be on the page, echoed into the form, and
	// they must be the corpus's actual top N by kudos. Asserting only
	// "some ao3_work appears" would pass for an arbitrary three works, which
	// is exactly the defect (a default that is not the answer to the
	// question the note claims).
	var want []string
	rows, err := db.Query(`SELECT id FROM works ORDER BY COALESCE(kudos,0) DESC, id ASC LIMIT ?`, defaultSeedCount)
	if err != nil {
		t.Fatalf("reference query: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		want = append(want, "ao3_work:"+itoa(id))
	}
	rows.Close()
	// The SEED INPUT, not any mention of the id. "ao3_work:25" also appears in
	// the results' own "rank from this" links, so a bare substring check
	// passed while the seeds were three entirely different works — measured
	// by mutating the ORDER BY to ASC and watching this test stay green.
	// name="seed" is the attribute the form posts, so it names the field the
	// reader sees.
	for _, seed := range want {
		if !strings.Contains(body, `name="seed" value="`+seed+`"`) {
			t.Errorf("default seeds are not the top %d by kudos: no seed input for %s (want %v)", len(want), seed, want)
		}
	}
	if n := strings.Count(body, `name="seed" value="ao3_work:`); n != len(want) {
		t.Errorf("%d seed inputs carry a work id, want exactly %d", n, len(want))
	}
}

// TestAnUnseededRecommendFallsBackToTheFormWithNoCorpus pins the refusal: a
// server with no corpus renders the empty form, not a 500 and not a list of
// nothing. The form is a true page for a mirror that has not been ingested.
func TestAnUnseededRecommendFallsBackToTheFormWithNoCorpus(t *testing.T) {
	d := Deps{Version: "test"}
	rr := httptest.NewRecorder()
	d.renderRecommend(rr, httptest.NewRequest(http.MethodGet, "/recommend", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, `data-testid="seed-origin"`) {
		t.Errorf("claimed default seeds with no corpus loaded")
	}
	if !strings.Contains(body, `data-empty="true"`) {
		t.Errorf("the no-seeds state is not rendered:\n%s", headOf(body, 400))
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func headOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var _ = context.Background
