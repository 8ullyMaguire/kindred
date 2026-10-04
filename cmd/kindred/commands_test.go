package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/config"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// corpusFixture writes a fixture corpus and returns its path.
//
// These tests exercise the real command functions against the real store, not
// a stubbed corpus: a subcommand that parses its flags correctly and then
// opens the wrong database passes a mock-based test and fails in a script.
func corpusFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "corpus.db")
	if _, err := testcorpus.New(40).Write(p); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return p
}

// ---------------------------------------------------------------- tune

// A tune that does not sum to 1 is a different scale, not a different
// ranking-preference, and storing it means two rows mean the same thing.
func TestTuneSetRefusesWeightsThatDoNotSumToOne(t *testing.T) {
	dir := t.TempDir()
	err := runTune(context.Background(), []string{
		"--corpus", corpusFixture(t),
		"--db", filepath.Join(dir, "state.db"),
		"--set", "tag_overlap=0.5,neighbourhood=0.9",
	})
	if err == nil {
		t.Fatal("tune summing to 1.4 was accepted")
	}
	if !strings.Contains(err.Error(), "sum to") {
		t.Errorf("error does not explain the sum rule: %v", err)
	}
}

// A typo'd signal name must be refused. Ignoring it would leave the reader
// believing they re-weighted neighbourhood when nothing changed -- the same
// silent-no-op class as the diversity cap that motivated this command.
func TestTuneSetRefusesAnUnknownSignal(t *testing.T) {
	err := runTune(context.Background(), []string{
		"--corpus", corpusFixture(t),
		"--db", filepath.Join(t.TempDir(), "state.db"),
		"--set", "tagoverlap=1.0",
	})
	if err == nil {
		t.Fatal("a typo'd signal name was accepted")
	}
	if !strings.Contains(err.Error(), "unknown signal") {
		t.Errorf("error does not say the signal is unknown: %v", err)
	}
	// And it must list what is available.
	for _, s := range []string{"tag_overlap", "neighbourhood"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error does not list the real signal %q: %v", s, err)
		}
	}
}

func TestTuneSetRejectsANegativeWeight(t *testing.T) {
	err := runTune(context.Background(), []string{
		"--corpus", corpusFixture(t),
		"--db", filepath.Join(t.TempDir(), "state.db"),
		"--set", "tag_overlap=-0.5,neighbourhood=1.5",
	})
	if err == nil {
		t.Fatal("a negative weight was accepted")
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("error does not explain why: %v", err)
	}
}

func TestTuneSetAcceptsTheShippedDefaults(t *testing.T) {
	// The defaults must survive the same validation as anything a user types.
	// 0.22+0.30+0.09+0.09+0.09+0.10+0.11 is not exactly 1 in binary floating
	// point, and an == 0 check would reject them.
	err := runTune(context.Background(), []string{
		"--corpus", corpusFixture(t),
		"--db", filepath.Join(t.TempDir(), "state.db"),
		"--set", "tag_overlap=0.22,neighbourhood=0.30,quality=0.09," +
			"recency=0.09,popularity=0.09,embedding=0.10,peer_rating=0.11",
	})
	if err != nil {
		t.Errorf("the shipped default weights were rejected: %v", err)
	}
}

