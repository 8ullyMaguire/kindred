package engine

// The walk dimension, at the level where "registered but never runs" would be
// invisible: the signal list, the tune weight, and whether any item actually
// carries a walk component.
//
// This repo has shipped a registered-but-unrunnable signal once (embedding, at
// weight 0) and left it silent for a session. A unit test of WalkMass cannot see
// that; these can.

import (
	"context"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/graph"
)

func TestTheWalkIsRegisteredWeightedAndActuallyScores(t *testing.T) {
	e := newFixtureEngine(t)
	// The 60-work fixture's own graph is too sparse for a walk to reach a tag
	// the seeds do not have: every work in it shares the same few tags, so the
	// non-seed mass is empty and the signal correctly skips. Replace it with a
	// densely connected graph over the same tag ids, which is the shape a real
	// corpus has and the shape this test needs to see the signal run.
	e.Graph = denselyConnected(e)

	res, err := e.Recommend(context.Background(), Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}}, N: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range res.Meta.Degraded {
		if name == "walk" {
			t.Fatalf("walk degraded on a fixture WITH a co-occurrence graph: %v", res.Meta.Degraded)
		}
	}
	var scored int
	for _, it := range res.Items {
		for _, ev := range it.Evidence {
			if ev.Signal == "walk" && ev.Value > 0 {
				scored++
			}
		}
	}
	if scored == 0 {
		t.Fatal("no item carries a walk component on a graph-backed fixture: the " +
			"signal is registered and inert, which is the bug this project has " +
			"shipped before")
	}
}

// denselyConnected builds a graph in which every tag co-occurs with every other,
// so a walk from any seed reaches every non-seed tag. It is deliberately not a
// realistic graph: it is the smallest one that makes the walk's mass non-empty,
// which is what "did the signal run" needs.
func denselyConnected(e *Engine) *graph.CSR {
	ctx := context.Background()
	var maxTag int64
	// Tags live in the CORPUS database, not in the state DB: a graph built from
	// the state handle queries no rows and comes back nil, which reads as "no
	// graph" rather than as the wrong handle.
	row := e.Store.Corpus.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM tags`)
	if err := row.Scan(&maxTag); err != nil {
		return nil
	}
	n := int(maxTag) + 1
	if n < 2 || n > 5000 {
		return nil
	}
	var edges []graph.Edge
	for a := int32(1); a < int32(n); a++ {
		for b := a + 1; b < int32(n); b++ {
			edges = append(edges, graph.Edge{A: a, B: b, Count: 1})
		}
	}
	return graph.Build(n, edges, nil, 0)
}

// With no graph the signal must still be registered, must skip, and must name
// itself -- otherwise "this mirror has no index" and "the walk is broken" are the
// same page.
func TestTheWalkNamesItselfWhenThereIsNoGraph(t *testing.T) {
	e := newFixtureEngine(t)
	e.Graph = nil
	res, err := e.Recommend(context.Background(), Request{
		Kind: "ao3_work", Seeds: []Seed{{Kind: "ao3_work", ID: 1}}, N: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	var named bool
	for _, name := range res.Meta.Degraded {
		if name == "walk" {
			named = true
		}
	}
	if !named {
		t.Fatalf("walk absent from meta.degraded without a graph: %v", res.Meta.Degraded)
	}
	for _, it := range res.Items {
		for _, ev := range it.Evidence {
			if ev.Signal == "walk" {
				t.Fatalf("walk scored %v with no graph: a zero would be a claim", ev.Value)
			}
		}
	}
}
