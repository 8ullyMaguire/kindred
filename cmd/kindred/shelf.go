package main

// `kindred shelf` -- the reader's shelf: the 100 fandoms they would most enjoy, and the works
// inside each one they are most likely to read next.
//
// This is the question that comes BEFORE a recommendation. `recommend --works 123` answers "more
// like this work"; a reader with 40 bookmarks wants "what should I read at all", and the only
// honest way to answer it is to compare whole fandoms against a taste built from those bookmarks.
// The scoring lives in internal/shelf; this file is the flag surface and the report.
//
// Output is deliberately three artefacts, because they answer different questions:
//
//   - shelf.json     the whole thing, for a program
//   - INDEX.md       the 100 fandoms with their evidence, for a decision
//   - <rank>-<slug>.md  one file per fandom with all its works, for reading
//
// A single 20,000-row markdown file is not a shelf; it is a download.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/harvest"
	"git.polarisocial.xyz/kindred/kindred/internal/shelf"
)

func runShelf(ctx context.Context, args []string) error {
	fs := newFlagSet("shelf")
	c := bindConfig(fs)
	var (
		user      = fs.String("user", "", "AO3 username (case-insensitive; every matching user row is merged)")
		userID    = fs.Int64("user-id", 0, "reader user id, instead of -user")
		catalog   = fs.String("catalog", "harvest-catalog.json", "fandom catalogue")
		fandoms   = fs.Int("fandoms", 100, "how many fandoms the shelf holds")
		per       = fs.Int("per", 200, "works per fandom")
		minWorks  = fs.Int("min-fandom-works", 3, "works in the mirror a fandom needs to be ranked")
		out       = fs.String("out", "shelf.json", "JSON shelf")
		dir       = fs.String("dir", "shelf", "directory for the markdown pages")
		catOut    = fs.String("catalog-out", "", "write a catalogue of the chosen fandoms (for the harvest to fill shortfalls)")
		authorTop = fs.Int("authors", 50, "how many authors the index lists")
		trace     = fs.Bool("trace", false, "print the cost of each phase")
	)
	if _, err := finishConfig(c, fs, args); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+c.CorpusDB+"?mode=ro&_pragma=busy_timeout(30000)")
	if err != nil {
		return fmt.Errorf("open corpus: %w", err)
	}
	defer db.Close()

	ids, name, err := resolveReader(ctx, db, *user, *userID)
	if err != nil {
		return err
	}
	cat, err := loadCatalogue(*catalog)
	if err != nil {
		return err
	}

	res, err := shelf.Build(ctx, db, cat, ids, shelf.Options{
		Fandoms: *fandoms, PerFandom: *per, MinFandomWorks: *minWorks,
		Logf: func(format string, a ...any) {
			if *trace {
				fmt.Printf(format+"\n", a...)
			}
		},
	})
	if err != nil {
		return err
	}
	res.Reader = name

	whyMark := time.Now()
	names := make([]string, len(res.Fandoms))
	for i, f := range res.Fandoms {
		names[i] = f.Name
	}
	whys, err := shelf.Whys(ctx, db, names, 3)
	if err != nil {
		return fmt.Errorf("why: %w", err)
	}
	for i := range res.Fandoms {
		res.Fandoms[i].Why = whys[res.Fandoms[i].Name]
	}
	if *trace {
		fmt.Printf("  %-14s %s\n", "why", time.Since(whyMark).Round(100*time.Millisecond))
	}

	if err := writeShelf(*out, res); err != nil {
		return err
	}
	if err := writeShelfPages(*dir, res, *authorTop); err != nil {
		return err
	}
	if *catOut != "" {
		if err := writeShelfCatalogue(*catOut, res, cat); err != nil {
			return err
		}
	}

	short := 0
	for _, f := range res.Fandoms {
		short += f.ShortBy
	}
	fmt.Printf("\nshelf: %d fandom(s), %d work(s) from the mirror", len(res.Fandoms), res.TotalWorks)
	if short > 0 {
		fmt.Printf("; %d work(s) short of %d per fandom", short, *per)
	}
	fmt.Printf("\n  %s\n  %s/INDEX.md\n", *out, *dir)
	if short > 0 && *catOut == "" {
		fmt.Println("  rerun with -catalog-out to get a harvest plan that fills the shortfall")
	}
	return nil
}

