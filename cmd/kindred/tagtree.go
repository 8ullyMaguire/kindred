package main

// `kindred tagtree` -- build the tag hierarchy from the embeddings (#37).
//
// The tree lands in two tables in the state database:
//
//	tag_family        id, label, parent (NULL at the top), size
//	tag_family_member family, tag_id, dist
//
// Parent is what makes it a TREE rather than a partition: a subfamily row points at its family,
// and a family row has no parent. One table would have to encode the level in the id, which is
// the same mistake as an id that means different things in different rows.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/tagfamily"
	"git.polarisocial.xyz/kindred/kindred/internal/tagtree"
)

func runTagTree(ctx context.Context, args []string) error {
	fs := newFlagSet("tagtree")
	c := bindConfig(fs)
	var (
		k          = fs.Int("k", 120, "top-level families")
		subK       = fs.Int("sub-k", 6, "subfamilies inside each family (0 disables the second level)")
		minSupport = fs.Int("min-support", 25, "drop tags carried by fewer works")
		maxTags    = fs.Int("max-tags", 40000, "cap the corpus, highest support first")
		balance    = fs.Float64("balance", 0, "cap a family at this multiple of the average size (0 = uncapped; see the Options comment)")
		write      = fs.Bool("write", false, "write tag_family / tag_family_member to the state database")
		asJSON     = fs.Bool("json", false, "print the report as JSON")
		show       = fs.Int("show", 8, "print this many families with their labels and top tags")
	)
	c, err := finishConfig(c, fs, args)
	if err != nil {
		return err
	}

	state, err := sql.Open("sqlite", "file:"+c.DB+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return fmt.Errorf("open state database: %w", err)
	}
	defer state.Close()
	corpus, err := sql.Open("sqlite", "file:"+c.CorpusDB+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return fmt.Errorf("open corpus: %w", err)
	}
	defer corpus.Close()

	o := tagtree.DefaultOptions()
	o.K, o.SubK, o.MinSupport, o.MaxTags = *k, *subK, *minSupport, *maxTags
	o.Balance = *balance

	vecs, err := tagtree.Load(ctx, state, corpus, o)
	if err != nil {
		return err
	}
	if len(vecs) == 0 {
		return fmt.Errorf("no tag has an embedding at support >= %d", o.MinSupport)
	}
	// Two builds: the second with another seed measures whether the tree is a property of the
	// data or of the seed. Cheap (a few seconds) and the only stability number available without
	// ground truth.
	tree, err := tagtree.BuildAndMeasure(vecs, o)
	if err != nil {
		return err
	}

	// Names, for labels and for the report: a tree of tag ids is not reviewable by a person.
	names, err := tagNames(ctx, corpus, tree)
	if err != nil {
		return err
	}
	tagtree.Labels(tree, names)

	purity, err := purityReport(ctx, corpus, tree, names)
	if err != nil {
		return err
	}

	if *write {
		if err := writeTree(ctx, state, tree); err != nil {
			return err
		}
	}

	type famOut struct {
		ID      int      `json:"id"`
		Label   string   `json:"label"`
		Size    int      `json:"size"`
		Subs    int      `json:"subfamilies"`
		TopTags []string `json:"top_tags"`
	}
	fams := make([]famOut, 0, len(tree.Families))
	// Biggest families first: a 40-tag family says more about the archive than a 3-tag one.
	sorted := append([]tagtree.Family(nil), tree.Families...)
	sort.Slice(sorted, func(a, b int) bool {
		if len(sorted[a].Members) != len(sorted[b].Members) {
			return len(sorted[a].Members) > len(sorted[b].Members)
		}
		return sorted[a].ID < sorted[b].ID
	})
	for _, f := range sorted {
		var top []string
		for i, m := range f.Members {
			if i == 5 {
				break
			}
			top = append(top, names[m.ID])
		}
		fams = append(fams, famOut{ID: f.ID, Label: f.Label, Size: len(f.Members),
			Subs: len(f.Subfamilies), TopTags: top})
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"tags":          tree.Tags,
			"k":             tree.K,
			"dim":           tree.Dim,
			"iterations":    tree.Iterations,
			"inertia":       tree.Inertia,
			"own_cosine":    tree.OwnCos,
			"random_cosine": tree.RandomCos,
			"families":      fams,
			"purity":        purity,
		})
	}

	fmt.Printf("tag tree (#37): %d tags, dim %d, %d families, %d iterations, inertia %.1f\n",
		tree.Tags, tree.Dim, len(tree.Families), tree.Iterations, tree.Inertia)
	fmt.Printf("coherence: own cosine %.3f against %.3f for a neighbouring family (gap %.3f)\n",
		tree.OwnCos, tree.RandomCos, tree.OwnCos-tree.RandomCos)
	fmt.Printf("stability: %.2f (adjusted Rand against a second seed), largest family %.0f%% of tags\n",
		tree.Stability, tree.Biggest*100)
	if tree.Biggest > 0.10 {
		// Named rather than silently accepted: the biggest family is where this hierarchy is
		// weakest, and a reader who wants it split further should know that is the choice.
		fmt.Printf("note: the largest family holds %.0f%% of the archive -- one dense region of the\n"+
			"      embedding space (popular character/pairing tags). --balance 1.5 evens the sizes but\n"+
			"      measured stability halves; the subfamily level is where this one is made usable.\n",
			tree.Biggest*100)
	}
	fmt.Println()

	n := *show
	if n > len(fams) {
		n = len(fams)
	}
	for _, f := range fams[:n] {
		fmt.Printf("  [%3d] %-34s %4d tags  %d subfamilies\n        %s\n",
			f.ID, f.Label, f.Size, f.Subs, strings.Join(f.TopTags, " · "))
	}

	if len(purity) > 0 {
		fmt.Println("\nrecovery of the hand-written universes (nothing told the clustering about them):")
		oks, measured, skipped := 0, 0, 0
		for _, p := range purity {
			if !p.Measured {
				skipped++
				fmt.Printf("    %-22s %d of %d spellings in the tree -- too few to measure\n",
					p.Name, p.Tags, p.Spellings)
				continue
			}
			measured++
			mark := " "
			if p.Share >= 0.6 {
				oks++
				mark = "*"
			}
			fmt.Printf("  %s %-22s %2d of %2d spellings, purity %.2f, family %d\n",
				mark, p.Name, p.Tags, p.Spellings, p.Share, p.Family)
		}
		fmt.Printf("  %d of %d measured universes have most of their tags in one family", oks, measured)
		if skipped > 0 {
			fmt.Printf(" (%d had too few tags in the tree to measure)", skipped)
		}
		fmt.Println()
	}
	if *write {
		fmt.Println("\nwrote tag_family and tag_family_member")
	}
	return nil
}

