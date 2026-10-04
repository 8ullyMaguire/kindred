// Package profile stores taste profiles and the feedback that shapes them.
//
// ## What a profile is
//
// A profile is a weighted bag of tag ids. It is not a list of liked works and
// not a vector: the weights are what let a reader who read 400 Harry Potter
// works and 6 Star Wars works get a ranking that reflects that ratio.
//
// ## Why feedback writes to the profile rather than re-ranking on the fly
//
// The sibling tool learned from Show-less events only for the For You pair, and
// the learning was not durable: a new process started from nothing. Storing
// feedback as tag weights makes the learning survive restarts, which is the
// whole difference between a preference and a one-off impression.
//
// ## Privacy
//
// A profile is tag ids and weights. It holds no user id and no work ids, so it
// can be snapshotted without anonymising it first.
package profile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrNotFound is returned for an unknown profile name.
var ErrNotFound = errors.New("profile: not found")

// Profile is a named bag of tag weights.
type Profile struct {
	Name   string            `json:"name"`
	Source string            `json:"source,omitempty"`
	Tags   map[int32]float64 `json:"tags"`
	Works  int               `json:"works"`
}

// String renders the profile for the CLI, heaviest tag first.
func (p *Profile) String() string {
	type kv struct {
		id int32
		w  float64
	}
	items := make([]kv, 0, len(p.Tags))
	for id, w := range p.Tags {
		items = append(items, kv{id, w})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].w != items[j].w {
			return items[i].w > items[j].w
		}
		return items[i].id < items[j].id
	})
	var b strings.Builder
	fmt.Fprintf(&b, "profile %q: %d tags over %d works", p.Name, len(p.Tags), p.Works)
	for i, it := range items {
		if i >= 10 {
			fmt.Fprintf(&b, "\n  ... and %d more", len(items)-10)
			break
		}
		fmt.Fprintf(&b, "\n  tag %d: %.3f", it.id, it.w)
	}
	return b.String()
}

// Store persists profiles.
type Store struct {
	db *sql.DB
}

// NewStore wraps a database handle.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// EnsureSchema creates the profile tables.
//
// It is called on every open rather than migrated, because a profile table
// holding preferences is not worth a schema-version gate: the table is
// additive and losing it costs a rebuild, not data anyone else depends on.
func (s *Store) EnsureSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS profiles(
			name TEXT PRIMARY KEY,
			source TEXT NOT NULL DEFAULT '',
			works INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS profile_tags(
			name TEXT NOT NULL,
			tag_id INTEGER NOT NULL,
			weight REAL NOT NULL,
			PRIMARY KEY(name, tag_id)
		)`)
	if err != nil {
		return fmt.Errorf("profile schema: %w", err)
	}
	return nil
}

// Save writes a profile, replacing any previous version of that name.
func (s *Store) Save(ctx context.Context, p *Profile) error {
	if p == nil || strings.TrimSpace(p.Name) == "" {
		return errors.New("profile: a name is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO profiles(name, source, works) VALUES(?,?,?)
		 ON CONFLICT(name) DO UPDATE SET source=excluded.source, works=excluded.works`,
		p.Name, p.Source, p.Works); err != nil {
		return fmt.Errorf("profile %q: %w", p.Name, err)
	}
	// Replace wholesale: a weight that dropped to zero must disappear rather
	// than linger, or a tag the reader has since grown out of keeps voting.
	if _, err := tx.ExecContext(ctx, `DELETE FROM profile_tags WHERE name = ?`, p.Name); err != nil {
		return err
	}
	for tagID, w := range p.Tags {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO profile_tags(name, tag_id, weight) VALUES(?,?,?)`,
			p.Name, tagID, w); err != nil {
			return fmt.Errorf("profile %q tag %d: %w", p.Name, tagID, err)
		}
	}
	return tx.Commit()
}

// Load reads a profile.
func (s *Store) Load(ctx context.Context, name string) (*Profile, error) {
	p := &Profile{Name: name, Tags: map[int32]float64{}}
	err := s.db.QueryRowContext(ctx,
		`SELECT source, works FROM profiles WHERE name = ?`, name).
		Scan(&p.Source, &p.Works)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT tag_id, weight FROM profile_tags WHERE name = ?`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int32
		var w float64
		if err := rows.Scan(&id, &w); err != nil {
			return nil, err
		}
		p.Tags[id] = w
	}
	return p, rows.Err()
}

