package web

// A judgement belongs to the arena that presented it. These tests exist because
// the web judge used to resolve the comparison it had just recorded with
// LastJudgedInSession, which is WORK-ONLY: a tag judgement would have resolved
// to the last WORK pair in the session and learned tag weights from a choice the
// reader never made, silently, behind a row that looked legitimate.

import (
	"context"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
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
