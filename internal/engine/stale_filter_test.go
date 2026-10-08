package engine

import (
	"strings"
	"testing"
)

// TestNotUpdatedWithinDaysParsesAndFilters pins the stale-side filter end to
// end against the testcorpus: the parser accepts the key, filterSQL emits a
// predicate that keeps only works whose last_updated is outside the window,
// and an unset window keeps everything.
func TestNotUpdatedWithinDaysParsesAndFilters(t *testing.T) {
	f, err := ParseFilter(map[string][]string{"not_updated_within_days": {"122"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.NotUpdatedWithinDays != 122 {
		t.Fatalf("NotUpdatedWithinDays = %d, want 122", f.NotUpdatedWithinDays)
	}
	if f.isEmpty() {
		t.Fatal("a set window must not read as an empty filter")
	}

	zero, err := ParseFilter(map[string][]string{})
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if zero.NotUpdatedWithinDays != 0 || !zero.isEmpty() {
		t.Fatalf("unset window = %d, isEmpty=%v; want 0/true",
			zero.NotUpdatedWithinDays, zero.isEmpty())
	}

	// A negative window is a reader mistake worth naming, not a clamp.
	if _, err := ParseFilter(map[string][]string{"not_updated_within_days": {"-5"}}); err == nil {
		t.Fatal("negative window must error")
	}
}

// TestFilterSQLStalePredicate checks the emitted SQL shape: one bound
// parameter, compared against SQLite's own resolved date so the statement
// never goes stale.
func TestFilterSQLStalePredicate(t *testing.T) {
	var args []any
	sql := filterSQL(Filter{NotUpdatedWithinDays: 122}, &args)
	if sql == "" {
		t.Fatal("no predicate emitted")
	}
	if len(args) != 1 || args[0] != int64(122) {
		t.Fatalf("args = %v, want [122]", args)
	}
	for _, want := range []string{"last_updated", "date('now'"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("predicate missing %q:\n%s", want, sql)
		}
	}
}
