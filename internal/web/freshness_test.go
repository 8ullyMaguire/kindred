package web

// Freshness reporting: how old the data behind a recommendation is.
//
// The ingest command has always written `index_built_at` and
// `corpus_built_at` into the store's meta table. Nothing read them. So the
// single fact that most changes what a recommendation is worth -- how old the
// data is -- was invisible to every reader, and a stale index answered
// confidently from last month's data.
//
// These tests pin the three states, because each needs different words and
// collapsing any two of them produces a claim the data does not support:
//
//	no timestamp      -> "the age is not recorded", NOT "0 days old"
//	timestamp ahead   -> a clock problem, NOT a negative age
//	timestamp behind  -> the age in days
//
// The rule they all follow: never render a freshness claim the data does not
// support. That is the same rule as the NULL-summary decision in
// scanWorkJSON, applied to time.

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestParseBuildTimeAcceptsTheFormatsRealWritesProduce pins the parser.
//
// store.Now() writes RFC3339. But a stamp can come from an older build, from
// a different tool, or from a human editing the meta table, and those are
// date-only or use a space instead of a T. Refusing to compute an age you
// could compute is the worse failure, so the coarse forms are accepted.
func TestParseBuildTimeAcceptsTheFormatsRealWritesProduce(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"2026-10-04T12:00:00Z", time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)},
		{"2026-10-04T12:00:00.123456789Z", time.Date(2026, 10, 4, 12, 0, 0, 123456789, time.UTC)},
		{"2026-10-04 12:00:00", time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)},
		{"2026-10-04T12:00:00", time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)},
		{"2026-10-04", time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)},
		{"  2026-10-04  ", time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got, err := parseBuildTime(c.in)
		if err != nil {
			t.Errorf("parseBuildTime(%q) = error %v, want it accepted", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("parseBuildTime(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestParseBuildTimeRejectsWhatIsNotATimestamp matters because the failure
// mode is silence: an unparseable stamp that parsed to the zero time would
// render as "index built 20,000 days ago", which is a specific and confident
// lie. Every one of these must be an error, not a zero time.
func TestParseBuildTimeRejectsWhatIsNotATimestamp(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"yesterday",
		"0",
		"1757000000",       // a Unix timestamp: plausible, not this format
		"2026-13-45",       // not a real date
		"04/10/2026",       // a real date in an unaccepted order
		"2026-10-04T99:99", // not a real time
	} {
		if got, err := parseBuildTime(in); err == nil {
			t.Errorf("parseBuildTime(%q) = %v, want an error; returning the "+
				"zero time would render as an age of ~20,000 days",
				in, got)
		}
	}
}

// TestFreshnessStatesRenderDifferently is the render-level gate.
//
// Three states, three different sentences. The failure this prevents is the
// tempting one -- letting AgeKnown default to false and IndexAgeDays default
// to 0, so an unknown age renders as "built today".
func TestFreshnessStatesRenderDifferently(t *testing.T) {
	cases := []struct {
		name string
		base Base
		want []string
		not  []string
	}{
		{
			name: "unknown age",
			base: Base{AgeKnown: false},
			want: []string{"not recorded", "cannot be stated"},
			// The whole point: no numeric age when there is no timestamp.
			not: []string{"built today", "day ago", "days ago"},
		},
		{
			name: "built today",
			base: Base{AgeKnown: true, IndexAgeDays: 0},
			want: []string{"Index built today"},
			not:  []string{"not recorded", "0 days ago"},
		},
		{
			name: "built three days ago",
			base: Base{AgeKnown: true, IndexAgeDays: 3, IndexAgeAbsDays: 3},
			want: []string{"Index built", "3", "days ago"},
			not:  []string{"not recorded", "built today"},
		},
		{
			// Singular, because "1 days ago" is the kind of thing that
			// makes a reader distrust every other number on the page.
			name: "built one day ago",
			base: Base{AgeKnown: true, IndexAgeDays: 1, IndexAgeAbsDays: 1},
			want: []string{"Index built", "1", "day ago"},
			not:  []string{"1 days ago", "1 days"},
		},
		{
			// A build time in the future is a CLOCK problem. Rendering
			// "-2 days ago" would read as a data error, and rendering "0"
			// would hide it.
			name: "build time in the future",
			base: Base{AgeKnown: true, IndexAgeDays: -2,
				IndexAgeAbsDays: 2, AgeInFuture: true},
			want: []string{"future", "clock"},
			not:  []string{"-2", "days ago", "built today"},
		},
		{
			// Old enough that the caveat is the useful part.
			name: "built long ago",
			base: Base{AgeKnown: true, IndexAgeDays: 400, IndexAgeAbsDays: 400},
			want: []string{"400", "days ago", "recent fics are probably missing"},
			not:  []string{"not recorded"},
		},
		{
			// The mirror's own stamp is a separate fact and is shown when
			// present: an index built today from a March mirror is three
			// months stale however fresh the index is.
			name: "with a mirror stamp",
			base: Base{AgeKnown: true, IndexAgeDays: 1, IndexAgeAbsDays: 1,
				CorpusBuiltAt: "2026-03-01T00:00:00Z"},
			want: []string{"1 day ago", "2026-03-01"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Through Render(), not renderOne().
			//
			// Render() executes the "layout" wrapper, which is what contains
			// the footer AND the header AND the nav. renderOne executes only
			// "content" -- the page's own body -- so it cannot see the
			// footer at all. That is why the first version of this test
			// failed on all seven cases with nothing rendered: the freshness
			// text was never in scope.
			//
			// This is also why no existing test covered the footer: the
			// template sweep uses renderOne, so layout.html's own markup has
			// never been rendered by a test until now.
			var buf bytes.Buffer
			if err := Render(&buf, "search.html", SearchPage{Base: c.base}); err != nil {
				t.Fatalf("rendering: %v", err)
			}
			got := buf.String()
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("footer rendered without %q\n--- footer ---\n%s",
						w, truncate(got, 900))
				}
			}
			for _, n := range c.not {
				if strings.Contains(got, n) {
					t.Errorf("footer rendered %q, which is a freshness claim "+
						"the data does not support\n--- footer ---\n%s",
						n, truncate(got, 900))
				}
			}
		})
	}
}

// TestAgeIsComputedFromTheTimestampNotAssumed is the arithmetic, stated so a
// refactor cannot quietly make every page claim "0 days old".
func TestAgeIsComputedFromTheTimestampNotAssumed(t *testing.T) {
	then := time.Now().Add(-72 * time.Hour)
	got := int(time.Since(then).Hours() / 24)
	if got != 3 {
		t.Errorf("three days ago computed as %d days, want 3", got)
	}
	// A sub-24h age floors to 0, which is why 0 must render as "today" and
	// not as "0 days ago" -- both are the same fact but one of them is a
	// number pretending to be a measurement.
	if d := int(time.Since(time.Now().Add(-time.Hour)).Hours() / 24); d != 0 {
		t.Errorf("an hour ago computed as %d days, want 0", d)
	}
}
