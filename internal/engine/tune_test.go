package engine

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// tuneCorpus builds a fixture with a store attached, so a named tune can be
// stored and then requested.
func tuneCorpus(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tunes.db")

	f := &testcorpus.Corpus{
		Tags: []testcorpus.Tag{{ID: 1, Name: "shared"}},
	}
	add := func(id int64, title string) {
		f.Works = append(f.Works, testcorpus.Work{
			ID: id, Title: title, Authors: "author",
			WordCount: 20000, Kudos: 100, Hits: 1000,
			Rating: "General Audiences", Language: "English", Complete: true,
		})
		f.WorkTags = append(f.WorkTags, testcorpus.WorkTag{
			WorkID: id, TagID: 1, TagType: "freeform"})
	}
	add(1, "one")
	add(2, "two")
	add(3, "three")
	add(9, "the seed")

	if _, err := f.Write(path); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// The tunes table lives in the STATE database, not the corpus, so an
	// in-memory state store is enough: no path, no migration of a fixture.
	st, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	return &Engine{Corpus: corpus.NewAO3(db), Store: st, PoolSize: 50}
}

func recommendWithTune(t *testing.T, e *Engine, name string) (*Result, error) {
	t.Helper()
	return e.Recommend(context.Background(), Request{
		Seeds:   []Seed{{Kind: corpus.AO3Kind, ID: 9}},
		Kind:    corpus.AO3Kind,
		N:       20,
		Exclude: true,
		Tune:    name,
	})
}

// TestANamedTuneActuallyChangesTheWeights is the test that would have caught
// the bug this feature actually had.
//
// `?tune=` was recorded into meta.Tune and had NO effect on ranking:
// signals() unconditionally built DefaultTune(). The parameter was accepted,
// echoed back at the bottom of the page as though it had been applied, and
// inert — which is the worst shape a control can have, because it reports
// success while doing nothing.
//
// The assertion is on the weights the response reports, not on the ORDER of
// the results. Order is the thing a reader would look at, and it is the wrong
// thing to assert here: whether a weight change reorders a given fixture
// depends on the scores, whereas the weight map is the contract itself.
func TestANamedTuneActuallyChangesTheWeights(t *testing.T) {
	e := tuneCorpus(t)

	if err := e.Store.SaveTune(context.Background(), "collab-heavy",
		map[string]float64{"collab": 9.5, "tag_overlap": 0.1}); err != nil {
		t.Fatalf("save tune: %v", err)
	}

	res, err := recommendWithTune(t, e, "collab-heavy")
	if err != nil {
		t.Fatalf("recommend with a stored tune: %v", err)
	}
	if res.Meta.Tune != "collab-heavy" {
		t.Fatalf("meta.Tune = %q, want the requested name", res.Meta.Tune)
	}
	// Compare RATIOS, not absolute weights. In full mode the engine drops the
	// embedding signal and renormalises the survivors to the same total, so
	// an absolute assertion would fail on a correct implementation — and a
	// test that fails on correct code is a test that gets deleted rather
	// than understood. The ratio is what "collab outweighs pmi 95:1" means,
	// and it survives any rescaling.
	const wantRatio = 9.5 / 0.1
	gotRatio := res.Tune["collab"] / res.Tune["tag_overlap"]
	if diff := gotRatio - wantRatio; diff > 0.001 || diff < -0.001 {
		t.Fatalf("collab:tag_overlap = %v, want %v (the stored 9.5:0.1). "+
			"The tune was not applied to the weights. Full map: %v",
			gotRatio, wantRatio, res.Tune)
	}
	// A signal the tune did not name keeps its default weight RELATIVE to the
	// other unnamed ones. Without this the test would also pass if the
	// override map REPLACED the whole tune instead of overriding parts of it,
	// because quality and popularity still have the same ratio either way.
	//
	// The comparison is quality:popularity rather than either against
	// tag_overlap, because tag_overlap was overridden and so is the one
	// value guaranteed to have moved.
	wantQ := DefaultTune().Weights["quality"]
	wantPop := DefaultTune().Weights["popularity"]
	gotQ := res.Tune["quality"] / res.Tune["popularity"]
	if diff := gotQ - wantQ/wantPop; diff > 0.001 || diff < -0.001 {
		t.Fatalf("unnamed signals changed relative to each other: "+
			"quality:popularity = %v, want the default %v. The override may "+
			"have replaced the tune instead of overriding it. Full map: %v",
			gotQ, wantQ/wantPop, res.Tune)
	}
}

