// Package fandom identifies which tags are fandoms.
//
// ## Why this package exists instead of a tag_type column
//
// The obvious implementation is `WHERE tag_type = 'fandoms'`. It returns almost
// nothing, and that is a measured property of the mirror rather than a bug in
// the query: 51 of 113,995 works carry a `fandoms` row. The same column is
// 'freeforms' for 3,890,504 of 3,891,300 work_tags rows — including fandom
// tags, because the blurb scraper flattened every tag into freeforms.
//
// That single fact is what made the previous deployment's `--max-per-fandom`
// a silent no-op: the cap's group key set was empty for every candidate, so
// the cap excluded nothing while reporting success.
//
// ## Name shape, not a column
//
// AO3 fandom tags follow a handful of conventions that non-fandom tags do not:
// `- all media types`, `(anime)`, `- fandom`, `(video game)`, `the ...`,
// and so on. This package recognises those shapes.
//
// The deliberate line, which the sibling tool also drew: this is a CANDIDATE
// FILTER, never a classification to be trusted. A guessed list used as input
// to a relationship graph invents relationships. The same list used to
// *narrow* a set, with counts attached to every row, stays checkable — you can
// see what it matched and argue with it.
package fandom

import (
	"regexp"
	"strings"
)

// suffixes are the media qualifiers that mark a fandom tag.
//
// These are the BARE qualifier words, with no parentheses: shapeRE captures the
// inside of the trailing parens, so storing "(anime)" here could never match
// "naruto (anime)". That mismatch is silent -- every parenthetical fandom tag
// fails the check and the diversity cap skips the whole category.
var suffixes = []string{
	"anime", "manga", "visual novel", "video game", "book",
	"movie", "tv show", "cartoon", "comics", "play", "podcast",
	"doujin", "anime & manga", "other media", "original work",
	"fandom", "tv", "cartoon/comics", "band", "musical", "novel",
	// AO3 writes combined qualifiers with an ampersand, and excluding & from
	// the qualifier character class would drop `bluey (cartoon & comics)`
	// while accepting the single-media forms.
	"cartoon & comics", "anime & manga & doujin", "book & movie",
}

// nameSuffixes are the trailing conventions.
var nameSuffixes = []string{
	" - all media types",
	" - fandom",
	" (fandom)",
}

// preambles mark a fandom tag that keeps its work title, e.g. "The Hobbit".
var preambles = []string{"the ", "les ", "los ", "las ", "el ", "die ", "der ", "das "}

// shapeRE matches a trailing parenthetical qualifier of ANY shape.
//
// Anchored at the end on purpose: "something (anime)" is a fandom tag, but
// "(anime) something" is not a shape AO3 produces, and an unanchored match
// would let any tag with the word anywhere be treated as a fandom.
//
// The captured group is deliberately permissive about its contents (any chars
// except parens) and the EXACTNESS is enforced by the `suffixes` lookup
// afterwards. Narrowing the character class here to avoid matching things we
// do not want only makes the failure mode quieter: `bluey (cartoon & comics)`
// stopped matching while `bluey (cartoon)` kept working, which looks like the
// corpus changed rather than like a regex got stricter.
var shapeRE = regexp.MustCompile(`\s*\(([^()]{1,60})\)$`)

// LooksLikeFandom reports whether a tag name has fandom shape.
//
// It is intentionally a filter with false negatives rather than false
// positives: returning "no" for an unusual fandom tag costs one uncapped
// group, while returning "yes" for `dark (fix-it)` costs the whole cap.
func LooksLikeFandom(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	// A bare fandom like `harry potter` with no qualifier is the hardest case,
	// and it is the one this package deliberately does NOT claim. Guessing here
	// is what produced a ranking that put `bluey` and `fionna and cake` in a
	// dark-villain reader's top 20. The corpus-query modes find these by
	// counting them instead.
	for _, s := range nameSuffixes {
		if strings.HasSuffix(n, s) {
			return true
		}
	}
	if m := shapeRE.FindStringSubmatch(n); m != nil {
		qual := strings.ToLower(m[1])
		for _, s := range suffixes {
			if qual == s {
				return true
			}
		}
	}
	return false
}

// Canonical reduces a fandom tag to a comparable key.
//
// `Harry Potter - All Media Types` and `Harry Potter (book)` are the same
// fandom for diversity purposes, and without this the cap is defeated by the
// qualifier alone: a reader gets six Harry Potter works, three under one
// qualifier and three under another, and the cap reports a clean pass while
// returning a monocrop.
func Canonical(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, s := range nameSuffixes {
		if strings.HasSuffix(n, s) {
			return strings.TrimSpace(strings.TrimSuffix(n, s))
		}
	}
	if m := shapeRE.FindStringSubmatch(n); m != nil {
		base := strings.TrimSpace(strings.TrimSuffix(n, m[0]))
		if base != "" {
			return base
		}
	}
	return n
}

// GroupKey returns the diversity group key for a candidate's tag set.
//
// It returns "" when no tag has fandom shape. "" is meaningful: the caller
// treats it as "do not cap this candidate", because a candidate with no
// identifiable fandom should compete on relevance alone rather than being
// grouped into a shared `unknown` bucket — which would silently cap every
// non-fandom work in the corpus at the same number, and that IS the monocrop,
// just wearing a different label.
func GroupKey(tagNames []string) string {
	for _, n := range tagNames {
		if LooksLikeFandom(n) {
			return Canonical(n)
		}
	}
	return ""
}
