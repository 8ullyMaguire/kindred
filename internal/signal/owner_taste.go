package signal

import (
	"fmt"
	"math"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// OwnerTaste is the owner cohort's taste as a GLOBAL prior: the one signal
// that is registered for every reader rather than for the reader who asked.
//
// It exists because of a request the whole deployment is shaped around —
// "I want my taste to influence recommendations for all users" — and it is
// deliberately the same shape as a per-reader taste signal, so that a
// reader's own taste layers on top of it instead of having to displace it.
// The score is the share of the candidate's tag mass the owner's vector
// likes: a work whose tags the owner cares about is nudged up for everyone,
// and the nudge is small and explicit in the tune rather than implicit in
// the tag_overlap signal, where it would be invisible and unauditable.
//
// Registered UNCONDITIONALLY, like peer_rating and collab, including on a
// deployment with no owner configured. Guarding this on `len(Weights) > 0`
// would be the bug this project has shipped and written down five times: a
// signal that is present only when it has data is a signal that is absent
// without saying so. With no weights it returns rank.ErrSkip for every
// candidate and names itself in meta.degraded[] — the observable
// difference between "this deployment has no owner" and "the signal does
// not work".
type OwnerTaste struct {
	// Weights is the owner's signed tag vector: positive means the owner
	// likes the tag, negative means they discriminate against it.
	//
	// Nil is the honest state of a deployment with no owner profile, and
	// it is not an error: it is a fact the response reports.
	Weights map[int32]float64

	// MaxTags bounds the tag mass read per candidate, for the same reason
	// TagOverlap bounds it: a work with thousands of freeform tags is a
	// fandom dump or a bot, and walking all of them for every candidate is
	// the per-candidate-recomputation cost class this engine has already
	// profiled and removed three times. Only the highest-|weight| tags
	// matter here, so a bound is free.
	MaxTags int

	// Graph resolves tag ids to names for the evidence line. Nil is fine:
	// the score does not depend on it, and the line falls back to the id.
	Graph Graph
}

func (OwnerTaste) Name() string { return "owner_taste" }

// defaultOwnerTasteMaxTags matches TagOverlap's bound. Stated once here
// rather than shared, because the two bounds are the same number for the
// same reason and tying them together would make one signal's tuning
// silently move the other's.
const defaultOwnerTasteMaxTags = 200

func (s OwnerTaste) Score(c rank.Candidate, _ []rank.Candidate, _ rank.Store) (float64, string, error) {
	if len(s.Weights) == 0 {
		// No owner configured, or an owner with no profile yet. Skip and
		// be named in meta.degraded[] rather than scoring a constant zero,
		// which would read as "we measured the owner's taste and it is
		// mediocre".
		return 0, "", rank.ErrSkip
	}
	if len(c.TagIDs) == 0 {
		// The candidate is not tag-shaped — an author or a collection.
		// There is nothing for a taste vector to score, which is an
		// absence of evidence about THIS candidate rather than a broken
		// dimension, so it scores 0 and the signal stays un-degraded.
		return 0, "", nil
	}

	maxTags := s.MaxTags
	if maxTags <= 0 {
		maxTags = defaultOwnerTasteMaxTags
	}
	ids := c.TagIDs
	if len(ids) > maxTags {
		ids = ids[:maxTags]
	}

	// liked and total are both needed: the score is the SHARE of the
	// candidate's taste mass that the owner likes, not the raw sum. A raw
	// sum would rank a work with sixty tags the owner mildly likes above a
	// work with three tags the owner loves, which is the wrong answer to
	// "would I want to read this".
	var liked, total float64
	type contribution struct {
		id     int32
		weight float64
	}
	top := make([]contribution, 0, 4)
	for _, id := range ids {
		w, ok := s.Weights[id]
		if !ok || math.IsNaN(w) {
			continue
		}
		total += math.Abs(w)
		if w > 0 {
			liked += w
		}
		top = append(top, contribution{id: id, weight: w})
	}
	if total == 0 {
		// No overlap at all is a legitimate 0, not a skip: the signal
		// worked, the answer is "the owner has no opinion about this
		// work". Saying so is what keeps the zero from being read as
		// silence.
		return 0, "owner cohort: no tag overlap", nil
	}

	value := liked / total
	if value > 1 {
		value = 1
	}

	// The two largest-|weight| contributors, heaviest first, with their
	// signs. Map iteration is random, so the slice is sorted rather than
	// taken in walk order — an evidence line that changes between identical
	// requests is indistinguishable from a bug.
	sort.SliceStable(top, func(i, j int) bool {
		return math.Abs(top[i].weight) > math.Abs(top[j].weight)
	})
	reason := "owner cohort:"
	for i, t := range top {
		if i == 2 {
			break
		}
		name := s.name(t.id)
		if name == "" {
			name = fmt.Sprintf("tag:%d", t.id)
		}
		if t.weight >= 0 {
			reason += fmt.Sprintf(" +%s", name)
		} else {
			reason += fmt.Sprintf(" -%s", name)
		}
	}
	return value, reason, nil
}

// name resolves a tag id for the evidence line.
//
// A miss is NOT a skip: the score does not depend on the name, and a
// signal that refused to score a work because it could not name a tag
// would drop the strongest candidates from the ranking for a cosmetic
// reason. The id is the fallback.
func (s OwnerTaste) name(id int32) string {
	if s.Graph == nil {
		return ""
	}
	return s.Graph.Name(id)
}
