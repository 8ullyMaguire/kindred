package engine

import (
	"math"
	"testing"
)

// The engine had no test file at all. 617 lines of ranking logic -- how a request
// becomes a ranked list -- with nothing exercising it. What is here covers the two
// functions that are pure and therefore testable without a store, a corpus or a
// graph: ParseSeed, which turns user input into a Seed, and renormalise, which
// re-weights signals when one is dropped.
//
// What this file does NOT cover is the reason the gap mattered. Recommend,
// poolFor, signals, seedVector and attachSummaries all take a context and reach
// into the store, so testing them means a fixture corpus and a live database.
// Those are the parts most worth testing, and they remain untested. See
// docs/specs/2026-10-05-engine-coverage.md.

func TestParseSeed(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Seed
		wantErr bool
	}{
		{name: "work", in: "ao3_work:1234", want: Seed{Kind: "ao3_work", ID: 1234}},
		{name: "user", in: "ao3_user:7", want: Seed{Kind: "ao3_user", ID: 7}},
		{name: "zero id", in: "ao3_work:0", want: Seed{Kind: "ao3_work", ID: 0}},
		{name: "surrounding space", in: "  ao3_work:9  ", want: Seed{Kind: "ao3_work", ID: 9}},

		{name: "no colon", in: "ao3_work", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "empty kind", in: ":1234", wantErr: true},
		{name: "empty id", in: "ao3_work:", wantErr: true},
		{name: "non numeric id", in: "ao3_work:abc", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSeed(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseSeed(%q) = %+v, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSeed(%q) returned %v, want %+v", tt.in, err, tt.want)
			}
			if got != tt.want {
				t.Fatalf("ParseSeed(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

// A seed is untrusted input arriving from a query parameter, so "1abc" matters
// separately from "abc": Sscanf stops at the first non-digit and leaves err nil.
func TestParseSeedRejectsTrailingGarbage(t *testing.T) {
	for _, in := range []string{"ao3_work:12abc", "ao3_work:1.5", "ao3_work:+", "ao3_work: 5"} {
		if got, err := ParseSeed(in); err == nil {
			t.Errorf("ParseSeed(%q) = %+v, want an error: Sscanf accepts a valid prefix and reports no error", in, got)
		}
	}
}

// renormalise is called when a signal is dropped (a seed kind that cannot be
// expanded, say). Its documented contract is in the source: "rescales the
// surviving weights to sum to the same total, so removing a dimension changes the
// ranking's balance but not its scale."
//
// So it does NOT rescale to 1. With {a:0.3, b:0.1, c:0.6} and a dropped, the
// survivors are b=0.1, c=0.6 (total 0.7), scale = (0.7+0.3)/0.7 = 10/7, giving
// b=0.1428..., c=0.8571..., total 1.0 -- which is exactly the original 1.0. That
// is the whole point: the ranking keeps its scale and loses only the dimension.
//
// An earlier draft of this test asserted a total of 0.7, i.e. that the function
// does not restore what it dropped. The code is right and the test was wrong; the
// doc comment settles it.
func TestRenormaliseRestoresTheOriginalTotal(t *testing.T) {
	in := map[string]float64{"a": 0.3, "b": 0.1, "c": 0.6}
	originalTotal := 1.0
	got := renormalise(in, "a")

	if _, present := got["a"]; present {
		t.Errorf("the dropped signal is still present in %v", got)
	}

	// Proportional: b and c keep their 1:6 ratio.
	const ratio = 0.1 / 0.6
	if math.Abs(got["b"]/got["c"]-ratio) > 1e-9 {
		t.Errorf("got b:c = %v, want %v -- the survivors must keep their ratio", got["b"]/got["c"], ratio)
	}

	var total float64
	for _, v := range got {
		total += v
	}
	if math.Abs(total-originalTotal) > 1e-9 {
		t.Errorf("total = %v, want %v: renormalise must preserve the total, not rescale to 1", total, originalTotal)
	}
}

func TestRenormaliseMutatesItsInput(t *testing.T) {
	in := map[string]float64{"a": 0.3, "b": 0.7}
	got := renormalise(in, "a")
	if len(in) != 1 {
		t.Errorf("input map has %d entries after the call, want 1: renormalise deletes from the map it was passed", len(in))
	}
	if len(got) != 1 {
		t.Errorf("returned map has %d entries, want 1", len(got))
	}
}

func TestRenormaliseWithNothingToSpread(t *testing.T) {
	// Dropping a key that is absent is a no-op: `lost == 0` returns before any
	// scaling. This is the common case -- renormalise is called for signal kinds
	// that may not appear in a given request's weights at all.
	in := map[string]float64{"a": 0.5, "b": 0.5}
	got := renormalise(in, "zzz")
	if len(got) != 2 || math.Abs(got["a"]-0.5) > 1e-9 || math.Abs(got["b"]-0.5) > 1e-9 {
		t.Fatalf("got %v, want a and b both still at 0.5: an absent key must be a no-op", got)
	}

	// A key present with weight ZERO is also a no-op, and that is worth pinning:
	// `lost == 0` is an exact comparison, so it cannot be fooled by a weight that is
	// merely small.
	small := map[string]float64{"a": 0.5, "b": 0.5, "c": 0}
	gotSmall := renormalise(small, "c")
	if len(gotSmall) != 2 || math.Abs(gotSmall["a"]-0.5) > 1e-9 {
		t.Fatalf("got %v, want a and b both still at 0.5: a zero-weight key is a no-op", gotSmall)
	}
}

// If the survivors sum to zero there is nothing to scale against. The function
// guards that, and the guard is worth pinning: dividing by a zero total would put
// NaN in every weight, every ranking key would be NaN, and the sort would be
// arbitrary -- silently, and only for requests whose signals happen to cancel.
func TestRenormaliseWithZeroSurvivorsDoesNotProduceNaN(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   map[string]float64
		drop string
	}{
		{"cancelling survivors", map[string]float64{"a": 1, "b": -1, "c": 0.5}, "c"},
		{"zero survivor", map[string]float64{"a": 1, "b": 0, "c": 0}, "a"},
		{"only signal dropped", map[string]float64{"a": 1}, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renormalise(tc.in, tc.drop)
			for k, v := range got {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("got[%q] = %v: a NaN or Inf makes the whole ranking arbitrary", k, v)
				}
			}
		})
	}
}

func TestDefaultTuneIsUsable(t *testing.T) {
	// DefaultTune is the weight set every request starts from, so it is the one
	// thing in this file that decides whether any recommendation looks sensible.
	// A missing or zero weight is a signal that has been silently switched off.
	tune := DefaultTune()

	for _, name := range []string{
		"tag_overlap", "neighbourhood", "quality", "recency",
		"popularity", "embedding", "peer_rating",
	} {
		w, ok := tune.Weights[name]
		if !ok {
			t.Errorf("DefaultTune has no weight for %q", name)
			continue
		}
		if w <= 0 {
			t.Errorf("DefaultTune weight for %q is %v; a non-positive weight switches the signal off silently", name, w)
		}
	}

	var total float64
	for _, w := range tune.Weights {
		total += w
	}
	if total <= 0 {
		t.Fatalf("DefaultTune weights sum to %v: the engine would have no opinion at all", total)
	}
	if tune.Name == "" {
		t.Error("DefaultTune has no name; rank.Meta reports it and nothing to report")
	}
}

func TestPoolSizesAreOrdered(t *testing.T) {
	// FullPoolSize is what a paging request can ask for; DefaultPoolSize is what it
	// gets without asking. If the default ever exceeded the maximum, the clamp in
	// poolFor would be doing something nobody intended.
	if DefaultPoolSize >= FullPoolSize {
		t.Fatalf("DefaultPoolSize %d is not below FullPoolSize %d", DefaultPoolSize, FullPoolSize)
	}
}
