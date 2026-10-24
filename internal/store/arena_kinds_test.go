// SPDX-License-Identifier: AGPL-3.0-only

// Tests for kind-aware arena state: the comparisons table's kind, the entity
// ratings and leaderboards, and the materialised candidate table.
//
// The properties pinned here are the ones a later change could quietly break:
// a pair seen in one kind must not suppress a pair in another, an entity id and
// a work id must never alias, and a kind that legitimately has no candidates
// must not be reloaded on every page view.

package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
)

func kindsStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// --------------------------------------------------------------- comparisons

func TestComparisonKindRoundTripsPerKind(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	if _, err := s.RecordPresentation(ctx, Comparison{
		Kind: arena.KindTag, WorkA: 11, WorkB: 22,
		SessionKey: "sess", OwnerKey: "owner", Strategy: "maxinfo",
	}); err != nil {
		t.Fatalf("record tag presentation: %v", err)
	}

	got, ok, err := s.UnjudgedInSessionKind(ctx, "sess", arena.KindTag)
	if err != nil || !ok {
		t.Fatalf("resume tag: ok=%v err=%v", ok, err)
	}
	if got.Kind != arena.KindTag || got.WorkA != 11 || got.WorkB != 22 {
		t.Fatalf("resumed %+v, want the tag pair", got)
	}

	// The same session key must not resume a WORK pair: the session is shared
	// between arenas, and a work arena handed a tag pair would present two
	// integers it cannot render.
	if _, ok, err := s.UnjudgedInSessionKind(ctx, "sess", arena.KindWork); err != nil || ok {
		t.Fatalf("a work resume found the tag pair: ok=%v err=%v", ok, err)
	}
	// Nor may the old work-only entry point see it.
	if _, ok, err := s.UnjudgedInSession(ctx, "sess"); err != nil || ok {
		t.Fatalf("the work-only resume found a tag pair: ok=%v err=%v", ok, err)
	}
}

