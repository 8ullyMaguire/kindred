package api

import (
	"net/http"
	"testing"
)

// The reader-facing spelling of the arena's tag filter. A fandom name is
// accepted because that is what a reader knows; an unknown name is refused
// rather than silently unscoped.
func TestArenaPairAcceptsAFandomNameAndRefusesAnUnknownOne(t *testing.T) {
	ts := newTestServer(t)

	res, err := http.Get(ts.URL + "/api/v1/arena/pair?fandom=tag1")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("fandom=tag1 status = %d, want 200 (the pair may be empty, but the scope resolved)",
			res.StatusCode)
	}

	res, err = http.Get(ts.URL + "/api/v1/arena/pair?fandom=not-a-real-fandom")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown fandom status = %d, want 400: an unscoped arena is the wrong answer",
			res.StatusCode)
	}
}
