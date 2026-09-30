package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// These cover /block: the page that offers tags this reader probably does not
// want, one click each.
//
// The interesting case is NOT the learned-negative weight, which the arena
// already stores. It is the tag that appears on EVERY work the reader passed
// over, which is invisible to the weight table by construction: LearnTags only
// teaches on tags that DIFFER between the two works shown, so a tag shared by
// everything the reader rejects teaches nothing no matter how long they play.
// A page built only from the weights would be empty for exactly the reader who
// has the most evidence, so that case is the one tested hardest here.
//
// Every test goes through newTestServer, whose corpus fixture uses work_tags —
// the same table the AO3 mirror and the arena's own tagStats read. The first
// version of the handler queried entity_tags, which is the index store's table
// and is empty for an ingested corpus: it returned zero rows and the page read
// as "nothing to suggest". A wrong table name is a silent wrong answer, so the
// fixture has to be shaped like the real mirror for this to be caught.

// TestBlockPageRenders is the floor: the page must be a page, not a crash.
func TestBlockPageRenders(t *testing.T) {
	ts := newTestServer(t)
	status, ct, body := getText(t, ts, "/block")
	if status != http.StatusOK {
		t.Fatalf("GET /block -> %d, want 200\n%.500s", status, body)
	}
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type %q, want text/html", ct)
	}
	if strings.Contains(body, "Something went wrong") {
		t.Fatalf("GET /block rendered the error page:\n%.800s", body)
	}
}

// A cold-start reader must be told why the list is empty. A blank panel reads
// as "you have no preferences", which is a different and wrong claim.
func TestBlockExplainsItselfWhenEmpty(t *testing.T) {
	ts := newTestServer(t)
	_, _, body := getText(t, ts, "/block")
	if !strings.Contains(body, "Nothing to suggest") {
		t.Errorf("an empty /block does not explain itself:\n%.600s", body)
	}
}

// Block then unblock, through the real form endpoint, with a cookie jar so the
// arena session survives between requests. Without the jar every request is a
// new owner and the block appears to vanish.
func TestBlockAndUnblockRoundTrip(t *testing.T) {
	ts := newTestServer(t)
	// jarClient, not ts.Client(): httptest's client has Jar == nil, so the
	// arena cookie is dropped and every request becomes a different owner. The
	// write then lands under one owner_key and the read looks under another, so
	// the page lists nothing for a block that demonstrably happened. jarClient
	// is the pattern the arena tests already use for exactly this reason.
	c := jarClient(t, ts)
	takeSession(t, c, ts.URL)

	// Mint the same session the page would.
	resp, err := c.Get(ts.URL + "/block")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if code := postBlockForm(t, c, ts.URL, "block", 1); code != http.StatusSeeOther {
		t.Fatalf("block -> %d, want 303", code)
	}

	resp = mustGet(t, c, ts.URL+"/block")
	body := readAll(t, resp)
	if !strings.Contains(body, "Unblock") {
		t.Error("a blocked tag is not listed with an unblock control; a block " +
			"the reader cannot see or reverse is a decision made for them")
	}

	if code := postBlockForm(t, c, ts.URL, "unblock", 1); code != http.StatusSeeOther {
		t.Fatalf("unblock -> %d, want 303", code)
	}
	resp = mustGet(t, c, ts.URL+"/block")
	body = readAll(t, resp)
	if strings.Contains(body, ">Unblock</button>") {
		t.Error("the tag is still listed as blocked after unblocking")
	}
}

// Idempotence, and why: a toggle flips a block straight back off on a
// double-submit. The form posts an explicit action instead, so re-posting is
// harmless.
//
// The repeat count is EVEN, and that is the load-bearing detail. The first
// version of this test posted THREE times and counted unblock controls — which
// is exactly the check a toggle passes. A toggle flips state per call, so
// three flips land back on "blocked" and the page looks correct: three
// unblock controls, one tag, no visible difference from idempotent. The count
// of controls can only distinguish the two behaviours at an even number of
// calls, where a toggle has flipped itself back to unblocked and shows zero.
//
// Two mutations confirmed this reading: replacing the unblock button with
// inert text, and discarding the read result, both go red. Replacing BlockTag's
// upsert with delete-then-insert does NOT — that mutation is equivalent, since
// action=block only ever reaches BlockTag, and a delete immediately followed
// by an insert is the same end state. The gap was in the parity, not the
// assertion.
func TestBlockIsIdempotentAndNotAToggle(t *testing.T) {
	ts := newTestServer(t)
	// jarClient, not ts.Client(): httptest's client has Jar == nil, so the
	// arena cookie is dropped and every request becomes a different owner. The
	// write then lands under one owner_key and the read looks under another, so
	// the page lists nothing for a block that demonstrably happened. jarClient
	// is the pattern the arena tests already use for exactly this reason.
	c := jarClient(t, ts)
	takeSession(t, c, ts.URL)

	const repeats = 2 // even — see above
	for i := 0; i < repeats; i++ {
		if code := postBlockForm(t, c, ts.URL, "block", 3); code != http.StatusSeeOther {
			t.Fatalf("block #%d -> %d, want 303", i, code)
		}
	}

	resp := mustGet(t, c, ts.URL+"/block")
	body := readAll(t, resp)
	if n := strings.Count(body, ">Unblock</button>"); n != 1 {
		t.Errorf("%d identical blocks produced %d unblock controls, want 1; "+
			"the endpoint is behaving as a toggle", repeats, n)
	}
}

