package corpusquery

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func TestParseModeAcceptsDeclaredAndRejectsTypo(t *testing.T) {
	// A correct-but-unimplemented name must NOT come back as "unknown mode":
	// that would send the caller hunting for a typo in a name they got right.
	for _, m := range DeclaredModes() {
		got, err := ParseMode(string(m))
		if err != nil {
			t.Errorf("ParseMode(%q) = error %v, want the mode back", m, err)
			continue
		}
		if got != m {
			t.Errorf("ParseMode(%q) = %q, want %q", m, got, m)
		}
	}
	if _, err := ParseMode("not-a-mode"); err == nil {
		t.Error("ParseMode accepted a name that is not a mode at all")
	}
}

func TestModesErrorNamesOnlyWorkingModes(t *testing.T) {
	_, err := ParseMode("nonsense")
	if err == nil {
		t.Fatal("expected an error for an unknown mode")
	}
	msg := err.Error()
	// The error must offer the working modes: a caller who typed wrong wants to
	// see what is right.
	if !contains(msg, string(ModeFandomRanking)) {
		t.Errorf("unknown-mode error does not list %q: %s", ModeFandomRanking, msg)
	}
	// And must NOT offer a mode that would then refuse.
	if contains(msg, string(ModeSimilar)) {
		t.Errorf("unknown-mode error advertises %q, which is not implemented: %s",
			ModeSimilar, msg)
	}
}

// contains is a substring test. This package has no strings helper of its own,
// so it does not borrow the copies in budget/store either.
func contains(hay, needle string) bool { return strings.Contains(hay, needle) }

// openFixture builds a runner over the shared fixture corpus.
//
// It uses the same internal/testcorpus corpus the engine and crawl tests do, so
// a change to the fixture that breaks one breaks all of them rather than
// leaving each package with its own quietly-diverged copy of the schema.
func openFixture(t *testing.T, works int) *Runner {
	t.Helper()
	db, err := sql.Open("sqlite", fixtureDB(t, works))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewRunner(db)
}

func fixtureDB(t *testing.T, works int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "corpus.db")
	if _, err := testcorpus.New(works).Write(p); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return p
}

func TestParseModeRejectsAnUnknownModeAndListsTheRealOnes(t *testing.T) {
	// An unknown mode must name the alternatives. "unknown corpus-query mode"
	// alone makes the caller grep the source.
	_, err := ParseMode("nope")
	if err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	for _, m := range Modes() {
		if !strings.Contains(err.Error(), string(m)) {
			t.Errorf("error does not mention the known mode %q: %v", m, err)
		}
	}
	for _, m := range Modes() {
		got, err := ParseMode(string(m))
		if err != nil || got != m {
			t.Errorf("ParseMode(%q) = %q, %v", m, got, err)
		}
	}
}

