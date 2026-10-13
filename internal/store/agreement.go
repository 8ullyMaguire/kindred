// Copyright 2026 Polarisocial. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package store

import (
	"context"
	"fmt"
	"math"
	"time"
)

// agreementCacheTTL is how long we cache a voter's agreement with the owner.
const agreementCacheTTL = 10 * time.Minute

// minEvidence is the minimum total evidence (sum of n) a voter must have
// before their taste is trusted to influence the global signal.
const minEvidence = 5

// agreementCacheEntry holds a cached agreement value and the time it was built.
type agreementCacheEntry struct {
	value float64
	built time.Time
}

// Store extends the agreement engine.
// Note: Store is defined in store.go with the added fields:
//   agreementCacheMu sync.Mutex
//   agreementCache map[string]agreementCacheEntry
//   ownerVector map[int64]float64
// We are adding methods here.

// BuildOwnerVector blends the owner's imported profile tags and the owner's
// learned arena tag weights into one vector, each source L2-normalised and
// contributing half. Both sources always contribute, so a cold arena cannot
// hollow the vector out and a thin profile cannot be drowned by it.
//
// profileTags: map from tag int32 to weight from the owner's profile.
// learned: map from tag int64 to learned weight from arena_user_tag_weights.
// Returns map[int64]float64 representing the blended owner vector.
func (s *Store) BuildOwnerVector(profileTags map[int32]float64, learned map[int64]float64) map[int64]float64 {
	// Normalise profileTags to unit L2 norm.
	var profileNormSq float64
	for _, w := range profileTags {
		profileNormSq += w * w
	}
	var profileNorm float64
	if profileNormSq > 0 {
		profileNorm = math.Sqrt(profileNormSq)
	}

	// Normalise learned weights to unit L2 norm.
	var learnedNormSq float64
	for _, w := range learned {
		learnedNormSq += w * w
	}
	var learnedNorm float64
	if learnedNormSq > 0 {
		learnedNorm = math.Sqrt(learnedNormSq)
	}

	// Blend: out[t] = 0.5*unitProfile[t] + 0.5*unitLearned[t] over the union.
	out := make(map[int64]float64)
	// First, add profile contribution.
	if profileNorm > 0 {
		for tagID32, w := range profileTags {
			tagID := int64(tagID32)
			out[tagID] += 0.5 * (w / profileNorm)
		}
	}
	// Then, add learned contribution.
	if learnedNorm > 0 {
		for tagID, w := range learned {
			out[tagID] += 0.5 * (w / learnedNorm)
		}
	}
	return out
}

// Agreement returns the cosine between two tag-weight vectors. Disjoint vectors
// score 0; either side empty scores 0. It never returns NaN.
func Agreement(a, b map[int64]float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for k, av := range a {
		if bv, has := b[k]; has {
			dot += av * bv
		}
		normA += av * av
	}
	for _, bv := range b {
		normB += bv * bv
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// AgreementFor returns the voter's taste agreement with the given owner
// vector, from the voter's arena_user_tag_weights rows. The owner's own key
// short-circuits to exactly 1.0 without a query.
//
// A voter with fewer than minEvidence total observations (n summed) has no
// taste worth trusting yet and scores 0 — the conservative default: the
// unproven voter does not move the global signal.
func (s *Store) AgreementFor(ctx context.Context, voterKey string, ownerVector map[int64]float64) (float64, error) {
	// Check cache.
	s.agreementCacheMu.Lock()
	// Initialise the cache here rather than in the constructors, and this is a
	// correctness choice rather than a style one. `Store` has three ways to come
	// into existence -- `Open`, `OpenMemory`, and the zero value a test may
	// build directly -- and a map made in each is a map made in whichever one the
	// next author remembers. The zero `Store` was reachable and panicked:
	// reading a nil map is fine, so the miss path looked healthy, and the first
	// WRITE below ("assignment to entry in nil map") took down every judgement
	// with a 500. `judge` is not a path that may panic, so the initialisation
	// lives at the single site every read and write already goes through.
	if s.agreementCache == nil {
		s.agreementCache = make(map[string]agreementCacheEntry)
	}
	if ent, ok := s.agreementCache[voterKey]; ok && time.Since(ent.built) < agreementCacheTTL {
		s.agreementCacheMu.Unlock()
		return ent.value, nil
	}
	s.agreementCacheMu.Unlock()

	// Load voter's weights.
	vRows, err := s.DB.QueryContext(ctx,
		`SELECT tag_id, weight, n FROM arena_user_tag_weights WHERE owner_key = ?`, voterKey)
	if err != nil {
		return 0, fmt.Errorf("agreement query: %w", err)
	}
	defer vRows.Close()

	voterWeights := map[int64]float64{}
	voterNormSq := 0.0
	totalEvidence := 0
	for vRows.Next() {
		var tagID int64
		var weight float64
		var n int
		if err := vRows.Scan(&tagID, &weight, &n); err != nil {
			return 0, fmt.Errorf("agreement scan: %w", err)
		}
		// Apply learned weight with evidence dampening: weight / sqrt(n)
		if n > 0 {
			applied := weight / math.Sqrt(float64(n))
			voterWeights[tagID] = applied
			voterNormSq += applied * applied
		}
		totalEvidence += n
	}
	if err := vRows.Err(); err != nil {
		return 0, fmt.Errorf("agreement rows: %w", err)
	}

	// If the voter has insufficient evidence, agreement is 0.
	if totalEvidence < minEvidence {
		s.agreementCacheMu.Lock()
		s.agreementCache[voterKey] = agreementCacheEntry{value: 0, built: time.Now()}
		s.agreementCacheMu.Unlock()
		return 0, nil
	}

	voterNorm := math.Sqrt(voterNormSq)

	// If ownerVector is nil or empty, agreement is 0.
	if len(ownerVector) == 0 {
		s.agreementCacheMu.Lock()
		s.agreementCache[voterKey] = agreementCacheEntry{value: 0, built: time.Now()}
		s.agreementCacheMu.Unlock()
		return 0, nil
	}

	// Compute cosine.
	var dot, normB float64
	for k, vw := range voterWeights {
		if ow, has := ownerVector[k]; has {
			dot += vw * ow
			normB += ow * ow
		}
	}
	if voterNorm == 0 || normB == 0 {
		s.agreementCacheMu.Lock()
		s.agreementCache[voterKey] = agreementCacheEntry{value: 0, built: time.Now()}
		s.agreementCacheMu.Unlock()
		return 0, nil
	}
	cosine := dot / (voterNorm * math.Sqrt(normB))

	// Cache the result.
	s.agreementCacheMu.Lock()
	s.agreementCache[voterKey] = agreementCacheEntry{value: cosine, built: time.Now()}
	s.agreementCacheMu.Unlock()

	return cosine, nil
}

// FlushAgreementCache clears the agreement cache. Used when the owner vector
// changes.
func (s *Store) FlushAgreementCache() {
	s.agreementCacheMu.Lock()
	s.agreementCache = make(map[string]agreementCacheEntry)
	s.agreementCacheMu.Unlock()
}
