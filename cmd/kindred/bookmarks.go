package main

// `kindred bookmarks` -- read a reader's PUBLIC AO3 bookmarks into the mirror, and turn them into
// the taste map the harvester and the recommender both need.
//
// Why this exists: the recommender's collaborative signals are starved (180,677 interaction rows
// over 6,261 readers, no ratings at all), and the harvest that just started is choosing 22,600
// works with a ten-tag guess. A reader's own bookmarks are the one taste signal that is BOTH
// theirs and free: AO3 publishes them at /users/<name>/bookmarks unless the reader made them
// private, 20 works a page, with the same blurb markup the crawler already parses.
//
// Two writes, and the difference matters:
//   - works rows are INSERT OR IGNORE: a bookmark must be able to point at its work, but an
//     existing row (with its summary, from a real page) is better than a blurb's.
//   - interactions are checked with NOT EXISTS rather than INSERT OR IGNORE, because the table's
//     key includes seed_work_id and this import writes NULL there: SQLite treats NULLs as
//     distinct, so OR IGNORE would happily insert the same bookmark every run.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
)

func runBookmarks(ctx context.Context, args []string) error {
	fs := newFlagSet("bookmarks")
	c := bindConfig(fs)
	var (
		user     = fs.String("user", "", "AO3 username whose public bookmarks to read")
		pages    = fs.Int("pages", 0, "stop after N pages (0 = until the listing ends)")
		doImport = fs.Bool("import", false, "write the bookmarks into the corpus")
		tasteOut = fs.String("taste-out", "", "write a taste map built from the bookmarks to this path")
		minCount = fs.Int("min-count", 2, "a tag must appear on this many bookmarked works to enter the taste map")
		maxTags  = fs.Int("max-tags", 300, "cap the taste map at this many tags")
		delay    = fs.Duration("crawl-delay", 0, "delay between requests (0 = robots.txt, else 30s)")
	)
	if _, err := finishConfig(c, fs, args); err != nil {
		return err
	}
	name := strings.TrimSpace(*user)
	if name == "" {
		return fmt.Errorf("bookmarks: -user is required (the AO3 username)")
	}

	base := strings.TrimRight(os.Getenv("KINDRED_AO3_BASE"), "/")
	if base == "" {
		base = "https://archiveofourown.org"
	}
	client := crawl.NewHTTPClient(base)
	if *delay > 0 {
		client = client.WithDelay(*delay)
	}
	defer client.Close()
	f := &patientFetch{
		// Six attempts over ~3 minutes: a reader's bookmark list is READ ONCE (the import is
		// incremental and idempotent), so spending three minutes to get a page beats failing and
		// having to be restarted by hand during an AO3 storm.
		inner: crawlerFetch{crawl.New(client)}, limit: 6, pause: 5 * time.Minute, attempts: 6,
		logf: func(format string, a ...any) { fmt.Printf(format+"\n", a...) },
	}

	corpus, err := sql.Open("sqlite", "file:"+c.CorpusDB+"?_pragma=busy_timeout(30000)")
	if err != nil {
		return fmt.Errorf("open corpus: %w", err)
	}
	defer corpus.Close()

	var all []crawl.Bookmark
	totalInserted := 0
	page := 1
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if *pages > 0 && page > *pages {
			break
		}
		rawURL := fmt.Sprintf("%s/users/%s/bookmarks?page=%d", base, url.PathEscape(name), page)
		body, err := f.Fetch(ctx, rawURL)
		if err != nil {
			// A page failing after the first is an outage, not an empty listing. What has been
			// read is IMPORTED before returning: the first version collected every page and
			// imported once at the end, so a 525 on page 2 threw away page 1 as well.
			if page == 1 {
				return fmt.Errorf("bookmarks page 1: %w", err)
			}
			fmt.Printf("  stopped at page %d: %v (importing the %d bookmark(s) read so far)\n", page, err, len(all))
			break
		}
		got, err := crawl.ParseBookmarks(body)
		if err != nil {
			if page == 1 {
				return fmt.Errorf("bookmarks page 1: %w", err)
			}
			fmt.Printf("  stopped at page %d: %v\n", page, err)
			break
		}
		if len(got) == 0 {
			break
		}
		all = append(all, got...)
		// Import page by page: a long import is a series of small steps that each survive an
		// interruption, not one step at the end that does not.
		if *doImport {
			n, err := importBookmarks(ctx, corpus, base, name, got)
			if err != nil {
				return fmt.Errorf("import page %d: %w", page, err)
			}
			totalInserted += n
		}
		fmt.Printf("  page %-3d %3d bookmark(s)  (total %d, imported %d)\n", page, len(got), len(all), totalInserted)
		page++
	}
	if len(all) == 0 {
		fmt.Printf("%s has no public bookmarks (or the account name is wrong)\n", name)
		return nil
	}

	report(name, all)

	if *doImport {
		fmt.Printf("imported: %d new interaction(s) for %s\n", totalInserted, name)
	}

	if *tasteOut != "" {
		taste := tasteFromBookmarks(all, *minCount, *maxTags)
		if err := writeJSON(*tasteOut, taste); err != nil {
			return err
		}
		fmt.Printf("taste: %d tag(s) written to %s\n", len(taste), *tasteOut)
	}
	return nil
}

