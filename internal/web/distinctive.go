package web

import (
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
)

// distinctiveTagCount is how many tags the work page calls distinctive.
//
// Five, because the point is a signature rather than a second tag list: the
// page already prints every tag below, and a "distinctive" block that repeats
// half of them teaches nothing.
const distinctiveTagCount = 5

// distinctiveTags picks the tags that say most about one work.
//
// The ordering is by DOCUMENT FREQUENCY, ascending: a tag on 40,000 works
// describes the corpus, and a tag on 12 describes this work. That is the
// complement of what every ranking surface already shows — the recommender
// scores a work by how much of its taste it shares with other works, which
// quietly rewards the common tags. "What makes this distinctive" is the same
// table read the other way round.
//
// Two refusals, both deliberate:
//
//   - No DocFreq table (a mirror that was never ingested) returns nothing.
//     The page renders no section rather than a section built from an empty
//     table, because "every tag is equally rare" is not a finding.
//   - A tag on EVERY work is dropped. It carries no distinguishing evidence,
//     and IDF is exactly zero for it; showing it would be showing the absence
//     of a signal as though it were one.
//   - A tag the table reports on ZERO works is dropped too, even though that
//     is the rarest count possible. This work carries the tag, so a zero means
//     the table and the work disagree — a stale side file. Reporting it as
//     "distinctive" would make the strongest claim in the block on the one
//     row whose evidence contradicts itself.
//
// Ties break on the name so two renders of one work agree. The tag ids are
// int32 in the corpus and the pairs already carry their names, so nothing
// here needs a second lookup.
func distinctiveTags(tags []corpus.TagPair, df *corpus.DocFreq, n int) []corpus.TagPair {
	if df == nil || n <= 0 || len(tags) == 0 {
		return nil
	}
	type scored struct {
		pair corpus.TagPair
		freq float64
	}
	ranked := make([]scored, 0, len(tags))
	for _, t := range tags {
		freq, ok := df.Count(t.ID)
		if !ok || freq <= 0 || freq >= df.Total {
			continue
		}
		ranked = append(ranked, scored{pair: t, freq: freq})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].freq != ranked[j].freq {
			return ranked[i].freq < ranked[j].freq
		}
		return ranked[i].pair.Name < ranked[j].pair.Name
	})
	if len(ranked) > n {
		ranked = ranked[:n]
	}
	out := make([]corpus.TagPair, len(ranked))
	for i, r := range ranked {
		out[i] = r.pair
	}
	return out
}
