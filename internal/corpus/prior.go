package corpus

// The corpus-wide kudos average, which is the prior a per-tag number is shrunk
// toward.
//
// Why it exists (#39): the tag page printed `mean kudos` for the tag's works. Over
// three works that number is not an estimate of anything — one hit and two nobody
// read gives a mean that reads like "this tag's works are loved". A median was
// already there and is robust, but a median over three works is also just the middle
// of three. The honest fourth number is a mean SHRUNK toward the corpus's own mean:
// a tag with three works says almost nothing, so it should say almost the corpus
// average; a tag with five thousand works should say its own.
//
//	shrunk = (sum of the tag's kudos + m * prior) / (n + m)
//
// with m pseudo-works at the prior. m is 10: at ten works the tag's own evidence and
// the prior weigh the same, which is the smallest number where a tag's own mean
// starts to be worth more than the population's.

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// priorPseudoWorks is m in the formula above.
const priorPseudoWorks = 10

// priorCache holds the corpus mean, computed once per process.
//
// Process-wide because it is a property of the mirror rather than of a request: the
// tag page is the only caller today, and a full scan of works per page view would be
// the sort of cost this project has already refused once (the doc-frequency cache in
// the engine exists for the same reason).
type priorCache struct {
	once  sync.Once
	value float64
	err   error
}

// CorpusMeanKudos returns the mean kudos of the mirror's works that have any.
//
// Works with ZERO kudos are excluded. Including them would make the prior a statement
// about the archive's reading habits rather than about what a kudos is worth: this
// mirror holds 112,937 works and most carry none, so the all-in mean is a number near
// zero and every tag would be shrunk toward "nobody liked this".
func (a *AO3) CorpusMeanKudos(ctx context.Context) (float64, error) {
	a.prior.once.Do(func() {
		// Detached from the caller's cancellation, for the reason the engine's
		// embedding index learned the hard way: this value is cached for the life
		// of the process, so a reader who closes a tab must not decide what the
		// process knows about its own corpus.
		qctx := context.WithoutCancel(ctx)
		row := a.DB.QueryRowContext(qctx,
			`SELECT AVG(CAST(kudos AS REAL)) FROM works WHERE kudos > 0`)
		var v sql.NullFloat64
		if err := row.Scan(&v); err != nil {
			a.prior.err = fmt.Errorf("corpus mean kudos: %w", err)
			return
		}
		if v.Valid {
			a.prior.value = v.Float64
		}
	})
	return a.prior.value, a.prior.err
}

// ShrinkKudos returns the tag's mean kudos pulled toward the prior by m pseudo-works.
//
// Both ends are exact, which is what makes it checkable: with no works the answer IS
// the prior, and as the work count grows the answer approaches the tag's own mean
// from whichever side it started.
func ShrinkKudos(sum float64, n int, prior float64) float64 {
	if n < 0 {
		n = 0
	}
	m := float64(priorPseudoWorks)
	return (sum + m*prior) / (float64(n) + m)
}
