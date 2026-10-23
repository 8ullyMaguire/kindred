package web

// The calibration block: what the order is worth, in the evaluation's own words.
//
// A recommendation list is an ORDER, and the page has never said whether the order
// carries information. Evidence per item says how a work got there (a weight of
// 0.41 from idf_overlap) and that is not a probability; printed alone it invites a
// reader to read a score as a chance.
//
// So the block quotes the only measured claim available: in the last evaluation,
// how often a work at this position was one the reader later bookmarked. Three
// bands, not ten positions — ten rows of tail noise is not a page.

import (
	"context"
	"fmt"

	"git.polarisocial.xyz/kindred/kindred/internal/eval"
)

// CalibrationBand is one row of the block.
type CalibrationBand struct {
	Label    string
	Hits     int
	Observed int
	// Percent is the measured rate, empty when the band has no observations or
	// the sample is too thin to print. Empty renders as an em dash plus the
	// reason, never as "0%".
	Percent string
	// OneIn is the same rate as "1 in N", which is how a reader uses it.
	OneIn string
}

// CalibrationView is what the template renders.
type CalibrationView struct {
	// Sentence is the headline: the population, the outcome, and what it does
	// not promise.
	Sentence string
	Bands    []CalibrationBand
	// Measured is false when no run has recorded positions; Enough is false when
	// the run was smaller than eval.MinCalibrationReaders.
	Measured bool
	Enough   bool
	Readers  int
	Needed   int
	RunID    int64
}

// calibrationView reads the newest recorded calibration and shapes it for the page.
//
// A store failure returns a view whose sentence says the calibration could not be
// read, NOT a page error: a recommendation list is still correct without this
// block, and an unreadable table must not take the list down with it.
func (d Deps) calibrationView(ctx context.Context) CalibrationView {
	out := CalibrationView{Needed: eval.MinCalibrationReaders}
	if d.Engine == nil || d.Engine.Store == nil {
		out.Sentence = "not calibrated: this deployment has no state store"
		return out
	}
	stored, err := d.Engine.Store.LatestCalibration(ctx)
	if err != nil {
		out.Sentence = fmt.Sprintf("not calibrated: the recorded calibration could not be read (%v)", err)
		return out
	}
	if !stored.OK {
		out.Sentence = "not calibrated yet: run `kindred eval` and this block will say how often " +
			"each position held a work the reader went on to bookmark."
		return out
	}

	c := eval.Calibration{K: stored.K, Readers: stored.Readers}
	for _, p := range stored.Positions {
		c.Positions = append(c.Positions, eval.PositionRate{
			Position: p.Position, Hits: p.Hits, Observed: p.Observed,
		})
	}
	out.Measured = c.Measured()
	out.Enough = stored.Readers >= eval.MinCalibrationReaders
	out.Readers = stored.Readers
	out.RunID = stored.RunID
	out.Sentence = c.Sentence()

	for _, b := range []struct {
		label    string
		from, to int
	}{
		{"the first three", 1, 3},
		{"the next four", 4, 7},
		{"the rest", 8, stored.K},
	} {
		p := c.Band(b.from, b.to)
		row := CalibrationBand{Label: b.label, Hits: p.Hits, Observed: p.Observed}
		// A rate is printed only when the sample can carry it, and never as
		// "0%" for a band nothing reached: those are different facts and the
		// page must not merge them.
		if rate, ok := p.Rate(); ok && out.Enough {
			row.Percent = fmt.Sprintf("%.0f%%", rate*100)
			row.OneIn = p.OneIn()
		}
		out.Bands = append(out.Bands, row)
	}
	return out
}
