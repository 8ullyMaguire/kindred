// Package rank composes signals into a ranked list.
//
// It is the only place that knows about weights, and it never computes a
// signal itself: a signal is a pure function and the ranker just adds up
// what it is told. The layering rule (SPEC §5) is that handlers never
// compute a signal, signals never touch SQL, and this package is the
// meeting point.
package rank

import (
	"context"
	"sort"
)

// Store is what a signal is allowed to read. Signals get this rather than
// a concrete store type so a signal cannot accidentally reach the corpus
// handle and start a query per candidate.
type Store interface {
	// Embedding returns the embedding of an entity, or nil if absent.
	Embedding(ctx context.Context, kind string, id int64) ([]float32, bool, error)
	// PeerRatings returns crowd ratings for ids, if the corpus has them.
	PeerRatings(ctx context.Context, kind string, ids []int64) (map[int64]float64, error)
}

// Signal is one scoring dimension.
//
// The reason string is not optional. A ranking that cannot explain itself
// is not one anyone can argue with, and a signal that fails must say so
// through ErrSkip rather than returning a zero that looks like a real
// score.
type Signal interface {
	Name() string
	Score(c Candidate, seeds []Candidate, s Store) (value float64, reason string, err error)
}

// ErrSkip is returned by a signal that cannot be computed for this input.
//
// Skipping is not the same as scoring zero, and the difference is the
// whole point: the previous deployment shipped a bug where two of its
// signals were inert in production and silent about it, so the scores
// looked plausible while two dimensions contributed nothing. A skipped
// signal appears in Meta.Degraded and is named in the response.
var ErrSkip = errSkip{}

type errSkip struct{}

func (errSkip) Error() string { return "signal: skipped" }

// IsSkip reports whether err is the skip sentinel.
func IsSkip(err error) bool { return err == ErrSkip }

// Candidate is an entity with its score and evidence.
type Candidate struct {
	ID       int64
	Kind     string
	Title    string
	URL      string
	Summary  string
	Stats    map[string]float64
	TagNames []string
	TagIDs   []int32

	Score    float64
	Evidence []Evidence
}

// Evidence is one signal's contribution to a score.
type Evidence struct {
	Signal string  `json:"signal"`
	Value  float64 `json:"value"`
	Weight float64 `json:"weight"`
	Reason string  `json:"reason"`
}

// Meta is the report accompanying a ranked list.
type Meta struct {
	Seeds      int      `json:"seeds"`
	Candidates int      `json:"candidates"`
	PoolSize   int      `json:"pool_size"`
	Tune       string   `json:"tune"`
	Degraded   []string `json:"degraded,omitempty"`
	Shortfall  *Short   `json:"shortfall,omitempty"`
	Truncated  bool     `json:"truncated"`
}

// Short records that fewer results were returned than asked for, and why.
//
// The alternative is padding from below the cap, which restores exactly
// the concentration the cap removed. A list of 90 that says "90 of 100:
// 10 exceeded the per-fandom cap" is honest; a list of 100 that ignored
// the cap is not.
type Short struct {
	Requested int    `json:"requested"`
	Returned  int    `json:"returned"`
	Reason    string `json:"reason"`
}

// Tune is a named weight vector over signals.
type Tune struct {
	Name    string             `json:"name"`
	Weights map[string]float64 `json:"weights"`
}

// Score applies the signals and weights to every candidate.
//
// A candidate that every signal skipped is dropped rather than ranked at
// zero: an entity with no signal at all is not a poor match, it is an
// unknown one, and putting it in the list as rank 500 is a lie.
func Score(ctx context.Context, cands []Candidate, seeds []Candidate, sigs []Signal, tune Tune, s Store) ([]Candidate, Meta, error) {
	meta := Meta{Seeds: len(seeds), Candidates: len(cands), Tune: tune.Name}

	// Run every signal once over the whole pool and record which ones
	// failed for the pool as a whole. A signal that errors for a single
	// candidate is skipped for that candidate only; one that errors
	// everywhere is reported once in Degraded.
	degraded := map[string]bool{}
	partial := map[string]int{}

	for i := range cands {
		var total float64
		var evidence []Evidence
		matched := 0
		for _, sig := range sigs {
			w := tune.Weights[sig.Name()]
			if w == 0 {
				continue
			}
			v, reason, err := sig.Score(cands[i], seeds, s)
			if err != nil {
				if IsSkip(err) {
					degraded[sig.Name()] = true
					partial[sig.Name()]++
					continue
				}
				return nil, meta, err
			}
			total += w * v
			evidence = append(evidence, Evidence{
				Signal: sig.Name(),
				Value:  v,
				Weight: w,
				Reason: reason,
			})
			matched++
		}
		if matched == 0 {
			continue // no signal had an opinion; not a zero-scoring match
		}
		cands[i].Score = total
		cands[i].Evidence = evidence
	}
	meta.PoolSize = len(cands)

	for name := range degraded {
		// A signal skipped for every candidate is degraded; one that
		// skipped for some is merely partial, which is normal.
		if partial[name] == len(cands) && len(cands) > 0 {
			meta.Degraded = append(meta.Degraded, name)
		}
	}
	sort.Strings(meta.Degraded)

	out := cands[:0]
	for _, c := range cands {
		if c.Evidence != nil {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID // stable, so equal scores are reproducible
	})
	return out, meta, nil
}