type universePurity struct {
	Name string `json:"name"`
	// Spellings is how many of the universe's spellings the tree CONTAINS, and Embedded is how
	// many of those were clustered. A purity over three tags is not evidence of anything; the
	// counts are here so a reader can see that before trusting the number.
	Spellings int     `json:"spellings"`
	Tags      int     `json:"tags"`
	Share     float64 `json:"share"`
	Family    int     `json:"family"`
	Measured  bool    `json:"measured"`
}

// purityReport measures the tree against `families.json`, the only hand-written ground truth.
//
// A universe whose tags are not all embedded is measured over the ones that are, and the count
// travels with the share: "8 of 9 tags together" and "1 of 1 tags together" are both 1.0 and
// only one of them is evidence.
func purityReport(ctx context.Context, corpus *sql.DB, tree *tagtree.Tree, names map[int64]string) ([]universePurity, error) {
	fams, err := tagfamily.LoadFamilies()
	if err != nil {
		// No families.json is not an error: the tree stands on its own.
		return nil, nil
	}
	byName := map[string]int64{}
	for id, n := range names {
		byName[strings.ToLower(n)] = id
	}
	// Which ids the tree actually holds, so a spelling that exists but was filtered by support
	// is not counted as clustered.
	inTree := map[int64]bool{}
	for _, f := range tree.Families {
		for _, m := range f.Members {
			inTree[m.ID] = true
		}
	}
	var out []universePurity
	for _, name := range fams.UniverseNames() {
		u, ok := fams.Universe(name)
		if !ok {
			continue
		}
		// Resolve every spelling the mirror has, not only the ones the clustering kept: the
		// point of the measurement is how many of a universe's KNOWN tags exist at all.
		var resolved []int64
		for _, sp := range u.Spellings {
			for _, cand := range []string{sp, sp + " - fandom", sp + " - freeform"} {
				if id, ok := byName[strings.ToLower(cand)]; ok {
					resolved = append(resolved, id)
					break
				}
			}
		}
		ids := intersect(resolved, inTree)
		if len(resolved) < 3 {
			continue
		}
		share, cluster, _ := tagtree.Purity(tree, ids)
		out = append(out, universePurity{
			Name: u.Label, Spellings: len(resolved), Tags: len(ids),
			Share: share, Family: cluster,
			// Five is the floor: with fewer, one tag moving changes the answer by more than 20
			// points, and a number that unstable is a rumour with a decimal point.
			Measured: len(ids) >= 5,
		})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Share != out[b].Share {
			return out[a].Share > out[b].Share
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}

func intersect(ids []int64, keep map[int64]bool) []int64 {
	out := ids[:0]
	seen := map[int64]bool{}
	for _, id := range ids {
		if keep[id] && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func tagNames(ctx context.Context, corpus *sql.DB, tree *tagtree.Tree) (map[int64]string, error) {
	out := map[int64]string{}
	var ids []int64
	for _, f := range tree.Families {
		for _, m := range f.Members {
			ids = append(ids, m.ID)
		}
	}
	const chunk = 500
	for i := 0; i < len(ids); i += chunk {
		j := i + chunk
		if j > len(ids) {
			j = len(ids)
		}
		part := ids[i:j]
		args := make([]any, len(part))
		for k, id := range part {
			args[k] = id
		}
		rows, err := corpus.QueryContext(ctx,
			`SELECT id, name FROM tags WHERE id IN (`+questionMarks(len(part))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("tag names: %w", err)
		}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("tag names: %w", err)
			}
			out[id] = name
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("tag names: %w", err)
		}
		_ = rows.Close()
	}
	return out, nil
}

// subfamilyBase keeps subfamily ids out of the family id space: families are 0..K-1 and
// subfamilies start here. 100 slots per family is above the 32 cap subK can reach.
const subfamilyBase = 1_000_000

func questionMarks(n int) string {
	if n == 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func writeTree(ctx context.Context, state *sql.DB, tree *tagtree.Tree) error {
	tx, err := state.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("tag tree write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS tag_family (
			id INTEGER PRIMARY KEY,
			parent INTEGER,
			label TEXT NOT NULL,
			size INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS tag_family_member (
			tag_id INTEGER PRIMARY KEY,
			family INTEGER NOT NULL,
			dist REAL NOT NULL)`,
		`DELETE FROM tag_family_member`,
		`DELETE FROM tag_family`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("tag tree write: %w", err)
		}
	}
	// Ids: a family is its own index (0..K-1); a subfamily is 1,000,000 + family*100 + sub, so
	// it names its parent arithmetically and cannot collide with a family. The first encoding
	// was (family<<8 | sub), which collides the moment K > 256 -- family 256 IS "family 1,
	// subfamily 0". Measured, not imagined: the write failed on a UNIQUE constraint at k=300.
	for _, f := range tree.Families {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tag_family (id, parent, label, size) VALUES (?, NULL, ?, ?)`,
			f.ID, f.Label, len(f.Members)); err != nil {
			return fmt.Errorf("tag tree write: %w", err)
		}
		subOf := map[int64]int{}
		for _, s := range f.Subfamilies {
			sid := subfamilyBase + f.ID*100 + s.ID
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO tag_family (id, parent, label, size) VALUES (?, ?, ?, ?)`,
				sid, f.ID, s.Label, len(s.Members)); err != nil {
				return fmt.Errorf("tag tree write: %w", err)
			}
			for _, m := range s.Members {
				subOf[m.ID] = sid
			}
		}
		for _, m := range f.Members {
			famID := f.ID
			if sid, ok := subOf[m.ID]; ok {
				famID = sid
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO tag_family_member (tag_id, family, dist) VALUES (?, ?, ?)
				 ON CONFLICT(tag_id) DO UPDATE SET family = excluded.family, dist = excluded.dist`,
				m.ID, famID, m.Dist); err != nil {
				return fmt.Errorf("tag tree write: %w", err)
			}
		}
	}
	return tx.Commit()
}
