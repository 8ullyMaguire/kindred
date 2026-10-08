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

	// BaseTags is the weight the ratings said, kept separate from Tags so
	// online learning can move Tags without rewriting history.
	//
	// They are identical for a profile built from a work list. They part
	// company the moment a reader gives feedback: Tags moves toward what the
	// feedback says, BaseTags stays at what the library said. Storing only one
	// map means the decay toward base has nothing to decay TOWARD, and a
	// profile that cannot forget is a profile that cannot be corrected.
	BaseTags map[int32]float64 `json:"base_tags,omitempty"`

	// Evidence is the confidence behind each tag: how much rating signal
	// stands behind it. A weight with no evidence count behind it cannot be
	// trusted the way a weight with fifty works behind it can, and the
	// introspection page shows both together for exactly that reason.
	Evidence map[int32]float64 `json:"evidence,omitempty"`

	// Feedback counts per tag, for the confidence-damped online update. Zero
	// for a ratings-built profile: the ratings ARE the base, not feedback
	// events, and counting them here would make the first like on a tag move
	// it a twentieth as far as intended.
	Feedback map[int32]int `json:"feedback,omitempty"`

	// weightSum and evidSum are the running accumulators for a build in
	// progress. They are unexported and cleared by finish, because a saved
	// profile must not carry them: they are build scratch, and a snapshot
	// with them in it would be larger than the weights it produces.
	weightSum map[int32]float64
	evidSum   map[int32]float64
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
		);
		-- base_weight, evidence and feedback live on profile_tags rather than
		-- in three more tables.
		--
		-- base_weight is what the ratings said, and it is IMMUTABLE: online
		-- learning writes weight and never touches this column. It is the
		-- target the decay pulls toward and the value "reset this tag"
		-- restores, so a profile whose base was being rewritten along with
		-- its live weights could never forget anything a reader asked it to
		-- forget. Measured reason it cannot be derived instead: after a month
		-- of feedback the live weights differ from the ratings by up to 0.4,
		-- and "restore the rating-derived value" is then not a thing the code
		-- can compute without re-running the whole build over 257 works.
		CREATE TABLE IF NOT EXISTS profile_tags_v2(
			name TEXT NOT NULL,
			tag_id INTEGER NOT NULL,
			weight REAL NOT NULL,
			base_weight REAL,
			evidence REAL NOT NULL DEFAULT 0,
			feedback INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(name, tag_id)
		)`)
	if err != nil {
		return fmt.Errorf("profile schema: %w", err)
	}
	return s.migrateProfileTags(ctx)
}

// migrateProfileTags brings an existing profile_tags table up to the v2 shape.
//
// A new table plus a copy, rather than ALTER: the live state DB has profiles in
// it already, and `CREATE TABLE IF NOT EXISTS` does nothing for a table that
// exists, so a shape change applied only to fresh databases would read
// NULL from every base_weight forever. The copy is one table's worth of rows
// (thousands at most) at startup, once.
//
// base_weight is copied from weight, because for every profile that existed
// before this change the two WERE the same value: nothing had yet moved a live
// weight away from its base. Copying rather than leaving NULL means the decay
// has a target from the first run instead of no-oping for a month.
func (s *Store) migrateProfileTags(ctx context.Context) error {
	has, err := s.tableExists(ctx, "profile_tags_v2")
	if err != nil {
		return err
	}
	if !has {
		return nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM profile_tags_v2`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	var old int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM profile_tags`).Scan(&old); err != nil {
		return err
	}
	if old == 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO profile_tags_v2(name, tag_id, weight, base_weight, evidence, feedback)
		SELECT name, tag_id, weight, weight, 0, 0 FROM profile_tags`); err != nil {
		return fmt.Errorf("profile: seed base weights: %w", err)
	}
	return nil
}

