package rank

import "testing"

func mergedIDs(items []InterleavedItem) []int64 {
	out := make([]int64, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

func interleaveCands(ids ...int64) []Candidate {
	out := make([]Candidate, len(ids))
	for i, id := range ids {
		out[i] = Candidate{ID: id, Kind: "ao3_work", Title: "w"}
	}
	return out
}

func sameIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The merge must alternate, not concatenate. A merge that took from A until A
// ran dry would give A every slot, and every credit would then be A's -- a win
// rate that measures the merge instead of the algorithms.
func TestTeamDraftAlternates(t *testing.T) {
	a := interleaveCands(1, 2, 3)
	b := interleaveCands(4, 5, 6)
	got := TeamDraft(a, b, "one", "two", 6)
	if !sameIDs(mergedIDs(got), []int64{1, 4, 2, 5, 3, 6}) {
		t.Fatalf("merged order %v, want [1 4 2 5 3 6]", mergedIDs(got))
	}
	arms := []string{got[0].Arm, got[1].Arm, got[2].Arm, got[3].Arm, got[4].Arm, got[5].Arm}
	want := []string{"one", "two", "one", "two", "one", "two"}
	for i := range want {
		if arms[i] != want[i] {
			t.Fatalf("slot %d arm %q, want %q (arms %v)", i, arms[i], want[i], arms)
		}
	}
	if ex := ArmExposure(got); ex["one"] != 3 || ex["two"] != 3 {
		t.Fatalf("exposure %v, want one=3 two=3", ex)
	}
}

// Equal exposure is the property the standings divide by. It is asserted
// separately from the order because a different merge could alternate in
// order yet still starve an arm when the lists have different lengths.
func TestTeamDraftGivesBothArmsHalfTheSlots(t *testing.T) {
	a := interleaveCands(1, 2, 3, 4, 5, 6, 7, 8)
	b := interleaveCands(11, 12, 13, 14, 15, 16, 17, 18)
	got := TeamDraft(a, b, "a", "b", 10)
	if ex := ArmExposure(got); ex["a"] != 5 || ex["b"] != 5 {
		t.Fatalf("exposure %v, want a=5 b=5 for a 10-slot merge of two full lists", ex)
	}
}

// An item both arms ranked appears once, credited to the arm whose turn
// reached it. Duplicating it would let one work be credited twice and inflate
// whichever arm was asked second.
func TestTeamDraftDedupesAcrossArms(t *testing.T) {
	a := interleaveCands(1, 2, 3)
	b := interleaveCands(2, 4, 5)
	got := TeamDraft(a, b, "a", "b", 6)
	seen := map[int64]int{}
	for _, it := range got {
		seen[it.ID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("work %d appears %d times in the merge", id, n)
		}
	}
	// 2 is B's first item and A's second: A's turn comes first, but A
	// reaches 1 first, so B gets 2 on its turn. Credited to B, once.
	for _, it := range got {
		if it.ID == 2 && it.Arm != "b" {
			t.Fatalf("work 2 credited to %q, want b (the arm whose turn reached it)", it.Arm)
		}
	}
}

// Within-arm order survives. A merge that reorders an arm's list is ranking
// for it, and the arm would be judged on a list it did not produce.
func TestTeamDraftPreservesEachArmsOrder(t *testing.T) {
	a := interleaveCands(1, 2, 3, 4)
	b := interleaveCands(9, 8, 7, 6)
	got := TeamDraft(a, b, "a", "b", 8)
	lastA, lastB := int64(-1), int64(-1)
	for _, it := range got {
		switch it.Arm {
		case "a":
			if it.ID <= lastA {
				t.Fatalf("arm a's items out of order: %v", mergedIDs(got))
			}
			lastA = it.ID
		case "b":
			if lastB >= 0 && it.ID >= lastB {
				t.Fatalf("arm b's items out of order: %v", mergedIDs(got))
			}
			lastB = it.ID
		}
	}
	if lastA != 4 || lastB != 6 {
		t.Fatalf("did not reach the end of both arms: lastA=%d lastB=%d", lastA, lastB)
	}
}

// A dry arm does not waste slots. The list is still full -- and the surviving
// arm's items still come out in its own order.
func TestTeamDraftFillsFromTheOtherArm(t *testing.T) {
	a := interleaveCands(1)
	b := interleaveCands(4, 5, 6, 7)
	got := TeamDraft(a, b, "a", "b", 4)
	if len(got) != 4 {
		t.Fatalf("merged %d items, want 4", len(got))
	}
	if ex := ArmExposure(got); ex["a"] != 1 || ex["b"] != 3 {
		t.Fatalf("exposure %v, want a=1 b=3", ex)
	}
	if !sameIDs(mergedIDs(got), []int64{1, 4, 5, 6}) {
		t.Fatalf("merged order %v, want [1 4 5 6]", mergedIDs(got))
	}
}

// n larger than the union returns the union, not padding and not a panic.
func TestTeamDraftStopsWhenBothArmsAreDry(t *testing.T) {
	got := TeamDraft(interleaveCands(1, 2), cands(3), "a", "b", 10)
	if !sameIDs(mergedIDs(got), []int64{1, 3, 2}) {
		t.Fatalf("merged order %v, want [1 3 2]", mergedIDs(got))
	}
	if len(got) != 3 {
		t.Fatalf("merged %d items, want 3", len(got))
	}
}

// A malformed call is empty, not a panic: the API validates n and the lists,
// but a merge that panics on an empty input is a 500 waiting for the first
// query that ranks nothing.
func TestTeamDraftEmptyInputs(t *testing.T) {
	if got := TeamDraft(nil, nil, "a", "b", 10); got != nil {
		t.Fatalf("merged %v from nothing", got)
	}
	if got := TeamDraft(interleaveCands(1), cands(2), "a", "b", 0); got != nil {
		t.Fatalf("merged %v with n=0", got)
	}
}
