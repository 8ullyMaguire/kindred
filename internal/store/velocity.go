package store

// The measurement #48 refuses to build on (#48, the gate).
//
// The row asks for a reader-velocity SIGNAL. A signal needs a weight, a weight needs a
// distribution, and the live state database on 2026-10-23 held 33 marked works across 8 owners
// and 8 reader-days -- no owner with five. Fitting a weight to that is fitting noise.
//
// So the row's own condition is mechanised here instead of being re-measured by hand each
// session: the numbers below are exactly the ones the row named, computed the same way every
// time, and `kindred velocity` prints them with a verdict. When the deployment has readers,
// the command says so and the signal has a reason to exist; until then it says no, with the
// count that would change its mind.

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// VelocityGate is the row's own threshold, written once.
//
//	at least 20% of owners have >= 5 reader-days, AND
//	the 90th-percentile mark rate is at least twice the median
//
// The second condition is what "wide enough to change a ranking" means concretely: with p90 and
// p50 inside a factor of two, a velocity weight would move every reader by about the same
// amount, which is a constant in disguise.
const (
	velocityMinReaderDays  = 5
	velocityMinOwnerShare  = 0.20
	velocityMinSpreadRatio = 2.0
)

// OwnerVelocity is one reader's pace.
type OwnerVelocity struct {
	// Owner is the hashed owner key. It is an identifier, so callers that print it should
	// truncate; the sort order is stable without it.
	Owner string
	// ReaderDays is how many distinct days this reader marked something.
	ReaderDays int
	// Marks is how many works they marked read at all.
	Marks int
	// PerDay is Marks / ReaderDays -- the pace a signal would be built on.
	PerDay float64
}

// VelocityReport is the distribution and the verdict.
type VelocityReport struct {
	Owners     int
	Marks      int
	ReaderDays int
	OwnerRows  []OwnerVelocity
	// WithFiveDays is how many owners have at least velocityMinReaderDays reader-days.
	WithFiveDays int
	// P50 and P90 are the median and 90th percentile of PerDay over owners.
	P50   float64
	P90   float64
	whyOK bool
	// Why is the sentence a caller prints: either the reason the gate fails, naming the count
	// that would pass it, or the reason it passes.
	Why string
}

// MeetsGate reports whether the distribution is thick enough to fit a weight to.
func (r VelocityReport) MeetsGate() bool { return r.whyOK }

// Velocity returns the reader-velocity distribution from the state database.
//
// "Marked read" is `seen_works.read_at`: a shown work is not a read work, and counting both
// would let a reader who never marks anything look fast. A reader-day is a distinct calendar
// day (the state database stores local wall-clock strings), which is the unit the row used.
func (s *Store) Velocity(ctx context.Context) (VelocityReport, error) {
	var rep VelocityReport
	if s == nil || s.DB == nil {
		return rep, fmt.Errorf("velocity: no state database")
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT owner_key, COUNT(*) AS marks, COUNT(DISTINCT substr(read_at, 1, 10)) AS days
		FROM seen_works
		WHERE read_at IS NOT NULL AND read_at <> ''
		GROUP BY owner_key`)
	if err != nil {
		if isMissingTable(err) {
			rep.Why = "no seen_works table: this deployment has no reader history"
			return rep, nil
		}
		return rep, fmt.Errorf("velocity: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ov OwnerVelocity
		var days int
		if err := rows.Scan(&ov.Owner, &ov.Marks, &days); err != nil {
			return rep, fmt.Errorf("velocity: scan: %w", err)
		}
		ov.ReaderDays = days
		if days > 0 {
			ov.PerDay = float64(ov.Marks) / float64(days)
		}
		rep.OwnerRows = append(rep.OwnerRows, ov)
		rep.Marks += ov.Marks
		rep.ReaderDays += days
		if days >= velocityMinReaderDays {
			rep.WithFiveDays++
		}
	}
	if err := rows.Err(); err != nil {
		return rep, fmt.Errorf("velocity: rows: %w", err)
	}
	rep.Owners = len(rep.OwnerRows)
	sort.Slice(rep.OwnerRows, func(a, b int) bool {
		if rep.OwnerRows[a].PerDay != rep.OwnerRows[b].PerDay {
			return rep.OwnerRows[a].PerDay > rep.OwnerRows[b].PerDay
		}
		return rep.OwnerRows[a].Owner < rep.OwnerRows[b].Owner
	})
	if rep.Owners == 0 {
		rep.Why = "no reader has marked a work read yet"
		return rep, nil
	}

	rates := make([]float64, 0, rep.Owners)
	for _, o := range rep.OwnerRows {
		rates = append(rates, o.PerDay)
	}
	rep.P50 = percentile(rates, 0.50)
	rep.P90 = percentile(rates, 0.90)

	needOwners := int(float64(rep.Owners)*velocityMinOwnerShare + 0.5)
	if needOwners < 1 {
		needOwners = 1
	}
	switch {
	case rep.WithFiveDays < needOwners:
		rep.Why = fmt.Sprintf(
			"%d of %d owners have %d+ reader-days (need %d); the gate fails on THICKNESS -- %d more owner(s) with %d days would clear it",
			rep.WithFiveDays, rep.Owners, velocityMinReaderDays, needOwners,
			needOwners-rep.WithFiveDays, velocityMinReaderDays)
	case rep.P50 > 0 && rep.P90/rep.P50 < velocityMinSpreadRatio:
		rep.Why = fmt.Sprintf(
			"the pace distribution is flat: p90 %.2f marks/day against p50 %.2f (need a factor of %.0f); a weight fitted to this moves every reader the same amount",
			rep.P90, rep.P50, velocityMinSpreadRatio)
	default:
		rep.whyOK = true
		rep.Why = fmt.Sprintf(
			"the gate passes: %d of %d owners have %d+ reader-days and p90 %.2f is %.1fx p50 %.2f -- a velocity weight has a distribution to fit",
			rep.WithFiveDays, rep.Owners, velocityMinReaderDays, rep.P90, safeRatio(rep.P90, rep.P50), rep.P50)
	}
	rep.Why = strings.TrimSpace(rep.Why)
	return rep, nil
}

// percentile is the nearest-rank percentile of a slice that is NOT assumed sorted.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	cp := make([]float64, len(values))
	copy(cp, values)
	sort.Float64s(cp)
	idx := int(p*float64(len(cp)) + 0.999999)
	if idx < 1 {
		idx = 1
	}
	if idx > len(cp) {
		idx = len(cp)
	}
	return cp[idx-1]
}

func safeRatio(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}