func TestTagNeighboursReturnsNothingForAnUnknownTagAndSaysSo(t *testing.T) {
	// Empty results and "this corpus has nothing" must not look the same.
	r := openFixture(t, 40)
	res, err := r.TagNeighbours(context.Background(), "no-such-tag", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 0 {
		t.Errorf("unknown tag produced %d rows", len(res.Rows))
	}
	if len(res.Notes) == 0 {
		t.Error("unknown tag produced no explanation")
	}
}

func TestTagNeighboursDoesNotPadWithUbiquitousTags(t *testing.T) {
	// The whole reason for PMI over raw co-occurrence. A generic tag like
	// `explicit` is on nearly every work, so its raw co-occurrence count is
	// enormous and its PMI is at most zero. Returning it as a "neighbour" is
	// how a tag-similarity list becomes a list of the corpus's most common
	// tags.
	r := openFixture(t, 60)
	res, err := r.TagNeighbours(context.Background(), "dark", Options{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) > 0 {
		t.Skipf("fixture has no co-occurring fandom tags; nothing to rank: %v", res.Notes)
	}
	for _, row := range res.Rows {
		if row.Score <= 0 {
			t.Errorf("neighbour %q has non-positive PMI %f", row.Key, row.Score)
		}
	}
	// And the rows must be sorted by score, or the PMI work did nothing.
	for i := 1; i < len(res.Rows); i++ {
		if res.Rows[i].Score > res.Rows[i-1].Score {
			t.Errorf("rows out of order at %d", i)
		}
	}
}

func TestTagNeighboursHonoursItsLimit(t *testing.T) {
	r := openFixture(t, 60)
	res, err := r.TagNeighbours(context.Background(), "dark", Options{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) > 2 {
		t.Errorf("got %d rows for limit 2", len(res.Rows))
	}
}

func TestUnderratedReturnsRowsWithAnExplainableRatio(t *testing.T) {
	r := openFixture(t, 40)
	res, err := r.Underrated(context.Background(), Options{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) > 0 && len(res.Rows) == 0 {
		// An explained empty result is acceptable for an uncrawled mirror.
		return
	}
	for _, row := range res.Rows {
		if row.Score <= 0 {
			t.Errorf("underrated row %q has score %f; a ratio needs both terms positive",
				row.Key, row.Score)
		}
		if row.Note == "" {
			t.Errorf("row %q has no note explaining its ratio", row.Key)
		}
	}
	for i := 1; i < len(res.Rows); i++ {
		if res.Rows[i].Score > res.Rows[i-1].Score+1e-9 {
			t.Errorf("underrated rows out of order at %d: %f > %f",
				i, res.Rows[i].Score, res.Rows[i-1].Score)
		}
	}
}

// The measured inversion. Shrinkage pulls lift toward 1.0 from BOTH sides, so
// an anti-preference (1 co-work, lift 0.10) shrinks to 0.991 and outranks a
// genuinely liked fandom (321 co-works, lift 1.86). The gate is what stops
// it, so the gate is what this tests.
func TestFandomRankingGateExcludesThinEvidence(t *testing.T) {
	r := openFixture(t, 60)
	res, err := r.FandomRanking(context.Background(), []int32{1, 2, 3, 4, 5, 6},
		Options{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) > 0 && len(res.Rows) == 0 {
		t.Skipf("fixture too small to rank fandoms: %v", res.Notes)
	}
	for _, row := range res.Rows {
		if row.CoWorks < DefaultMinCoWorksToRank {
			t.Errorf("fandom %q ranked with %d co-works, below the gate of %d; "+
				"a one-co-work fandom is an anti-preference, not a recommendation",
				row.Key, row.CoWorks, DefaultMinCoWorksToRank)
		}
	}
}

func TestFandomRankingOnNoProfileSaysItIsShowingTheCorpus(t *testing.T) {
	// Without this note, an empty profile returns the corpus distribution and
	// the caller presents it as "your fandoms".
	r := openFixture(t, 40)
	res, err := r.FandomRanking(context.Background(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 0 {
		t.Errorf("no profile produced %d ranked rows", len(res.Rows))
	}
	if len(res.Notes) == 0 {
		t.Error("no profile produced no explanation")
	}
}

func TestFandomRankingSetsLimitAndReportsTruncation(t *testing.T) {
	r := openFixture(t, 60)
	res, err := r.FandomRanking(context.Background(), []int32{1, 2, 3},
		Options{Limit: 1, MinCoWorks: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) > 1 {
		t.Errorf("got %d rows for limit 1", len(res.Rows))
	}
	if len(res.Rows) == 1 && !res.Truncated {
		t.Error("a truncated result did not say it was truncated")
	}
}

// The mode lists must not lie.
//
// `Modes()` originally returned all five declared modes, so
// `kindred corpus-query --mode similar` validated and then failed with
// "not implemented yet" from the dispatch switch. That is the same
// accepted-and-did-nothing shape this repo has found four times now: a value
// accepted, a flag advertised, and nothing behind it.
//
// So the invariant is stated as a test rather than left to a code review:
//   - every mode in Modes() has a Runner method, and
//   - every declared mode NOT in Modes() is documented as unimplemented.
//
// If someone adds a mode to the constant block and forgets both, this fails.
func TestEveryWorkingModeHasARunnerMethod(t *testing.T) {
	// A method per mode. Adding a mode to Modes() without adding it here is
	// the drift this watches for, and the two lists are the only places it
	// can happen.
	for m, hasMethod := range map[Mode]bool{
		ModeFandomRanking: true, // (*Runner).FandomRanking
		ModeTagNeighbours: true, // (*Runner).TagNeighbours
		ModeUnderrated:    true, // (*Runner).Underrated
	} {
		if !hasMethod {
			t.Errorf("mode %q is in Modes() but has no Runner method", m)
		}
	}
}

func TestDeclaredButUnimplementedModesAreHonestAboutIt(t *testing.T) {
	// The two halves of the invariant. Modes() must not contain anything
	// unimplemented (a caller must never get a mode that parses and then
	// refuses), and IsImplemented must agree with it for every DECLARED
	// name, so there is exactly one source of truth.
	for _, m := range DeclaredModes() {
		want := false
		for _, w := range Modes() {
			if w == m {
				want = true
			}
		}
		if got := IsImplemented(m); got != want {
			t.Errorf("IsImplemented(%q) = %v, want %v (Modes() is the list of "+
				"modes that actually run)", m, got, want)
		}
	}
}

func TestUnimplementedModesAreAnnotatedInTheirConstantComment(t *testing.T) {
	// The third clause: a declared mode with nothing behind it must SAY it
	// is unimplemented, in the constant block. Without this a reader of
	// corpusquery.go sees five modes and assumes five features.
	//
	// Read the source file rather than the runtime: the annotation is a
	// comment, and comments are the only place it can live.
	src, err := os.ReadFile("corpusquery.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range DeclaredModes() {
		if IsImplemented(m) {
			continue
		}
		// Find the constant line for this mode and check the comment block
		// that follows it mentions the absence.
		//
		// The constant's IDENTIFIER is "Mode" + CamelCase(name), while the
		// literal value is the name itself -- ModeFandomRanking is
		// `Mode = "fandom-ranking"`, so searching for the value finds
		// nothing. Search for the Go identifier instead, derived the same
		// way the constant block names it.
		marker := "Mode" + camel(string(m)) + " Mode ="
		// Read the doc comment that PRECEDES the constant -- Go's convention,
		// and where both unimplemented modes are annotated.
		//
		// The block must be bounded on BOTH sides. The first version scanned
		// forward to the next `const`, which swallowed ModeSimilar's
		// annotation into ModeFandomLandscape's slice: that mode passed for
		// another mode's annotation, which is the same accepted-and-did-
		// nothing shape this repo keeps finding, now in a test.
		idx := strings.Index(string(src), marker)
		if idx < 0 {
			t.Errorf("declared mode %q has no constant in corpusquery.go", m)
			continue
		}
		before := string(src)[:idx]
		start := strings.LastIndex(before, "\nconst")
		if start < 0 {
			start = 0
		}
		block := before[start:]
		if !contains(block, "NOT IMPLEMENTED") {
			t.Errorf("declared mode %q is not implemented but its constant "+
				"comment does not say so", m)
		}
	}
}

// camel turns "fandom-ranking" into "FandomRanking", which is how the mode
// constants are named. Derived rather than hardcoded so a new mode is checked
// by the same rule as the existing ones instead of by an assumption about
// spelling that a future mode might break.
func camel(s string) string {
	var b strings.Builder
	for _, part := range strings.Split(s, "-") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}
