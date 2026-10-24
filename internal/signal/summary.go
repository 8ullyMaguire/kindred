package signal

// Writing-style and subject similarity from the summaries (#52).
//
// Every other signal in this package reads TAGS. Tags are a person's summary of a person's
// summary: "Angst" says nothing about whether two works read alike, and two works with the
// same eleven tags can share no subject at all. The mirror has 112,136 summaries, averaging
// 424 characters -- the only prose in it -- and until this signal nothing read them.
//
// Cosine over IDF-weighted shared terms, using the corpus-wide table from `kindred ingest`
// (corpus.SummaryDocFreq): a word on every summary says nothing, a word on three says a
// little, and a word on one is not in the table at all because it can relate at most one
// other work.
//
// The seeds' summaries are UNIONED rather than averaged: a reader with three seeds is asking
// for works near any of them, and averaging three summaries of different fandoms produces a
// midpoint that resembles none.

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// SummaryStyle scores text agreement between a candidate's summary and the seeds'.
type SummaryStyle struct {
	// DocFreq is the corpus-wide term table. Nil means this mirror was never ingested, and
	// the signal SKIPS (naming itself in meta.degraded) rather than scoring zero.
	DocFreq *corpus.SummaryDocFreq
	// MaxSeeds bounds how many seed summaries are read. Three is enough to describe a taste
	// and keeps the per-candidate cost flat when a request carries twenty seeds.
	MaxSeeds int
	// MinScore hides the noise floor: below this cosine the works share only unremarkable
	// words, and a small number in the evidence list invites reading meaning into it.
	MinScore float64
}

func (SummaryStyle) Name() string { return "summary_style" }

func (s SummaryStyle) Score(c rank.Candidate, seeds []rank.Candidate, _ rank.Store) (float64, string, error) {
	if s.DocFreq == nil || len(seeds) == 0 || strings.TrimSpace(c.Summary) == "" {
		return 0, "", rank.ErrSkip
	}
	maxSeeds := s.MaxSeeds
	if maxSeeds <= 0 {
		maxSeeds = 3
	}

	// The seed vector: each term once, weighted by its idf. A term repeated across seeds is
	// still one term -- repetition is not evidence about the corpus, it is evidence about
	// which works the reader opened.
	seedWeight := map[string]float64{}
	for i, seed := range seeds {
		if i >= maxSeeds {
			break
		}
		for _, tok := range corpus.SummaryTokens(seed.Summary) {
			if _, seen := seedWeight[tok]; seen {
				continue
			}
			if w := s.DocFreq.IDF(tok); w > 0 {
				seedWeight[tok] = w
			}
		}
	}
	if len(seedWeight) == 0 {
		return 0, "", rank.ErrSkip
	}

	candSeen := map[string]struct{}{}
	var dot, candNorm float64
	type shared struct {
		term string
		w    float64
	}
	var hits []shared
	for _, tok := range corpus.SummaryTokens(c.Summary) {
		if _, dup := candSeen[tok]; dup {
			continue
		}
		candSeen[tok] = struct{}{}
		w := s.DocFreq.IDF(tok)
		if w <= 0 {
			continue
		}
		candNorm += w * w
		if sw, ok := seedWeight[tok]; ok {
			dot += sw * w
			hits = append(hits, shared{tok, sw * w})
		}
	}
	if candNorm == 0 || dot == 0 {
		return 0, "", rank.ErrSkip
	}
	var seedNorm float64
	for _, w := range seedWeight {
		seedNorm += w * w
	}
	score := dot / (math.Sqrt(seedNorm) * math.Sqrt(candNorm))
	if s.MinScore > 0 && score < s.MinScore {
		return 0, "", rank.ErrSkip
	}

	// Rarest first: the term that did the work is the one a reader can argue with.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].w != hits[j].w {
			return hits[i].w > hits[j].w
		}
		return hits[i].term < hits[j].term
	})
	names := make([]string, 0, 4)
	for i, h := range hits {
		if i >= 4 {
			break
		}
		names = append(names, h.term)
	}
	more := ""
	if len(hits) > len(names) {
		more = fmt.Sprintf(" (+%d more)", len(hits)-len(names))
	}
	return score, fmt.Sprintf("shares %d weighted subject word%s with the seeds: %s%s",
		len(hits), pluralS(len(hits)), strings.Join(names, ", "), more), nil
}

// pluralS is the suffix for a count. Local because `engine` owns its own copy and a shared
// helper between two packages that are allowed to format independently would be a coupling
// for one letter.
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
