package corpus

// The shrunk tag number (#39), on values small enough to check by hand.
//
// The failure this prevents is a thin tag bragging: one work with 900 kudos and a
// mean of 900 reads as "this tag's works are loved", when the tag has three works and
// two of them nobody read. Shrinkage is not a tie-break or a smoothing pass — it is
// the statement that a small sample belongs mostly to the population it was drawn
// from.

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// With no works the answer IS the prior. Written as a test because every shrinkage
// implementation has an n=0 branch, and the wrong branch returns 0 — a number that
// reads as "this tag's works are terrible" rather than "nothing is known".
func TestNoWorksMeansThePriorExactly(t *testing.T) {
	if got := ShrinkKudos(0, 0, 42); got != 42 {
		t.Fatalf("shrunk over no works = %v, want the prior 42", got)
	}
	// A negative count is a bug upstream, not a licence to divide by a negative.
	if got := ShrinkKudos(0, -5, 42); got != 42 {
		t.Fatalf("negative work count gave %v", got)
	}
}

// The two ends of the curve, and the fact that it stays between them.
func TestShrinkageStartsAtThePriorAndApproachesTheMean(t *testing.T) {
	const prior = 20
	// One work at 900: almost all prior. 900 is the tag's evidence, and one work
	// of it is worth about a tenth.
	one := ShrinkKudos(900, 1, prior)
	if want := (900 + 10*20) / 11.0; math.Abs(one-want) > 1e-9 {
		t.Fatalf("one work = %v, want %v", one, want)
	}
	if one > 120 {
		t.Fatalf("a single work of 900 kudos shrank only to %v: the prior is not "+
			"holding the thin tag back", one)
	}
	// Ten thousand works at 900: the tag's own mean, to within the prior's
	// remaining pull -- which is m*(mean-prior)/(n+m) = 10*880/10010 = 0.88, not
	// zero. A test that demanded 0.1 here would be asserting a formula the code
	// does not implement, and the "fix" would be to stop shrinking.
	many := ShrinkKudos(900*10000, 10000, prior)
	if want := (900.0*10000 + 10*prior) / (10000 + 10); math.Abs(many-want) > 1e-9 {
		t.Fatalf("ten thousand works = %v, want %v", many, want)
	}
	if 900-many > 1.0 {
		t.Fatalf("ten thousand works sit %v below the tag's own mean: the prior is "+
			"still doing more than its last pseudo-work's worth", 900-many)
	}
	// A tag worse than the corpus is pulled UP, not only down: shrinkage is
	// symmetric and a one-sided version is a popularity boost in disguise.
	bad := ShrinkKudos(1, 1, prior)
	if bad <= 1 || bad >= prior {
		t.Fatalf("one work of 1 kudo shrank to %v, want between 1 and the prior", bad)
	}
}

// The prior is the mean of works that HAVE kudos, and it is computed once.
func TestThePriorIgnoresWorksNobodyKudosedAndIsCached(t *testing.T) {
	c := testcorpus.New(40)
	path := filepath.Join(t.TempDir(), "corpus.db")
	if _, err := c.Write(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := NewAO3(db)
	ctx := context.Background()

	// The fixture's works: compute the expected prior from the same exclusion the
	// code uses, rather than hard-coding a number that a fixture change would
	// silently invalidate.
	var want float64
	if err := db.QueryRow(`SELECT AVG(CAST(kudos AS REAL)) FROM works WHERE kudos > 0`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	got, err := a.CorpusMeanKudos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("prior = %v, want %v", got, want)
	}
	// The all-in mean (including works with none) must be lower: this mirror is
	// mostly unkudosed works, so the two numbers are not interchangeable.
	var allIn float64
	if err := db.QueryRow(`SELECT AVG(CAST(kudos AS REAL)) FROM works`).Scan(&allIn); err != nil {
		t.Fatal(err)
	}
	if want < allIn {
		t.Fatalf("the exclusion made the prior LOWER (%v vs %v); zero-kudos works are "+
			"supposed to be the ones dragging it down", want, allIn)
	}
	// Cached: a second call cannot see a change, because the value is the
	// process's property of the mirror and re-reading it per page view is the
	// cost this exists to avoid.
	if _, err := db.Exec(`UPDATE works SET kudos = kudos + 1000`); err != nil {
		t.Fatal(err)
	}
	again, err := a.CorpusMeanKudos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again != got {
		t.Fatalf("prior changed from %v to %v after the first read: not cached", got, again)
	}
}

// The shelf carries the number, its prior and the weight — and a tag's own mean is
// still reported beside it, because the two answer different questions.
func TestTheShelfCarriesTheShrunkNumberWithItsPrior(t *testing.T) {
	c := testcorpus.New(40)
	path := filepath.Join(t.TempDir(), "corpus.db")
	if _, err := c.Write(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := NewAO3(db)
	ctx := context.Background()

	var tagID int64
	if err := db.QueryRow(`SELECT tag_id FROM work_tags LIMIT 1`).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	shelf, err := a.TagShelf(ctx, tagID)
	if err != nil {
		t.Fatal(err)
	}
	if shelf.Works == 0 {
		t.Skip("fixture tag has no works")
	}
	if shelf.PriorWeight != priorPseudoWorks {
		t.Fatalf("prior weight = %d, want %d", shelf.PriorWeight, priorPseudoWorks)
	}
	if shelf.PriorKudos <= 0 {
		t.Fatal("no prior recorded: a shrunk number whose prior is hidden cannot be checked")
	}
	// Between the tag's own mean and the prior, inclusive, on whichever side.
	lo := math.Min(float64(shelf.MeanKudos), shelf.PriorKudos)
	hi := math.Max(float64(shelf.MeanKudos), shelf.PriorKudos)
	if shelf.ShrunkKudos < lo-1e-9 || shelf.ShrunkKudos > hi+1e-9 {
		t.Fatalf("shrunk %v is outside [%v, %v] (mean %d, prior %v)",
			shelf.ShrunkKudos, lo, hi, shelf.MeanKudos, shelf.PriorKudos)
	}
}
