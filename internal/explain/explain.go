package explain

// Two sentence kinds, one duel, and the rule that decides when a duel is not asked.
//
// A recommendation on this deployment already carries EVIDENCE (signal, raw value, weight), and
// that is not what this package writes. Evidence is written for someone who wants to audit the
// arithmetic; an explanation is written for the reader asking "why is this here". They are
// different sentences, and the roadmap row (#55) is that nobody has ever asked which of them a
// reader finds convincing.
//
// The two kinds are the only two this deployment can BACK with a number:
//
//   - TAGS: works sharing named tags with the seed. The tags are real, the count is real.
//   - READERS: readers who bookmarked both. A count of people, not a score.
//
// What is deliberately absent is a third kind: an LLM-written sentence. This deployment has no
// runtime model, so a generated explanation would be a sentence nobody could check, and the duel
// would measure prose style rather than evidence.
//
// THE RULE THAT MATTERS: a duel is asked only when BOTH sentences can be written. An item with
// one shared tag and no shared readers cannot be compared, and rendering its single sentence
// beside an empty column invites a vote about nothing -- the same defect the /algo page already
// refuses for two identical tunes.

import (
	"fmt"
	"strings"
)

// Kind names one way of explaining a recommendation.
type Kind string

const (
	// KindTags explains a work by the tags it shares with the seed.
	KindTags Kind = "tags"
	// KindReaders explains a work by the readers who took both.
	KindReaders Kind = "readers"
)

// Kinds is every kind the duel can ask about, in the order a page should show them.
var Kinds = [...]Kind{KindTags, KindReaders}

// Explanation is one sentence and the number it rests on.
type Explanation struct {
	Kind Kind `json:"kind"`
	// Text is the sentence a reader is shown. It is written HERE rather than in a template so
	// that the page and the JSON API cannot disagree about what was explained.
	Text string `json:"text"`
	// Support is the count behind the sentence: tags shared, or readers who took both.
	Support int `json:"support"`
	// Tags are the shared tag names, empty for the readers kind.
	Tags []string `json:"tags,omitempty"`
}

// Duel is one work, explained both ways.
type Duel struct {
	WorkID   int64          `json:"work_id"`
	Title    string         `json:"title"`
	Sentence [2]Explanation `json:"sentences"`
}

// MaxTagsShown keeps the tag sentence readable. A work sharing eleven tags is explained by the
// three the caller ranked highest plus a count, not by a list that wraps four lines and buries
// the seed name it is comparing to.
const MaxTagsShown = 3

// MinSharedReaders is the floor below which a reader count is not a pattern. ONE reader who
// bookmarked both is a coincidence that says nothing about why a work is recommended, and
// printing "1 reader bookmarked both" as a competing explanation would be offering a reader a
// sentence with no evidence in it.
const MinSharedReaders = 2

// TagsExplanation writes the tag sentence, or reports that it cannot.
//
// `shared` must arrive in the order the caller wants them shown (rarest first is the useful
// order: the shared "coffee shop au" explains nothing, the shared "chronic pain" explains a lot).
func TagsExplanation(seedTitle string, shared []string) (Explanation, bool) {
	if len(shared) == 0 {
		return Explanation{}, false
	}
	shown := shared
	if len(shown) > MaxTagsShown {
		shown = shown[:MaxTagsShown]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "shares %d tag%s with %s: %s",
		len(shared), plural(len(shared)), quote(seedTitle), strings.Join(shown, ", "))
	if extra := len(shared) - len(shown); extra > 0 {
		fmt.Fprintf(&b, " and %d more", extra)
	}
	return Explanation{Kind: KindTags, Text: b.String(), Support: len(shared), Tags: shared}, true
}

// ReadersExplanation writes the co-reader sentence, or reports that it cannot.
func ReadersExplanation(seedTitle string, readers int) (Explanation, bool) {
	if readers < MinSharedReaders {
		return Explanation{}, false
	}
	return Explanation{
		Kind:    KindReaders,
		Text:    fmt.Sprintf("%d readers bookmarked both this and %s", readers, quote(seedTitle)),
		Support: readers,
	}, true
}

// Build returns the duel for a candidate, and false when one of the two sentences cannot be
// written -- which is the only case in which the page must not ask.
func Build(workID int64, title, seedTitle string, shared []string, readers int) (Duel, bool) {
	tags, okTags := TagsExplanation(seedTitle, shared)
	readersX, okReaders := ReadersExplanation(seedTitle, readers)
	if !okTags || !okReaders {
		return Duel{}, false
	}
	return Duel{WorkID: workID, Title: title, Sentence: [2]Explanation{tags, readersX}}, true
}

// quote wraps a title in typographic quotes. It is not cosmetic: both sentences name the seed,
// and without quotes "shares 3 tags with A Long Winter" reads as if the tags were long and
// winter.
func quote(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "your work"
	}
	return "\u201c" + s + "\u201d"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