// List returns every profile name.
func (s *Store) List(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// TagIDs returns a profile's tag ids, heaviest first, capped at max.
//
// The cap exists because the pool builder and the fandom ranking both take a
// slice of these, and a profile built from 400 works carries thousands of tag
// ids, of which the tail contributes nothing but query cost.
func (s *Store) TagIDs(ctx context.Context, name string, max int) ([]int32, error) {
	p, err := s.Load(ctx, name)
	if err != nil {
		return nil, err
	}
	type kv struct {
		id int32
		w  float64
	}
	items := make([]kv, 0, len(p.Tags))
	for id, w := range p.Tags {
		items = append(items, kv{id, w})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].w != items[j].w {
			return items[i].w > items[j].w
		}
		return items[i].id < items[j].id
	})
	if max > 0 && len(items) > max {
		items = items[:max]
	}
	out := make([]int32, len(items))
	for i, it := range items {
		out[i] = it.id
	}
	return out, nil
}

// LearningRate is how much one feedback event moves a tag weight.
//
// Small on purpose. The sibling's For You pair retrained per impression and
// oscillated; a reader who dismisses one work should not have their whole
// profile rewritten by it. This is large enough to matter over a session and
// small enough that no single event dominates.
const LearningRate = 0.1

// Feedback is one reader's verdict on one work.
type Feedback struct {
	WorkID int64   `json:"work_id"`
	Liked  bool    `json:"liked"`
	TagIDs []int32 `json:"tag_ids"`
	At     string  `json:"at,omitempty"`
}

// Apply folds feedback into a profile and returns the updated one.
//
// It is in-memory and returned rather than saved, so a caller can inspect the
// change and decide: a profile that rewrites itself silently on every click is
// a profile nobody can debug.
func Apply(p *Profile, f Feedback) *Profile {
	if p == nil {
		p = &Profile{Tags: map[int32]float64{}}
	}
	if p.Tags == nil {
		p.Tags = map[int32]float64{}
	}
	if f.Liked {
		p.Works++
	}
	delta := LearningRate
	if !f.Liked {
		delta = -LearningRate
	}
	for _, id := range f.TagIDs {
		// Weights are clamped to [0,1]: a weight can fall to "no opinion"
		// but never to a negative score, because a negative weight inverts
		// the signal's meaning and a tag nobody likes is not the opposite of
		// a tag everybody likes.
		w := p.Tags[id] + delta
		if w < 0 {
			w = 0
		}
		if w > 1 {
			w = 1
		}
		p.Tags[id] = w
	}
	return p
}

// BuildFromWorks derives a profile from a set of liked works.
//
// Each liked work contributes its tags weighted by the work's own engagement,
// so a bookmarked 200k fic counts for more than a kudos-only one. The log
// transform is what keeps one 40k-bookmark outlier from becoming the entire
// profile.
func BuildFromWorks(ctx context.Context, db *sql.DB, workIDs []int64, name string) (*Profile, error) {
	if len(workIDs) == 0 {
		return nil, errors.New("profile: no works to build from")
	}
	p := &Profile{Name: name, Source: "works", Tags: map[int32]float64{}}

	for _, id := range workIDs {
		rows, err := db.QueryContext(ctx,
			`SELECT t.id, COALESCE(w.kudos,0) + COALESCE(w.bookmarks,0)
			 FROM work_tags wt JOIN tags t ON t.id = wt.tag_id
			 JOIN works w ON w.id = wt.work_id
			 WHERE wt.work_id = ?`, id)
		if err != nil {
			return nil, fmt.Errorf("profile: work %d tags: %w", id, err)
		}
		for rows.Next() {
			var tagID int32
			var engagement int
			if err := rows.Scan(&tagID, &engagement); err != nil {
				rows.Close()
				return nil, err
			}
			w := logWeight(engagement)
			// Accumulate ROWS, not distinct pairs: work_tags has PK
			// (work_id, tag_id, tag_type), so one tag can appear twice on one
			// work and a distinct-count would silently halve it.
			p.Tags[tagID] += w
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	p.Works = len(workIDs)
	normalise(p.Tags)
	return p, nil
}

// normalise scales weights into [0,1] so profiles are comparable.
func normalise(tags map[int32]float64) {
	max := 0.0
	for _, w := range tags {
		if w > max {
			max = w
		}
	}
	if max <= 0 {
		return
	}
	for id := range tags {
		tags[id] /= max
	}
}

// logWeight is log10(engagement+1) normalised into [0,1] by a measured ceiling.
func logWeight(engagement int) float64 {
	if engagement <= 0 {
		return 0.05
	}
	// log10(10000+1) ~= 4.0, so this maps 10k bookmarks to 1.0.
	v := float64(engagement)
	w := 0.0
	for v > 1 {
		w++
		v /= 10
	}
	if w > 4 {
		w = 4
	}
	return (w + (v - 1)) / 4
}