// resolveReader finds every user row for one AO3 name.
//
// AO3 usernames are case-insensitive, and this mirror holds TWO rows for one account that differ
// only in case ('Leather_Release_9057' from the original crawl, 'leather_release_9057' from the
// bookmark import). Using one row loses whatever the other had -- here, 44 bookmarks against 20.
func resolveReader(ctx context.Context, db *sql.DB, user string, id int64) ([]int64, string, error) {
	if id != 0 {
		var name string
		if err := db.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ?`, id).Scan(&name); err != nil {
			return nil, "", fmt.Errorf("user %d: %w", id, err)
		}
		return []int64{id}, name, nil
	}
	user = strings.TrimSpace(user)
	if user == "" {
		return nil, "", fmt.Errorf("shelf: -user or -user-id is required")
	}
	rows, err := db.QueryContext(ctx, `SELECT id, username FROM users WHERE lower(username) = lower(?)`, user)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var (
		ids  []int64
		name = user
	)
	for rows.Next() {
		var id int64
		var n string
		if err := rows.Scan(&id, &n); err != nil {
			return nil, "", err
		}
		ids = append(ids, id)
		if len(n) > len(name) || strings.EqualFold(n, user) {
			name = n
		}
	}
	if len(ids) == 0 {
		return nil, "", fmt.Errorf("shelf: no user named %q in the mirror", user)
	}
	return ids, name, nil
}

func writeShelf(path string, res *shelf.Result) error {
	body, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

// writeShelfPages writes the index and one page per fandom.
func writeShelfPages(dir string, res *shelf.Result, authorTop int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Clear the previous generation: a rebuild after a harvest changes the ranking, so the old
	// 001-... and 002-... pages stay behind as files nothing links to. A directory of 272 pages
	// for a 100-fandom shelf is a directory whose index is the only thing telling the truth.
	if stale, err := filepath.Glob(filepath.Join(dir, "*.md")); err == nil {
		for _, f := range stale {
			if err := os.Remove(f); err != nil {
				return err
			}
		}
	}
	for i, f := range res.Fandoms {
		name := fmt.Sprintf("%03d-%s.md", i+1, slug(f.Name))
		if err := os.WriteFile(filepath.Join(dir, name), fandomPage(i+1, f), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "INDEX.md"), shelfIndex(res, authorTop), 0o644)
}

// fandomPage is one fandom's works, best first.
func fandomPage(rank int, f shelf.Fandom) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# %d. %s\n\n", rank, f.Name)
	fmt.Fprintf(&b, "- score **%.3f** (mean of its best works' taste match)\n", f.Score)
	fmt.Fprintf(&b, "- %d work(s) in the mirror; %d in AO3's catalogue\n", f.MirrorWorks, f.CatalogueWorks)
	if f.Bookmarked > 0 {
		fmt.Fprintf(&b, "- **%d of your bookmarks are in this fandom**\n", f.Bookmarked)
	}
	if len(f.Why) > 0 {
		fmt.Fprintf(&b, "- why: %s\n", strings.Join(f.Why, ", "))
	}
	if f.ShortBy > 0 {
		fmt.Fprintf(&b, "- **%d work(s) short of the shelf size**: the mirror does not hold them yet\n", f.ShortBy)
	}
	fmt.Fprintf(&b, "\n| # | work | author | rating | words | kudos | match |\n")
	fmt.Fprintf(&b, "|--:|---|---|---|---:|------:|------:|\n")
	for i, w := range f.Works {
		title := strings.ReplaceAll(w.Title, "|", "\\|")
		author := strings.ReplaceAll(w.Authors, "|", "\\|")
		state := ""
		if !w.Complete {
			state = " (WIP)"
		}
		fmt.Fprintf(&b, "| %d | [%s](%s)%s | %s | %s | %s | %s | %.2f |\n",
			i+1, title, w.URL, state, author, w.Rating, commas(w.Words), commas(w.Kudos), w.Score)
	}
	return []byte(b.String())
}

// shelfIndex is the front page: the fandoms, the authors, and what the taste was built from.
func shelfIndex(res *shelf.Result, authorTop int) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Shelf for %s\n\n", res.Reader)
	fmt.Fprintf(&b, "%d bookmark(s) read from the mirror; %d fandom(s); %d work(s).\n\n",
		res.Bookmarks, len(res.Fandoms), res.TotalWorks)

	fmt.Fprintf(&b, "## Taste it was built from\n\n")
	for _, t := range topTaste(res.TasteTags, 25) {
		fmt.Fprintf(&b, "- %s (%.2f)\n", t.Name, t.Weight)
	}

	fmt.Fprintf(&b, "\n## Fandoms\n\n")
	fmt.Fprintf(&b, "| # | fandom | score | in mirror | yours | page |\n|--:|---|------:|---------:|-----:|---|\n")
	for i, f := range res.Fandoms {
		name := strings.ReplaceAll(f.Name, "|", "\\|")
		yours := ""
		if f.Bookmarked > 0 {
			yours = fmt.Sprintf("%d", f.Bookmarked)
		}
		fmt.Fprintf(&b, "| %d | %s | %.3f | %s | %s | [open](%03d-%s.md) |\n",
			i+1, name, f.Score, commas(f.MirrorWorks), yours, i+1, slug(f.Name))
	}

	authors := shelf.AuthorsFrom(res, authorTop)
	if len(authors) > 0 {
		fmt.Fprintf(&b, "\n## Authors on the shelf\n\n")
		fmt.Fprintf(&b, "| author | works | fandoms |\n|---|---:|---|\n")
		for _, a := range authors {
			fmt.Fprintf(&b, "| %s | %d | %s |\n", strings.ReplaceAll(a.Name, "|", "\\|"), a.Works,
				strings.Join(a.Fandoms, "; "))
		}
	}
	return []byte(b.String())
}

func topTaste(ts []shelf.TasteTag, n int) []shelf.TasteTag {
	if len(ts) <= n {
		return ts
	}
	return ts[:n]
}

// writeShelfCatalogue writes the chosen fandoms as a harvest catalogue, with the QUOTA the shelf
// still needs rather than AO3's total. Feeding the harvest AO3's sizes would make it plan the
// allocation by a number that says nothing about what this reader is missing.
func writeShelfCatalogue(path string, res *shelf.Result, cat *harvest.Catalogue) error {
	byName := make(map[string]harvest.Fandom, len(cat.Fandoms))
	for _, f := range cat.Fandoms {
		byName[strings.ToLower(f.Name)] = f
	}
	out := harvest.Catalogue{
		Media: cat.Media, MediaPaths: cat.MediaPaths, FetchedAt: cat.FetchedAt,
		Fandoms: []harvest.Fandom{},
	}
	short := 0
	for _, f := range res.Fandoms {
		if f.ShortBy <= 0 {
			continue
		}
		src, ok := byName[strings.ToLower(f.Name)]
		if !ok {
			continue
		}
		// Works is what the harvester allocates from, so it carries the SHORTFALL. A fandom that
		// needs 190 more and one that needs 2 must not be weighted by their catalogue size.
		src.Works = f.ShortBy
		out.Fandoms = append(out.Fandoms, src)
		short += f.ShortBy
	}
	sort.Slice(out.Fandoms, func(i, j int) bool { return out.Fandoms[i].Works > out.Fandoms[j].Works })
	body, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return err
	}
	fmt.Printf("catalogue: %d fandom(s), %d work(s) to harvest -> %s\n", len(out.Fandoms), short, path)
	return nil
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = slugRE.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	if s == "" {
		s = "fandom"
	}
	return s
}

// commas formats an integer with thousands separators -- the shelves are read as tables, and
// 19796 is a number nobody parses at a glance.
func commas(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