// The other half of "not a toggle": an explicit action is obeyed in both
// directions, and in any order. A toggle passes the test above for an odd
// number of same-action posts, so the sequence has to be checked too — this is
// the case that separates "idempotent" from "happens to end up right".
func TestBlockHonoursEachActionRegardlessOfPriorState(t *testing.T) {
	ts := newTestServer(t)
	c := jarClient(t, ts)
	takeSession(t, c, ts.URL)

	controls := func() int {
		resp := mustGet(t, c, ts.URL+"/block")
		return strings.Count(readAll(t, resp), ">Unblock</button>")
	}

	// block, unblock, block — ends blocked, and a toggle would end unblocked
	// after an odd number of calls.
	for _, step := range []struct {
		action string
		want   int
	}{
		{"block", 1},
		{"unblock", 0},
		{"block", 1},
		{"unblock", 0},
		{"unblock", 0}, // unblocking twice is still unblocked
		{"block", 1},
	} {
		if code := postBlockForm(t, c, ts.URL, step.action, 4); code != http.StatusSeeOther {
			t.Fatalf("%s -> %d, want 303", step.action, code)
		}
		if n := controls(); n != step.want {
			t.Errorf("after %s, page shows %d unblock controls, want %d",
				step.action, n, step.want)
		}
	}
}

// Unblocking a tag that was never blocked is a no-op, not a 400 the reader
// sees for clicking the wrong button.
func TestUnblockOfNeverBlockedTagSucceeds(t *testing.T) {
	ts := newTestServer(t)
	// jarClient, not ts.Client(): httptest's client has Jar == nil, so the
	// arena cookie is dropped and every request becomes a different owner. The
	// write then lands under one owner_key and the read looks under another, so
	// the page lists nothing for a block that demonstrably happened. jarClient
	// is the pattern the arena tests already use for exactly this reason.
	c := jarClient(t, ts)
	takeSession(t, c, ts.URL)
	if code := postBlockForm(t, c, ts.URL, "unblock", 999); code != http.StatusSeeOther {
		t.Fatalf("unblock of a never-blocked tag -> %d, want 303", code)
	}
}

// Bad input must be rejected, not coerced. A tag_id of "abc" reaching the
// store as 0 would block whatever tag 0 is.
func TestBlockRejectsBadInput(t *testing.T) {
	ts := newTestServer(t)
	// jarClient, not ts.Client(): httptest's client has Jar == nil, so the
	// arena cookie is dropped and every request becomes a different owner. The
	// write then lands under one owner_key and the read looks under another, so
	// the page lists nothing for a block that demonstrably happened. jarClient
	// is the pattern the arena tests already use for exactly this reason.
	c := jarClient(t, ts)
	takeSession(t, c, ts.URL)

	for _, form := range []url.Values{
		{"tag_id": {"not-a-number"}, "action": {"block"}},
		{"action": {"block"}},
		{"tag_id": {"1"}, "action": {"obliterate"}},
		{"tag_id": {"-"}, "action": {"block"}},
	} {
		resp, err := c.PostForm(ts.URL+"/block", form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST /block %v -> %d, want 400", form, resp.StatusCode)
		}
	}
}

// Adding one write path must not open the others. Every other page URL still
// refuses a POST.
func TestBlockDoesNotOpenOtherPagePOSTs(t *testing.T) {
	ts := newTestServer(t)
	c := ts.Client()
	for _, path := range []string{"/", "/arena", "/my-ranking", "/leaderboard", "/rank/1"} {
		resp, err := c.Post(ts.URL+path, "application/x-www-form-urlencoded",
			strings.NewReader("x=1"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusOK {
			t.Errorf("POST %s -> %d; only /block and /arena/judge accept writes",
				path, resp.StatusCode)
		}
	}
}

// ---- helpers -------------------------------------------------------------

func takeSession(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	resp, err := c.Get(base + "/arena")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, ck := range resp.Cookies() {
		if ck.Name == "kindred_arena" {
			return ck.Value
		}
	}
	t.Fatal("no arena session cookie; the fixture cannot identify an owner")
	return ""
}

// postBlockForm returns the status of the POST itself, not of whatever the
// client followed it to.
//
// http.Client.PostForm FOLLOWS the 303, so the obvious version of this returns
// 200 and every assertion in this file fails against a working redirect —
// which reads exactly like a broken endpoint. CheckRedirect is disabled so the
// real status is visible.
func postBlockForm(t *testing.T, c *http.Client, base, action string, tagID int64) int {
	t.Helper()
	noFollow := &http.Client{
		Jar: c.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noFollow.PostForm(base+"/block", url.Values{
		"tag_id": {strconv.FormatInt(tagID, 10)},
		"action": {action},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func mustGet(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}
