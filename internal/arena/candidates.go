// SPDX-License-Identifier: AGPL-3.0-only

package arena

import "context"

// EntityCandidate is one thing an arena can present for comparison.
//
// Weight is the evidence behind the candidate — how many works carry the tag,
// how often the byline appears — and it exists so a caller can rank or filter
// without asking the corpus again. It is not a preference: nothing here knows
// about the owner's taste.
type EntityCandidate struct {
	ID     int64
	Name   string
	Weight int
}

// EntityCandidateSource is the corpus side of an arena: the one thing needed to
// materialise a kind's candidates.
//
// It is an interface in this package, and implemented by `internal/corpus`,
// because the loaders are slow by nature — grouping the mirror's 3,890,504 tag
// rows measured 2.2s on 2026-10-14 — while an arena page must not be. The
// state store owns the materialised copy; this is how it is filled. Defining
// the interface at the consumer keeps `arena` free of a database dependency,
// which is why every other file here can be tested without one.
type EntityCandidateSource interface {
	EntityCandidates(ctx context.Context, kind EntityKind, limit int) ([]EntityCandidate, error)
}
