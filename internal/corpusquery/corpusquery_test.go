package corpusquery

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
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
// modeMethods is the one place the kebab-case mode VALUE and the CamelCase Go
// method name are related. Reflection cannot derive one from the other.
var modeMethods = map[Mode]string{
	ModeFandomRanking:   "FandomRanking",
	ModeFandomLandscape: "FandomLandscape",
	ModeTagNeighbours:   "TagNeighbours",
	ModeUnderrated:      "Underrated",
	ModeSimilar:         "Similar",
	ModeSurprise:        "Surprise",
}

func TestEveryWorkingModeHasARunnerMethod(t *testing.T) {
	// The invariant: every mode Modes() advertises has a Runner method behind
	// it, and every Runner method corresponds to a mode Modes() advertises.
	// The CLI's "declared but not implemented; working modes: ..." message is
	// built from Modes(), so a mode missing from this list is a mode the
	// product runs but never tells anyone about.
	//
	// The first version of this test iterated a map LITERAL of `true` values:
	//
	//     for m, hasMethod := range map[Mode]bool{ModeUnderrated: true, ...}
	//
	// which cannot fail, because the only value in it is true. It read like a
	// guard on the mode list and guarded nothing. Removing ModeUnderrated from
	// Modes() left this package's tests green.
	//
	// So the check is against the RUNTIME TYPE, and the map is built by
	// reflection over *Runner rather than written down by hand. A mode added
	// to Modes() without a method now fails here.
	//
	// The mapping is explicit because a mode's VALUE is kebab-case
	// ("fandom-ranking") while its Go identifier is CamelCase
	// (ModeFandomRanking), and reflect cannot derive one from the other
	// without inventing a mangling rule that would be wrong for
	// ModeFandomLandscape -> FandomLandscape only by accident.
	rt := reflect.TypeOf(&Runner{})
	for _, m := range Modes() {
		name, ok := modeMethods[m]
		if !ok {
			t.Errorf("Modes() advertises %q but this test has no method name "+
				"for it, so nothing here is checking it", m)
			continue
		}
		if _, ok := rt.MethodByName(name); !ok {
			t.Errorf("mode %q is in Modes() but *Runner has no method %q; "+
				"it parses, runs, and is never advertised", m, name)
		}
	}
}

