package tagtree

// Work vectors and theme labels (#47).
//
// A work has no embedding of its own: the archive holds 32-dimension vectors for TAGS, because
// a tag is a thing thousands of works share and a work appears once. A work's position is
// therefore DERIVED -- the weighted mean of its tags' vectors -- and the weighting is the whole
// argument:
//
//	weight(tag) = 1 / sqrt(support(tag))
//
// A flat mean would let the biggest tags decide everything. "Fluff" is on 60,000 works and
// "Alternate Universe - Time Loop" on 400; a flat mean makes every fluffy work identical and
// the time-loop work indistinguishable from anything else it is tagged with. Inverse sqrt is the
// standard dampening (it is what BM25 does to term frequency, and what the summary signal
// already does with its evidence counts): a tag on ten times as many works counts a third as
// much, so a rare, specific tag can still move the vector.
//
// Labels are NOT "the biggest tags in the cluster" -- those are the archive's water. A theme is
// named by the tags that are CONCENTRATED in it: share of the cluster's works carrying the tag,
// divided by the share of all works carrying it.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
)

// WorkOpts bounds the build.
type WorkOpts struct {
	// MinTags drops works with fewer tags: a work with one tag has no direction, it has that
	// tag, and averaging one vector gives a cluster of that tag's clones.
	MinTags int
	// MaxWorks caps the corpus, highest kudos first. A theme page nobody scrolls past the first
	// screen of does not need the 60,000th rarest work in it.
	MaxWorks int
	// LabelTags is how many tags name a theme.
	LabelTags int
	// MinLabelShare is the share of a cluster's works a tag must reach to be considered.
	MinLabelShare float64
	// MinLift hides tags that are no more common inside the cluster than outside it.
	MinLift float64
}

// DefaultWorkOpts is what `kindred worktree` runs with.
func DefaultWorkOpts() WorkOpts {
	return WorkOpts{MinTags: 3, MaxWorks: 60000, LabelTags: 3, MinLabelShare: 0.08, MinLift: 2.0}
}

// WorkVec is one work's derived vector.
type WorkVec struct {
	ID    int64
	V     []float32
	Kudos int
	NTags int
}

// LoadWorks builds a vector per work from the tag embeddings.
//
// The join is done in Go rather than SQL because the vectors are BLOBs in one database and the
// tag assignments in another: an ATTACH would work, but the embedding decode is needed either
// way and a 40k-row map is cheaper than a 1M-row join over a cross-database attachment.
func LoadWorks(ctx context.Context, state, corpus *sql.DB, o WorkOpts) ([]WorkVec, error) {
	if state == nil || corpus == nil {
		return nil, fmt.Errorf("worktree: both the state database (embeddings) and the corpus (tags) are required")
	}
	emb := map[int64][]float32{}
	rows, err := state.QueryContext(ctx, `SELECT entity_id, dim, vec FROM embeddings WHERE kind = 'tag'`)
	if err != nil {
		return nil, fmt.Errorf("worktree: read tag embeddings: %w", err)
	}
	for rows.Next() {
		var id int64
		var dim int
		var blob []byte
		if err := rows.Scan(&id, &dim, &blob); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("worktree: scan embedding: %w", err)
		}
		if len(blob) != dim*4 {
			_ = rows.Close()
			return nil, fmt.Errorf("worktree: tag %d has dim %d but %d bytes", id, dim, len(blob))
		}
		v := make([]float32, dim)
		for i := 0; i < dim; i++ {
			bits := uint32(blob[i*4]) | uint32(blob[i*4+1])<<8 | uint32(blob[i*4+2])<<16 | uint32(blob[i*4+3])<<24
			v[i] = math.Float32frombits(bits)
		}
		emb[id] = v
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("worktree: embeddings: %w", err)
	}
	_ = rows.Close()

	support := map[int64]int{}
	srows, err := corpus.QueryContext(ctx, `SELECT tag_id, COUNT(*) FROM work_tags GROUP BY tag_id`)
	if err != nil {
		return nil, fmt.Errorf("worktree: tag support: %w", err)
	}
	for srows.Next() {
		var id int64
		var n int
		if err := srows.Scan(&id, &n); err != nil {
			_ = srows.Close()
			return nil, fmt.Errorf("worktree: scan tag support: %w", err)
		}
		support[id] = n
	}
	if err := srows.Err(); err != nil {
		_ = srows.Close()
		return nil, fmt.Errorf("worktree: tag support: %w", err)
	}
	_ = srows.Close()

	// Works ranked by kudos: a cap should keep the works a reader is likely to meet.
	type acc struct {
		sum  []float64
		n    int
		kudo int
	}
	accs := map[int64]*acc{}
	wrows, err := corpus.QueryContext(ctx, `
		SELECT w.id, w.kudos, wt.tag_id
		  FROM works w JOIN work_tags wt ON wt.work_id = w.id
		 ORDER BY w.id`)
	if err != nil {
		return nil, fmt.Errorf("worktree: read work tags: %w", err)
	}
	for wrows.Next() {
		var wid int64
		var kudos int
		var tid int64
		if err := wrows.Scan(&wid, &kudos, &tid); err != nil {
			_ = wrows.Close()
			return nil, fmt.Errorf("worktree: scan work tag: %w", err)
		}
		v, ok := emb[tid]
		if !ok {
			continue
		}
		a := accs[wid]
		if a == nil {
			a = &acc{sum: make([]float64, len(v)), kudo: kudos}
			accs[wid] = a
		}
		w := 1.0
		if s := support[tid]; s > 0 {
			w = 1 / math.Sqrt(float64(s))
		}
		for d := range v {
			a.sum[d] += float64(v[d]) * w
		}
		a.n++
	}
	if err := wrows.Err(); err != nil {
		_ = wrows.Close()
		return nil, fmt.Errorf("worktree: work tags: %w", err)
	}
	_ = wrows.Close()

	out := make([]WorkVec, 0, len(accs))
	for wid, a := range accs {
		if a.n < o.MinTags {
			continue
		}
		v := make([]float32, len(a.sum))
		var norm float64
		for d := range a.sum {
			v[d] = float32(a.sum[d])
			norm += a.sum[d] * a.sum[d]
		}
		if norm == 0 {
			continue
		}
		// Unit length, so Euclidean distance in Build is cosine distance.
		norm = math.Sqrt(norm)
		for d := range v {
			v[d] = float32(float64(v[d]) / norm)
		}
		out = append(out, WorkVec{ID: wid, V: v, Kudos: a.kudo, NTags: a.n})
	}
	// Deterministic order: kudos desc, then id, so a cap keeps the same works every run.
	sort.Slice(out, func(a, b int) bool {
		if out[a].Kudos != out[b].Kudos {
			return out[a].Kudos > out[b].Kudos
		}
		return out[a].ID < out[b].ID
	})
	if o.MaxWorks > 0 && len(out) > o.MaxWorks {
		out = out[:o.MaxWorks]
	}
	return out, nil
}

