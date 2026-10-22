package engine

import (
	"context"
	"testing"
)

// TestLambdaOneRanksOnRelevanceAlone pins Request.Lambda, which exists for the
// algorithm comparison and nothing else.
//
// ## Why the knob exists
//
// /algo promises "both columns rank this same work, from the same candidate
// pool: only the weights change". That promise was FALSE. The engine ran
// diversify.Apply with 0.75 on both columns, and MMR scores a candidate
// as `lambda*score - (1-lambda)*similarity` — so a fixed novelty penalty shares
// units with the score. Two tunes whose weights produce different score
// MAGNITUDES (the fixture: 0.27-0.48 against 1.03-1.24) are therefore penalised
// differently, and the diversifier, not the weights, decided the order. Measured
// on the fixture: `default` and an extreme quality/recency-only tune returned
// the SAME five works in the SAME order.
//
// ## What this test holds
//
// At Lambda=1 the output IS the relevance order, and at the default it is not.
// The second half is what keeps the first honest: on a fixture where the
// diversifier happens to preserve order, the first assertion would pass with
// the knob ignored.
func TestLambdaOneRanksOnRelevanceAlone(t *testing.T) {
	eng := newFixtureEngine(t)
	ctx := context.Background()

	req := Request{
		Kind:    "ao3_work",
		Seeds:   []Seed{{Kind: "ao3_work", ID: 1}},
		N:       12,
		Exclude: true,
	}

	// Lambda unset: the site's own ranking, which is what every other caller
	// must keep getting.
	prod, err := eng.Recommend(ctx, req)
	if err != nil {
		t.Fatalf("Recommend (default lambda): %v", err)
	}

	pureReq := req
	pureReq.Lambda = 1
	pure, err := eng.Recommend(ctx, pureReq)
	if err != nil {
		t.Fatalf("Recommend (Lambda=1): %v", err)
	}

	// A list of two cannot demonstrate an ordering.
	if len(pure.Items) < 3 {
		t.Fatalf("fixture returned %d items at Lambda=1; ordering assertions would be vacuous",
			len(pure.Items))
	}

	for i := 1; i < len(pure.Items); i++ {
		if pure.Items[i].Score > pure.Items[i-1].Score {
			t.Fatalf("Lambda=1 is not the relevance order: item %d (work %d, score %.6f) "+
				"outranks item %d (work %d, score %.6f). The diversifier reshaped the list.",
				i, pure.Items[i].ID, pure.Items[i].Score,
				i-1, pure.Items[i-1].ID, pure.Items[i-1].Score)
		}
	}

	// The negative half. If the default produced the same sequence, this
	// fixture could not tell the two settings apart and the test above would
	// pass no matter what Recommend did with the field.
	if len(prod.Items) == len(pure.Items) {
		same := true
		for i := range prod.Items {
			if prod.Items[i].ID != pure.Items[i].ID {
				same = false
				break
			}
		}
		if same {
			t.Fatalf("default and Lambda=1 returned the same %d works in the same order: "+
				"this fixture cannot distinguish them, so the assertion above proves nothing",
				len(pure.Items))
		}
	}
}

// TestLambdaOutOfRangeFallsBackToTheSiteDefault pins the guard: a caller cannot
// disable the diversifier by passing 0 (which must mean "unset", not "off"),
// and a nonsensical value cannot silently produce a different ranking.
func TestLambdaOutOfRangeFallsBackToTheSiteDefault(t *testing.T) {
	eng := newFixtureEngine(t)
	ctx := context.Background()

	base := Request{
		Kind:    "ao3_work",
		Seeds:   []Seed{{Kind: "ao3_work", ID: 1}},
		N:       8,
		Exclude: true,
	}
	want, err := eng.Recommend(ctx, base)
	if err != nil {
		t.Fatalf("Recommend (default): %v", err)
	}

	// Out of range means "not a trade-off at all": zero and below (which must
	// read as UNset, since 0 is the field's own zero value) and above one
	// (which would invert the formula and reward similarity). Any value inside
	// (0, 1] is a legitimate setting and IS honoured — 0.0001 is a legal, if
	// extreme, request for almost-pure diversity.
	for _, lambda := range []float64{-1, 0, 1.5, 42} {
		req := base
		req.Lambda = lambda
		got, err := eng.Recommend(ctx, req)
		if err != nil {
			t.Fatalf("Recommend (Lambda=%v): %v", lambda, err)
		}
		if len(got.Items) != len(want.Items) {
			t.Fatalf("Lambda=%v returned %d items, default returned %d",
				lambda, len(got.Items), len(want.Items))
		}
		for i := range got.Items {
			if got.Items[i].ID != want.Items[i].ID {
				t.Fatalf("Lambda=%v changed the ranking at position %d (work %d, default %d): "+
					"an out-of-range value must fall back to the site default, not invent a ranking",
					lambda, i, got.Items[i].ID, want.Items[i].ID)
			}
		}
	}
}
