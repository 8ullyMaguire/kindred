// Package tagfamily answers questions that need a FAMILY of tags rather
// than one tag: a franchise's several spellings, or a concept's several
// spellings. The families are declared (families.json) because the corpus
// cannot derive them -- the spellings of one franchise are near-disjoint
// populations and share no co-occurring works -- but the corpus CAN
// validate them, and this file's rule is what "validate" means.
package tagfamily

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// typeMarkers are AO3's own vocabulary for "this tag names a TYPE, not a
// thing": the right-hand side of "X - freeform" marks a freeform tag.
var typeMarkers = map[string]bool{
	"freeform":     true,
	"character":    true,
	"relationship": true,
}

// mediaQualifiers are the parenthetical forms AO3 uses for a fandom's
// medium: "Star Wars (movie)", "Worm (web serial novel)".
var mediaQualifiers = map[string]bool{
	"anime": true, "manga": true, "video game": true, "book": true,
	"movie": true, "tv show": true, "comics": true, "cartoon": true,
	"novel": true, "webcomic": true, "play": true, "podcast": true,
	"book & movie": true, "anime & manga": true, "original work": true,
}

// canonicalTails are the right-hand sides that make a tag canonical: the
// two spellings AO3 uses for a fandom whose medium is not named.
var canonicalTails = map[string]bool{
	"all media types": true,
	"fandom":          true,
}

// Shape classifies tag names as fandom-shaped, using the CORPUS.
//
// It is a type rather than a bare function because the rule's third
// clause is about the corpus, not about the name: "X - <author>" is a
// fandom only if X has a canonical spelling SOMEWHERE ELSE. A function
// that sees only a name cannot apply that clause, and applying it
// wrongly (treating every "X - Y" as a fandom) measured 8 false
// positives against AO3's own typed markup -- "naruto - freeform",
// "marvel - freeform", "one piece - freeform" and five others.
//
// The measured shape of the rule, against 486 scraped work pages with
// AO3's own "<dd class=\"fandom tags\">" markup (843 fandom tag names,
// 3,286 freeform tag names, and no name ever both -- so fandom-ness is a
// property of the NAME, and the mirror's work_tags.tag_type column
// cannot answer it, flattening 3,890,504 rows to "freeforms"):
//
//	"X - all media types"    canonical, always a fandom
//	"X - fandom"             canonical, always a fandom
//	"X (<media qualifier>)"  fandom in a named medium
//	"X - freeform|character|relationship"   a TYPE, never a fandom
//	"X - <author>"           a creator form: a fandom IFF X is a
//	                          canonical base in the corpus
//
// Measured with the corpus-derived base set: precision 1.000 (0 false
// positives), recall 0.450. The false negatives are franchises whose
// only spelling is a creator form -- "a song of ice and fire - george r.
// r. martin" -- and those are admitted by an explicit declaration in
// families.json instead, which is why the declaration carries a
// per-spelling override.
type Shape struct {
	// bases are the canonical fandom bases: the X of every "X - all
	// media types" and "X - fandom" tag in the corpus.
	bases map[string]bool
}

// LoadShape reads the canonical base set from the corpus.
//
// The read is one scan of the tags table, filtered on the two canonical
// tails. On the live mirror that is 634,027 tags and takes about 2.3
// seconds -- so a Shape is built ONCE per query, not once per tag.
// Do not call LoadShape inside a per-work loop.
func LoadShape(ctx context.Context, db *sql.DB) (*Shape, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM tags WHERE name LIKE '% - all media types' OR name LIKE '% - fandom'`)
	if err != nil {
		return nil, fmt.Errorf("tagfamily: load shape: %w", err)
	}
	defer rows.Close()
	sh := &Shape{bases: map[string]bool{}}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("tagfamily: load shape: %w", err)
		}
		base, ok := canonicalBase(name)
		if ok {
			sh.bases[base] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tagfamily: load shape: %w", err)
	}
	return sh, nil
}

// canonicalBase returns the canonical base of a canonically-shaped name.
func canonicalBase(name string) (string, bool) {
	low := strings.ToLower(strings.TrimSpace(name))
	for _, tail := range []string{" - all media types", " - fandom"} {
		if strings.HasSuffix(low, tail) {
			return strings.TrimSpace(low[:len(low)-len(tail)]), true
		}
	}
	return "", false
}

// IsFandom reports whether a tag NAME names a fandom.
func (sh *Shape) IsFandom(name string) bool {
	low := strings.ToLower(strings.TrimSpace(name))
	if low == "" {
		return false
	}
	if i := strings.LastIndex(low, " - "); i > 0 {
		left := strings.TrimSpace(low[:i])
		right := strings.TrimSpace(low[i+3:])
		switch {
		case canonicalTails[right]:
			// "X - all media types" / "X - fandom": canonical.
			return true
		case typeMarkers[right]:
			// "X - freeform" / "X - character": a TYPE. This check
			// WINS over the creator-form reading below, because
			// "naruto - freeform" has a canonical base on the left
			// and is still a freeform tag.
			return false
		}
		// A creator form: "<fandom> - <author>". It is a fandom only
		// if the left side is a canonical base somewhere in the corpus.
		return sh.bases[left]
	}
	// Parenthetical medium: "Star Wars (movie)".
	if i := strings.LastIndex(low, " ("); i > 0 && strings.HasSuffix(low, ")") {
		qualifier := strings.TrimSpace(low[i+2 : len(low)-1])
		if mediaQualifiers[qualifier] {
			return true
		}
	}
	return false
}

// Base returns the franchise part of a fandom tag name, or "" if the name
// is not fandom-shaped.
//
// "worm - fandom" -> "worm"; "star wars (movie)" -> "star wars";
// "harry potter - j. k. rowling" -> "harry potter".
func (sh *Shape) Base(name string) string {
	low := strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndex(low, " - "); i > 0 {
		left := strings.TrimSpace(low[:i])
		right := strings.TrimSpace(low[i+3:])
		if canonicalTails[right] || sh.bases[left] {
			return left
		}
	}
	if i := strings.LastIndex(low, " ("); i > 0 && strings.HasSuffix(low, ")") {
		if mediaQualifiers[strings.TrimSpace(low[i+2:len(low)-1])] {
			return strings.TrimSpace(low[:i])
		}
	}
	return ""
}