func TestSeenPairsDoesNotCrossKinds(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	if _, err := s.RecordPresentation(ctx, Comparison{
		Kind: arena.KindTag, WorkA: 3, WorkB: 4,
		SessionKey: "s", OwnerKey: "owner", Strategy: "maxinfo",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	// DIFFERENT ids in the work arena, which is the point: with the same pair
	// in both kinds the two answers would agree whether or not the query
	// filters, and the test would pass on a query that ignores the kind.
	if _, err := s.RecordPresentation(ctx, Comparison{
		Kind: arena.KindWork, WorkA: 7, WorkB: 8,
		SessionKey: "s", OwnerKey: "owner", Strategy: "maxinfo",
	}); err != nil {
		t.Fatalf("record work: %v", err)
	}

	tags, err := s.SeenPairsKind(ctx, "owner", arena.KindTag, 100)
	if err != nil {
		t.Fatalf("seen tags: %v", err)
	}
	works, err := s.SeenPairsKind(ctx, "owner", arena.KindWork, 100)
	if err != nil {
		t.Fatalf("seen works: %v", err)
	}
	if len(tags) != 1 || len(works) != 1 {
		t.Fatalf("seen sets: tags=%d works=%d, want one each", len(tags), len(works))
	}
	if !tags[[2]int64{3, 4}] || !works[[2]int64{7, 8}] {
		t.Fatal("each kind must remember its own pair")
	}
	// And neither may leak into the other: the whole point of the kind.
	if tags[[2]int64{7, 8}] {
		t.Fatal("a work pair is suppressing tag candidates")
	}
	if works[[2]int64{3, 4}] {
		t.Fatal("a tag pair is suppressing work candidates")
	}
}

func TestRecordPresentationDefaultsAnUnsetKindToWork(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	// Every call site that predates kinds leaves Kind zero. Refusing would
	// break them; writing NULL would produce a row no query can classify.
	if _, err := s.RecordPresentation(ctx, Comparison{
		WorkA: 1, WorkB: 2, SessionKey: "s", OwnerKey: "o", Strategy: "maxinfo",
	}); err != nil {
		t.Fatalf("record without a kind: %v", err)
	}
	var kind string
	if err := s.DB.QueryRow(`SELECT kind FROM arena_comparisons`).Scan(&kind); err != nil {
		t.Fatalf("read kind: %v", err)
	}
	if kind != string(arena.KindWork) {
		t.Fatalf("stored kind %q, want %q", kind, arena.KindWork)
	}
	if _, ok, err := s.UnjudgedInSession(ctx, "s"); err != nil || !ok {
		t.Fatalf("the work-only resume missed a defaulted row: ok=%v err=%v", ok, err)
	}
}

func TestJudgedAfterCarriesTheKind(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)
	since := time.Now().UTC().Add(-time.Minute)

	if _, err := s.RecordPresentation(ctx, Comparison{
		Kind: arena.KindFandom, WorkA: 5, WorkB: 6,
		SessionKey: "s", OwnerKey: "o", Strategy: "maxinfo",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := s.TouchSession(ctx, "s", "o"); err != nil {
		t.Fatalf("touch session: %v", err)
	}
	// The kind-taking entry point, because the work-only one deliberately
	// does not touch another kind's row: judging a fandom pair through it
	// would silently do nothing at all.
	if err := s.RecordJudgementKind(ctx, "s", "a", arena.KindFandom); err != nil {
		t.Fatalf("judge: %v", err)
	}

	rows, err := s.JudgedAfter(ctx, since, 10)
	if err != nil {
		t.Fatalf("judged after: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d judged rows, want 1", len(rows))
	}
	if rows[0].Kind != arena.KindFandom {
		t.Fatalf("judged row kind %q, want %q — RunBatch routes on this", rows[0].Kind, arena.KindFandom)
	}
	if rows[0].Winner != 5 || rows[0].Loser != 6 {
		t.Fatalf("winner/loser = %d/%d, want 5/6", rows[0].Winner, rows[0].Loser)
	}
}

// -------------------------------------------------------------------- ratings

// The id spaces must not alias. A tag with id 7 and a work with id 7 are
// different entities, and if the two rating tables ever answered the same
// question the arena would rank a tag with a work's record.
func TestRatingForKindSeparatesTheIdSpaces(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	if err := s.SaveRating(ctx, RatingRow{
		WorkID: 7, Rating: arena.Rating{Mu: 1700, Phi: 80, Sigma: 0.06}, Comparisons: 12, Period: 3,
	}); err != nil {
		t.Fatalf("save work rating: %v", err)
	}
	if err := s.SaveRatingKind(ctx, arena.KindTag, RatingRow{
		WorkID: 7, Rating: arena.Rating{Mu: 1200, Phi: 120, Sigma: 0.06}, Comparisons: 4, Period: 2,
	}); err != nil {
		t.Fatalf("save tag rating: %v", err)
	}

	tags, err := s.RatingForKind(ctx, arena.KindTag, []int64{7})
	if err != nil {
		t.Fatalf("tag ratings: %v", err)
	}
	if got := tags[7].Rating.Mu; got != 1200 {
		t.Fatalf("tag rating mu = %v, want 1200 (not the work's 1700)", got)
	}
	works, err := s.RatingForKind(ctx, arena.KindWork, []int64{7})
	if err != nil {
		t.Fatalf("work ratings: %v", err)
	}
	if got := works[7].Rating.Mu; got != 1700 {
		t.Fatalf("work rating mu = %v, want 1700", got)
	}
}

func TestSaveRatingKindRefusesNonFiniteAndIsIdempotentPerPeriod(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	err := s.SaveRatingKind(ctx, arena.KindTag, RatingRow{
		WorkID: 9, Rating: arena.Rating{Mu: math.NaN(), Phi: 100, Sigma: 0.06},
	})
	if err == nil {
		t.Fatal("a NaN mu must be refused: SQLite stores it happily and every later comparison of that entity is poisoned")
	}

	for i := 0; i < 2; i++ {
		if err := s.SaveRatingKind(ctx, arena.KindAuthor, RatingRow{
			WorkID: 42, Rating: arena.Rating{Mu: 1500, Phi: 200, Sigma: 0.06}, Comparisons: 1, Period: 5,
		}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	hist, err := s.RatingHistoryKind(ctx, arena.KindAuthor, 42, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history has %d rows for one period, want 1 (a re-run must overwrite)", len(hist))
	}
}

func TestRatingHistoryKindIsOldestFirst(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	for _, period := range []int64{9, 3, 7} {
		if err := s.SaveRatingKind(ctx, arena.KindFandom, RatingRow{
			WorkID: 1, Rating: arena.Rating{Mu: 1400 + float64(period), Phi: 100, Sigma: 0.06},
			Comparisons: 1, Period: period,
		}); err != nil {
			t.Fatalf("save period %d: %v", period, err)
		}
	}
	hist, err := s.RatingHistoryKind(ctx, arena.KindFandom, 1, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("history has %d rows, want 3", len(hist))
	}
	if hist[0].Period != 3 || hist[2].Period != 9 {
		t.Fatalf("history periods %v, want 3,7,9 — a sparkline reads left to right", []int64{hist[0].Period, hist[1].Period, hist[2].Period})
	}
}

// ---------------------------------------------------------------- leaderboards

func TestLeaderboardForKindRanksOnlyItsKindAndNamesEntries(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	if _, err := s.ReplaceEntityCandidates(ctx, arena.KindTag, []arena.EntityCandidate{
		{ID: 100, Name: "time travel", Weight: 40},
		{ID: 101, Name: "slow burn", Weight: 30},
	}); err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if err := s.SaveRatingKind(ctx, arena.KindTag, RatingRow{
		WorkID: 100, Rating: arena.Rating{Mu: 1900, Phi: 60, Sigma: 0.06}, Comparisons: 20, Wins: 15, Period: 4,
	}); err != nil {
		t.Fatalf("high rating: %v", err)
	}
	if err := s.SaveRatingKind(ctx, arena.KindTag, RatingRow{
		WorkID: 101, Rating: arena.Rating{Mu: 1600, Phi: 60, Sigma: 0.06}, Comparisons: 20, Wins: 5, Period: 4,
	}); err != nil {
		t.Fatalf("low rating: %v", err)
	}
	// A work with the same id and a HIGHER rating, which must not appear.
	if err := s.SaveRating(ctx, RatingRow{
		WorkID: 100, Rating: arena.Rating{Mu: 2100, Phi: 50, Sigma: 0.06}, Comparisons: 30, Period: 4,
	}); err != nil {
		t.Fatalf("work rating: %v", err)
	}

	entries, err := s.LeaderboardForKind(ctx, arena.KindTag, 10, 0)
	if err != nil {
		t.Fatalf("leaderboard: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want the 2 tags", len(entries))
	}
	if entries[0].WorkID != 100 || entries[0].Title != "time travel" {
		t.Fatalf("top entry %+v, want tag 100 named 'time travel'", entries[0])
	}
	if entries[0].Kind != arena.KindTag {
		t.Fatalf("entry kind %q, want %q", entries[0].Kind, arena.KindTag)
	}
	if entries[0].Effective == 0 {
		t.Fatal("Effective is unset: the page sorts and displays this, not the raw mu")
	}

	// The works leaderboard is unchanged: one entry, the work's own rating.
	works, err := s.LeaderboardForKind(ctx, arena.KindWork, 10, 0)
	if err != nil {
		t.Fatalf("work leaderboard: %v", err)
	}
	if len(works) != 1 || works[0].WorkID != 100 {
		t.Fatalf("work leaderboard %+v, want the single work", works)
	}
}

// The anchor that damps a cold-start rating must come from the kind being
// ranked. Sharing the works median would pull every tag toward 1500 on a scale
// whose own centre is somewhere else, and the ordering would silently disagree
// with the raw mus.
func TestLeaderboardForKindDampsAgainstItsOwnMedian(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	// Two tag rows at 100 and 200: their median is 150.
	for i, mu := range []float64{100, 200} {
		if err := s.SaveRatingKind(ctx, arena.KindTag, RatingRow{
			WorkID: int64(200 + i), Rating: arena.Rating{Mu: mu, Phi: 30, Sigma: 0.06},
			Comparisons: 10, Period: 1,
		}); err != nil {
			t.Fatalf("tag rating: %v", err)
		}
	}
	// A work far away from that, which would move the anchor if it leaked in.
	if err := s.SaveRating(ctx, RatingRow{
		WorkID: 1, Rating: arena.Rating{Mu: 1500, Phi: 30, Sigma: 0.06}, Comparisons: 10, Period: 1,
	}); err != nil {
		t.Fatalf("work rating: %v", err)
	}

	entries, err := s.LeaderboardForKind(ctx, arena.KindTag, 10, 0)
	if err != nil {
		t.Fatalf("leaderboard: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	for _, e := range entries {
		want := EffectiveRating(e.Rating.Mu, e.Rating.Phi, 150)
		if e.Effective != want {
			t.Fatalf("effective %v for mu %v, want %v (anchored on the tags' median, not the works')",
				e.Effective, e.Rating.Mu, want)
		}
	}
}

func TestMedianOfRated(t *testing.T) {
	muAt := func(vals ...float64) func(int) (float64, bool) {
		return func(offset int) (float64, bool) {
			if offset < 0 || offset >= len(vals) {
				return 0, false
			}
			return vals[offset], true
		}
	}
	if got := medianOfRated(0, muAt()); got != arena.MU_INIT {
		t.Fatalf("empty median = %v, want MU_INIT", got)
	}
	if got := medianOfRated(3, muAt(10, 20, 30)); got != 20 {
		t.Fatalf("odd median = %v, want 20", got)
	}
	if got := medianOfRated(4, muAt(10, 20, 30, 40)); got != 25 {
		t.Fatalf("even median = %v, want 25 (the mean of the middle two)", got)
	}
	if got := medianOfRated(2, muAt(10)); got != arena.MU_INIT {
		t.Fatalf("unreadable median = %v, want MU_INIT rather than a half-computed value", got)
	}
}

// ----------------------------------------------------------------- candidates

func TestReplaceEntityCandidatesSwapsAndMarks(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	if n, err := s.ReplaceEntityCandidates(ctx, arena.KindTag, []arena.EntityCandidate{
		{ID: 1, Name: "a", Weight: 5}, {ID: 2, Name: "b", Weight: 4}, {ID: 3, Name: "c", Weight: 3},
	}); err != nil || n != 3 {
		t.Fatalf("replace = %d, err=%v; want 3", n, err)
	}
	if n, err := s.ReplaceEntityCandidates(ctx, arena.KindTag, []arena.EntityCandidate{
		{ID: 1, Name: "a", Weight: 5},
	}); err != nil || n != 1 {
		t.Fatalf("second replace = %d, err=%v; want 1", n, err)
	}
	// Replace, not merge: a candidate that no longer clears the floor must
	// leave, or the arena keeps pairing things the crawl no longer supports.
	got, err := s.EntityCandidates(ctx, arena.KindTag, 50)
	if err != nil {
		t.Fatalf("read candidates: %v", err)
	}
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("candidates %+v, want only id 1", got)
	}

	offered, err := s.KindsOffered(ctx)
	if err != nil {
		t.Fatalf("kinds offered: %v", err)
	}
	if len(offered) != 1 || offered[0].Kind != arena.KindTag || offered[0].Candidates != 1 {
		t.Fatalf("offered %+v, want one loaded tag kind with 1 candidate", offered)
	}
	if offered[0].Refreshed.IsZero() {
		t.Fatal("Refreshed is zero: a switcher cannot say when the set was measured")
	}
}

// A kind the mirror has almost none of is a normal outcome, not a failure, and
// it must be recorded as loaded so it is not re-queried forever.
func TestReplaceEntityCandidatesRecordsAnEmptyKind(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	if n, err := s.ReplaceEntityCandidates(ctx, arena.KindCharacter, nil); err != nil || n != 0 {
		t.Fatalf("replace empty = %d, err=%v", n, err)
	}
	offered, err := s.KindsOffered(ctx)
	if err != nil {
		t.Fatalf("kinds offered: %v", err)
	}
	if len(offered) != 1 || offered[0].Candidates != 0 {
		t.Fatalf("offered %+v, want the empty kind recorded", offered)
	}
}

func TestEnsureEntityCandidatesLoadsOnlyWhenStale(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	calls := 0
	src := fakeSource{load: func(_ context.Context, kind arena.EntityKind) ([]arena.EntityCandidate, error) {
		calls++
		if kind != arena.KindTag {
			t.Errorf("loader asked for %q", kind)
		}
		return []arena.EntityCandidate{{ID: 1, Name: "a", Weight: 9}}, nil
	}}

	loaded, err := s.EnsureEntityCandidates(ctx, arena.KindTag, src, 100, time.Hour)
	if err != nil || !loaded {
		t.Fatalf("first ensure: loaded=%v err=%v", loaded, err)
	}
	loaded, err = s.EnsureEntityCandidates(ctx, arena.KindTag, src, 100, time.Hour)
	if err != nil || loaded {
		t.Fatalf("second ensure: loaded=%v err=%v; want a no-op within the TTL", loaded, err)
	}
	if calls != 1 {
		t.Fatalf("loader ran %d times, want 1", calls)
	}
	if _, err := s.EnsureEntityCandidates(ctx, arena.KindTag, src, 100, 0); err != nil {
		t.Fatalf("ttl=0 ensure: %v", err)
	}
	if calls != 2 {
		t.Fatalf("loader ran %d times, want 2 (ttl=0 always reloads)", calls)
	}
}

// The 2.2s-per-page bug this exists to prevent: a kind that comes back EMPTY
// must still be marked as loaded, or its slow query runs on every view.
func TestEnsureEntityCandidatesDoesNotReloadAnEmptyKind(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	calls := 0
	src := fakeSource{load: func(context.Context, arena.EntityKind) ([]arena.EntityCandidate, error) {
		calls++
		return nil, nil
	}}

	if _, err := s.EnsureEntityCandidates(ctx, arena.KindCharacter, src, 100, time.Hour); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if _, err := s.EnsureEntityCandidates(ctx, arena.KindCharacter, src, 100, time.Hour); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if calls != 1 {
		t.Fatalf("the loader ran %d times for an empty kind, want 1", calls)
	}
}

func TestEnsureEntityCandidatesRetriesAfterAFailedLoad(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	calls := 0
	fail := true
	src := fakeSource{load: func(context.Context, arena.EntityKind) ([]arena.EntityCandidate, error) {
		calls++
		if fail {
			return nil, errors.New("corpus is a directory")
		}
		return nil, nil
	}}

	if _, err := s.EnsureEntityCandidates(ctx, arena.KindAuthor, src, 100, time.Hour); err == nil {
		t.Fatal("a loader failure must surface")
	}
	// No marker was written, so the next attempt must try again rather than
	// serve a kind that was never loaded.
	fail = false
	if _, err := s.EnsureEntityCandidates(ctx, arena.KindAuthor, src, 100, time.Hour); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if calls != 2 {
		t.Fatalf("loader ran %d times, want 2", calls)
	}
}

func TestEntityNameReportsUnknownRatherThanEmpty(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	if _, err := s.ReplaceEntityCandidates(ctx, arena.KindTag, []arena.EntityCandidate{
		{ID: 5, Name: "", Weight: 1},
		{ID: 6, Name: "named", Weight: 1},
	}); err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if name, ok, err := s.EntityName(ctx, arena.KindTag, 6); err != nil || !ok || name != "named" {
		t.Fatalf("known name = %q ok=%v err=%v", name, ok, err)
	}
	if _, ok, err := s.EntityName(ctx, arena.KindTag, 999); err != nil || ok {
		t.Fatalf("unknown id: ok=%v err=%v, want ok=false and no error", ok, err)
	}
}

// ------------------------------------------------------------------ migration

// The load-bearing migration test: a database written before `kind` existed must
// come back with its rows intact and classified as works.
func TestMigrationAddsKindToAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// The pre-kind shape, agreement included (the previous migration's work).
	if _, err := raw.Exec(`
		CREATE TABLE arena_comparisons (
		  id           INTEGER PRIMARY KEY,
		  work_a       INTEGER NOT NULL,
		  work_b       INTEGER NOT NULL,
		  choice       TEXT,
		  session_key  TEXT NOT NULL,
		  owner_key    TEXT NOT NULL,
		  strategy     TEXT NOT NULL,
		  presented_at TEXT NOT NULL DEFAULT (datetime('now')),
		  judged_at    TEXT,
		  agreement    REAL NOT NULL DEFAULT 1.0
		);
		INSERT INTO arena_comparisons
		  (work_a, work_b, choice, session_key, owner_key, strategy, agreement, judged_at)
		  VALUES (7, 8, 'a', 'old-session', 'old-owner', 'maxinfo', 0.75, datetime('now'));`); err != nil {
		t.Fatalf("old schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s, err := Open(ctx, path, "")
	if err != nil {
		t.Fatalf("open and migrate: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var kind, strategy string
	var agreement float64
	var judged sql.NullString
	if err := s.DB.QueryRowContext(ctx,
		`SELECT kind, strategy, agreement, judged_at FROM arena_comparisons WHERE session_key = 'old-session'`).
		Scan(&kind, &strategy, &agreement, &judged); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	// judged_at survives the rebuild. A migration that dropped it would make
	// every judged row resumable again, which is a pair shown twice.
	if !judged.Valid {
		t.Fatal("judged_at was lost in the migration")
	}
	if kind != string(arena.KindWork) {
		t.Fatalf("migrated kind %q, want %q", kind, arena.KindWork)
	}
	if strategy != "maxinfo" || agreement != 0.75 {
		t.Fatalf("row mangled: strategy=%q agreement=%v", strategy, agreement)
	}
	// And the migrated database must actually work: the resume query names the
	// kind column, so a migration that forgot it fails here and not in
	// production.
	if _, ok, err := s.UnjudgedInSessionKind(ctx, "old-session", arena.KindWork); err != nil {
		t.Fatalf("resume after migration: %v", err)
	} else if ok {
		t.Fatal("the migrated row was judged, so it must not resume")
	}
}

// -------------------------------------------------------------------- helpers

// fakeSource stands in for the corpus loaders, which need a corpus database and
// 2.2 seconds. What is under test here is the caching decision, not the query.
//
// The counter lives in the closure so every test owns one: the interface method
// is what Ensure calls, and counting the callback instead made a failing load
// look like no call at all.
type fakeSource struct {
	load func(ctx context.Context, kind arena.EntityKind) ([]arena.EntityCandidate, error)
}

func (f fakeSource) EntityCandidates(ctx context.Context, kind arena.EntityKind, _ int) ([]arena.EntityCandidate, error) {
	return f.load(ctx, kind)
}

// Judging a tag pair while a NEWER work pair sits in the same session must
// resolve the choice to the tag pair. Both are integers, so getting this wrong
// moves the wrong entities and shows up only as a rating that drifts.
func TestLastJudgedInSessionKindPicksItsOwnKind(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)
	if _, err := s.TouchSession(ctx, "shared", "o"); err != nil {
		t.Fatalf("touch: %v", err)
	}

	// The tag pair is judged first...
	if _, err := s.RecordPresentation(ctx, Comparison{
		Kind: arena.KindTag, WorkA: 11, WorkB: 12, SessionKey: "shared", OwnerKey: "o", Strategy: "maxinfo",
	}); err != nil {
		t.Fatalf("record tag: %v", err)
	}
	if err := s.RecordJudgementKind(ctx, "shared", "b", arena.KindTag); err != nil {
		t.Fatalf("judge tag: %v", err)
	}
	// ...then a work pair, which is the newest row overall.
	if _, err := s.RecordPresentation(ctx, Comparison{
		Kind: arena.KindWork, WorkA: 50, WorkB: 51, SessionKey: "shared", OwnerKey: "o", Strategy: "maxinfo",
	}); err != nil {
		t.Fatalf("record work: %v", err)
	}
	if err := s.RecordJudgementKind(ctx, "shared", "a", arena.KindWork); err != nil {
		t.Fatalf("judge work: %v", err)
	}

	tag, ok, err := s.LastJudgedInSessionKind(ctx, "shared", arena.KindTag)
	if err != nil || !ok {
		t.Fatalf("tag lookup: ok=%v err=%v", ok, err)
	}
	if tag.WorkA != 11 || tag.WorkB != 12 || tag.Choice != "b" {
		t.Fatalf("tag row %+v, want the (11,12) pair judged b", tag)
	}
	if tag.Kind != arena.KindTag {
		t.Fatalf("kind %q, want %q", tag.Kind, arena.KindTag)
	}
	work, ok, err := s.LastJudgedInSessionKind(ctx, "shared", arena.KindWork)
	if err != nil || !ok {
		t.Fatalf("work lookup: ok=%v err=%v", ok, err)
	}
	if work.WorkA != 50 || work.WorkB != 51 {
		t.Fatalf("work row %+v, want the (50,51) pair", work)
	}
	// A kind this session has never judged answers "nothing", not the other
	// kind's row.
	if _, ok, err := s.LastJudgedInSessionKind(ctx, "shared", arena.KindAuthor); err != nil || ok {
		t.Fatalf("author lookup: ok=%v err=%v, want none", ok, err)
	}
}

// One choice must judge ONE comparison. Both arenas share a session, so a WHERE
// clause that pins only the session judges every pending row at once: the tag
// pair would be recorded as chosen with an answer the user never gave, and its
// agreement would be an agreement for a comparison that never happened.
func TestJudgingOneKindLeavesTheOtherPending(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)
	if _, err := s.TouchSession(ctx, "two", "o"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	for _, c := range []Comparison{
		{Kind: arena.KindTag, WorkA: 21, WorkB: 22, SessionKey: "two", OwnerKey: "o", Strategy: "maxinfo"},
		{Kind: arena.KindWork, WorkA: 60, WorkB: 61, SessionKey: "two", OwnerKey: "o", Strategy: "maxinfo"},
	} {
		if _, err := s.RecordPresentation(ctx, c); err != nil {
			t.Fatalf("record %s: %v", c.Kind, err)
		}
	}

	if err := s.RecordJudgementKind(ctx, "two", "a", arena.KindWork); err != nil {
		t.Fatalf("judge work: %v", err)
	}

	// The tag pair is untouched...
	if c, ok, err := s.UnjudgedInSessionKind(ctx, "two", arena.KindTag); err != nil || !ok {
		t.Fatalf("the tag pair was judged by a work choice: ok=%v err=%v", ok, err)
	} else if c.WorkA != 21 {
		t.Fatalf("pending tag pair is %+v, want (21,22)", c)
	}
	// ...and the work pair is the one that got the answer.
	w, ok, err := s.LastJudgedInSessionKind(ctx, "two", arena.KindWork)
	if err != nil || !ok {
		t.Fatalf("work judgement: ok=%v err=%v", ok, err)
	}
	if w.Choice != "a" {
		t.Fatalf("work choice %q, want a", w.Choice)
	}
	// The entity-comparison row it wrote carries the kind it belongs to, and
	// the winner/loser resolve from the side that was chosen.
	var kind string
	var winner, loser int64
	if err := s.DB.QueryRow(`SELECT kind, winner_id, loser_id FROM arena_entity_comparisons
		ORDER BY rowid DESC LIMIT 1`).Scan(&kind, &winner, &loser); err != nil {
		t.Fatalf("entity comparison: %v", err)
	}
	if kind != string(arena.KindWork) || winner != 60 || loser != 61 {
		t.Fatalf("entity comparison (kind=%s winner=%d loser=%d), want work/60/61", kind, winner, loser)
	}

	// Now the tag pair, judged in its own kind: the work row must not be
	// re-judged (its choice and timestamp stay as they were).
	before, _, _ := s.LastJudgedInSessionKind(ctx, "two", arena.KindWork)
	if err := s.RecordJudgementKind(ctx, "two", "b", arena.KindTag); err != nil {
		t.Fatalf("judge tag: %v", err)
	}
	tag, ok, err := s.LastJudgedInSessionKind(ctx, "two", arena.KindTag)
	if err != nil || !ok || tag.Choice != "b" {
		t.Fatalf("tag judgement: ok=%v choice=%q err=%v", ok, tag.Choice, err)
	}
	// The second judgement's OWN entity-comparison row, which is where two
	// things are visible that the work case cannot show: the read-back must
	// resolve THIS kind's pair (the work row has the higher id and would win
	// an unfiltered "newest judged" scan), and the insert must carry THIS
	// kind rather than defaulting to the one it started with.
	var kind2 string
	var winner2, loser2 int64
	if err := s.DB.QueryRow(`SELECT kind, winner_id, loser_id FROM arena_entity_comparisons
		ORDER BY rowid DESC LIMIT 1`).Scan(&kind2, &winner2, &loser2); err != nil {
		t.Fatalf("second entity comparison: %v", err)
	}
	if kind2 != string(arena.KindTag) {
		t.Fatalf("second entity comparison kind %q, want %q", kind2, arena.KindTag)
	}
	// The choice is a SIDE ('a'/'b'), so 'b' makes the second id the winner:
	// 22 beats 21 here. Reading it as "the first id wins" is the mistake the
	// store's own comment warns about, and it is mine to make in the assertion
	// rather than to hide by adjusting the code.
	if winner2 != 22 || loser2 != 21 {
		t.Fatalf("second entity comparison (winner=%d loser=%d), want 22/21 — the tag pair, not the work pair",
			winner2, loser2)
	}
	after, _, _ := s.LastJudgedInSessionKind(ctx, "two", arena.KindWork)
	if after.Choice != before.Choice || after.ID != before.ID {
		t.Fatalf("judging the tag pair changed the work pair: %+v -> %+v", before, after)
	}
}

// BTDuels reads the whole judged history for ONE kind, weights each duel by the
// comparison's agreement, and drops rows the Glicko batch would also drop (#53).
//
// The kind filter is the one that matters: the arena's id spaces alias, so a fandom
// with id 7 and a work with id 7 are different entities, and a BT fit that read both
// would rank one against the other.
func TestBTDuelsReadsOneKindWithAgreementWeights(t *testing.T) {
	ctx := context.Background()
	s := kindsStore(t)

	// A work comparison judged 'a' (work_a wins), at full agreement.
	if _, err := s.RecordPresentation(ctx, Comparison{
		Kind: arena.KindWork, WorkA: 11, WorkB: 12,
		SessionKey: "w1", OwnerKey: "o", Strategy: "maxinfo",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TouchSession(ctx, "w1", "o"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordJudgementKind(ctx, "w1", "a", arena.KindWork); err != nil {
		t.Fatal(err)
	}
	// A fandom comparison in the same table: it must not reach the work fit.
	if _, err := s.RecordPresentation(ctx, Comparison{
		Kind: arena.KindFandom, WorkA: 11, WorkB: 12,
		SessionKey: "f1", OwnerKey: "o", Strategy: "maxinfo",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TouchSession(ctx, "f1", "o"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordJudgementKind(ctx, "f1", "b", arena.KindFandom); err != nil {
		t.Fatal(err)
	}

	duels, n, err := s.BTDuels(ctx, arena.KindWork)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("read %d comparisons, want only the work one", n)
	}
	if len(duels) != 1 {
		t.Fatalf("produced %d duels, want 1", len(duels))
	}
	d := duels[0]
	if d.Winner != 11 || d.Loser != 12 {
		t.Fatalf("duel = %d over %d, want 11 over 12 (choice 'a')", d.Winner, d.Loser)
	}
	if d.Weight != 1 {
		t.Fatalf("duel weight %v, want the comparison's agreement 1", d.Weight)
	}
}
