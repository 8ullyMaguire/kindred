package profile

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func fixture(t *testing.T) *sql.DB {
	t.Helper()
	p := filepath.Join(t.TempDir(), "corpus.db")
	if _, err := testcorpus.New(40).Write(p); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := NewStore(fixture(t))
	ctx := context.Background()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	want := &Profile{Name: "reader", Source: "works", Works: 12,
		Tags: map[int32]float64{1: 1.0, 7: 0.5, 9: 0.25}}
	if err := s.Save(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, "reader")
	if err != nil {
		t.Fatal(err)
	}
	if got.Works != 12 || got.Source != "works" {
		t.Errorf("metadata lost: %+v", got)
	}
	if len(got.Tags) != len(want.Tags) {
		t.Errorf("got %d tags, want %d", len(got.Tags), len(want.Tags))
	}
	for id, w := range want.Tags {
		if got.Tags[id] != w {
			t.Errorf("tag %d = %v, want %v", id, got.Tags[id], w)
		}
	}
}

func TestLoadUnknownProfileIsDistinguishableFromAnEmptyOne(t *testing.T) {
	// "not found" and "found, no tags" must be different outcomes: one means
	// "build it", the other means "this reader likes nothing yet".
	s := NewStore(fixture(t))
	ctx := context.Background()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown profile error = %v, want ErrNotFound", err)
	}
	if err := s.Save(ctx, &Profile{Name: "empty"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, "empty"); err != nil {
		t.Errorf("a saved empty profile did not load: %v", err)
	}
}

func TestSaveReplacesRatherThanAccumulates(t *testing.T) {
	// A tag that dropped out of a reader's taste must disappear. An upsert
	// that only adds keeps the stale weight voting forever.
	s := NewStore(fixture(t))
	ctx := context.Background()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, &Profile{Name: "r", Tags: map[int32]float64{1: 1, 2: 0.5}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, &Profile{Name: "r", Tags: map[int32]float64{1: 1}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, still := got.Tags[2]; still {
		t.Error("a dropped tag survived a re-save")
	}
	if len(got.Tags) != 1 {
		t.Errorf("got %d tags, want 1", len(got.Tags))
	}
}

func TestApplyClampsWeightsAtZeroAndOne(t *testing.T) {
	// A negative weight inverts a signal's meaning: a tag nobody likes is not
	// the opposite of a tag everybody likes.
	p := &Profile{Name: "r", Tags: map[int32]float64{1: 0.05, 2: 0.95}}
	p = Apply(p, Feedback{WorkID: 1, TagIDs: []int32{1, 2}})
	if p.Tags[1] < 0 {
		t.Errorf("weight went negative: %v", p.Tags[1])
	}
	if p.Tags[2] > 1 {
		t.Errorf("weight exceeded 1: %v", p.Tags[2])
	}
}

func TestApplyCountsOnlyLikesAsWorks(t *testing.T) {
	p := &Profile{Name: "r", Tags: map[int32]float64{}}
	p = Apply(p, Feedback{WorkID: 1, Liked: true, TagIDs: []int32{1}})
	p = Apply(p, Feedback{WorkID: 2, Liked: false, TagIDs: []int32{1}})
	if p.Works != 1 {
		t.Errorf("Works = %d after one like and one dismiss, want 1", p.Works)
	}
}

// Liking and then dismissing the same tag must return it toward neutral, not
// drift it monotonically. This is the oscillation the sibling's per-impression
// retraining had.
func TestApplyIsReversible(t *testing.T) {
	p := &Profile{Name: "r", Tags: map[int32]float64{}}
	p = Apply(p, Feedback{WorkID: 1, Liked: true, TagIDs: []int32{7}})
	after := p.Tags[7]
	p = Apply(p, Feedback{WorkID: 2, Liked: false, TagIDs: []int32{7}})
	if after <= 0 {
		t.Fatalf("a like did not move the weight: %v", after)
	}
	if p.Tags[7] >= after {
		t.Errorf("a dismissal did not move the weight down: %v -> %v", after, p.Tags[7])
	}
}

func TestApplyOnANilProfileDoesNotPanic(t *testing.T) {
	// A profile built from zero works is nil, not an empty struct, and a
	// panic here would take down the request that fed the first event.
	if p := Apply(nil, Feedback{WorkID: 1, Liked: true, TagIDs: []int32{3}}); p == nil || p.Tags[3] == 0 {
		t.Errorf("Apply(nil, ...) = %+v", p)
	}
}

func TestBuildFromWorksProducesNormalisedWeights(t *testing.T) {
	db := fixture(t)
	p, err := BuildFromWorks(context.Background(), db, []int64{1, 2, 3}, "reader")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Tags) == 0 {
		t.Skip("fixture works carry no tags")
	}
	if p.Works != 3 {
		t.Errorf("Works = %d, want 3", p.Works)
	}
	max := 0.0
	for _, w := range p.Tags {
		if w > max {
			max = w
		}
	}
	if max != 1 {
		t.Errorf("weights are not normalised: max = %v", max)
	}
}

func TestBuildFromNoWorksIsAnError(t *testing.T) {
	// An empty profile from an empty work list looks like "this reader likes
	// nothing", which is a different claim from "you gave me no works".
	if _, err := BuildFromWorks(context.Background(), fixture(t), nil, "r"); err == nil {
		t.Error("building from no works reported success")
	}
}

func TestTagIDsAreHeaviestFirstAndCapped(t *testing.T) {
	s := NewStore(fixture(t))
	ctx := context.Background()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, &Profile{Name: "r", Tags: map[int32]float64{
		1: 0.1, 2: 0.9, 3: 0.5}}); err != nil {
		t.Fatal(err)
	}
	ids, err := s.TagIDs(ctx, "r", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d ids for cap 2", len(ids))
	}
	if ids[0] != 2 || ids[1] != 3 {
		t.Errorf("got %v, want [2 3] (heaviest first)", ids)
	}
}

func TestEnsureSchemaIsIdempotent(t *testing.T) {
	// It runs on every open, so a second call must not fail.
	s := NewStore(fixture(t))
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := s.EnsureSchema(ctx); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestListReturnsEveryProfile(t *testing.T) {
	s := NewStore(fixture(t))
	ctx := context.Background()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c"} {
		if err := s.Save(ctx, &Profile{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("listed %v, want 3 names", got)
	}
}
