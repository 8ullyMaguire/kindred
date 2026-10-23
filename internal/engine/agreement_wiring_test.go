package engine

import (
	"context"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
)

// TestSeedAgreementIsRegisteredAndContributes pins the wiring, not the
// arithmetic: the signal package's own tests prove the score, and those tests
// are satisfied by a signal nobody registers. This one asks the engine.
//
// With two seeds and a frequency table, every item's evidence must carry
// seed_agreement with a positive value — a registered signal that skips for
// every candidate on a two-seed request would score exactly nothing while
// looking wired.
func TestSeedAgreementIsRegisteredAndContributes(t *testing.T) {
	e := newFixtureEngine(t)
	// Every fixture tag on 6 of 60 works: uniformly rare, so coverage is
	// readable and no tag is universal.
	byTag := map[int32]float64{}
	for id := int32(1); id <= 40; id++ {
		byTag[id] = 6
	}
	e.DocFreq = &corpus.DocFreq{Total: 60, ByTag: byTag}

	res, err := e.Recommend(context.Background(), Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}, {Kind: "ao3_work", ID: 2}},
		N: 5, Exclude: true,
	})
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(res.Items) == 0 {
		t.Fatal("no items")
	}
	seen, positive := 0, 0
	for _, it := range res.Items {
		for _, ev := range it.Evidence {
			if ev.Signal != "seed_agreement" {
				continue
			}
			seen++
			if ev.Value > 0 {
				positive++
			}
		}
	}
	if seen == 0 {
		t.Fatalf("seed_agreement appears in no item's evidence; it is registered nowhere")
	}
	if positive == 0 {
		t.Errorf("seed_agreement is present but scored 0 on all %d items; it contributes nothing", seen)
	}
	for _, d := range res.Meta.Degraded {
		if d == "seed_agreement" {
			t.Errorf("seed_agreement reported degraded on a two-seed request with a frequency table")
		}
	}
}

// TestSeedAgreementNamesItselfAsDegradedWithOneSeed is the other half of the
// unconditional-registration rule: with one seed there is no agreement to
// measure, and that must be visible in the response rather than indistinguishable
// from a working signal.
func TestSeedAgreementNamesItselfAsDegradedWithOneSeed(t *testing.T) {
	e := newFixtureEngine(t)
	byTag := map[int32]float64{}
	for id := int32(1); id <= 40; id++ {
		byTag[id] = 6
	}
	e.DocFreq = &corpus.DocFreq{Total: 60, ByTag: byTag}

	res, err := e.Recommend(context.Background(), Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}}, N: 5, Exclude: true,
	})
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	found := false
	for _, d := range res.Meta.Degraded {
		if d == "seed_agreement" {
			found = true
		}
	}
	if !found {
		t.Errorf("one seed, no seed_agreement in degraded %v: a signal that cannot run must say so", res.Meta.Degraded)
	}
}
