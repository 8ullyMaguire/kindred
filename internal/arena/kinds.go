// SPDX-License-Identifier: AGPL-3.0-only

// Entity kinds: the id spaces an arena can compare things from.
//
// Every arena in this repo ranks ENTITY PAIRS with the same Glicko machinery;
// what differs between a work arena and a tag arena is only which id space the
// two entities come from. This file is the one place that space is defined, so
// a new kind is added here and nowhere else.
//
// The six kinds below are the ones a reader can be offered. What the corpus
// actually holds is a different question, and it is answered by measurement
// rather than by this list — see `Kind.Note` and `Store.KindsOffered`.
//
// Measured against the live mirror (2026-10-14, 112,935 works):
//
//	work_tags.tag_type:  freeforms 3,890,504   characters 304
//	                     relationships 175     fandoms 104
//	works.authors:       a comma-space list of "pseud (username)" bylines
//
// Two consequences this file exists to keep honest:
//
//  1. A tag arena over `freeforms` has 3.89M candidates, and it also reaches
//     characters, ships and fandoms BY NAME, because those are where the mirror
//     filed almost every tag. The tag-type-specific kinds are kept because they
//     are the same code path and cost nothing, but a reader offered a
//     "characters" arena today would find a few dozen candidates, not thousands.
//     The web switcher therefore lists the kinds the corpus can actually serve
//     instead of the kinds this file can name.
//  2. An author's id cannot come from the corpus, which has no author table: it
//     is derived from the byline string. Two different people who share a byline
//     collide, and that is accepted here for the same reason the /author route
//     accepts it — the corpus has nothing finer to offer.
package arena

import (
	"hash/fnv"
	"strings"
)

// EntityKind names an id space an arena compares within.
//
// The values are stored in the state database and appear in URLs, so they are
// stable: `ao3_` prefixed because the corpus is AO3-shaped, and the tag-ish
// values match the corpus's own `work_tags.tag_type` strings so a kind and its
// tag type cannot drift apart.
type EntityKind string

const (
	// KindWork compares works. The default, and the only kind that also
	// feeds tag learning — a tag-vs-tag vote feeding tag weights would be
	// circular.
	KindWork EntityKind = "ao3_work"
	// KindTag compares tags of any type. Candidates come from `freeforms`,
	// which is where the mirror filed 3,890,504 of its 3,891,300 tag rows.
	KindTag EntityKind = "ao3_tag"
	// KindCharacter compares characters. Sparse in the current corpus.
	KindCharacter EntityKind = "ao3_character"
	// KindRelationship compares pairings. Sparse in the current corpus.
	KindRelationship EntityKind = "ao3_relationship"
	// KindFandom compares fandoms. Sparse in the current corpus.
	KindFandom EntityKind = "ao3_fandom"
	// KindAuthor compares authors, by byline. See AuthorEntityID.
	KindAuthor EntityKind = "ao3_author"
)

// Kinds is the allowlist, in the order a switcher should offer them: the kinds
// with data first, because that is the order a reader wants to see them in and
// the order the switcher will filter anyway.
var Kinds = []EntityKind{
	KindWork,
	KindTag,
	KindAuthor,
	KindCharacter,
	KindRelationship,
	KindFandom,
}

// Valid reports whether k is one of the allowlisted kinds.
//
// The zero EntityKind is NOT valid: an unset kind must be defaulted deliberately
// (see ParseKind) rather than passing a check by accident.
func (k EntityKind) Valid() bool {
	for _, known := range Kinds {
		if k == known {
			return true
		}
	}
	return false
}

// ParseKind turns a user-supplied kind into an allowlisted one.
//
// Anything empty, misspelled or not on the allowlist becomes KindWork, which is
// the kind that existed before this file did. Invalid input therefore degrades
// to the old behaviour instead of producing a 400 on a route that used to work —
// the JSON API is a public surface and its callers predate `?kind=`.
func ParseKind(s string) EntityKind {
	k := EntityKind(strings.ToLower(strings.TrimSpace(s)))
	if k.Valid() {
		return k
	}
	return KindWork
}

// Label is the short human name for a kind, for switcher links and headings.
func (k EntityKind) Label() string {
	switch k {
	case KindWork:
		return "works"
	case KindTag:
		return "tags"
	case KindCharacter:
		return "characters"
	case KindRelationship:
		return "relationships"
	case KindFandom:
		return "fandoms"
	case KindAuthor:
		return "authors"
	default:
		return string(k)
	}
}

// TagType is the `work_tags.tag_type` a tag-ish kind's candidates come from.
//
// The second return is false for the kinds that are not tags — works and
// authors — which is the check a caller wants, rather than comparing against
// the empty string.
func (k EntityKind) TagType() (string, bool) {
	switch k {
	case KindTag:
		return "freeforms", true
	case KindCharacter:
		return "characters", true
	case KindRelationship:
		return "relationships", true
	case KindFandom:
		return "fandoms", true
	default:
		return "", false
	}
}

// IsTag reports whether the kind's entities are tags, i.e. whether their names
// resolve through the corpus's `tags` table rather than through entity_names.
func (k EntityKind) IsTag() bool {
	_, ok := k.TagType()
	return ok
}

// AuthorEntityID derives an author's entity id from a byline.
//
// The corpus has no author table, so the id is a hash of the normalised name:
// lowercased, whitespace collapsed, and the trailing ` (username)` dropped when
// the mirror's crawler appended one (`-Roulette- (Roulette_CV)` is the author
// `-Roulette-`). Leading and inner punctuation is kept, because `-Roulette-` and
// `Roulette` are different AO3 accounts and collapsing them would merge two
// people.
//
// A collision between two people who share a byline is accepted, documented here
// and in the plan, and not worked around: the same-name case is unresolvable
// from the corpus, and a wrong-but-consistent id beats an id that changes
// whenever a work is re-crawled.
func AuthorEntityID(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(NormalizeAuthorName(name)))
	return int64(h.Sum64())
}

// NormalizeAuthorName is the exact form AuthorEntityID hashes, exported so a
// caller can display or compare it without reimplementing the rule.
func NormalizeAuthorName(name string) string {
	base := NameWithoutPseud(name)
	// Collapse runs of whitespace: the corpus holds `-  Scrap - Paper -`,
	// with two spaces, and a re-crawl that normalised them differently
	// would otherwise produce a second id for the same person.
	return strings.ToLower(strings.Join(strings.Fields(base), " "))
}

// NameWithoutPseud drops the mirror's trailing ` (username)` from a byline.
//
// Only a suffix that is a parenthesised group at the very end is removed, and
// only one of them: an author whose *name* ends in a bracket, or a byline with
// several groups, must not be mangled into a different person. A byline with no
// suffix is returned trimmed and otherwise untouched.
func NameWithoutPseud(name string) string {
	s := strings.TrimSpace(name)
	if !strings.HasSuffix(s, ")") {
		return s
	}
	open := strings.LastIndex(s, "(")
	if open <= 0 {
		// No opening bracket, or the name IS the bracketed part — in
		// which case the brackets are the name and must survive.
		return s
	}
	return strings.TrimSpace(s[:open])
}

// SplitAuthors splits a work's `authors` column into individual bylines.
//
// The crawler joins bylines with ", " (`internal/crawl/parse.go`), so that is
// what this splits on. A single-author corpus never exercises this, which is
// exactly why it is here and tested: reading the whole column as one id would
// mint an author who does not exist the first time a multi-author work appears.
func SplitAuthors(authors string) []string {
	parts := strings.Split(authors, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}
