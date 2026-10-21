package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestHandleBrowseReturnsTheFamilyQueryWithItsCounts(t *testing.T) {
	ts := newTestServer(t)
	// The parameters are FAMILY KEYS: "worm" and "isekai", the same
	// vocabulary the CLI and the /browse page use.
	resp, err := http.Get(ts.URL + "/api/v1/browse?universe=worm&concept=isekai&recent=365&n=5")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, readAll(t, resp))
	}
	body := readAll(t, resp)
	for _, want := range []string{`"items"`, `"meta"`, `"universe_works"`, `"concept_works"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}

func TestHandleBrowseRejectsAQueryWithNoFamily(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/v1/browse")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (a query with no family asks nothing)", resp.StatusCode)
	}
}

func TestHandleBrowseRejectsAnUnknownFamily(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/v1/browse?universe=not-a-family")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (an unknown family must not match "+
			"everything silently)", resp.StatusCode)
	}
}
