package engine

import (
	"context"
	"strings"
	"testing"
)

// Every fixture work's byline is author1..author5, and their tag profiles
// overlap through the rotating freeform tags. SimilarAuthors must return OTHER
// authors, with evidence naming the shared tags, and must not treat the seed
// as its own neighbour.
func TestSimilarAuthorsReturnsOtherAuthorsWithEvidence(t *testing.T) {
	e := newFixtureEngine(t)
	got, err := e.SimilarAuthors(context.Background(), "author1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no similar authors for a fixture where every author shares tags")
	}
	for i, a := range got {
		if strings.EqualFold(a.Name, "author1") {
			t.Fatalf("the seed author is its own neighbour: %+v", a)
		}
		if len(a.Evidence) == 0 {
			t.Fatalf("%q has no evidence; an unexplained author recommendation cannot be argued with", a.Name)
		}
		if a.Evidence[0].Signal != "author_tag_profile" {
			t.Fatalf("%q evidence signal = %q", a.Name, a.Evidence[0].Signal)
		}
		if a.Score <= 0 || a.SharedWorks <= 0 {
			t.Fatalf("%q score=%v works=%d, want both positive", a.Name, a.Score, a.SharedWorks)
		}
		if i > 0 && got[i-1].Score < a.Score {
			t.Fatalf("not sorted by score: %v before %v", got[i-1].Score, a.Score)
		}
	}
}

func TestSimilarAuthorsUnknownNameIsNotFound(t *testing.T) {
	e := newFixtureEngine(t)
	_, err := e.SimilarAuthors(context.Background(), "nobody-at-all", 5)
	if err == nil {
		t.Fatal("an unknown author returned a result")
	}
	if !strings.Contains(err.Error(), "no work in this mirror") {
		t.Fatalf("error = %q, want it to name the missing byline", err)
	}
}

func TestSimilarAuthorsLimitIsHonoured(t *testing.T) {
	e := newFixtureEngine(t)
	got, err := e.SimilarAuthors(context.Background(), "author1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 2 {
		t.Fatalf("got %d authors, want at most 2", len(got))
	}
}
