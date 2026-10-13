package web

// W5: the pure helpers in arena.go — trim, sessionRand, deltaKeys, round1,
// pairOK, isNoPairErr. None of them had a test, and the file is 1,300 lines.

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

func TestTrimCollapsesWhitespace(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"  hello   world  ", "hello world"},
		{"a\tb\nc", "a b c"},
		{"hi", "hi"},
		// Empty stays empty. It must NOT become "..." — "no summary" and
		// "a summary that was cut" are different claims, and only one is true.
		{"", ""},
		{"   ", ""},
		{"\n\n", ""},
	} {
		if got := trim(tc.in, 40); got != tc.want {
			t.Errorf("trim(%q, 40) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestTrimNeverExceedsItsBudget(t *testing.T) {
	// The bug this function was written for: cutting to max bytes and THEN
	// appending "..." overshoots by three. "party 🎉🎉🎉🎉🎉🎉 time" at max=20 cut
	// to 18 bytes and grew to 21, and a field that narrow then has to be
	// escaped or truncated by whatever renders it.
	//
	// So the property is about the bytes that come OUT, not about where the cut
	// landed -- which is also why a table of exact strings would not catch it.
	inputs := []string{
		"party 🎉🎉🎉🎉🎉🎉 time",
		"日本語のテキストです、これは長い要約です",
		"a summary that is entirely plain ascii and rather long indeed",
		"e\u0301\u0301\u0301 combining marks",
		"🎉",
		"ab",
	}

	for _, in := range inputs {
		for max := 0; max <= 40; max++ {
			got := trim(in, max)

			// Valid UTF-8: a byte-wise cut can split a multi-byte rune in half.
			if !utf8.ValidString(got) {
				t.Errorf("trim(%q, %d) = %q; not valid UTF-8", in, max, got)
				continue
			}

			if len(got) > max {
				// The one licensed overrun: a single rune wider than the whole
				// budget. Keeping one rune beats returning "", which would read
				// as "no summary".
				if utf8.RuneCountInString(got) != 1 {
					t.Errorf("trim(%q, %d) = %q (%d bytes); over budget and not a single rune",
						in, max, got, len(got))
				}
				continue
			}

			// Anything actually cut is marked with an ellipsis -- EXCEPT the
			// documented single-rune case below, where keeping one rune is the
			// deliberate answer and an ellipsis would not fit anyway. So the
			// rule is: no ellipsis implies the result is exactly one rune.
			collapsed := strings.Join(strings.Fields(in), " ")
			truncated := len(collapsed) > max && got != collapsed
			if truncated && !strings.HasSuffix(got, "...") {
				if utf8.RuneCountInString(got) != 1 {
					t.Errorf("trim(%q, %d) = %q; truncated from %q but not marked with an ellipsis",
						in, max, got, collapsed)
				}
			}
		}
	}
}

func TestTrimDropsTrailingPunctuationBeforeTheEllipsis(t *testing.T) {
	// "hello,..." reads as though the text contained a comma there. Cut on a
	// word that ends in punctuation and the cleaner ellipsis is "hello...".
	if got := trim("hello, world", 8); got != "hello..." {
		t.Errorf("trim(%q, 8) = %q; want %q", "hello, world", got, "hello...")
	}
}

func TestTrimIsIdempotentOnItsOwnOutput(t *testing.T) {
	// Re-trimming a value the page already trimmed must not keep shrinking it,
	// or a value passed through two layers loses text each time.
	once := trim("party 🎉🎉🎉🎉🎉🎉 time", 20)
	twice := trim(once, 20)
	if once != twice {
		t.Errorf("trim is not idempotent: %q then %q", once, twice)
	}
}

func TestSessionRandIsDeterministicPerSeed(t *testing.T) {
	// The contract, from the comment: deterministic per session, NOT globally
	// seeded. A reload must not reshuffle an unjudged pair, and two readers must
	// not see the same sequence.
	a := sessionRand("session-abc")
	b := sessionRand("session-abc")
	for i := 0; i < 50; i++ {
		if got, want := a(1000), b(1000); got != want {
			t.Fatalf("draw %d differs for the same seed: %d vs %d", i, got, want)
		}
	}

	c := sessionRand("session-abd")
	d := sessionRand("session-abd")
	same := true
	x, y := sessionRand("session-abc"), sessionRand("session-abd")
	for i := 0; i < 50; i++ {
		if x(1000) != y(1000) {
			same = false
			break
		}
	}
	if same {
		t.Error("two different seeds produced 50 identical draws; the seed is not affecting the sequence")
	}
	// And the same seed is reproducible from the same string, not from a global.
	for i := 0; i < 20; i++ {
		if c(1000) != d(1000) {
			t.Fatalf("draw %d differs for the same seed %q", i, "session-abd")
		}
	}
}

func TestSessionRandStaysInRange(t *testing.T) {
	r := sessionRand("seed")
	for i := 0; i < 200; i++ {
		if n := 7; true {
			got := r(n)
			if got < 0 || got >= n {
				t.Fatalf("draw %d = %d; want in [0,%d)", i, got, n)
			}
		}
	}
	// n <= 0 must not panic and must not divide by zero.
	for _, n := range []int{0, -1, -100} {
		if got := r(n); got != 0 {
			t.Errorf("r(%d) = %d; want 0", n, got)
		}
	}
}

func TestDeltaKeysIsSortedAndTotal(t *testing.T) {
	// Sorted because Go map order is randomised: an unsorted list would make the
	// same deltas render in a different order on every load.
	got := deltaKeys(map[int64]float64{9: 1, 2: 1, 40: 1, 13: 1, 6: 1})
	want := []int64{2, 6, 9, 13, 40}
	if len(got) != len(want) {
		t.Fatalf("deltaKeys = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("deltaKeys = %v; want %v", got, want)
		}
	}

	if got := deltaKeys(nil); len(got) != 0 {
		t.Errorf("deltaKeys(nil) = %v; want empty", got)
	}
	if got := deltaKeys(map[int64]float64{}); len(got) != 0 {
		t.Errorf("deltaKeys(empty) = %v; want empty", got)
	}
}

func TestRound1RoundsHalfAwayFromZero(t *testing.T) {
	// Values pinned by computing them, not by guessing: 1.45*10 is exactly 14.5
	// in float64, so this rounds to 1.5. 1.25 -> 1.3 shows the rounding is half
	// AWAY FROM ZERO, not banker's rounding (which would give 1.2) — a
	// distinction that only shows up on exact halves.
	for _, tc := range []struct {
		in   float64
		want float64
	}{
		{1.44, 1.4}, {1.45, 1.5}, {1.24, 1.2}, {1.26, 1.3}, {1.25, 1.3},
		{2.5, 2.5}, {1464.0583, 1464.1}, {-1.45, -1.5}, {-0.04, 0.0}, {0, 0},
	} {
		if got := round1(tc.in); got != tc.want {
			t.Errorf("round1(%v) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestRound1TurnsNonFiniteIntoZero(t *testing.T) {
	// A rating is a float that means "about this much"; NaN rendered into a
	// template prints "NaN", and +Inf prints "+Inf", both of which are worse
	// than the honest 0.
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := round1(f); got != 0 {
			t.Errorf("round1(%v) = %v; want 0", f, got)
		}
	}
}

func TestPairOKNeedsAllThreeIDs(t *testing.T) {
	ok := store.Comparison{ID: 1, WorkA: 2, WorkB: 3}
	if !pairOK(ok) {
		t.Error("pairOK rejected a comparison with three positive ids")
	}
	// Each field independently zero or negative: an id of 0 is the arena's
	// "unset", and a pair with an unset id is not a pair.
	for _, c := range []store.Comparison{
		{ID: 0, WorkA: 2, WorkB: 3},
		{ID: 1, WorkA: 0, WorkB: 3},
		{ID: 1, WorkA: 2, WorkB: 0},
		{ID: -1, WorkA: 2, WorkB: 3},
		{ID: 1, WorkA: -2, WorkB: 3},
		{ID: 1, WorkA: 2, WorkB: -3},
		{},
	} {
		if pairOK(c) {
			t.Errorf("pairOK(%+v) = true; want false", c)
		}
	}
}

func TestIsNoPairErrRecognisesTheSentinelAndItsWrappers(t *testing.T) {
	if !isNoPairErr(store.ErrNoPair) {
		t.Error("isNoPairErr(store.ErrNoPair) = false; want true")
	}
	if isNoPairErr(nil) {
		t.Error("isNoPairErr(nil) = true; want false")
	}
	if isNoPairErr(errors.New("something else")) {
		t.Error("isNoPairErr(another error) = true; want false")
	}

	// A WRAPPED sentinel is still the sentinel, and this is the case the string
	// comparison cannot see: fmt.Errorf("...: %w", ErrNoPair).Error() is
	// "pick a pair: no pair available", which is not equal to the sentinel's
	// message. internal/api/arena_handlers.go already uses errors.Is for exactly
	// this reason; this package should agree with it.
	wrapped := fmt.Errorf("pick a pair: %w", store.ErrNoPair)
	if !isNoPairErr(wrapped) {
		t.Errorf("isNoPairErr(wrapped sentinel) = false; want true (%q)", wrapped)
	}

	// And a same-message impostor must still be rejected, so the fix cannot be
	// "compare less strictly".
	if isNoPairErr(errors.New(store.ErrNoPair.Error())) {
		t.Error("isNoPairErr(same message, different error) = true; want false")
	}
}
