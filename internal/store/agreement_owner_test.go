package store

// The agreement feature shipped inert, and these tests are about the three
// ways it was inert rather than about the cosine, which was always correct:
//
//  1. `ownerVector` had no writer, so every judgement recorded a constant 0
//     into a column nothing read. `SetOwnerVector` is the writer.
//  2. The owner's own votes were documented as short-circuiting to 1.0, and
//     no such branch existed anywhere. `AgreementWithOwner` is that branch.
//  3. The value AGREEMENT IS becomes the influence weight the batch applies
//     to a comparison. Recording 0 for a deployment with no owner would
//     therefore have multiplied every vote in the estate by zero the moment
//     the weighting step landed -- silently, because agreement had always
//     been 0 and nothing had ever read it. The first test below is that
//     guard, and it is the one that would have cost the most to learn later.

import (
	"context"
	"math"
	"testing"
)

// evidenceOn teaches a voter `n` observations about one tag, through the
// real upsert path rather than by writing the row, so the evidence count the
// agreement floor reads is produced the way production produces it.
func evidenceOn(t *testing.T, s *Store, ownerKey string, tagID int64, delta float64, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if err := s.UpsertTagWeight(ctx, ownerKey, tagID, delta); err != nil {
			t.Fatalf("seed weight for %s on tag %d: %v", ownerKey, tagID, err)
		}
	}
}

// The regression guard: an unconfigured deployment must keep full influence.
func TestAnUnconfiguredDeploymentGivesEveryVoteFullInfluence(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Nothing is installed. This is the zero-value Store's state, the state
	// Open and OpenMemory leave behind, and the state of every deployment
	// that never set KINDRED_OWNER_SESSION.
	got, err := s.AgreementWithOwner(ctx, "some-voter")
	if err != nil {
		t.Fatalf("AgreementWithOwner with no owner: %v", err)
	}
	if got != 1.0 {
		t.Fatalf("agreement with no owner configured = %v; want 1.0 (full influence, the pre-feature behaviour)", got)
	}

	// And explicitly configured-off is the same answer as never configured,
	// because they are the same fact: there is no owner to disagree with.
	s.SetOwnerVector("", nil)
	got, err = s.AgreementWithOwner(ctx, "some-voter")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1.0 {
		t.Fatalf("agreement with an explicitly empty owner key = %v; want 1.0", got)
	}
}

// The owner's own comparison is 1.0 by definition, and the test proves it is
// the SHORT-CIRCUIT rather than a computation: the owner has no learned
// weights at all, so the computed path would answer 0 (no evidence) or 0
// again (empty voter vector). Only the short-circuit can produce 1.0.
func TestTheOwnerAgreesWithThemselfWithoutAQuery(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.SetOwnerVector("owner-key", map[int64]float64{1: 1, 2: 0.5})

	got, err := s.AgreementWithOwner(ctx, "owner-key")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1.0 {
		t.Fatalf("the owner's agreement with themself = %v; want exactly 1.0", got)
	}

	// A different voter with no evidence is NOT the owner, and must not get
	// the owner's answer: that is the boundary this test is really about.
	other, err := s.AgreementWithOwner(ctx, "someone-else")
	if err != nil {
		t.Fatal(err)
	}
	if other == 1.0 {
		t.Fatal("a voter with no evidence scored 1.0; the owner short-circuit is matching everyone")
	}
}

// An owner is configured but has no taste yet: no profile, no learned
// weights. 0 is the conservative answer, and it is deliberately NOT the same
// as the unconfigured case.
func TestAConfiguredOwnerWithNoTasteScoresZero(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.SetOwnerVector("owner-key", nil)
	got, err := s.AgreementWithOwner(ctx, "voter")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("agreement against a configured owner with no vector = %v; want 0", got)
	}
}

// The measurement itself: parallel vectors score 1, disjoint vectors score 0,
// and the evidence floor still holds.
func TestAgreementMeasuresTheVoterAgainstTheOwnerVector(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.SetOwnerVector("owner-key", map[int64]float64{1: 1})

	// Same tag as the owner, with enough evidence to clear the floor.
	evidenceOn(t, s, "aligned", 1, 1.0, minEvidence)
	aligned, err := s.AgreementWithOwner(ctx, "aligned")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(aligned-1.0) > 1e-9 {
		t.Fatalf("agreement of a voter who only taught the owner's tag = %v; want 1.0", aligned)
	}

	// A different tag entirely: the owner has no weight for it, so the
	// cosine is 0 rather than a small positive number.
	evidenceOn(t, s, "disjoint", 2, 1.0, minEvidence)
	disjoint, err := s.AgreementWithOwner(ctx, "disjoint")
	if err != nil {
		t.Fatal(err)
	}
	if disjoint != 0 {
		t.Fatalf("agreement of a voter sharing no tag with the owner = %v; want 0", disjoint)
	}

	// Below the evidence floor: the unproven voter does not move the global
	// signal, whatever their vector correlates with.
	evidenceOn(t, s, "unproven", 1, 1.0, minEvidence-1)
	unproven, err := s.AgreementWithOwner(ctx, "unproven")
	if err != nil {
		t.Fatal(err)
	}
	if unproven != 0 {
		t.Fatalf("agreement of a voter with %d observations = %v; want 0 (below the floor)", minEvidence-1, unproven)
	}
}

// Replacing the owner vector must invalidate every cached answer.
//
// The cache is keyed by voter ALONE, so a vector swap without a flush leaves
// cached agreements answering a question about the previous vector — and the
// answer looks entirely plausible, which is what makes it dangerous. This
// test fails if `SetOwnerVector` merely assigns.
func TestSetOwnerVectorFlushesTheAgreementCache(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.SetOwnerVector("owner-key", map[int64]float64{1: 1})
	evidenceOn(t, s, "voter", 1, 1.0, minEvidence)

	first, err := s.AgreementWithOwner(ctx, "voter")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(first-1.0) > 1e-9 {
		t.Fatalf("first agreement = %v; want 1.0", first)
	}

	// The owner's taste changes to a tag the voter knows nothing about.
	s.SetOwnerVector("owner-key", map[int64]float64{2: 1})

	second, err := s.AgreementWithOwner(ctx, "voter")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("agreement after the owner vector changed = %v, unchanged from the cached %v: SetOwnerVector did not flush", second, first)
	}
	if second != 0 {
		t.Fatalf("agreement after the owner vector changed = %v; want 0", second)
	}
}
