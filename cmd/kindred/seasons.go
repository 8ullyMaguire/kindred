package main

// `kindred seasons` -- which tags belong to which month (#80).
//
// Detection only. The table it writes (`tag_season`) is a fact about the archive; using it to
// move a ranking is #44's calendar term, and that is not here.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/tagseason"
)

func runSeasons(ctx context.Context, args []string) error {
	fs := newFlagSet("seasons")
	c := bindConfig(fs)
	var (
		minSupport = fs.Int("min-support", 80, "dated works that must carry the tag")
		minLift    = fs.Float64("min-lift", 1.5, "how much more often the tag appears in its month")
		minYear    = fs.Float64("min-year-share", 0.75, "share of the tag's years whose peak month must agree")
		minYears   = fs.Int("min-years", 3, "years that must have voted (a one-year tag is viral, not seasonal)")
		month      = fs.String("month", "", "show one month (12, dec, december)")
		write      = fs.Bool("write", false, "write tag_season to the state database")
		asJSON     = fs.Bool("json", false, "print the report as JSON")
		top        = fs.Int("top", 6, "tags to print per month")
	)
	c, err := finishConfig(c, fs, args)
	if err != nil {
		return err
	}

	corpus, err := sql.Open("sqlite", "file:"+c.CorpusDB+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return fmt.Errorf("open corpus: %w", err)
	}
	defer corpus.Close()

	counts, err := tagseason.LoadCounts(ctx, corpus)
	if err != nil {
		return err
	}
	o := tagseason.DefaultOpts()
	o.MinSupport, o.MinLift, o.MinYearShare, o.MinYears = *minSupport, *minLift, *minYear, *minYears
	all := tagseason.Build(counts, o)

	names, err := seasonNames(ctx, corpus, all)
	if err != nil {
		return err
	}

	if *write {
		state, err := sql.Open("sqlite", "file:"+c.DB+"?_pragma=busy_timeout(10000)")
		if err != nil {
			return fmt.Errorf("open state database: %w", err)
		}
		defer state.Close()
		if err := writeSeasons(ctx, state, all); err != nil {
			return err
		}
	}

	if *asJSON {
		type row struct {
			Tag   string  `json:"tag"`
			Month int     `json:"month"`
			Lift  float64 `json:"lift"`
			Works int     `json:"works"`
			Years float64 `json:"year_share"`
		}
		out := make([]row, 0, len(all))
		for _, s := range all {
			out = append(out, row{Tag: names[s.TagID], Month: s.Month, Lift: s.Lift,
				Works: s.Works, Years: s.YearShare})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"dated_works": counts.Total, "undated_works": counts.Undated,
			"seasonal_tags": len(all), "tags": out,
		})
	}

	fmt.Printf("seasonal tags (#80): %d dated works, %d undated, %d tags pass\n",
		counts.Total, counts.Undated, len(all))
	if counts.Total == 0 {
		fmt.Println("no work has a parseable update date; nothing to measure")
		return nil
	}

	// Which months have any signal at all is part of the finding: an archive that posts evenly
	// all year has fewer seasonal tags than one that does not, and that is a fact about the
	// data, not about the model.
	byMonth := tagseason.Keep(all, *top)

	want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	if *month != "" {
		m, err := tagseason.ParseMonth(*month)
		if err != nil {
			return err
		}
		want = []int{m}
	}
	for _, m := range want {
		rows := byMonth[m]
		fmt.Printf("\n%s (%d tags)\n", tagseason.MonthNames[m], len(rows))
		if len(rows) == 0 {
			fmt.Println("  nothing is seasonal in this month")
			continue
		}
		for _, s := range rows {
			fmt.Printf("  %-42s lift %4.1fx  %5d works  %d/%d years agree\n",
				shorten(names[s.TagID], 42), s.Lift, s.Works, int(s.YearShare*float64(s.Years)+0.5), s.Years)
		}
	}
	if *write {
		fmt.Println("\nwrote tag_season")
	}
	return nil
}

func seasonNames(ctx context.Context, corpus *sql.DB, all []tagseason.Season) (map[int64]string, error) {
	out := make(map[int64]string, len(all))
	if len(all) == 0 {
		return out, nil
	}
	ids := make([]int64, 0, len(all))
	for _, s := range all {
		ids = append(ids, s.TagID)
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

func writeSeasons(ctx context.Context, state *sql.DB, all []tagseason.Season) error {
	tx, err := state.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("seasons write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS tag_season (
			tag_id INTEGER NOT NULL,
			month INTEGER NOT NULL,
			lift REAL NOT NULL,
			works INTEGER NOT NULL,
			support INTEGER NOT NULL,
			year_share REAL NOT NULL,
			years INTEGER NOT NULL,
			PRIMARY KEY (tag_id))`,
		`DELETE FROM tag_season`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("seasons write: %w", err)
		}
	}
	// Sorted by month then tag so two writes of the same data produce the same file bytes.
	sorted := append([]tagseason.Season(nil), all...)
	sort.Slice(sorted, func(a, b int) bool {
		if sorted[a].Month != sorted[b].Month {
			return sorted[a].Month < sorted[b].Month
		}
		return sorted[a].TagID < sorted[b].TagID
	})
	for _, s := range sorted {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tag_season (tag_id, month, lift, works, support, year_share, years)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(tag_id) DO UPDATE SET
			  month = excluded.month, lift = excluded.lift, works = excluded.works,
			  support = excluded.support, year_share = excluded.year_share, years = excluded.years`,
			s.TagID, s.Month, s.Lift, s.Works, s.Support, s.YearShare, s.Years); err != nil {
			return fmt.Errorf("seasons write: %w", err)
		}
	}
	return tx.Commit()
}
