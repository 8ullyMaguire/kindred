package engine

// The per-kind related surface.
//
// Each mechanism is tested against the one data shape it is FOR, because every
// one of them can be written so that it returns a plausible list for the wrong
// reason:
//
//   - co-occurrence is tested with two tags that carry DIFFERENT numbers of
//     works, so a raw co-count and a Jaccard normalisation disagree;
//   - the popularity baseline is tested with its own ordering, so it cannot be
//     mistaken for a personalised list;
//   - an author with no readers is tested for the missing dimension rather than
//     for an empty list, and a refused algorithm is tested to be refused BY
//     NAME.

import (
	"context"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/algo"
	"git.polarisocial.xyz/kindred/kindred/internal/arena"
	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

// relatedCorpus: a seed tag carrying 3 works; "wide" filed on all 3 too and on
// 30 other works (so it co-occurs often but is everywhere); "narrow" filed on 2
// of the same 3 works and nothing else (so it co-occurs less but is specific).
//
// Jaccard must rank narrow above wide. A raw co-occurrence count ranks wide
// first, which is the ranking that makes "related tags" a list of the most
// popular tags in the corpus.
func relatedCorpus(t *testing.T) *Engine {
	t.Helper()
	c := testcorpus.New(40)
	const seedTag, wideTag, narrowTag = 90, 91, 92
	c.Tags = append(c.Tags,
		testcorpus.Tag{ID: seedTag, Name: "zz slow burn"},
		testcorpus.Tag{ID: wideTag, Name: "zz fluff"},
		testcorpus.Tag{ID: narrowTag, Name: "zz time travel fix-it"},
	)
	for i := 0; i < 3; i++ {
		c.WorkTags = append(c.WorkTags, testcorpus.WorkTag{
			WorkID: int64(1 + i), TagID: seedTag, TagType: "freeforms",
		})
	}
	for i := 0; i < 3; i++ {
		c.WorkTags = append(c.WorkTags, testcorpus.WorkTag{
			WorkID: int64(1 + i), TagID: wideTag, TagType: "freeforms",
		})
	}
	for i := 0; i < 2; i++ {
		c.WorkTags = append(c.WorkTags, testcorpus.WorkTag{
			WorkID: int64(1 + i), TagID: narrowTag, TagType: "freeforms",
		})
	}
	// wide's extra company: it is filed on many works the seed has nothing to do
	// with, which is exactly what the normalisation has to discount.
	for i := 0; i < 30; i++ {
		c.WorkTags = append(c.WorkTags, testcorpus.WorkTag{
			WorkID: int64(10 + i), TagID: wideTag, TagType: "freeforms",
		})
	}
	return newFixtureEngineFrom(t, c)
}

func TestTagCooccurrencePrefersTheSpecificTag(t *testing.T) {
	e := relatedCorpus(t)
	res, err := e.Related(context.Background(), arena.KindTag, 90, algo.Cooccur, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) < 2 {
		t.Fatalf("got %d neighbours, want at least 2 — the fixture files two other "+
			"tags on the seed's works", len(res.Items))
	}
	if res.Items[0].ID != 92 {
		t.Fatalf("top neighbour is tag %d (%q), want 92 (the specific tag): a raw "+
			"co-occurrence count would put the ubiquitous tag first, which is the "+
			"ranking this normalisation exists to prevent", res.Items[0].ID, res.Items[0].Label)
	}
	for _, it := range res.Items {
		if it.Score <= 0 || it.Score > 1 {
			t.Fatalf("tag %d scored %.3f: Jaccard lives in (0,1] and a score outside "+
				"it cannot be compared across tags", it.ID, it.Score)
		}
		if !strings.Contains(it.Why, "Jaccard") {
			t.Fatalf("row %d does not say how it was scored: %q", it.ID, it.Why)
		}
	}
}

// TestTagPopularityIsNotPersonalised: the baseline must be clearly the baseline,
// or a reader will take it for a recommendation.
func TestTagPopularityIsNotPersonalised(t *testing.T) {
	e := relatedCorpus(t)
	res, err := e.Related(context.Background(), arena.KindTag, 90, algo.Popular, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) == 0 {
		t.Fatal("the baseline returned nothing")
	}
	if res.Items[0].ID != 91 {
		t.Fatalf("the baseline's first row is tag %d, want 91: popularity must rank "+
			"by how many works carry the tag, and `fluff` is on 33 of them", res.Items[0].ID)
	}
	if !strings.Contains(res.Items[0].Why, "baseline") {
		t.Fatalf("the baseline does not call itself one: %q", res.Items[0].Why)
	}
}

// TestAuthorsWithoutReadersNameTheMissingDimension: an empty list is ambiguous
// between "nothing similar" and "this dimension could not run", and the second
// is the one a reader needs named.
func TestAuthorsWithoutReadersNameTheMissingDimension(t *testing.T) {
	c := testcorpus.New(20)
	e := newFixtureEngineFrom(t, c)
	// Author id derived from the byline the fixture gives its works.
	byline := ""
	_ = e.Corpus.DB.QueryRow(`SELECT authors FROM works WHERE authors <> '' LIMIT 1`).Scan(&byline)
	if byline == "" {
		t.Skip("the fixture has no bylines to seed an author walk")
	}
	res, err := e.Related(context.Background(), arena.KindAuthor, arena.AuthorEntityID(byline), algo.CoBookAuthors, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 {
		t.Fatalf("a fixture with no read events produced %d authors", len(res.Items))
	}
	if len(res.Missing) == 0 {
		t.Fatal("an empty author list names no missing dimension, so it reads as " +
			"\"no author is similar\" rather than \"there are no readers to walk\"")
	}
	if res.Note == "" {
		t.Fatal("no note explains the empty author list")
	}
}

// TestARefusedAlgorithmIsRefusedByName, at the engine boundary.
func TestARefusedAlgorithmIsRefusedByName(t *testing.T) {
	e := relatedCorpus(t)
	_, err := e.Related(context.Background(), arena.KindTag, 90, algo.PageRank, 5)
	if err == nil {
		t.Fatal("ppr ran for tags; it is disabled and must be refused")
	}
	if !strings.Contains(err.Error(), "cannot run") {
		t.Fatalf("the refusal does not say the algorithm cannot run here: %v", err)
	}
}

// TestEveryLiveAlgorithmAnswersForItsKinds: a registry entry with no
// implementation is a page that offers a choice and then fails. This walks the
// registry and calls every live algorithm for every kind, so adding one without
// an implementation fails here rather than in production.
func TestEveryLiveAlgorithmAnswersForItsKinds(t *testing.T) {
	e := relatedCorpus(t)
	ctx := context.Background()
	for _, kind := range arena.Kinds {
		for _, id := range algo.LiveIDs(kind) {
			var seed int64 = 1
			if kind == arena.KindTag || kind == arena.KindCharacter ||
				kind == arena.KindRelationship || kind == arena.KindFandom {
				seed = 90
			} else if kind == arena.KindAuthor {
				byline := ""
				_ = e.Corpus.DB.QueryRow(`SELECT authors FROM works WHERE authors <> '' LIMIT 1`).Scan(&byline)
				if byline == "" {
					continue
				}
				seed = arena.AuthorEntityID(byline)
			}
			res, err := e.Related(ctx, kind, seed, id, 3)
			if err != nil {
				t.Fatalf("%s/%s: %v", kind, id, err)
			}
			if res.Algo != string(id) {
				t.Fatalf("%s/%s answered as %q", kind, id, res.Algo)
			}
		}
	}
}
