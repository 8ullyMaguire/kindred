package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/authorcf"
	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
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

// The tag floor: a tag on one or two works cannot relate two authors, so a profile made of
// them has nothing to search.
//
// Found on the live mirror, not in a fixture: `Masterweaver` has 26 works and 196 distinct
// tags, 37 of them on a SINGLE work. Rarest-first kept exactly those 24, so the candidate
// query returned only the author's own works and `/authors/similar` answered an empty list
// for an author with 61 readers. Two of 25 sampled authors failed this way.
func TestSimilarAuthorsRefusesAProfileOfSingletonTags(t *testing.T) {
	e := newFixtureEngine(t)
	// Every freeform tag on two works, every fandom on all sixty: the fandoms have idf 0,
	// the freeforms are below the floor. Nothing survives it.
	e.DocFreq = &corpus.DocFreq{Total: 60, ByTag: map[int32]float64{
		1: 60, 2: 60, 3: 60, 4: 60,
		5: 2, 6: 2, 7: 2, 8: 2, 9: 2, 10: 2, 11: 2,
	}}
	e.AuthorMinDF = 0 // the default floor, which is the thing under test
	got, err := e.SimilarAuthors(context.Background(), "author1", 10)
	if err == nil {
		t.Fatalf("a profile of two-work tags returned %d authors and no error", len(got))
	}
	if !errors.Is(err, ErrAuthorProfileEmpty) {
		t.Fatalf("error is %v, want ErrAuthorProfileEmpty: the author exists, the neighbourhood does not", err)
	}
	if !strings.Contains(err.Error(), "fewer than 3 works") {
		t.Fatalf("the error does not name the floor: %v", err)
	}

	// With the freeforms on twenty works the profile is real again, so the same engine
	// answers authors: the floor is what changed, not the corpus.
	e.DocFreq = &corpus.DocFreq{Total: 60, ByTag: map[int32]float64{
		1: 60, 2: 60, 3: 60, 4: 60,
		5: 20, 6: 20, 7: 20, 8: 20, 9: 20, 10: 20, 11: 20,
	}}
	got, err = e.SimilarAuthors(context.Background(), "author1", 10)
	if err != nil {
		t.Fatalf("shared tags were refused: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no similar authors for a profile of tags on twenty works")
	}
}

// Without a document-frequency table there are no document frequencies, and a floor applied
// to a number nobody measured deletes every profile: a mirror that has never been ingested
// must still answer.
func TestSimilarAuthorsAppliesNoFloorWithoutATable(t *testing.T) {
	e := newFixtureEngine(t)
	e.DocFreq = nil
	e.AuthorMinDF = 0
	got, err := e.SimilarAuthors(context.Background(), "author1", 10)
	if err != nil {
		t.Fatalf("no table should mean no floor, not no answers: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no similar authors without a docfreq table")
	}
}

// The merge (#35): co-readers rank by the SHARE of the seed's readers who read a candidate,
// a tag-only author ranks after every co-read candidate, and a candidate on both arms keeps
// the tag cosine and both pieces of evidence.
//
// The ranking is a unit so the arithmetic is checkable without a corpus: the arms' own
// correctness is measured by `kindred author-eval` on a real mirror, which no fixture can
// stand in for.
func TestMergeAuthorsOrdersByReadShare(t *testing.T) {
	const seedReaders = 100 // readers of the seed author
	// The two arms must DISAGREE, or the test cannot see which one ordered the list. X has
	// five readers and a weak tag score; Y has a strong tag score and no reader evidence is
	// needed to place it -- so a comparator rewritten to sort by tag score puts Y first.
	// Z is on BOTH arms too, with the STRONGER tag score and the weaker reader count: only a
	// comparator that orders by readers can put X above Z, so the mutant has somewhere to
	// show. (The tag-only row is appended after the sort by design -- it never competes -- so
	// it cannot be the discriminating case.)
	tagged := []SimilarAuthor{
		{Name: "Y: tags only", Score: 0.9, SharedWorks: 3,
			Evidence: []rank.Evidence{{Signal: "author_tag_profile"}}},
		{Name: "X: both arms", Score: 0.1, SharedWorks: 1,
			Evidence: []rank.Evidence{{Signal: "author_tag_profile"}}},
		{Name: "Z: readers only", Score: 0.9, SharedWorks: 2,
			Evidence: []rank.Evidence{{Signal: "author_tag_profile"}}},
	}
	pairs := []authorcf.Pair{
		{Author: "X: both arms", Key: authorcf.Key("X: both arms"), Readers: 5},
		{Author: "Z: readers only", Key: authorcf.Key("Z: readers only"), Readers: 2},
	}
	got := (&Engine{}).mergeAuthors(tagged, pairs, seedReaders, "seed", 10)
	if len(got) != 3 {
		t.Fatalf("merged %d authors, want 3", len(got))
	}
	if got[0].Name != "X: both arms" {
		t.Fatalf("first is %q, want X: five readers rank above a stronger tag score, because the "+
			"measured question is what readers do", got[0].Name)
	}
	if got[1].Name != "Z: readers only" {
		t.Fatalf("second is %q, want Z: two readers, and a tag score that would put it first", got[1].Name)
	}
	if got[2].Name != "Y: tags only" {
		t.Fatalf("third is %q, want Y: a tag-only author ranks after everyone a reader placed", got[2].Name)
	}
	if got[1].Score != 0.9 {
		t.Fatalf("Z's tag score = %v, want 0.9 kept from the tag arm", got[1].Score)
	}
	if got[0].Score != 0.1 || got[0].SharedWorks != 1 {
		t.Fatalf("the both-arms row lost its tag numbers: score=%v works=%d", got[0].Score, got[0].SharedWorks)
	}
	if len(got[0].Evidence) != 2 {
		t.Fatalf("the both-arms row has %d evidence entries, want both arms named", len(got[0].Evidence))
	}
	if got[0].Evidence[0].Signal != "author_co_readers" {
		t.Fatalf("first evidence is %q, want author_co_readers first: it is what ranked the row",
			got[0].Evidence[0].Signal)
	}
	if got[0].ReadShare != 0.05 {
		t.Fatalf("read share = %v, want 5/100", got[0].ReadShare)
	}
}
