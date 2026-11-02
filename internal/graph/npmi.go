package graph

import "math"

// NPMI is normalised pointwise mutual information: PMI divided by its own entropy
// term, so the result is bounded.
//
//	p(a,b)    = co / totalWorks
//	p(a),p(b) = freqA/totalWorks, freqB/totalWorks
//	PMI       = ln( p(a,b) / (p(a)p(b)) )        -- unbounded
//	nPMI      = PMI / -ln(p(a,b))                -- in [-1, 1]
//
// The bound is the point. Neighbourhood sums one vote per associated tag, and a sum
// of unbounded terms cannot be compared with signals that return [0,1]: that is why
// this deployment needed NeighbourSaturation = 100, a hand-calibrated constant
// standing in for a normalisation. nPMI needs no such constant — 1 means "always
// together", 0 means "independently often", negative means "less than chance".
//
// nPMI is still biased toward rare pairs (co=1 on two df=2 tags reads about 0.88), so
// it is NOT a licence to drop the co-occurrence floor; it only makes the magnitudes
// comparable. signal.TagNPMI.MinCount is the gate that does that work.
//
// Degenerate inputs return 0 rather than NaN or ±Inf, matching PMI: a pair with no
// co-occurrence has no association to score, and a caller that sums votes must not be
// handed a NaN that poisons the total.
func NPMI(freqA, freqB, co, totalWorks float64) float64 {
	if co <= 0 || freqA <= 0 || freqB <= 0 || totalWorks <= 0 {
		return 0
	}
	pab := co / totalWorks
	if pab >= 1 {
		// Every work carries both tags: one population, no surprise left.
		return 1
	}
	joint := (freqA / totalWorks) * (freqB / totalWorks)
	if joint <= 0 {
		return 0
	}
	v := math.Log(pab/joint) / -math.Log(pab)
	switch {
	case math.IsNaN(v), math.IsInf(v, 0):
		return 0
	case v > 1:
		return 1
	case v < -1:
		return -1
	}
	return v
}