func TestTuneRoundTripsThroughTheStore(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	corpus := corpusFixture(t)

	set := []string{"--corpus", corpus, "--db", dbPath,
		"--set", "tag_overlap=0.50,neighbourhood=0.50"}
	if err := runTune(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	// A second process reading the same DB must see the stored tune.
	db, err := openStateDB(&config.Config{DB: dbPath, CorpusDB: corpus})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tn, stored, err := loadTune(context.Background(), db, "default")
	if err != nil {
		t.Fatal(err)
	}
	if !stored {
		t.Error("a tune that was just written does not read back as stored")
	}
	if tn.Weights["tag_overlap"] != 0.50 {
		t.Errorf("tag_overlap = %v, want 0.50", tn.Weights["tag_overlap"])
	}
}

// A typo'd tune NAME must not silently resolve to the built-in default. That
// would hand the caller weights they never asked for and never wrote.
func TestTuneShowRejectsAnUnknownName(t *testing.T) {
	err := runTune(context.Background(), []string{
		"--corpus", corpusFixture(t),
		"--db", filepath.Join(t.TempDir(), "state.db"),
		"--name", "defualt", // typo
	})
	if err == nil {
		t.Fatal("a typo'd tune name resolved instead of erroring")
	}
	if !strings.Contains(err.Error(), "no tune named") {
		t.Errorf("error does not say the name is unknown: %v", err)
	}
}

func TestParseTuneSpecErrors(t *testing.T) {
	for _, spec := range []string{"", "  ", "tag_overlap", "tag_overlap=x"} {
		if _, err := parseTuneSpec(spec); err == nil {
			t.Errorf("parseTuneSpec(%q) succeeded, want an error", spec)
		}
	}
	w, err := parseTuneSpec("a=0.25, b=0.75")
	if err != nil {
		t.Fatal(err)
	}
	if w["a"] != 0.25 || w["b"] != 0.75 {
		t.Errorf("got %v", w)
	}
}

// ---------------------------------------------------------------- profile

func TestProfileBuildAndShowRoundTrip(t *testing.T) {
	corpus := corpusFixture(t)
	// The SAME state db across invocations: a profile is durable state, so
	// showing it in a second call is exactly the property worth testing. A
	// fresh t.TempDir() per call would give a different database and the test
	// would pass on an error.
	stateDB := filepath.Join(t.TempDir(), "state.db")
	// Flags go AFTER the subverb: runProfile dispatches on args[0], so a
	// leading --corpus is read as the subcommand name.
	build := []string{"build",
		"--corpus", corpus, "--db", stateDB,
		"--name", "reader", "--works", "1,2,3,4,5"}
	if err := runProfile(context.Background(), "", build); err != nil {
		t.Fatalf("profile build: %v", err)
	}
	// show and list need the same --corpus and --db as the build, because
	// they are separate invocations against separate processes' worth of
	// state: a test that passed the path only to build would be testing a
	// command line nobody would type.
	if err := runProfile(context.Background(), corpus,
		[]string{"show", "--corpus", corpus, "--db", stateDB, "reader"}); err != nil {
		t.Errorf("profile show after build: %v", err)
	}
	if err := runProfile(context.Background(), corpus,
		[]string{"list", "--corpus", corpus, "--db", stateDB}); err != nil {
		t.Errorf("profile list: %v", err)
	}
	// And rating into it must persist only with --save.
	if err := runProfile(context.Background(), corpus,
		[]string{"rate", "--corpus", corpus, "--db", stateDB,
			"--name", "reader", "--work", "7", "--like"}); err != nil {
		t.Errorf("profile rate: %v", err)
	}
}

// A --corpus passed to the subcommand must win over the environment default.
// Otherwise a script that passes the flag silently reads a different database
// than the one it named.
func TestProfileSubcommandHonoursItsOwnCorpusFlag(t *testing.T) {
	corpus := corpusFixture(t)
	// The env-derived default is deliberately wrong here: if the flag is
	// ignored, this opens a database that does not exist.
	err := runProfile(context.Background(), filepath.Join(t.TempDir(), "absent.db"),
		[]string{"list", "--corpus", corpus})
	if err != nil {
		t.Errorf("the --corpus flag was not honoured: %v", err)
	}
}

func TestProfileBuildRequiresItsFlags(t *testing.T) {
	corpus := corpusFixture(t)
	for _, args := range [][]string{
		{"build", "--works", "1"},
		{"build", "--name", "x"},
	} {
		if err := runProfile(context.Background(), corpus, args); err == nil {
			t.Errorf("%v succeeded, want a missing-flag error", args)
		}
	}
}

func TestProfileShowUnknownNameIsAnError(t *testing.T) {
	if err := runProfile(context.Background(), corpusFixture(t),
		[]string{"show", "nobody"}); err == nil {
		t.Error("showing an unknown profile succeeded")
	}
}

// ---------------------------------------------------------------- corpus-query

func TestCorpusQueryRejectsAnUnknownModeAndNamesTheRealOnes(t *testing.T) {
	corpus := corpusFixture(t)
	err := runCorpusQuery(context.Background(), corpus,
		[]string{"--corpus", corpus, "--db", filepath.Join(t.TempDir(), "s.db"),
			"--query", "nope"})
	if err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if !strings.Contains(err.Error(), "fandom-ranking") {
		t.Errorf("error does not list the real modes: %v", err)
	}
}

func TestCorpusQueryUnderratedRuns(t *testing.T) {
	corpus := corpusFixture(t)
	if err := runCorpusQuery(context.Background(), corpus,
		[]string{"--corpus", corpus, "--db", filepath.Join(t.TempDir(), "s.db"),
			"--query", "underrated", "--limit", "5"}); err != nil {
		t.Errorf("underrated: %v", err)
	}
}

func TestCorpusQueryTagNeighboursRequiresATag(t *testing.T) {
	// Without this, it returns an explained empty list, which reads as "this
	// tag has no neighbours" when the truth is "you named no tag".
	corpus := corpusFixture(t)
	err := runCorpusQuery(context.Background(), corpus,
		[]string{"--corpus", corpus, "--db", filepath.Join(t.TempDir(), "s.db"),
			"--query", "tag-neighbours"})
	if err == nil {
		t.Fatal("tag-neighbours with no tag succeeded")
	}
	if !strings.Contains(err.Error(), "--tag") {
		t.Errorf("error does not name the missing flag: %v", err)
	}
}

func TestCorpusQueryOnAMissingCorpusIsAnError(t *testing.T) {
	// "no rows" and "no database" must not look the same.
	absent := filepath.Join(t.TempDir(), "absent.db")
	if err := runCorpusQuery(context.Background(), absent,
		[]string{"--corpus", absent, "--db", filepath.Join(t.TempDir(), "s.db"),
			"--query", "underrated"}); err == nil {
		t.Error("querying a missing corpus reported success")
	}
}