// report says what was read, in the terms the next step needs.
func report(name string, bs []crawl.Bookmark) {
	fandoms := map[string]int{}
	tags := map[string]int{}
	for _, b := range bs {
		for _, f := range b.Fandoms {
			fandoms[f]++
		}
		for _, t := range b.Tags {
			tags[strings.ToLower(t.Name)]++
		}
	}
	fmt.Printf("\n%s: %d bookmarked work(s), %d fandom(s), %d distinct tag(s)\n",
		name, len(bs), len(fandoms), len(tags))
	for _, kv := range topN(fandoms, 8) {
		fmt.Printf("  %-44.44s %3d\n", kv.k, kv.n)
	}
	fmt.Println("most frequent tags:")
	for _, kv := range topN(tags, 10) {
		fmt.Printf("  %-44.44s %3d\n", kv.k, kv.n)
	}
}

type counted struct {
	k string
	n int
}

func topN(m map[string]int, n int) []counted {
	out := make([]counted, 0, len(m))
	for k, v := range m {
		out = append(out, counted{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].k < out[j].k
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// tasteFromBookmarks weights a tag by how often it appears across the reader's bookmarks.
//
// Normalised against the most frequent tag so the strongest signal is 1.0, which is what the
// harvester's Score expects (it compares an average affinity against log-scaled quality). Tags on
// a single work are dropped by default: one bookmark is an anecdote, and a taste map built from
// them would be as long as the list it came from.
func tasteFromBookmarks(bs []crawl.Bookmark, minCount, maxTags int) map[string]float64 {
	counts := map[string]int{}
	for _, b := range bs {
		for _, t := range b.Tags {
			counts[strings.ToLower(strings.TrimSpace(t.Name))]++
		}
	}
	top := topN(counts, maxTags)
	if len(top) == 0 {
		return map[string]float64{}
	}
	max := float64(top[0].n)
	out := make(map[string]float64, len(top))
	for _, kv := range top {
		if kv.n < minCount {
			continue
		}
		out[kv.k] = float64(kv.n) / max
	}
	return out
}

// importBookmarks writes the works and the bookmark interactions in one transaction.
func importBookmarks(ctx context.Context, db *sql.DB, base, name string, bs []crawl.Bookmark) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	userURL := base + "/users/" + url.PathEscape(name)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (url, username, first_seen, last_updated) VALUES (?, ?, ?, ?)
		ON CONFLICT(url) DO UPDATE SET last_updated = excluded.last_updated`,
		userURL, name, now, now); err != nil {
		return 0, fmt.Errorf("user row: %w", err)
	}
	var userID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE url = ?`, userURL).Scan(&userID); err != nil {
		return 0, fmt.Errorf("user id: %w", err)
	}

	inserted := 0
	for _, b := range bs {
		workURL := fmt.Sprintf("%s/works/%d", base, b.WorkID)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO works (id, url, title, authors, rating, complete, first_seen, last_updated)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO NOTHING`,
			b.WorkID, workURL, b.Title, strings.Join(b.Authors, ", "), b.Rating, boolInt(b.Complete), now, now); err != nil {
			return 0, fmt.Errorf("work %d: %w", b.WorkID, err)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO user_work_interactions (user_id, work_id, interaction_type, seed_work_id, timestamp)
			SELECT ?, ?, 'bookmarked', NULL, ?
			WHERE NOT EXISTS (
				SELECT 1 FROM user_work_interactions
				WHERE user_id = ? AND work_id = ? AND interaction_type = 'bookmarked' AND seed_work_id IS NULL)`,
			userID, b.WorkID, now, userID, b.WorkID)
		if err != nil {
			return 0, fmt.Errorf("interaction %d: %w", b.WorkID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET bookmark_count = ? WHERE id = ?`, len(bs), userID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
