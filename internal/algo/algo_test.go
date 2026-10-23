package algo

// The registry's claims, tested.
//
// A registry like this is mostly prose, and prose does not fail a build. These
// tests pin the four properties that would make the page lie:
//
//   - every kind has exactly one default, and it is live;
//   - every disabled entry names a reason (an excuse-free omission is the thing
//     the package doc forbids);
//   - a disabled id never resolves as if it could run;
//   - the arm split is stable per reader and does not drift with input order.

import (
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
)

func TestEveryKindHasExactlyOneLiveDefault(t *testing.T) {
	for _, kind := range arena.Kinds {
		var defaults []ID
		for _, a := range For(kind) {
			if a.Default {
				defaults = append(defaults, a.ID)
				if a.Status != Live {
					t.Errorf("%s: the default %s is not live", kind, a.ID)
				}
			}
		}
		if len(defaults) != 1 {
			t.Fatalf("%s has %d defaults (%v), want exactly 1: a page offering a "+
				"choice must be able to say which one it ships", kind, len(defaults), defaults)
		}
		if DefaultFor(kind) != defaults[0] {
			t.Fatalf("%s: DefaultFor disagrees with the registry", kind)
		}
	}
}

func TestEveryDisabledAlgorithmSaysWhy(t *testing.T) {
	for _, kind := range arena.Kinds {
		for _, a := range For(kind) {
			if a.Status == Disabled && strings.TrimSpace(a.Reason) == "" {
				t.Errorf("%s: %s is disabled with no reason; a page that lists it "+
					"must be able to answer why it cannot be chosen", kind, a.ID)
			}
			if a.Status == Live && strings.TrimSpace(a.Reason) != "" {
				t.Errorf("%s: %s is live but carries a reason (%q); a live algorithm "+
					"with an excuse is a contradiction", kind, a.ID, a.Reason)
			}
			if a.Label == "" || a.Blurb == "" {
				t.Errorf("%s: %s has no label or blurb", kind, a.ID)
			}
			if a.Status == Live && strings.TrimSpace(a.Cost) == "" {
				t.Errorf("%s: %s is live and does not say what a request costs", kind, a.ID)
			}
		}
	}
}

// TestADisabledAlgorithmIsNotSelectable: asking for one must fall back to the
// default and SAY why. A silent fallback would let a reader believe they had
// chosen a graph neural network.
func TestADisabledAlgorithmIsNotSelectable(t *testing.T) {
	kind := arena.KindWork
	id, why := Parse(kind, string(LightGCN))
	if id != DefaultFor(kind) {
		t.Fatalf("asking for %s returned %s, want the default %s", LightGCN, id, DefaultFor(kind))
	}
	if !strings.Contains(why, "not available") {
		t.Fatalf("the refusal does not say it is unavailable: %q", why)
	}
	if !strings.Contains(why, "training pipeline") {
		t.Fatalf("the refusal does not name the reason the registry gave: %q", why)
	}
	if _, live := Lookup(kind, LightGCN); live {
		t.Fatal("Lookup says a disabled algorithm can serve this kind")
	}
}

func TestEveryLiveAlgorithmCanBeSelected(t *testing.T) {
	for _, kind := range arena.Kinds {
		for _, id := range LiveIDs(kind) {
			got, why := Parse(kind, string(id))
			if got != id || why != "" {
				t.Errorf("%s: selecting live %s gave %s (%q)", kind, id, got, why)
			}
		}
	}
}

// TestAnUnknownAlgorithmSaysItIsUnknown: "no algorithm called x" and "x cannot
// run here" are different answers, and a reader who typed a name needs the
// first.
func TestAnUnknownAlgorithmSaysItIsUnknown(t *testing.T) {
	id, why := Parse(arena.KindWork, "quantum_vibes")
	if id != DefaultFor(arena.KindWork) {
		t.Fatalf("got %s, want the default", id)
	}
	if !strings.Contains(why, "no algorithm called") {
		t.Fatalf("an unknown name is answered as %q", why)
	}
}

// TestTheArmIsStablePerReader: the same reader must land in the same arm on
// every call, and the two arms must both be reachable — a hash that always
// returns the default is an experiment that never runs.
func TestTheArmIsStablePerReader(t *testing.T) {
	kind := arena.KindWork
	alt := Challenger(kind)
	if alt == "" || alt == DefaultFor(kind) {
		t.Fatalf("no challenger for %s: an A/B with one arm is not an experiment", kind)
	}
	var def, other int
	for _, owner := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		first := ArmFor(kind, owner)
		for i := 0; i < 20; i++ {
			if got := ArmFor(kind, owner); got != first {
				t.Fatalf("%s moved from %s to %s between calls", owner, first, got)
			}
		}
		switch first {
		case DefaultFor(kind):
			def++
		case alt:
			other++
		default:
			t.Fatalf("%s was assigned %s, which is neither arm", owner, first)
		}
	}
	if def == 0 || other == 0 {
		t.Fatalf("eight readers split %d default / %d challenger: one arm is "+
			"unreachable, so the experiment cannot conclude anything", def, other)
	}
}

// TestAnOwnerlessRequestGetsTheDefault: there is no stable arm for a reader with
// no session, so assigning one would be a coin flip recorded as data.
func TestAnOwnerlessRequestGetsTheDefault(t *testing.T) {
	for _, kind := range arena.Kinds {
		if got := ArmFor(kind, ""); got != DefaultFor(kind) {
			t.Fatalf("%s: an ownerless request got %s, want the default", kind, got)
		}
	}
}

// TestAnExplicitRequestBeatsTheArm.
func TestAnExplicitRequestBeatsTheArm(t *testing.T) {
	kind := arena.KindWork
	alt := Challenger(kind)
	id, why := Resolve(kind, string(alt), "", "owner-with-a-default-arm")
	if id != alt || why != "" {
		t.Fatalf("an explicit request for %s resolved to %s (%q)", alt, id, why)
	}
}

// TestAStoredChoiceBeatsTheArm, and a stored choice that is no longer live does
// not pin the reader out of the experiment.
func TestAStoredChoiceBeatsTheArm(t *testing.T) {
	kind := arena.KindWork
	alt := Challenger(kind)
	if id, why := Resolve(kind, "", string(alt), "anything"); id != alt || why != "" {
		t.Fatalf("a stored choice of %s resolved to %s (%q)", alt, id, why)
	}
	retired := string(LightGCN)
	if id, _ := Resolve(kind, "", retired, "x"); id == LightGCN {
		t.Fatal("a retired algorithm was still selected from storage")
	}
}

// TestTheTagishKindsShareTheTagAlgorithms: the corpus files characters, ships
// and fandoms under `freeforms`, so a separate mechanism for them would be the
// same query behind a filter that matches nothing.
func TestTheTagishKindsShareTheTagAlgorithms(t *testing.T) {
	want := LiveIDs(arena.KindTag)
	for _, kind := range []arena.EntityKind{arena.KindCharacter, arena.KindRelationship, arena.KindFandom} {
		got := LiveIDs(kind)
		if len(got) != len(want) {
			t.Fatalf("%s offers %d algorithms, the tag kind offers %d", kind, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s offers %s where tags offer %s", kind, got[i], want[i])
			}
		}
	}
}
