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

func TestRunBatchLeavesEntityComparisonsAloneAndSaysSo(t *testing.T) {
	ctx := context.Background()
	s, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer s.Close()
	if _, err := s.TouchSession(ctx, "sess", "owner"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	// One entity comparison, judged.
	if _, err := s.RecordPresentation(ctx, store.Comparison{
		Kind: arena.KindTag, WorkA: 900, WorkB: 901,
		SessionKey: "sess", OwnerKey: "owner", Strategy: "maxinfo",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.RecordJudgementKind(ctx, "sess", "a", arena.KindTag); err != nil {
		t.Fatalf("judge tag: %v", err)
	}
	// AND a work comparison judged in the same period. Without it the stall is
	// unobservable: a fixture containing only entity rows takes the same path
	// whether the batch stalls or partitions, which is how the first version of
	// this test passed on a fence that would have discarded these rows.
	if _, err := s.RecordPresentation(ctx, store.Comparison{
		Kind: arena.KindWork, WorkA: 4001, WorkB: 4002,
		SessionKey: "sess", OwnerKey: "owner", Strategy: "maxinfo",
	}); err != nil {
		t.Fatalf("record work: %v", err)
	}
	if err := s.RecordJudgementKind(ctx, "sess", "b", arena.KindWork); err != nil {
		t.Fatalf("judge work: %v", err)
	}

	svc := NewArenaService(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := svc.RunBatch(ctx)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if !strings.Contains(res.Skipped, "ao3_tag=1") {
		t.Fatalf("Skipped = %q, want it to name the pending entity comparisons", res.Skipped)
	}

	// The tag's id must not have been rated as a work...
	if rows, err := s.RatingFor(ctx, []int64{900, 901}); err != nil {
		t.Fatalf("ratings: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("the tag ids were rated as works: %+v", rows)
	}
	// ...nor may the work row have been rated: processing it would let the
	// watermark pass this period and discard the tag comparison's influence.
	if rows, err := s.RatingFor(ctx, []int64{4001, 4002}); err != nil {
		t.Fatalf("work ratings: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("the batch processed the work row while a tag comparison was pending: %+v", rows)
	}
	// ...and the watermark must not have moved past rows nothing processed,
	// or their influence is discarded permanently when the per-kind batch
	// arrives.
	if last, err := s.MetaInt(ctx, "arena_last_run"); err != nil {
		t.Fatalf("watermark: %v", err)
	} else if last != 0 {
		t.Fatalf("the watermark advanced to %d over unprocessed entity comparisons", last)
	}
}

// The standings of an entity arena. The rows are seeded directly through the
// store: the per-kind batch does not exist yet, so nothing else can produce a
// rated entity — and this endpoint has to be right before it does, or the page
// that reads it would be the first place a mistake showed.
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

	// The work arena answers through the same endpoint and is unchanged.
	res2, err := ts.Client().Get(ts.URL + "/api/v1/arena/leaderboard")
	if err != nil {
		t.Fatalf("work get: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("work leaderboard status %d", res2.StatusCode)
	}

	// An unknown kind is refused here too, not only on /pair.
	res3, err := ts.Client().Get(ts.URL + "/api/v1/arena/leaderboard?kind=ao3_nope")
	if err != nil {
		t.Fatalf("bad kind get: %v", err)
	}
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown kind status %d, want 400", res3.StatusCode)
	}
}