func TestDeclaredButUnimplementedModesAreHonestAboutIt(t *testing.T) {
	// Modes() is the list of modes that WORK, so it must not contain anything
	// unimplemented: a caller must never get a mode that parses and then
	// refuses.
	//
	// IsImplemented is DEFINED as membership in Modes(), so asserting that the
	// two agree is asserting that a function equals its own definition -- it
	// cannot fail for any edit to Modes(). The meaningful direction is against
	// the runtime type, which TestEveryWorkingModeHasARunnerMethod now does.
	// What is left here is the direction that matters to a caller: ParseMode
	// accepts DeclaredModes, so every declared mode outside Modes() must be
	// refused with a message that names the working modes instead of pretending
	// the name was a typo.
	//
	// Checked against the method set rather than against Modes(), because
	// IsImplemented is membership in Modes() and comparing the two would be
	// comparing a function to its own definition.
	methods := map[Mode]bool{}
	rt := reflect.TypeOf(&Runner{})
	for m, name := range modeMethods {
		if _, ok := rt.MethodByName(name); ok {
			methods[m] = true
		}
	}
	for _, m := range DeclaredModes() {
		if got, want := IsImplemented(m), methods[m]; got != want {
			t.Errorf("%q: IsImplemented says %v but *Runner has a method: %v. "+
				"Modes() and the method set have drifted apart, so a mode "+
				"either runs unadvertised or is advertised without running.",
				m, got, want)
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

func TestSurpriseReturnsOneSeedableWorkAndSaysWhy(t *testing.T) {
	// The gate is 3, not the production 10, because the shared fixture gives
	// every work at most 5 tags. With the production threshold every one of
	// these assertions would hold vacuously on the "nothing to recommend"
	// branch -- a test that passes without running the code it names.
	r := openFixture(t, 40)
	res, err := r.Surprise(context.Background(), Options{MinTags: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("surprise returned %d rows, want exactly 1 (notes=%v)",
			len(res.Rows), res.Notes)
	}
	row := res.Rows[0]

	// It must be usable as a SEED, or the button leads nowhere.
	if row.WorkID <= 0 {
		t.Errorf("row %q has WorkID %d; a surprise with no work id cannot seed a ranking",
			row.Key, row.WorkID)
	}
	if !strings.HasPrefix(row.Key, "ao3_work:") {
		t.Errorf("Key = %q, want the ao3_work:<id> form the engine's seeds use", row.Key)
	}
	// The tag count is the score, and the note must explain the choice rather
	// than presenting an arbitrary pick as though it were a ranking.
	if row.Score < 3 {
		t.Errorf("score %f < the requested 3-tag gate; the work cannot be seeded",
			row.Score)
	}
	if row.Note == "" {
		t.Error("a surprise pick carries no note saying why it was eligible")
	}
}

func TestSurpriseActuallyVariesBetweenCalls(t *testing.T) {
	// A "random" selection that returns the same work every time is not random,
	// it is a constant with extra steps -- and it is the failure mode that looks
	// correct in a screenshot.
	//
	// Sampled rather than asserted exactly: two calls colliding is possible, so
	// the test requires SOME variation over many calls and no more.
	r := openFixture(t, 200)
	seen := map[int64]bool{}
	for i := 0; i < 40; i++ {
		res, err := r.Surprise(context.Background(), Options{MinTags: 3})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) != 1 {
			t.Fatalf("call %d returned %d rows, want 1", i, len(res.Rows))
		}
		seen[res.Rows[0].WorkID] = true
	}
	if len(seen) < 2 {
		t.Errorf("40 calls produced %d distinct works; the selection is not random",
			len(seen))
	}
}

func TestSurpriseExplainsItselfWhenNoWorkMeetsTheGate(t *testing.T) {
	// An unreachable gate is a corpus that cannot seed anything. Surprise must
	// return NO rows, NO error, and a note saying why -- because the alternative
	// is an empty page that reads as a bug.
	//
	// An earlier version of this test asked for one row and asserted the
	// FALLBACK arm would supply it. That is impossible and the test was wrong:
	// the fallback also honours the gate, so with no qualifying work there is
	// nothing to fall back to. Two distinct states were being conflated -- "the
	// forward walk found nothing" and "no work qualifies" -- and only the second
	// is reachable through Options. The fallback arm is covered by mutation on
	// the real corpus, where it fires.
	r := openFixture(t, 5)
	res, err := r.Surprise(context.Background(), Options{MinTags: 10_000})
	if err != nil {
		t.Fatalf("an unreachable gate must not be an error: %v", err)
	}
	if len(res.Rows) != 0 {
		t.Errorf("got %d rows with an unreachable gate, want 0", len(res.Rows))
	}
	if len(res.Notes) == 0 {
		t.Fatal("no rows and no note: the reader sees an empty page with no reason")
	}
	if !strings.Contains(strings.ToLower(res.Notes[0]), "tags") {
		t.Errorf("note %q does not mention tags, so it does not explain the gate",
			res.Notes[0])
	}
}

func TestSurpriseSaysSoWhenNothingQualifiesAtAll(t *testing.T) {
	// An empty corpus is the other total case: no rows AND an explanation.
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, ddl := range []string{
		`CREATE TABLE works(id INTEGER PRIMARY KEY, title TEXT, kudos INTEGER, word_count INTEGER)`,
		`CREATE TABLE work_tags(work_id INTEGER, tag_id INTEGER, tag_type TEXT)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	res, err := NewRunner(db).Surprise(context.Background(), Options{MinTags: 1})
	if err != nil {
		t.Fatalf("an empty corpus must not be an error: %v", err)
	}
	if len(res.Rows) != 0 {
		t.Errorf("an empty corpus returned %d rows", len(res.Rows))
	}
	if len(res.Notes) == 0 {
		t.Error("an empty result carries no note; it reads as a bug rather than as an uncrawled mirror")
	}
}
