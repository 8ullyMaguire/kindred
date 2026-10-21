package tagfamily

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

//go:embed families.json
var familiesJSON []byte

// Universe is one franchise's set of fandom spellings.
//
// It is DECLARED, not derived, and the corpus cannot derive it: the
// spellings of one franchise are near-disjoint populations (measured: the
// two biggest Worm spellings share 17 of ~2,000 works each), and character
// tags do not bridge franchises either (measured: "harry potter - j. k.
// rowling" and "star wars - all media types" share 4,619 character tags).
// What the corpus CAN do is validate the declaration, which is what
// ValidateUniverse does.
type Universe struct {
	Name      string   `json:"-"`
	Label     string   `json:"label"`
	Spellings []string `json:"spellings"`
	// SpellingsExplicit lists spellings the SHAPE RULE cannot recognise
	// but that a reader plainly means as this universe.
	//
	// It is one list, not one list per reason, because the reasons do not
	// matter to the query: a plain-named fandom ("harry potter"), a
	// creator form whose left side is not a canonical base anywhere
	// ("parahumans series - wildbow"), a medium the qualifier table does
	// not know ("worm (web serial novel)") and a freeform-shaped tag
	// ("worm - freeform") are all the same thing to the query -- a
	// declared member. The validation report says how many members were
	// admitted by DECLARATION rather than by rule, so a reader can see
	// which ones the shape rule did not vouch for.
	SpellingsExplicit []string `json:"spellings_explicit"`
}

// Concept is one idea's set of tag spellings.
//
// A concept is also declared, for the opposite reason: the token "isekai"
// occurs across 14,126 works in the corpus, including names scoped to
// another fandom ("pureblood culture (harry potter)"), so the token alone
// is far too wide to be the family.
type Concept struct {
	Name  string   `json:"-"`
	Label string   `json:"label"`
	Tags  []string `json:"tags"`
}

// Families is the loaded family set.
type Families struct {
	universes map[string]*Universe
	concepts  map[string]*Concept
}

// Universe returns a declared universe by key.
func (f *Families) Universe(name string) (*Universe, bool) {
	u, ok := f.universes[name]
	return u, ok
}

// Concept returns a declared concept by key.
func (f *Families) Concept(name string) (*Concept, bool) {
	c, ok := f.concepts[name]
	return c, ok
}

// UniverseNames lists the universe keys, sorted.
func (f *Families) UniverseNames() []string {
	out := make([]string, 0, len(f.universes))
	for k := range f.universes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Universes lists the declared universes, sorted by key.
//
// It returns the universes themselves, not just their keys, because the
// web page needs each one's Label to show the reader ("Worm (Parahumans)")
// while the URL carries the key ("worm").
func (f *Families) Universes() []*Universe {
	out := make([]*Universe, 0, len(f.universes))
	for _, k := range f.UniverseNames() {
		out = append(out, f.universes[k])
	}
	return out
}

// Concepts lists the declared concepts, sorted by key.
func (f *Families) Concepts() []*Concept {
	out := make([]*Concept, 0, len(f.concepts))
	for _, k := range f.ConceptNames() {
		out = append(out, f.concepts[k])
	}
	return out
}

// ConceptNames lists the concept keys, sorted.
func (f *Families) ConceptNames() []string {
	out := make([]string, 0, len(f.concepts))
	for k := range f.concepts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LoadFamilies parses the embedded family declarations.
func LoadFamilies() (*Families, error) {
	var raw struct {
		Universes map[string]*Universe `json:"universes"`
		Concepts  map[string]*Concept  `json:"concepts"`
	}
	if err := json.Unmarshal(familiesJSON, &raw); err != nil {
		return nil, fmt.Errorf("tagfamily: parse families.json: %w", err)
	}
	f := &Families{universes: raw.Universes, concepts: raw.Concepts}
	for k, u := range f.universes {
		u.Name = k
	}
	for k, c := range f.concepts {
		c.Name = k
	}
	return f, nil
}

// UniverseReport is the result of checking a declared universe against
// the corpus. It exists so a member that does not exist is REPORTED, not
// silently skipped: a filter that quietly drops a term looks like one that
// did not fire.
type UniverseReport struct {
	Present   []string `json:"present"`
	Missing   []string `json:"missing"`
	NonFandom []string `json:"non_fandom_shape"`
	// Explicit are the members the shape rule did not vouch for, which
	// were admitted by declaration instead.
	Explicit []string `json:"explicit"`
}

// ValidateUniverse checks every declared spelling against the mirror.
//
// The shape argument is the corpus-derived Shape from Task 1: whether a
// spelling "looks like a fandom" is a question about the corpus, not
// about the name, so the caller builds the Shape once and passes it in.
func ValidateUniverse(ctx context.Context, db *sql.DB, sh *Shape, u *Universe) (UniverseReport, error) {
	var rep UniverseReport
	explicit := map[string]bool{}
	for _, s := range u.SpellingsExplicit {
		explicit[s] = true
	}
	for _, s := range u.Spellings {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tags WHERE name = ? COLLATE NOCASE`, s).Scan(&n); err != nil {
			return rep, fmt.Errorf("tagfamily: validate %s: %w", s, err)
		}
		if n == 0 {
			rep.Missing = append(rep.Missing, s)
			continue
		}
		rep.Present = append(rep.Present, s)
		if !sh.IsFandom(s) {
			if explicit[s] {
				rep.Explicit = append(rep.Explicit, s)
			} else {
				rep.NonFandom = append(rep.NonFandom, s)
			}
		}
	}
	return rep, nil
}
