package api

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestParseWorksFiltersAcceptsAFamilyPair(t *testing.T) {
	// The values are FAMILY KEYS, not tag names. The parser resolves
	// them against the embedded families.json into the spellings that
	// make the family, so this test needs no database.
	q := parseQuery(map[string][]string{
		"universe": {"worm"},
		"concept":  {"isekai"},
	})
	filters, err := parseWorksFilters(q)
	if err != nil {
		t.Fatalf("parseWorksFilters: %v", err)
	}
	clause := strings.Join(filtersClauses(filters), " AND ")
	if !strings.Contains(clause, "t.name COLLATE NOCASE IN") {
		t.Errorf("clause %q has no tag IN filter", clause)
	}
	if n := strings.Count(clause, "t.name COLLATE NOCASE IN"); n != 2 {
		t.Errorf("clause %q has %d tag IN filters, want 2", clause, n)
	}
	// The worm family's SPELLINGS must be the bound arguments, because
	// "worm" on its own is only 7 works out of the family's ~4,300.
	// They are arguments, not clause text -- the clause carries only
	// placeholders -- so the assertion has to look at the args.
	var args []any
	for _, f := range filters {
		args = append(args, f.args...)
	}
	joined := fmt.Sprint(args)
	if !strings.Contains(joined, "parahumans series - wildbow") {
		t.Errorf("args %v do not carry the worm family's spellings; the "+
			"family key was used as a tag name", args)
	}
	// And the concept family's too.
	if !strings.Contains(joined, "reverse isekai") {
		t.Errorf("args %v do not carry the isekai family's tags", args)
	}
}

func TestParseWorksFiltersRejectsAnUnknownFamily(t *testing.T) {
	q := parseQuery(map[string][]string{"universe": {"not-a-family"}})
	if _, err := parseWorksFilters(q); err == nil {
		t.Fatal("an unknown universe= was accepted; a filter that cannot fire " +
			"must be a 400, not a query that returns everything")
	}
}

// parseQuery builds url.Values from a map, for the test.
func parseQuery(m map[string][]string) url.Values {
	v := url.Values{}
	for k, vals := range m {
		for _, val := range vals {
			v.Add(k, val)
		}
	}
	return v
}

// filtersClauses extracts the SQL clauses from parsed filters.
func filtersClauses(filters []worksFilter) []string {
	out := make([]string, 0, len(filters))
	for _, f := range filters {
		out = append(out, f.clause)
	}
	return out
}
