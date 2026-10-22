package corpus_test

// The consumer half of the live date check: a real work's parsed date must make
// the recency signal FIRE. Recency reads 0 as "undated" and returns ErrSkip, so
// before the display-date layout was accepted this failed for every work in the
// corpus while the hermetic suite stayed green.
//
// External test package (corpus_test) because signal imports corpus: an
// internal test importing signal is an import cycle.

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
	"git.polarisocial.xyz/kindred/kindred/internal/signal"
)

func TestLiveRecencyFiresForARealWork(t *testing.T) {
	path := os.Getenv("KINDRED_LIVE_CORPUS")
	if path == "" {
		t.Skip("KINDRED_LIVE_CORPUS not set")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var id int64
	// The HIGHEST id, not an arbitrary one: recent crawls get recent ids, and
	// a 2008 work scores ~0.00005 on a 365-day half-life -- a true number that
	// reads like a failure. The point is a date that parsed INTO the window.
	if err := db.QueryRow(`SELECT id FROM works
		WHERE update_date IS NOT NULL AND update_date != ''
		ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("pick a work: %v", err)
	}
	ent, err := corpus.NewAO3(db).Entity(context.Background(), id)
	if err != nil {
		t.Fatalf("entity %d: %v", id, err)
	}
	score, reason, err := signal.Recency{Now: float64(time.Now().Unix() / 86400)}.Score(
		rank.Candidate{ID: ent.ID, Kind: ent.Kind, Stats: ent.Stats}, nil, nil)
	if err != nil {
		t.Fatalf("recency on work %d: %v (ErrSkip means the date did not parse)", id, err)
	}
	if score <= 0 || reason == "" {
		t.Fatalf("recency on work %d scored %v (%q); a dated work must score", id, score, reason)
	}
	t.Logf("recency on live work %d: %.3f — %s", id, score, reason)
}
