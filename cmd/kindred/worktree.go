package main

// `kindred worktree` -- cluster works by theme and name the clusters (#47).
//
//	tag_tag   the tag embeddings (32-dim, 634k rows)
//	work_tags which tags each work carries
//	→ one vector per work (inverse-sqrt weighted mean of its tags, unit length)
//	→ k-means families + subfamilies (the same machinery as #37)
//	→ work_family / work_family_member, named by the tags CONCENTRATED in each family
//
// The label is the part that decides whether this is a feature or a table. "The biggest tags"
// names every theme "fluff · angst · alternate universe"; the tags over-represented inside a
// cluster and rare outside it name an actual theme.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/tagtree"
)

func runWorkTree(ctx context.Context, args []string) error {
	fs := newFlagSet("worktree")
	c := bindConfig(fs)
	var (
		k        = fs.Int("k", 200, "top-level themes")
		subK     = fs.Int("sub-k", 6, "subthemes inside each theme (0 disables the second level)")
		maxWorks = fs.Int("max-works", 60000, "cap the corpus, highest kudos first")
		minTags  = fs.Int("min-tags", 3, "drop works carrying fewer tags")
		write    = fs.Bool("write", false, "write work_family / work_family_member to the state database")
		asJSON   = fs.Bool("json", false, "print the report as JSON")
		show     = fs.Int("show", 10, "print this many themes")
		samples  = fs.Int("samples", 3, "print this many sample works per theme")
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

	wo := tagtree.DefaultWorkOpts()
	wo.MinTags, wo.MaxWorks = *minTags, *maxWorks

	works, err := tagtree.LoadWorks(ctx, state, corpus, wo)
	if err != nil {
		return err
	}
	if len(works) == 0 {
		return fmt.Errorf("no work has %d tags with embeddings", wo.MinTags)
	}

	o := tagtree.DefaultOptions()
	o.K, o.SubK = *k, *subK
	// Works are already unit vectors and the mean-centring that helps tag embeddings is wrong
	// here: the shared popularity direction IS what a theme is (many works about the same
	// popular thing), and subtracting it would cut the themes apart.
	o.Center = false
	o.Balance = 0

	tree, err := tagtree.BuildAndMeasure(tagtree.AsVecs(works), o)
	if err != nil {
		return err
	}

	families := make([][]int64, len(tree.Families))
	for i, f := range tree.Families {
		for _, m := range f.Members {
			families[i] = append(families[i], m.ID)
		}
	}

	// Tag assignments per work, for the labels: one pass, and only for the works in the tree.
	tagged, err := workTags(ctx, corpus, families)
	if err != nil {
		return err
	}
	names, err := tagNameMap(ctx, corpus, tagged)
	if err != nil {
		return err
	}
	labels := tagtree.LabelThemes(families, names, tagged, wo)
	for i := range tree.Families {
		tree.Families[i].Label = tagtree.ThemeName(labels[i])
	}

	titles, err := workTitles(ctx, corpus, families, *samples)
	if err != nil {
		return err
	}

	if *write {
		if err := writeWorkTree(ctx, state, tree); err != nil {
			return err
		}
	}

	type themeOut struct {
		ID      int      `json:"id"`
		Label   string   `json:"label"`
		Size    int      `json:"size"`
		Subs    int      `json:"subfamilies"`
		Labels  []string `json:"labels"`
		Samples []string `json:"samples"`
	}
	// Biggest first: a theme of 40 works is a curiosity, one of 3,000 is a way into the archive.
	order := make([]int, len(tree.Families))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		fa, fb := tree.Families[order[a]], tree.Families[order[b]]
		if len(fa.Members) != len(fb.Members) {
			return len(fa.Members) > len(fb.Members)
		}
		return fa.ID < fb.ID
	})
	var themes []themeOut
	for _, i := range order {
		f := tree.Families[i]
		var ls []string
		for _, l := range labels[i] {
			ls = append(ls, fmt.Sprintf("%s (%.1fx)", l.Name, l.Lift))
		}
		themes = append(themes, themeOut{
			ID: f.ID, Label: f.Label, Size: len(f.Members), Subs: len(f.Subfamilies),
			Labels: ls, Samples: titles[f.ID],
		})
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"works": len(works), "k": tree.K, "themes": themes,
			"coherence": tree.OwnCos, "random_cosine": tree.RandomCos,
			"stability": tree.Stability, "largest_share": tree.Biggest,
			"iterations": tree.Iterations,
		})
	}

	fmt.Printf("work themes (#47): %d works, %d themes, %d iterations\n", len(works), tree.K, tree.Iterations)
	fmt.Printf("coherence: own cosine %.3f against %.3f for a neighbouring theme (gap %.3f)\n",
		tree.OwnCos, tree.RandomCos, tree.OwnCos-tree.RandomCos)
	fmt.Printf("stability: %.2f (adjusted Rand against a second seed), largest theme %.0f%% of works\n\n",
		tree.Stability, tree.Biggest*100)

	n := *show
	if n > len(themes) {
		n = len(themes)
	}
	for _, t := range themes[:n] {
		fmt.Printf("  [%3d] %-52s %5d works\n", t.ID, shorten(t.Label, 52), t.Size)
		for _, s := range t.Samples {
			fmt.Printf("        · %s\n", shorten(s, 96))
		}
	}
	if *write {
		fmt.Println("\nwrote work_family and work_family_member")
	}
	return nil
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// workTags reads the tag ids of the works in the tree, in one pass.
func workTags(ctx context.Context, corpus *sql.DB, families [][]int64) (map[int64][]int64, error) {
	want := map[int64]bool{}
	for _, ws := range families {
		for _, w := range ws {
			want[w] = true
		}
	}
	out := make(map[int64][]int64, len(want))
	rows, err := corpus.QueryContext(ctx, `SELECT work_id, tag_id FROM work_tags`)
	if err != nil {
		return nil, fmt.Errorf("work tags: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var wid, tid int64
		if err := rows.Scan(&wid, &tid); err != nil {
			return nil, fmt.Errorf("work tags: %w", err)
		}
		if want[wid] {
			out[wid] = append(out[wid], tid)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("work tags: %w", err)
	}
	return out, nil
}

func tagNameMap(ctx context.Context, corpus *sql.DB, tagged map[int64][]int64) (map[int64]string, error) {
	seen := map[int64]bool{}
	var ids []int64
	for _, ts := range tagged {
		for _, t := range ts {
			if !seen[t] {
				seen[t] = true
				ids = append(ids, t)
			}
		}
	}
	out := make(map[int64]string, len(ids))
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

// workTitles fetches a few of each theme's highest-kudos works, for the report and the page.
func workTitles(ctx context.Context, corpus *sql.DB, families [][]int64, per int) (map[int][]string, error) {
	out := make(map[int][]string, len(families))
	if per <= 0 {
		return out, nil
	}
	for fi, ws := range families {
		if len(ws) == 0 {
			continue
		}
		ids := ws
		if len(ids) > per {
			ids = ids[:per]
		}
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		rows, err := corpus.QueryContext(ctx,
			`SELECT id, title FROM works WHERE id IN (`+questionMarks(len(ids))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("work titles: %w", err)
		}
		byID := map[int64]string{}
		for rows.Next() {
			var id int64
			var title string
			if err := rows.Scan(&id, &title); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("work titles: %w", err)
			}
			byID[id] = title
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("work titles: %w", err)
		}
		_ = rows.Close()
		for _, id := range ids {
			if t, ok := byID[id]; ok {
				out[fi] = append(out[fi], t)
			}
		}
	}
	return out, nil
}

func writeWorkTree(ctx context.Context, state *sql.DB, tree *tagtree.Tree) error {
	tx, err := state.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("work tree write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS work_family (
			id INTEGER PRIMARY KEY,
			parent INTEGER,
			label TEXT NOT NULL,
			size INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS work_family_member (
			work_id INTEGER PRIMARY KEY,
			family INTEGER NOT NULL,
			dist REAL NOT NULL)`,
		`DELETE FROM work_family_member`,
		`DELETE FROM work_family`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("work tree write: %w", err)
		}
	}
	for _, f := range tree.Families {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO work_family (id, parent, label, size) VALUES (?, NULL, ?, ?)`,
			f.ID, f.Label, len(f.Members)); err != nil {
			return fmt.Errorf("work tree write: %w", err)
		}
		subOf := map[int64]int{}
		for _, s := range f.Subfamilies {
			sid := subfamilyBase + f.ID*100 + s.ID
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO work_family (id, parent, label, size) VALUES (?, ?, ?, ?)`,
				sid, f.ID, s.Label, len(s.Members)); err != nil {
				return fmt.Errorf("work tree write: %w", err)
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
				`INSERT INTO work_family_member (work_id, family, dist) VALUES (?, ?, ?)
				 ON CONFLICT(work_id) DO UPDATE SET family = excluded.family, dist = excluded.dist`,
				m.ID, famID, m.Dist); err != nil {
				return fmt.Errorf("work tree write: %w", err)
			}
		}
	}
	return tx.Commit()
}
