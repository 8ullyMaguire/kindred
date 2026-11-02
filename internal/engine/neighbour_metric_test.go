package engine

import "testing"

// The metric name is a parsed parameter, not a free string: an unknown value must
// fail loudly rather than silently falling back to pmi, because an eval arm that
// silently ran the baseline twice looks like a result.
func TestParseNeighbourMetric(t *testing.T) {
	cases := map[string]string{
		"":      "pmi",
		"pmi":   "pmi",
		"PMI":   "pmi",
		" npmi": "npmi",
		"nPMI":  "npmi",
	}
	for in, want := range cases {
		got, err := ParseNeighbourMetric(in)
		if err != nil {
			t.Fatalf("ParseNeighbourMetric(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseNeighbourMetric(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := ParseNeighbourMetric("cosine"); err == nil {
		t.Fatal("ParseNeighbourMetric accepted an unknown metric")
	}
}