// TestAnUnknownTuneIsAnError pins the other half: a name that does not exist
// must not be answered with the default weights.
//
// Falling back would produce a normal-looking list ranked by weights the
// reader did not choose, with no indication anything was wrong. The error
// names the alternatives, so the reader can correct it in one step.
func TestAnUnknownTuneIsAnError(t *testing.T) {
	e := tuneCorpus(t)

	_, err := recommendWithTune(t, e, "no-such-tune")
	if err == nil {
		t.Fatal("an unknown tune was accepted and ranked with the default weights")
	}
	if !errors.Is(err, ErrUnknownTune) {
		t.Fatalf("error is %v, want ErrUnknownTune", err)
	}
	// The message must be actionable: it names the alternatives.
	if msg := err.Error(); !contains(msg, "default") {
		t.Fatalf("the error does not name what IS available: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestNoTuneMeansTheDefaults pins the ordinary path: a request naming no
// tune is the built-in weights and must not fail on an engine with a store.
func TestNoTuneMeansTheDefaults(t *testing.T) {
	e := tuneCorpus(t)

	for _, name := range []string{"", "default", "  default  "} {
		res, err := recommendWithTune(t, e, name)
		if err != nil {
			t.Fatalf("tune %q: %v", name, err)
		}
		if res.Meta.Tune != "default" {
			t.Fatalf("tune %q reported %q, want default", name, res.Meta.Tune)
		}
		// Ratios again, for the renormalisation reason above: what matters
		// is that no tune was named and so no override was applied, which
		// shows up as the DEFAULT ratios surviving.
		want := DefaultTune()
		for _, sig := range []string{"tag_overlap", "neighbourhood"} {
			w, present := want.Weights[sig]
			if !present {
				continue
			}
			wantRatio := w / want.Weights["tag_overlap"]
			gotRatio := res.Tune[sig] / res.Tune["tag_overlap"]
			if diff := gotRatio - wantRatio; diff > 0.001 || diff < -0.001 {
				t.Fatalf("tune %q changed the %s ratio to %v, want the default %v",
					name, sig, gotRatio, wantRatio)
			}
		}
	}
}

// TestANamedTuneNeedsAStore pins that an engine with no store says so rather
// than pretending the default is what was asked for.
func TestANamedTuneNeedsAStore(t *testing.T) {
	e := tuneCorpus(t)
	e.Store = nil

	_, err := recommendWithTune(t, e, "collab-heavy")
	if err == nil {
		t.Fatal("a named tune was honoured by an engine with no store")
	}
	if !errors.Is(err, ErrUnknownTune) {
		t.Fatalf("error is %v, want ErrUnknownTune", err)
	}
}

// TestAnOverrideForAnUnknownSignalIsDroppedAndReported pins that a weight for
// a signal that does not exist cannot be smuggled in.
//
// Accepting it would store a weight that names a signal contributing nothing
// to the score: a reader reading back a tune they saved would see a number
// that does nothing, with no way to know.
func TestAnOverrideForAnUnknownSignalIsDroppedAndReported(t *testing.T) {
	base := rank.Tune{Weights: map[string]float64{"pmi": 1, "collab": 2}}

	got := base.WithOverrides(map[string]float64{
		"collab":       5,
		"not_a_signal": 99,
	})
	if got.Weights["collab"] != 5 {
		t.Fatalf("a known signal was not overridden: %v", got.Weights)
	}
	if _, present := got.Weights["not_a_signal"]; present {
		t.Fatalf("an unknown signal's weight was kept: %v", got.Weights)
	}
	unknown := base.UnknownSignals(map[string]float64{"not_a_signal": 99, "collab": 5})
	if len(unknown) != 1 || unknown[0] != "not_a_signal" {
		t.Fatalf("UnknownSignals = %v, want [not_a_signal]", unknown)
	}
	// The receiver must be untouched: two concurrent requests sharing one
	// tune map is the bug DefaultTune()'s fresh-map design exists to prevent.
	if base.Weights["collab"] != 2 {
		t.Fatalf("WithOverrides mutated the receiver: %v", base.Weights)
	}
}

// TestStoreRoundTripsATune is the persistence half, so a tune saved by one
// request can be used by the next.
func TestStoreRoundTripsATune(t *testing.T) {
	e := tuneCorpus(t)
	ctx := context.Background()
	st := e.Store

	if err := st.SaveTune(ctx, "mine", map[string]float64{"tag_overlap": 3.25, "collab": 0.5}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.Tune(ctx, "mine")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got["tag_overlap"] != 3.25 || got["collab"] != 0.5 {
		t.Fatalf("round trip lost weights: %v", got)
	}

	// A save under the same name replaces rather than duplicating.
	if err := st.SaveTune(ctx, "mine", map[string]float64{"tag_overlap": 1}); err != nil {
		t.Fatalf("second save: %v", err)
	}
	names, err := st.TuneNames(ctx)
	if err != nil {
		t.Fatalf("names: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("a second save of the same name produced %v", names)
	}

	// An unknown name is ErrNotFound, not an empty weight map: an empty map
	// would silently rank by defaults.
	if _, err := st.Tune(ctx, "absent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown tune error is %v, want store.ErrNotFound", err)
	}
	// "default" is a name that must always work and is not stored.
	if _, err := st.Tune(ctx, "default"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the unstored default returned %v, want ErrNotFound", err)
	}
}

// TestACorruptTuneRowIsReported pins that a malformed stored weight blob is
// an error. Ranking with the defaults because a JSON blob is malformed is
// the accepted-and-ignored shape, and it is invisible: the request succeeds.
func TestACorruptTuneRowIsReported(t *testing.T) {
	e := tuneCorpus(t)
	ctx := context.Background()

	if _, err := e.Store.DB.ExecContext(ctx,
		`INSERT INTO tunes (name, weights) VALUES ('broken', 'not json')`); err != nil {
		t.Fatalf("insert corrupt row: %v", err)
	}
	if _, err := e.Store.Tune(ctx, "broken"); err == nil {
		t.Fatal("a corrupt tune row loaded successfully")
	}
	_, err := recommendWithTune(t, e, "broken")
	if err == nil {
		t.Fatal("a corrupt tune row ranked with the default weights instead of failing")
	}
}
