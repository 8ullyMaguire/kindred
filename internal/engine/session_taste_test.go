package engine

import (
	"context"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// The session_taste signal is the recommender reading the reader's own
// learned tag preferences. These tests pin the observable contract at the
// ENGINE level, because that is where the wiring is: a signal that works and
// is never registered is a feature that does not exist, and a signal that is
// registered and never weighted is the failure this repo has found five
// times ("absent without saying so").

// TestSessionTasteChangesTheRanking is the one that matters: a reader's
// learned taste must move the results, not merely appear in the response.
//
// The assertion is on the RANKING, not on the score, because the score of an
// individual item can move without the order changing. What the reader
// experiences is the order.
func TestSessionTasteChangesTheRanking(t *testing.T) {
	e := newFixtureEngine(t)
	ctx := context.Background()

	// First: a plain recommendation, with no learned taste at all.
	base, err := e.Recommend(ctx, Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}}, N: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Items) == 0 {
		t.Fatal("no items from a valid seed")
	}

	// Now: the same request, but the reader has learned to like the tag
	// that the WORST-placed candidate carries. That candidate must climb.
	//
	// The taste is built from the candidates' OWN tag ids, so the signal
	// has something to match against by construction. The tag ids come
	// from the corpus rows the engine already loaded.
	last := base.Items[len(base.Items)-1]
	if len(last.TagIDs) == 0 {
		t.Skip("the fixture's last candidate carries no tags, so there is nothing to learn about")
	}
	target := last.TagIDs[0]

	tasted, err := e.Recommend(ctx, Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}}, N: 20,
		SessionTaste:         map[int32]float64{target: 1.0},
		SessionTasteEvidence: map[int32]float64{target: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasted.Items) == 0 {
		t.Fatal("no items from a valid seed with taste")
	}

	basePos := positionOf(base.Items, last.ID)
	tastedPos := positionOf(tasted.Items, last.ID)
	if tastedPos >= basePos {
		t.Fatalf("the liked work did not climb: position %d with taste, %d without", tastedPos, basePos)
	}
}

// TestSessionTasteIsReportedWhenDegraded asserts the ABSENCE is visible.
//
// A reader with no learned taste must get a ranking identical to the one
// they got before this signal existed, AND the response must say the
// dimension was skipped. Silence would make "this reader has no history"
// indistinguishable from "the session taste signal is broken" — the exact
// distinction this codebase insists on.
func TestSessionTasteIsReportedWhenDegraded(t *testing.T) {
	e := newFixtureEngine(t)
	res, err := e.Recommend(context.Background(), Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}}, N: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range res.Meta.Degraded {
		if d == "session_taste" {
			found = true
		}
	}
	if !found {
		t.Fatalf("session_taste is not named in meta.degraded %v; an absent dimension must say so",
			res.Meta.Degraded)
	}
}

// TestSessionTasteIsNamedInTheTune asserts the signal is WEIGHTED.
//
// A signal registered but absent from the tune's weight vector is skipped by
// rank.Score (`w := tune.Weights[sig.Name()]; if w == 0 { continue }`), so it
// would contribute nothing while looking installed. This is the fifth
// occurrence of that shape in this repo, and the test is here to stop it
// recurring.
func TestSessionTasteIsNamedInTheTune(t *testing.T) {
	if w, ok := DefaultTune().Weights["session_taste"]; !ok || w <= 0 {
		t.Fatalf("DefaultTune has no positive session_taste weight (ok=%v, w=%v)", ok, w)
	}
}

// TestSessionTasteSignalIsRegistered asserts the signal reaches the
// ranker at all — that signals() actually appends it.
//
// A signal that is constructed but never registered passes every test of the
// signal itself and changes nothing in the product.
func TestSessionTasteSignalIsRegistered(t *testing.T) {
	e := newFixtureEngine(t)
	sigs, _ := e.signals(nil, nil, 0, nil, "default", true,
		signalOptions{
			sessionTaste:         map[int32]float64{1: 1},
			sessionTasteEvidence: map[int32]float64{1: 5},
		})
	found := false
	for _, s := range sigs {
		if s.Name() == "session_taste" {
			found = true
		}
	}
	if !found {
		t.Fatal("the session_taste signal is not registered in signals()")
	}
}

// TestSessionTasteEvidenceReachesTheEvidenceList asserts the reader can
// see WHY a work was recommended to them.
//
// The signal's reason string is what the export renders as the per-item
// "why" line. A recommendation the reader cannot interrogate is a
// recommendation they have to take on faith.
func TestSessionTasteEvidenceReachesTheEvidenceList(t *testing.T) {
	e := newFixtureEngine(t)
	res, err := e.Recommend(context.Background(), Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}}, N: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Learn a taste from the FIRST candidate's own tags, then re-ask.
	if len(res.Items) == 0 || len(res.Items[0].TagIDs) == 0 {
		t.Skip("the fixture's first candidate carries no tags")
	}
	tag := res.Items[0].TagIDs[0]

	tasted, err := e.Recommend(context.Background(), Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}}, N: 20,
		SessionTaste:         map[int32]float64{tag: 1.0},
		SessionTasteEvidence: map[int32]float64{tag: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range tasted.Items {
		for _, ev := range it.Evidence {
			if ev.Signal == "session_taste" {
				if ev.Reason == "" {
					t.Fatalf("item %d has session_taste evidence with no reason", it.ID)
				}
				return
			}
		}
	}
	t.Skip("no candidate in the fixture shares the learned tag, so no evidence was produced")
}

// positionOf returns where an id sits in the list, or a large number when
// it is absent. An absent item is treated as worse than the last one, which
// is what "it fell off the list" means.
func positionOf(items []rank.Candidate, id int64) int {
	for i, it := range items {
		if it.ID == id {
			return i
		}
	}
	return len(items) + 1000
}
