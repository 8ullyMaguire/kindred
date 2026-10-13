package web

// W3 (continued): the freshness/age helpers and profileTagIDs.
//
// formatAge/ageHeaderString/ageSeconds/versionFor are pure functions of a Base
// and no test referenced any of them by name; freshness_test.go covers the
// *rendered* states, so these were exercised only indirectly. profileTagIDs is
// pure over a *profile.Profile.

import (
	"testing"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/profile"
)

func TestFormatAgeDistinguishesUnknownFromZero(t *testing.T) {
	// Three states, and the whole point is that they are three and not two:
	// "no build stamp", "built now", and "built long ago". Collapsing unknown
	// into zero makes a mirror of unknown vintage render as brand new.
	unknown := formatAge(Base{AgeKnown: false})
	if unknown != unknownAgeSentinel {
		t.Errorf("formatAge(AgeKnown:false) = %v; want the sentinel %v", unknown, unknownAgeSentinel)
	}
	if unknown == 0 {
		t.Error("the unknown sentinel is 0; it must be distinct from a measured zero")
	}

	now := formatAge(Base{AgeKnown: true, IndexAgeDays: 0})
	if now != 0 {
		t.Errorf("formatAge(known, 0 days) = %v; want 0", now)
	}
	if now == unknownAgeSentinel {
		t.Error("a measured zero age equals the unknown sentinel")
	}

	if got, want := formatAge(Base{AgeKnown: true, IndexAgeDays: 3}), 72*time.Hour; got != want {
		t.Errorf("formatAge(3 days) = %v; want %v", got, want)
	}
}

func TestFormatAgeRendersAFutureStampAsNegative(t *testing.T) {
	// A build time in the future is a clock problem, and the sign is how the
	// page says so. The magnitude comes from IndexAgeAbsDays, because the
	// template must not have to negate anything.
	got := formatAge(Base{AgeKnown: true, AgeInFuture: true, IndexAgeAbsDays: 3, IndexAgeDays: -3})
	if want := -72 * time.Hour; got != want {
		t.Errorf("formatAge(future, 3 abs days) = %v; want %v", got, want)
	}
}

func TestAgeHeaderStringUsesSecondsNotWholeDays(t *testing.T) {
	// The regression this function exists for: a mirror ingested 5m26s ago has
	// IndexAgeDays == 0, and a header derived from the day count read "0s" —
	// claiming the index was rebuilt this second. Verified live at the time.
	got := ageHeaderString(Base{AgeKnown: true, IndexAgeDays: 0, AgeSeconds: 326})
	if got == "0s" {
		t.Fatal(`ageHeaderString read "0s" for a 5m26s-old index; the day count is being used instead of AgeSeconds`)
	}
	if want := (326 * time.Second).String(); got != want {
		t.Errorf("ageHeaderString(326s) = %q; want %q", got, want)
	}
	if got != "5m26s" {
		t.Errorf("ageHeaderString(326s) = %q; want \"5m26s\"", got)
	}
}

func TestAgeHeaderStringSpellsUnknownAsAWord(t *testing.T) {
	// Not a negative number of seconds, and not "0s": the word is the only
	// spelling that cannot be read as a measurement.
	if got := ageHeaderString(Base{AgeKnown: false}); got != "unknown" {
		t.Errorf("ageHeaderString(unknown) = %q; want \"unknown\"", got)
	}
}

func TestAgeSecondsIsZeroOnlyForUnknown(t *testing.T) {
	// 0 is the machine-readable spelling of "no measurement", which is why the
	// human-readable half says "unknown" — the two spellings carry different
	// claims and both are sent.
	if got := ageSeconds(Base{AgeKnown: false}); got != "0" {
		t.Errorf("ageSeconds(unknown) = %q; want \"0\"", got)
	}
	if got := ageSeconds(Base{AgeKnown: true, AgeSeconds: 326}); got != "326" {
		t.Errorf("ageSeconds(326) = %q; want \"326\"", got)
	}
	if got := ageSeconds(Base{AgeKnown: true, AgeSeconds: 0}); got != "0" {
		t.Errorf("ageSeconds(measured 0) = %q; want \"0\"", got)
	}
}

func TestVersionForNamesTheBuildOrSaysUnknown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		built string
		want  string
	}{
		{"no stamp", "", "unknown"},
		{"garbage", "not a date", "unknown"},
		{"rfc3339", "2026-10-13T20:00:00Z", "2026-10-13"},
		{"date only", "2026-10-13", "2026-10-13"},
		{"sql-ish", "2026-10-13 20:00:00", "2026-10-13"},
		// The conversion is to UTC, so an offset that crosses midnight must
		// report the UTC date and not the local one.
		{"offset crossing midnight", "2026-10-13T01:30:00+02:00", "2026-10-12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionFor(Base{IndexBuiltAt: tc.built}); got != tc.want {
				t.Errorf("versionFor(%q) = %q; want %q", tc.built, got, tc.want)
			}
		})
	}
}

func TestProfileTagIDsSortsByWeightThenIDAndCaps(t *testing.T) {
	p := &profile.Profile{Tags: map[int32]float64{
		7: 0.5, 3: 0.9, 5: 0.5, 1: 0.1,
	}}

	// Sorted by weight descending; ties broken by id ascending. Without the
	// tie-break, Go's randomised map order makes the same profile rank fandoms
	// differently on every load — indistinguishable from a bug.
	got := profileTagIDs(p, 0)
	want := []int32{3, 5, 7, 1}
	if len(got) != len(want) {
		t.Fatalf("profileTagIDs = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("profileTagIDs = %v; want %v", got, want)
		}
	}

	// n caps the list, keeping the heaviest.
	if got := profileTagIDs(p, 2); len(got) != 2 || got[0] != 3 || got[1] != 5 {
		t.Errorf("profileTagIDs(p, 2) = %v; want [3 5]", got)
	}
	// A cap larger than the map is not an error.
	if got := profileTagIDs(p, 99); len(got) != 4 {
		t.Errorf("profileTagIDs(p, 99) = %v; want all 4", got)
	}
}

func TestProfileTagIDsOnNilAndEmpty(t *testing.T) {
	if got := profileTagIDs(nil, 5); got != nil {
		t.Errorf("profileTagIDs(nil) = %v; want nil", got)
	}
	if got := profileTagIDs(&profile.Profile{}, 5); got != nil {
		t.Errorf("profileTagIDs(no tags) = %v; want nil", got)
	}
}

func TestProfileTagIDsIsDeterministic(t *testing.T) {
	// Equal weights throughout: the tie-break is the only thing deciding the
	// order, so this is the input that would expose its absence.
	p := &profile.Profile{Tags: map[int32]float64{9: 1, 2: 1, 40: 1, 13: 1, 6: 1}}
	first := profileTagIDs(p, 0)
	for i := 0; i < 20; i++ {
		again := profileTagIDs(p, 0)
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("call %d differs at %d: %v vs %v", i, j, first, again)
			}
		}
	}
	// And the tie-break is ascending by id, not merely stable.
	want := []int32{2, 6, 9, 13, 40}
	for i := range want {
		if first[i] != want[i] {
			t.Fatalf("profileTagIDs = %v; want ascending ids %v", first, want)
		}
	}
}