// AsVecs converts to the generic clustering input.
func AsVecs(ws []WorkVec) []Vec {
	out := make([]Vec, len(ws))
	for i, w := range ws {
		out[i] = Vec{ID: w.ID, V: w.V, Support: w.Kudos}
	}
	return out
}

// ThemeLabel is one tag proposed as part of a theme's name.
type ThemeLabel struct {
	TagID int64
	Name  string
	// Share is the fraction of the cluster's works carrying the tag.
	Share float64
	// Lift is Share divided by the tag's share across all clustered works.
	Lift float64
}

// LabelThemes names each family by the tags CONCENTRATED in it.
//
// Two filters, both of which exist because the unfiltered version is useless:
//
//   - MinLabelShare: a tag on 2 of 400 works is a coincidence, however concentrated.
//   - MinLift: a tag as common inside the cluster as outside says nothing about the cluster.
//     Without it every theme is named "fluff" and "angst", which is what the first run produced.
func LabelThemes(families [][]int64, names map[int64]string, tagged map[int64][]int64, o WorkOpts) [][]ThemeLabel {
	// Global share: how many clustered works carry each tag.
	global := map[int64]int{}
	total := 0
	for _, ws := range families {
		for _, wid := range ws {
			total++
			for _, tid := range tagged[wid] {
				global[tid]++
			}
		}
	}
	out := make([][]ThemeLabel, len(families))
	if total == 0 {
		return out
	}
	for fi, ws := range families {
		count := map[int64]int{}
		for _, wid := range ws {
			for _, tid := range tagged[wid] {
				count[tid]++
			}
		}
		var labels []ThemeLabel
		for tid, n := range count {
			share := float64(n) / float64(len(ws))
			if share < o.MinLabelShare {
				continue
			}
			g := float64(global[tid]) / float64(total)
			if g == 0 {
				continue
			}
			lift := share / g
			if lift < o.MinLift {
				continue
			}
			labels = append(labels, ThemeLabel{TagID: tid, Name: names[tid], Share: share, Lift: lift})
		}
		sort.Slice(labels, func(a, b int) bool {
			// Lift first: a tag three times over-represented names the theme better than one
			// that is merely frequent. Share breaks ties, then the name for determinism.
			if labels[a].Lift != labels[b].Lift {
				return labels[a].Lift > labels[b].Lift
			}
			if labels[a].Share != labels[b].Share {
				return labels[a].Share > labels[b].Share
			}
			return labels[a].Name < labels[b].Name
		})
		if len(labels) > o.LabelTags {
			labels = labels[:o.LabelTags]
		}
		out[fi] = labels
	}
	return out
}

// ThemeName joins a labels list into one string.
func ThemeName(labels []ThemeLabel) string {
	if len(labels) == 0 {
		return "(unnamed)"
	}
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, ShortTag(l.Name))
	}
	return strings.Join(parts, " · ")
}

// ShortTag trims a tag name to something that fits beside two others.
//
// A theme label is three tag names and a column: "akimichi chouji · haruno sakura & uchiha
// sasuke & uzumaki naruto · uchiha sasuke" is 84 characters of a 52-character row, so the label
// a reader sees is the first tag and an ellipsis. Cutting at a WORD boundary keeps it readable
// ("akimichi chouji · haruno sakura & uchiha…").
func ShortTag(name string) string {
	const max = 26
	r := []rune(name)
	if len(r) <= max {
		return name
	}
	cut := string(r[:max])
	if i := strings.LastIndex(cut, " "); i > 10 {
		cut = cut[:i]
	}
	return cut + "…"
}