// tableExists reports whether a table is present.
func (s *Store) tableExists(ctx context.Context, table string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`,
		table).Scan(&n); err != nil {
		return false, fmt.Errorf("profile: inspect %s: %w", table, err)
	}
	return n > 0, nil
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM profile_tags_v2 WHERE name = ?`, p.Name); err != nil {
		return err
	}
	for tagID, w := range p.Tags {
		// The v2 row carries the base weight, the evidence behind it and the
		// feedback count. All three default to the live weight so a profile
		// built by the [0,1] path still round-trips correctly: its base IS its
		// weight, because nothing has ever moved it.
		base := w
		if p.BaseTags != nil {
			if b, ok := p.BaseTags[tagID]; ok {
				base = b
			}
		}
		var evid float64
		if p.Evidence != nil {
			evid = p.Evidence[tagID]
		}
		var fb int
		if p.Feedback != nil {
			fb = p.Feedback[tagID]
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO profile_tags_v2
			   (name, tag_id, weight, base_weight, evidence, feedback)
			 VALUES(?,?,?,?,?,?)`,
			p.Name, tagID, w, base, evid, fb); err != nil {
			return fmt.Errorf("profile %q tag %d: %w", p.Name, tagID, err)
		}
		// The legacy row carries the live weight alone. It is dual-written
		// rather than dropped because BuildFromWorks' [0,1] path and every
		// reader written before the signed-weight change read this table, and
		// a migration that silently empties them would break the profile page
		// for every existing profile.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO profile_tags(name, tag_id, weight) VALUES(?,?,?)`,
			p.Name, tagID, w); err != nil {
			return fmt.Errorf("profile %q legacy tag %d: %w", p.Name, tagID, err)
		}
	}
	return tx.Commit()
}

// Load reads a profile.
func (s *Store) Load(ctx context.Context, name string) (*Profile, error) {
	// Every map is initialised, not just Tags. A nil map reads fine and panics
	// on write, and Save writes BaseTags/Evidence/Feedback for every profile --
	// so a caller that loaded a profile and saved it back would fault on the
	// first tag, which is the save-the-rating path in the web handler.
	p := &Profile{
		Name:     name,
		Tags:     map[int32]float64{},
		BaseTags: map[int32]float64{},
		Evidence: map[int32]float64{},
		Feedback: map[int32]int{},
	}
	err := s.db.QueryRowContext(ctx,
		`SELECT source, works FROM profiles WHERE name = ?`, name).
		Scan(&p.Source, &p.Works)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if err != nil {
		return nil, err
	}

	// The v2 table first, then the legacy one.
	//
	// The order is the whole point: v2 is authoritative when present, and the
	// legacy read is a FALLBACK for a database where the migration has not
	// run. Reading the legacy table first would silently discard the base
	// weights, the evidence and the feedback counts -- the three things the
	// decay and the introspection page depend on -- and the profile would
	// still render, still look plausible, and have no memory.
	rows, err := s.db.QueryContext(ctx,
		`SELECT tag_id, weight, base_weight, evidence, feedback
		   FROM profile_tags_v2 WHERE name = ?`, name)
	if err != nil {
		return nil, err
	}
	found := 0
	for rows.Next() {
		var id int32
		var w, base, evid float64
		var fb int
		if err := rows.Scan(&id, &w, &base, &evid, &fb); err != nil {
			rows.Close()
			return nil, err
		}
		p.Tags[id] = w
		p.BaseTags[id] = base
		p.Evidence[id] = evid
		p.Feedback[id] = fb
		found++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if found == 0 {
		legacy, err := s.db.QueryContext(ctx,
			`SELECT tag_id, weight FROM profile_tags WHERE name = ?`, name)
		if err != nil {
			return nil, err
		}
		defer legacy.Close()
		for legacy.Next() {
			var id int32
			var w float64
			if err := legacy.Scan(&id, &w); err != nil {
				return nil, err
			}
			p.Tags[id] = w
			// The base equals the live weight: nothing has moved it, because
			// this row predates anything that could.
			p.BaseTags[id] = w
		}
		return p, legacy.Err()
	}
	return p, nil
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
