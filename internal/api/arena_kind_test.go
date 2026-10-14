// SPDX-License-Identifier: AGPL-3.0-only

// Tests for the arena's kind parameter: resolution, refusal, and the fence that
// keeps entity comparisons out of the work rating batch.
//
// The fence is the load-bearing one. An entity id and a work id are both bare
// integers, so a batch that rated every judged row as a work would move a work's
// rating from a comparison of two tags -- a wrong answer that looks like a
// working one, and one that no assertion about the work path would notice.

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

func TestArenaKindResolvesAndRefuses(t *testing.T) {
	cases := []struct {
		query string
		want  arena.EntityKind
		bad   bool
	}{
		{"", arena.KindWork, false},
		{"?kind=ao3_work", arena.KindWork, false},
		{"?kind=ao3_tag", arena.KindTag, false},
		{"?kind=ao3_author", arena.KindAuthor, false},
		// Present but unknown is an ERROR, not a silent fallback to works:
		// "you asked for tags and I showed you works" is a wrong answer that
		// looks like a right one.
		{"?kind=ao3_whatever", "", true},
		{"?kind=", arena.KindWork, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/v1/arena/pair"+c.query, nil)
		got, err := ArenaKind(r)
		if c.bad {
			if err == nil {
				t.Errorf("%q: no error, want a refusal", c.query)
				continue
			}
			// The message has to say what to ask for instead.
			if !strings.Contains(err.Error(), "ao3_work") || !strings.Contains(err.Error(), "ao3_tag") {
				t.Errorf("%q: refusal %q does not name the allowlist", c.query, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.query, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q resolved to %q, want %q", c.query, got, c.want)
		}
	}
}

func TestRunBatchNeverRatesAnEntityAsAWork(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	if _, err := st.TouchSession(ctx, "sess", "owner"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	// A tag comparison judged, and nothing else.
	if _, err := st.RecordPresentation(ctx, store.Comparison{
		Kind: arena.KindTag, WorkA: 900, WorkB: 901,
		SessionKey: "sess", OwnerKey: "owner", Strategy: "maxinfo",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := st.RecordJudgementKind(ctx, "sess", "a", arena.KindTag); err != nil {
		t.Fatalf("judge: %v", err)
	}

	svc := NewArenaService(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := svc.RunBatch(ctx); err != nil {
		t.Fatalf("batch: %v", err)
	}

	// The load-bearing property, and the only one: a tag's id must never be
	// rated in the work table. An entity id and a work id are both bare
	// integers, so a batch that rated every judged row as a work would move some
	// WORK's rating from a comparison of two tags -- a wrong answer that looks
	// like a working one, and that no assertion about the work path would catch.
	if rows, err := st.RatingFor(ctx, []int64{900, 901}); err != nil {
		t.Fatalf("ratings: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("the tag ids were rated as works: %+v", rows)
	}
	// And the work arena's watermark did not move: the period had nothing in it,
	// and advancing the watermark over an empty period is what discards rows.
	if last, err := st.MetaInt(ctx, "arena_last_run"); err != nil {
		t.Fatalf("watermark: %v", err)
	} else if last != 0 {
		t.Fatalf("the work watermark advanced to %d over an empty work period", last)
	}
}

// The point of the per-kind batch: a comparison in a tag arena moves a TAG's
// rating, through its own tables, its own period and its own watermark, while the
// work arena keeps the bare keys it has always used.
func TestRunBatchRunsOnePeriodPerKind(t *testing.T) {
	ts, st := newTestServerFull(t)
	defer ts.Close()
	ctx := context.Background()

	if _, err := st.ReplaceEntityCandidates(ctx, arena.KindTag, []arena.EntityCandidate{
		{ID: 501, Name: "slow burn", Weight: 40},
		{ID: 502, Name: "time travel", Weight: 30},
	}); err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if _, err := st.TouchSession(ctx, "sess", "owner"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	// Two arenas judged in the same period.
	if _, err := st.RecordPresentation(ctx, store.Comparison{
		Kind: arena.KindTag, WorkA: 501, WorkB: 502,
		SessionKey: "sess", OwnerKey: "owner", Strategy: "maxinfo",
	}); err != nil {
		t.Fatalf("record tag: %v", err)
	}
	if err := st.RecordJudgementKind(ctx, "sess", "a", arena.KindTag); err != nil {
		t.Fatalf("judge tag: %v", err)
	}
	if _, err := st.RecordPresentation(ctx, store.Comparison{
		Kind: arena.KindWork, WorkA: 4001, WorkB: 4002,
		SessionKey: "sess", OwnerKey: "owner", Strategy: "maxinfo",
	}); err != nil {
		t.Fatalf("record work: %v", err)
	}
	if err := st.RecordJudgementKind(ctx, "sess", "b", arena.KindWork); err != nil {
		t.Fatalf("judge work: %v", err)
	}
	// Agreement is what decides influence and no endpoint sets it, so a test of
	// the batch has to reach the column. 1.0 is full influence.
	if _, err := st.DB.ExecContext(ctx, `UPDATE arena_comparisons SET agreement = 1.0`); err != nil {
		t.Fatalf("agreement: %v", err)
	}

	svc := NewArenaService(st, nil)
	res, err := svc.RunBatch(ctx)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if res.Updated != 4 {
		t.Fatalf("updated %d rows (%+v), skipped %q -- want 4: two tags and two works",
			res.Updated, res.ByKind, res.Skipped)
	}
	if res.Period == 0 {
		t.Fatal("the work period did not advance")
	}
	if res.ByKind[string(arena.KindTag)] == 0 {
		t.Fatalf("the tag arena has no period: %+v", res.ByKind)
	}

	// The tag's rating moved, and reads back through the store's own reader --
	// which is what pins the batch's column list to SaveRatingKind's.
	tags, err := st.RatingForKind(ctx, arena.KindTag, []int64{501, 502})
	if err != nil {
		t.Fatalf("tag ratings: %v", err)
	}
	win, ok := tags[501]
	if !ok {
		t.Fatalf("tag 501 has no rating after the batch: %+v", tags)
	}
	if win.Rating.Mu == arena.MU_INIT {
		t.Fatalf("tag 501 is still at the initial rating: %+v", win)
	}
	if win.Comparisons != 1 || win.Wins != 1 {
		t.Fatalf("tag 501 counted %d comparisons and %d wins, want 1 and 1", win.Comparisons, win.Wins)
	}
	if h, err := st.RatingHistoryKind(ctx, arena.KindTag, 501, 10); err != nil {
		t.Fatalf("history: %v", err)
	} else if len(h) != 1 {
		t.Fatalf("tag 501 has %d history rows, want 1", len(h))
	}

	// The work's rating moved through the work tables, and the work arena is
	// still at the bare watermark key -- an install that has been running must
	// not re-fold its history against a key that did not exist before.
	works, err := st.RatingFor(ctx, []int64{4001, 4002})
	if err != nil {
		t.Fatalf("work ratings: %v", err)
	}
	if w, ok := works[4001]; !ok || w.Comparisons != 1 {
		t.Fatalf("work 4001 was not rated: %+v", works)
	}
	if v, err := st.MetaInt(ctx, "arena_last_run"); err != nil || v == 0 {
		t.Fatalf("work watermark = %d, err %v; want the bare key set", v, err)
	}
	if _, err := st.MetaInt(ctx, "arena_last_run:ao3_tag"); err != nil {
		t.Fatalf("tag watermark missing: %v", err)
	}
}

// The standings of an entity arena. The rows are seeded directly through the
// store: the per-kind batch gives a tag period a rating, and this endpoint has to
// be right in the same shape whether the rows came from a batch or from a fixture.
func TestLeaderboardForAnEntityKind(t *testing.T) {
	ts, st := newTestServerFull(t)
	defer ts.Close()
	ctx := context.Background()

	if _, err := st.ReplaceEntityCandidates(ctx, arena.KindTag, []arena.EntityCandidate{
		{ID: 501, Name: "slow burn", Weight: 40},
		{ID: 502, Name: "time travel", Weight: 30},
	}); err != nil {
		t.Fatalf("candidates: %v", err)
	}
	for _, r := range []store.RatingRow{
		{WorkID: 501, Rating: arena.Rating{Mu: 1800, Phi: 60, Sigma: 0.06}, Comparisons: 12, Period: 1},
		{WorkID: 502, Rating: arena.Rating{Mu: 1500, Phi: 60, Sigma: 0.06}, Comparisons: 8, Period: 1},
	} {
		if err := st.SaveRatingKind(ctx, arena.KindTag, r); err != nil {
			t.Fatalf("rating %d: %v", r.WorkID, err)
		}
	}

	res, err := ts.Client().Get(ts.URL + "/api/v1/arena/leaderboard?kind=ao3_tag")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", res.StatusCode)
	}
	var out struct {
		Kind    string  `json:"kind"`
		Median  float64 `json:"median"`
		Entries []struct {
			Rank   int    `json:"rank"`
			WorkID int64  `json:"work_id"`
			Title  string `json:"title"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Kind != string(arena.KindTag) {
		t.Fatalf("kind %q, want %q", out.Kind, arena.KindTag)
	}
	if len(out.Entries) != 2 {
		t.Fatalf("got %d entries (%+v), want the 2 tags", len(out.Entries), out.Entries)
	}
	if out.Entries[0].Rank != 1 || out.Entries[0].WorkID != 501 || out.Entries[0].Title != "slow burn" {
		t.Fatalf("top entry %+v, want rank 1, id 501, named 'slow burn'", out.Entries[0])
	}
	if out.Entries[1].Rank != 2 {
		t.Fatalf("second entry rank %d, want 2", out.Entries[1].Rank)
	}
	if out.Median <= 0 {
		t.Fatal("median is unset: the note tells a reader what 1500 means, so it has to be a number")
	}

	res2, err := ts.Client().Get(ts.URL + "/api/v1/arena/leaderboard")
	if err != nil {
		t.Fatalf("work get: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("work leaderboard status %d", res2.StatusCode)
	}

	res3, err := ts.Client().Get(ts.URL + "/api/v1/arena/leaderboard?kind=ao3_nope")
	if err != nil {
		t.Fatalf("bad kind get: %v", err)
	}
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown kind status %d, want 400", res3.StatusCode)
	}
}
