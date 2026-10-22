package engine

import (
	"context"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// bridgeFixture adds one crossover work and one work with no fandom.
//
// The shared fixture gives every work exactly one fandom family, so it cannot
// express the thing S11 is about. It also cannot express "no fandom at all",
// which is the state the empty note has to explain.
func bridgeFixture(t *testing.T) *Engine {
	t.Helper()
	c := testcorpus.New(60)
	c.Tags = append(c.Tags, testcorpus.Tag{ID: 12, Name: "crossover fandom (anime)"})
	// Work 7 was a Star Wars work. Give it the Harry Potter fandom and a
	// third family, so it is a bridge out of work 1 and a stronger one than
	// a two-fandom crossover.
	c.WorkTags = append(c.WorkTags,
		testcorpus.WorkTag{WorkID: 7, TagID: 1, TagType: "fandoms"},
		testcorpus.WorkTag{WorkID: 7, TagID: 12, TagType: "fandoms"},
	)
	// Work 61 has no fandom-shaped tag.
	c.Works = append(c.Works, testcorpus.Work{
		ID: 61, Title: "No Fandom Work", Authors: "author1",
		WordCount: 4000, Kudos: 10, Hits: 100, UpdateDate: "2026-01-15",
	})
	c.WorkTags = append(c.WorkTags,
		testcorpus.WorkTag{WorkID: 61, TagID: 5, TagType: "freeforms"},
		testcorpus.WorkTag{WorkID: 61, TagID: 6, TagType: "freeforms"},
	)
	return newFixtureEngineFrom(t, c)
}

func TestBridgesFindsWorksThatAlsoCarryAnotherFandom(t *testing.T) {
	e := bridgeFixture(t)
	res, err := e.Bridges(context.Background(), 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SeedFandoms) == 0 {
		t.Fatal("seed fandoms are empty; work 1 carries a fandom-shaped tag")
	}
	if len(res.Items) != 1 {
		t.Fatalf("got %d bridges (%v), want exactly work 7: work 1 has no other crossover",
			len(res.Items), bridgeIDs(res))
	}
	b := res.Items[0]
	if b.ID != 7 {
		t.Fatalf("bridge id = %d, want 7", b.ID)
	}
	joined := strings.Join(b.OtherFandoms, "|")
	if !strings.Contains(joined, "crossover fandom") || !strings.Contains(joined, "star wars") {
		t.Fatalf("other fandoms = %v, want the crossover and star wars", b.OtherFandoms)
	}
	if len(b.Evidence) == 0 || b.Evidence[0].Signal != "fandom_bridge" {
		t.Fatalf("bridge carries no fandom_bridge evidence: %+v", b.Evidence)
	}
	for _, item := range res.Items {
		if item.ID == 1 {
			t.Fatal("the seed itself was returned as a bridge")
		}
	}
}

func TestBridgesExplainsAWorkWithNoFandom(t *testing.T) {
	e := bridgeFixture(t)
	res, err := e.Bridges(context.Background(), 61, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("got %v, want no bridges", bridgeIDs(res))
	}
	if !strings.Contains(res.Note, "no fandom-shaped tag") {
		t.Fatalf("note = %q, want it to say the work has no fandom-shaped tag", res.Note)
	}
}

func bridgeIDs(res BridgeResult) []int64 {
	out := make([]int64, len(res.Items))
	for i := range res.Items {
		out[i] = res.Items[i].ID
	}
	return out
}
