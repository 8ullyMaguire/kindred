package web

// A judgement belongs to the arena that presented it. These tests exist because
// the web judge used to resolve the comparison it had just recorded with
// LastJudgedInSession, which is WORK-ONLY: a tag judgement would have resolved
// to the last WORK pair in the session and learned tag weights from a choice the
// reader never made, silently, behind a row that looked legitimate.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func judgeDeps(t *testing.T) (Deps, *store.Store) {
	t.Helper()
	st, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return Deps{Version: "test", Engine: &engine.Engine{Store: st}}, st
}

func present(t *testing.T, st *store.Store, kind arena.EntityKind, a, b int64) {
	t.Helper()
	ctx := context.Background()
	// The session row has to exist first: judging reads owner_key FROM
	// arena_sessions and fails with "sql: no rows in result set" otherwise --
	// a message that reads like a broken session table rather than a fixture
	// that never minted one.
	if _, err := st.TouchSession(ctx, "s", "o"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordPresentation(ctx, store.Comparison{
		Kind: kind, SessionKey: "s", OwnerKey: "o", WorkA: a, WorkB: b,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAWebJudgementLandsInTheArenaThatPresentedIt(t *testing.T) {
	ctx := context.Background()
	d, st := judgeDeps(t)
	present(t, st, arena.KindWork, 1, 2)
	present(t, st, arena.KindTag, 10, 11)

	if err := d.judge(ctx, "s", "a", arena.KindTag); err != nil {
		t.Fatalf("judging a tag pair: %v", err)
	}

	comp, ok, err := st.LastJudgedInSessionKind(ctx, "s", arena.KindTag)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the tag judgement was not recorded at all")
	}
	if comp.Kind != arena.KindTag {
		t.Fatalf("recorded kind = %q, want %q", comp.Kind, arena.KindTag)
	}

	// The work presentation is untouched. Note what this asks: the earlier form
	// of this assertion used LastJudgedInSession and was WRONG -- that method
	// resolves the last judgement in the SESSION, not in one arena, so it found
	// the tag row and the test failed against correct code. A method's name is
	// not its filter; the kind-taking form is.
	if _, ok, err := st.LastJudgedInSessionKind(ctx, "s", arena.KindWork); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("a TAG judgement consumed the session's WORK presentation: " +
			"the judge path is resolving by session, not by kind")
	}
}

// NOT HERE: a work-path test. d.judge on a work pair reaches TagsForWorks, which reads the
// CORPUS database, and store.OpenMemory leaves that nil -- the call panics on a nil *sql.DB
// (database/sql.(*DB).conn(0x0)). Writing it needs the testcorpus fixture; until then the
// work path is covered by internal/api's own judge tests.

// GET /arena is a user-facing surface for the entity arenas, and these tests pin
// what it says rather than how it says it: the switcher offers every kind, an
// unknown kind lands on the work arena instead of failing, and a mirror with no
// corpus attached gets an honest message rather than a nil-handle crash.
func arenaBody(t *testing.T, d Deps, target string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	d.route(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		// The body carries the reason; a bare status code sends the next person
		// hunting through the router for it.
		t.Logf("GET %s -> %d: %s", target, rec.Code, rec.Body.String())
	}
	return rec.Code, rec.Body.String()
}

func TestTheArenaSwitcherOffersEveryKind(t *testing.T) {
	d, _ := judgeDeps(t)
	code, body := arenaBody(t, d, "/arena")
	if code != http.StatusOK {
		t.Fatalf("GET /arena = %d", code)
	}
	if !strings.Contains(body, "arena-kinds") {
		t.Fatal("the arena page has no switcher at all")
	}
	for _, k := range arena.Kinds {
		if !strings.Contains(body, k.Label()) {
			t.Errorf("the switcher never names %q", k)
		}
		// The ACTIVE arena is a marker, not a link to itself -- so it is the one
		// kind with no href, and asserting an href for every kind was wrong
		// when this test was first written.
		wantHref := k != arena.KindWork
		if got := strings.Contains(body, "?kind="+string(k)); got != wantHref {
			t.Errorf("href for %q present = %v, want %v", k, got, wantHref)
		}
	}
}

func TestAnEntityArenaSaysWhichKindItIsJudging(t *testing.T) {
	d, _ := judgeDeps(t)
	// No corpus attached in this fixture, so the honest outcome is the message
	// naming the kind -- which is also the assertion that the kind reached the
	// entity flow at all rather than falling through to works.
	code, body := arenaBody(t, d, "/arena?kind=ao3_tag")
	if code != http.StatusOK {
		t.Fatalf("GET /arena?kind=ao3_tag = %d", code)
	}
	if !strings.Contains(body, "tag") {
		t.Fatalf("the page for a tag arena does not name tags: %s", body)
	}
	if !strings.Contains(body, "Compare two tags") {
		t.Error("the heading does not name the arena being judged")
	}
}

func TestAnUnknownKindLandsOnTheWorkArenaRatherThanFailing(t *testing.T) {
	d, _ := judgeDeps(t)
	plain, bare := arenaBody(t, d, "/arena")
	_, nonsense := arenaBody(t, d, "/arena?kind=not-a-kind")
	if plain != http.StatusOK {
		t.Fatalf("GET /arena = %d", plain)
	}
	if nonsense != bare {
		t.Fatal("?kind=not-a-kind rendered something other than the work arena: " +
			"ParseKind is supposed to send anything unknown to works")
	}
}

// entityArenaDeps builds a deployment with a real mirror behind it: the entity
// kinds draw candidates and names from the corpus, and the loaders refuse a tag
// with fewer than three works, so a fixture with no tag rows cannot reach the
// pair path at all.
func entityArenaDeps(t *testing.T) (Deps, *store.Store) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	corpusPath, err := testcorpus.New(120).Write(filepath.Join(dir, "corpus.db"))
	if err != nil {
		t.Fatalf("fixture corpus: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "state.db"), corpusPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return Deps{Version: "test", Engine: &engine.Engine{Store: st}}, st
}

func TestAnEntityArenaRendersTwoNamedEntities(t *testing.T) {
	d, _ := entityArenaDeps(t)
	code, body := arenaBody(t, d, "/arena?kind=ao3_tag")
	if code != http.StatusOK {
		t.Fatalf("GET /arena?kind=ao3_tag = %d", code)
	}
	if n := strings.Count(body, "arena-card"); n != 2 {
		t.Fatalf("rendered %d entity cards, want 2: %s", n, body)
	}
	if !strings.Contains(body, "/tag/") {
		t.Error("an entity card does not link to the entity's own page")
	}
	for _, msg := range []string{"too few", "No tag pair"} {
		if strings.Contains(body, msg) {
			t.Fatalf("the pair path did not produce a pair (%q in the page): %s", msg, body)
		}
	}
	// A card that fell back to "tag 4711" would still render two cards, so the
	// assertion that matters is that a NAME came through the candidate table.
	if strings.Contains(body, `>tag `) {
		t.Error("a card is showing an id instead of a name")
	}
}

// The plan's own acceptance test for Step E: judge one entity pair through the
// FORM path, and check it landed in the entity's arena and not in the works'.
func TestJudgingAnEntityPairFromTheFormRecordsItInThatArena(t *testing.T) {
	ctx := context.Background()
	d, st := entityArenaDeps(t)

	rec := httptest.NewRecorder()
	d.route(rec, httptest.NewRequest(http.MethodGet, "/arena?kind=ao3_tag", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /arena?kind=ao3_tag = %d: %s", rec.Code, rec.Body.String())
	}
	var session string
	for _, c := range rec.Result().Cookies() {
		if c.Name == ArenaCookieName {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("the arena page set no session cookie")
	}
	if _, ok, err := st.UnjudgedInSessionKind(ctx, session, arena.KindTag); err != nil || !ok {
		t.Fatalf("the page presented no tag pair to judge (ok=%v err=%v)", ok, err)
	}

	form := url.Values{"session": {session}, "kind": {"ao3_tag"}, "choice": {"a"}}
	post := httptest.NewRequest(http.MethodPost, "/arena/judge", strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.AddCookie(&http.Cookie{Name: ArenaCookieName, Value: session})
	prec := httptest.NewRecorder()
	d.route(prec, post)
	if prec.Code != http.StatusSeeOther && prec.Code != http.StatusOK {
		t.Fatalf("POST /arena/judge = %d: %s", prec.Code, prec.Body.String())
	}

	comp, ok, err := st.LastJudgedInSessionKind(ctx, session, arena.KindTag)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || comp.Choice != "a" {
		t.Fatalf("the tag judgement was not recorded (ok=%v choice=%q)", ok, comp.Choice)
	}
	if _, ok, err := st.LastJudgedInSessionKind(ctx, session, arena.KindWork); err != nil || ok {
		t.Fatalf("the tag judgement also landed in the work arena (ok=%v err=%v)", ok, err)
	}
}
