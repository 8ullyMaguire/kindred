package rank

import (
	"context"
	"errors"
	"testing"
)

// stubSignal returns a fixed value, or an error.
type stubSignal struct {
	name  string
	value float64
	skip  bool
	fail  error
	calls int
}

func (s *stubSignal) Name() string { return s.name }

func (s *stubSignal) Score(c Candidate, seeds []Candidate, _ Store) (float64, string, error) {
	s.calls++
	if s.fail != nil {
		return 0, "", s.fail
	}
	if s.skip {
		return 0, "", ErrSkip
	}
	return s.value, s.name + " says " + itoa(s.value), nil
}

func itoa(f float64) string {
	if f == 1 {
		return "1"
	}
	if f == 0 {
		return "0"
	}
	return "other"
}

type nopStore struct{}

func (nopStore) Embedding(context.Context, string, int64) ([]float32, bool, error) {
	return nil, false, nil
}
func (nopStore) PeerRatings(context.Context, string, []int64) (map[int64]float64, error) {
	return nil, nil
}

func cands(ids ...int64) []Candidate {
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, Candidate{ID: id, Kind: "test", Title: "t"})
	}
	return out
}

func TestScoreSumsWeightedSignals(t *testing.T) {
	a := &stubSignal{name: "a", value: 1}
	b := &stubSignal{name: "b", value: 0.5}
	c := []Candidate{{ID: 1}, {ID: 2}}
	tune := Tune{Name: "t", Weights: map[string]float64{"a": 0.4, "b": 0.6}}

	out, meta, err := Score(context.Background(), c, nil, []Signal{a, b}, tune, nopStore{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d candidates, want 2", len(out))
	}
	// 1*0.4 + 0.5*0.6 = 0.7
	if got := out[0].Score; got < 0.69 || got > 0.71 {
		t.Fatalf("score = %v, want 0.7", got)
	}
	if len(out[0].Evidence) != 2 {
		t.Fatalf("evidence has %d entries, want 2 — a ranking that cannot explain itself is not one anyone can argue with", len(out[0].Evidence))
	}
	if meta.PoolSize != 2 || meta.Candidates != 2 {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestScoreOrdersByScoreThenID(t *testing.T) {
	// Equal scores must be ordered by id, so two identical requests
	// return identical lists.
	all := &stubSignal{name: "a", value: 1}
	out, _, err := Score(context.Background(), cands(3, 1, 2), nil,
		[]Signal{all}, Tune{Name: "t", Weights: map[string]float64{"a": 1}}, nopStore{})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{1, 2, 3} {
		if out[i].ID != want {
			t.Fatalf("position %d = %d, want %d (ties must break by id)", i, out[i].ID, want)
		}
	}
}

func TestZeroWeightSignalIsNotRun(t *testing.T) {
	// A signal weighted zero contributes nothing, so running it is waste.
	a := &stubSignal{name: "a", value: 1}
	zero := &stubSignal{name: "zero", value: 1}
	out, _, err := Score(context.Background(), cands(1), nil, []Signal{a, zero},
		Tune{Name: "t", Weights: map[string]float64{"a": 1}}, nopStore{})
	if err != nil {
		t.Fatal(err)
	}
	if zero.calls != 0 {
		t.Fatalf("the zero-weighted signal ran %d times", zero.calls)
	}
	if len(out[0].Evidence) != 1 {
		t.Fatalf("evidence = %+v, want only the weighted signal", out[0].Evidence)
	}
}

// TestSkippedSignalAppearsInMeta is the test for the bug the previous
// deployment shipped: signals that were inert in production and silent
// about it, so the scores looked plausible while two dimensions
// contributed nothing.
func TestSkippedSignalAppearsInMeta(t *testing.T) {
	good := &stubSignal{name: "tag_overlap", value: 0.8}
	dead := &stubSignal{name: "peer_rating", skip: true}

	out, meta, err := Score(context.Background(), cands(1), cands(9),
		[]Signal{good, dead}, Tune{Name: "t", Weights: map[string]float64{
			"tag_overlap": 0.5, "peer_rating": 0.5,
		}}, nopStore{})
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Degraded) != 1 || meta.Degraded[0] != "peer_rating" {
		t.Fatalf("meta.Degraded = %v, want [peer_rating] — a skipped signal must be named", meta.Degraded)
	}
	if len(out) != 1 {
		t.Fatalf("got %d candidates", len(out))
	}
	for _, e := range out[0].Evidence {
		if e.Signal == "peer_rating" {
			t.Fatal("a skipped signal appears in the evidence as though it contributed")
		}
	}
}

func TestDegradedIsSortedAndAbsentWhenHealthy(t *testing.T) {
	b := &stubSignal{name: "zeta", skip: true}
	a := &stubSignal{name: "alpha", skip: true}
	_, meta, err := Score(context.Background(), cands(1), cands(2),
		[]Signal{b, a}, Tune{Name: "t", Weights: map[string]float64{"zeta": 1, "alpha": 1}}, nopStore{})
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Degraded) != 2 || meta.Degraded[0] != "alpha" {
		t.Fatalf("Degraded = %v, want sorted [alpha zeta]", meta.Degraded)
	}

	ok := &stubSignal{name: "a", value: 1}
	_, meta2, err := Score(context.Background(), cands(1), cands(2),
		[]Signal{ok}, Tune{Name: "t", Weights: map[string]float64{"a": 1}}, nopStore{})
	if err != nil {
		t.Fatal(err)
	}
	if meta2.Degraded != nil {
		t.Fatalf("a healthy run reported Degraded = %v", meta2.Degraded)
	}
}

// TestCandidateWithNoSignalsIsDropped covers the distinction between "no
// opinion" and "a poor match". An entity every signal skipped is unknown,
// and ranking it at zero invents a fact.
func TestCandidateWithNoSignalsIsDropped(t *testing.T) {
	dead := &stubSignal{name: "dead", skip: true}
	good := &stubSignal{name: "good", value: 1}
	out, meta, err := Score(context.Background(), cands(1, 2), cands(9),
		[]Signal{good, dead}, Tune{Name: "t", Weights: map[string]float64{"good": 1, "dead": 1}}, nopStore{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d candidates, want 2: the one signal that fired applied to both", len(out))
	}
	_ = meta
}

func TestAllSignalsSkippedYieldsNothing(t *testing.T) {
	dead := &stubSignal{name: "dead", skip: true}
	out, meta, err := Score(context.Background(), cands(1, 2), cands(9),
		[]Signal{dead}, Tune{Name: "t", Weights: map[string]float64{"dead": 1}}, nopStore{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("got %d candidates, want 0", len(out))
	}
	if len(meta.Degraded) != 1 {
		t.Fatalf("Degraded = %v, want the signal named", meta.Degraded)
	}
}

func TestRealSignalErrorPropagates(t *testing.T) {
	// An unexpected error is not a skip: a bug must not be reported as a
	// signal that merely had no opinion.
	boom := errors.New("store exploded")
	bad := &stubSignal{name: "bad", fail: boom}
	_, _, err := Score(context.Background(), cands(1), cands(2),
		[]Signal{bad}, Tune{Name: "t", Weights: map[string]float64{"bad": 1}}, nopStore{})
	if err == nil {
		t.Fatal("a real error was swallowed")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the underlying one", err)
	}
}

func TestIsSkip(t *testing.T) {
	if !IsSkip(ErrSkip) {
		t.Fatal("ErrSkip is not recognised")
	}
	if IsSkip(errors.New("x")) {
		t.Fatal("an ordinary error was treated as a skip")
	}
	if IsSkip(nil) {
		t.Fatal("nil was treated as a skip")
	}
}
