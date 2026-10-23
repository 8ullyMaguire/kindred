package eval

// Calibration: how often a POSITION in a returned list was right.
//
// The recommendation page prints one number per hit (Evidence{Signal, Value,
// Weight, Reason}), which says how a work got there and not whether the reader
// should believe it. Those are different questions. A weight of 0.41 is not a
// probability and never becomes one by being printed twice.
//
// So this measures the only thing the evaluation can honestly measure: for each
// position in the list, over every evaluated reader, how often the work that
// landed there was a work the reader later bookmarked. That is a hit RATE with a
// denominator, not a score, and it is the number a reader-facing sentence should
// quote.
//
// WHAT THIS IS NOT. It is not "the chance this reader will like this work". The
// rate is a property of the evaluation's population and of the recommender as it
// stood when the run happened; it answers "lists like the ones this tool built
// were right about this often", and the page says so in those words. Nothing here
// is computed from a single reader's list, so nothing here can be personal.
//
// AND IT IS NOT A BAND LABEL. The obvious product move is to print "strong" beside
// positions 1-3 and "speculative" beside 8-10. Those words would be an opinion the
// measurement does not contain: the data says "1 in 4" or "1 in 50", and a reader
// can act on that without being told which one to call strong. The label IS the
// rate.

import "fmt"

// PositionRate is one position's record across a run.
//
// Observed is the number of readers whose returned list was long enough to HAVE
// this position — not the number of readers evaluated. A recommender that returns
// five ids for one reader and ten for another has fewer observations at position
// 8, and dividing by the reader count would report the short lists as misses at
// positions they never filled.
type PositionRate struct {
	Position int `json:"position"`
	Hits     int `json:"hits"`
	Observed int `json:"observed"`
}

// Rate is the share of observations at this position that were hits, and whether
// there were any. ok=false is NOT zero: a position nothing reached has no rate,
// and printing 0.0 for it would say the recommender missed where it never aimed.
func (p PositionRate) Rate() (float64, bool) {
	if p.Observed <= 0 {
		return 0, false
	}
	return float64(p.Hits) / float64(p.Observed), true
}

// OneIn renders the rate as "1 in N", which is the form a reader can act on and
// the form that does not imply precision the sample cannot support. It returns ""
// when there is no rate.
func (p PositionRate) OneIn() string {
	rate, ok := p.Rate()
	if !ok || rate <= 0 {
		return ""
	}
	return fmt.Sprintf("1 in %d", int(1/rate+0.5))
}

// Calibration is a run's per-position record.
type Calibration struct {
	Positions []PositionRate `json:"positions"`
	// Readers is how many lists the positions were observed over, and K the
	// depth they were recorded to. Both are printed, because "1 in 4" over 3
	// readers and over 300 are different claims wearing the same sentence.
	Readers int `json:"readers"`
	K       int `json:"k"`
	// Skipped names why there is nothing to show, when there is nothing. It is
	// the same rule as the diversity columns: unmeasured is not zero.
	Skipped string `json:"skipped,omitempty"`
}

// At returns the record for a 1-based position.
func (c Calibration) At(position int) (PositionRate, bool) {
	if position < 1 {
		return PositionRate{}, false
	}
	for _, p := range c.Positions {
		if p.Position == position {
			return p, true
		}
	}
	return PositionRate{}, false
}

// Measured reports whether the run recorded any observations at all.
func (c Calibration) Measured() bool {
	for _, p := range c.Positions {
		if p.Observed > 0 {
			return true
		}
	}
	return false
}

// Band aggregates a range of positions, because a per-position table is 10 rows
// of noise at the tail and one sentence is what the page has room for. Both ends
// are 1-based and inclusive; positions the run never recorded are excluded from
// the denominator, never counted as misses.
func (c Calibration) Band(from, to int) PositionRate {
	out := PositionRate{Position: from}
	for _, p := range c.Positions {
		if p.Position < from || p.Position > to {
			continue
		}
		out.Hits += p.Hits
		out.Observed += p.Observed
	}
	return out
}

// MinCalibrationReaders is the sample a position needs before its rate is
// printed. Below it the honest output is the reader count and no rate: three
// readers produce rates of 0, 0.33, 0.67 and 1, and a page that prints "1 in 3" for
// that is printing a coin flip with a decimal point.
const MinCalibrationReaders = 20

// Trustworthy reports whether enough readers were evaluated for the rate at this
// position to be worth printing.
func (c Calibration) Trustworthy(position int) bool {
	p, ok := c.At(position)
	return ok && p.Observed >= MinCalibrationReaders
}

// Sentence is the line the page prints above the list.
//
// It deliberately names the population ("readers like this one", "in the last
// run") and the outcome ("bookmarked") instead of saying "you will like this 22%
// of the time", which no measurement in this project can support.
func (c Calibration) Sentence() string {
	if !c.Measured() {
		if c.Skipped != "" {
			return c.Skipped
		}
		return "not calibrated yet: no evaluation has recorded where hits land"
	}
	if c.Readers < MinCalibrationReaders {
		return fmt.Sprintf("%d reader(s) evaluated — too few to calibrate against (needs %d)",
			c.Readers, MinCalibrationReaders)
	}
	top := c.Band(1, 3)
	mid := c.Band(4, 7)
	tail := c.Band(8, c.K)
	parts := make([]string, 0, 3)
	for _, b := range []struct {
		name string
		p    PositionRate
	}{{"first three", top}, {"next four", mid}, {"the rest", tail}} {
		if b.p.Observed == 0 {
			continue
		}
		rate, _ := b.p.Rate()
		parts = append(parts, fmt.Sprintf("%s %s", b.name, percent(rate)))
	}
	if len(parts) == 0 {
		return "not calibrated yet: the run recorded no positions"
	}
	return fmt.Sprintf("In the last evaluation, %d readers' lists were right at: %s. "+
		"\"Right\" means the work was one the reader later bookmarked — not that a "+
		"particular reader will like a particular work.",
		c.Readers, joinComma(parts))
}

func percent(rate float64) string {
	return fmt.Sprintf("%.0f%%", rate*100)
}

func joinComma(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	}
	out := ""
	for i, p := range parts {
		if i == len(parts)-1 {
			out += " and " + p
			continue
		}
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
