package signal

// summary_style (#52): the only signal that reads prose.
//
// The properties worth pinning are the ones that separate a real text signal from a
// plausible-looking number: a rare shared word outranks a common one, a candidate with no
// summary is a SKIP rather than a zero, and a mirror with no term table says so instead of
// quietly contributing nothing.

import (
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

func testSummaryTable() *corpus.SummaryDocFreq {
	// 1000 documents: "dragon" on 4 (very rare), "castle" on 50, "the" on all 1000 (and it
	// would be dropped by the band anyway -- absent from the map means weight 0).
	return &corpus.SummaryDocFreq{
		Total: 1000,
		ByHash: map[uint64]float64{
			corpus.SummaryHash("dragon"): 4,
			corpus.SummaryHash("hoard"):  4,
			corpus.SummaryHash("castle"): 50,
			corpus.SummaryHash("sorrow"): 200,
			// Filler with real weights, so a candidate can be padded with MEASURED terms that
			// the seed does not carry. Without these, "diluted" summaries have no weight to
			// dilute with and the candidate-side norm is untestable.
			corpus.SummaryHash("spaceship"): 100,
			corpus.SummaryHash("quantum"):   100,
			corpus.SummaryHash("lantern"):   100,
			corpus.SummaryHash("meadow"):    100,
			corpus.SummaryHash("silence"):   100,
		},
	}
}

func TestSummaryStyleRareSharedWordOutranksCommon(t *testing.T) {
	s := SummaryStyle{DocFreq: testSummaryTable(), MaxSeeds: 3}
	seeds := []rank.Candidate{{Summary: "A dragon guards a castle"}}

	rare, reason, err := s.Score(rank.Candidate{Summary: "the dragon returns"}, seeds, nil)
	if err != nil {
		t.Fatalf("rare: %v", err)
	}
	common, _, err := s.Score(rank.Candidate{Summary: "the castle falls"}, seeds, nil)
	if err != nil {
		t.Fatalf("common: %v", err)
	}
	if rare <= common {
		t.Fatalf("a work sharing the RARE word scored %v, one sharing the COMMON word %v: "+
			"the weight is supposed to be the rarity", rare, common)
	}
	if !strings.Contains(reason, "dragon") {
		t.Fatalf("the evidence does not name the word that did the work: %q", reason)
	}
}

func TestSummaryStyleSkipsWithoutASummaryOrATable(t *testing.T) {
	seeds := []rank.Candidate{{Summary: "A dragon guards a castle"}}

	if _, _, err := (SummaryStyle{DocFreq: testSummaryTable()}).Score(
		rank.Candidate{Summary: "   "}, seeds, nil); !rank.IsSkip(err) {
		t.Fatalf("a candidate with no summary returned %v, want a skip", err)
	}
	// No table: SKIP, not 0. A zero is a claim that the works read differently; a skip says
	// this dimension did not run, and the response names it.
	if _, _, err := (SummaryStyle{}).Score(
		rank.Candidate{Summary: "the dragon returns"}, seeds, nil); !rank.IsSkip(err) {
		t.Fatalf("no term table returned %v, want a skip", err)
	}
	// Seeds with nothing in common with the table at all.
	if _, _, err := (SummaryStyle{DocFreq: testSummaryTable()}).Score(
		rank.Candidate{Summary: "quantum"}, []rank.Candidate{{Summary: "spaceship"}}, nil); !rank.IsSkip(err) {
		t.Fatalf("two summaries with no measurable term returned %v, want a skip", err)
	}
}

// A summary that repeats one word is not more similar than one that mentions it once: the
// candidate vector is a SET, and the reason is that repetition measures a writer's habit,
// not agreement about subject.
func TestSummaryStyleDoesNotRewardRepetition(t *testing.T) {
	s := SummaryStyle{DocFreq: testSummaryTable()}
	seeds := []rank.Candidate{{Summary: "dragon"}}
	once, _, err := s.Score(rank.Candidate{Summary: "dragon"}, seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	many, _, err := s.Score(rank.Candidate{Summary: "dragon dragon dragon dragon"}, seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if once != many {
		t.Fatalf("once=%v many=%v: repetition changed the score", once, many)
	}
}

// The seeds are UNIONED: a reader with three seeds of different fandoms wants works near any
// of them, so a candidate matching the second seed must not be penalised for missing the
// first.
func TestSummaryStyleUnionsTheSeeds(t *testing.T) {
	s := SummaryStyle{DocFreq: testSummaryTable()}
	seeds := []rank.Candidate{
		{Summary: "a castle in the snow"},
		{Summary: "a dragon in the snow"},
	}
	got, _, err := s.Score(rank.Candidate{Summary: "the dragon returns"}, seeds, nil)
	if err != nil {
		t.Fatalf("a candidate matching the second seed was skipped: %v", err)
	}
	if got <= 0 {
		t.Fatalf("score = %v, want positive", got)
	}
	// And MaxSeeds bounds how many are read: with MaxSeeds 1 only the first seed counts.
	one := SummaryStyle{DocFreq: testSummaryTable(), MaxSeeds: 1}
	if _, _, err := one.Score(rank.Candidate{Summary: "the dragon returns"}, seeds, nil); !rank.IsSkip(err) {
		t.Fatalf("with MaxSeeds=1 the second seed was still read: %v", err)
	}
}

// Cosine, not a raw dot product: a summary that mentions ONE of the seed's words among
// twenty unrelated ones agrees less than a summary that is about that word alone. Without the
// candidate-side norm the longer summary wins for having more terms, which is a length bias
// wearing a relevance costume -- and the repetition test cannot see it, because the candidate
// vector is a set.
func TestSummaryStyleNormalisesByLength(t *testing.T) {
	s := SummaryStyle{DocFreq: testSummaryTable()}
	seeds := []rank.Candidate{{Summary: "a dragon hoard"}}
	precise, _, err := s.Score(rank.Candidate{Summary: "dragon"}, seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Same single matched term as the precise summary, padded with measured words the seed
	// does NOT carry: under cosine the padding only enlarges the candidate's norm.
	diluted, _, err := s.Score(rank.Candidate{
		Summary: "dragon spaceship quantum lantern meadow silence castle sorrow",
	}, seeds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if precise <= diluted {
		t.Fatalf("the precise summary scored %v and the diluted one %v: the candidate norm is "+
			"what makes a long summary no better than a short one", precise, diluted)
	}
}
