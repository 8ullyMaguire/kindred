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
	"io"
	"log/slog"
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
		t.Fatalf("judge: %v", err)
	}

	svc := NewArenaService(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := svc.RunBatch(ctx)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if !strings.Contains(res.Skipped, "entity comparisons") {
		t.Fatalf("Skipped = %q, want it to name the entity comparisons waiting", res.Skipped)
	}

	// The tag's id must not have been rated as a work...
	if rows, err := s.RatingFor(ctx, []int64{900, 901}); err != nil {
		t.Fatalf("ratings: %v", err)
	} else if len(rows) != 0 {
		t.Fatalf("the tag ids were rated as works: %+v", rows)
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
