package main

// `kindred harvest -backfill-types` -- give the pre-existing 112k works AO3's real tag categories.
//
// The mirror's work_tags.tag_type is 'freeforms' for essentially every row the original crawl
// wrote (3.89M freeform rows against ~110 fandom rows across 112,953 works), because the tool that
// built it flattened the categories. Anything fandom-aware -- "which fandom is this work in",
// "recommend by character", a fandom page -- cannot be built on that column while it says the same
// thing for every tag.
//
// The categories are recoverable WITHOUT fetching 112k work pages: a listing page carries twenty
// works with their typed tags, and the works in the mirror are exactly the popular ones that a
// fandom's kudos ordering surfaces first. So this sweeps fandom listings and rewrites the type of
// each tag it recognises, leaving anything it does not touch alone.
//
// It is idempotent and its own resume state: a row that is no longer 'freeforms' is not updated
// again, so an interrupted run continues from the listings it had reached.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
	"git.polarisocial.xyz/kindred/kindred/internal/harvest"
)

func runBackfillTypes(ctx context.Context, f harvest.Fetcher, db *sql.DB, cat *harvest.Catalogue, base string, maxPages, maxFandoms, target int) error {
	fandoms := append([]harvest.Fandom(nil), cat.Fandoms...)
	// Biggest fandoms first: that is where the mirror's works are, since the original crawl
	// followed popularity.
	sort.Slice(fandoms, func(i, j int) bool {
		if fandoms[i].Works != fandoms[j].Works {
			return fandoms[i].Works > fandoms[j].Works
		}
		return fandoms[i].Name < fandoms[j].Name
	})
	if maxFandoms > 0 && len(fandoms) > maxFandoms {
		fandoms = fandoms[:maxFandoms]
	}

	hasWork, err := db.PrepareContext(ctx, `SELECT 1 FROM works WHERE id = ?`)
	if err != nil {
		return err
	}
	defer hasWork.Close()
	// OR IGNORE because a work can already hold the same tag id under the type this update would
	// write: the primary key is (work_id, tag_id, tag_type), so a no-op move is a constraint
	// violation, not an error worth stopping a 20-hour run for.
	updateType, err := db.PrepareContext(ctx, `
		UPDATE OR IGNORE work_tags SET tag_type = ?
		WHERE work_id = ? AND tag_type = 'freeforms'
		  AND tag_id = (SELECT id FROM tags WHERE name = ?)`)
	if err != nil {
		return err
	}
	defer updateType.Close()

	annotated := map[int64]bool{}
	start := time.Now()
	pages := 0
	retagged := 0
	for i, fan := range fandoms {
		if err := ctx.Err(); err != nil {
			return err
		}
		if target > 0 && len(annotated) >= target {
			break
		}
		for page := 1; page <= maxPages; page++ {
			rawURL := fmt.Sprintf("%s/tags/%s/works?%s&page=%d", base, url.PathEscape(fan.Path),
				url.Values{"work_search[sort_column]": {"kudos_count"}, "work_search[sort_direction]": {"desc"}}.Encode(), page)
			body, err := f.Fetch(ctx, rawURL)
			pages++
			if err != nil {
				if page == 1 {
					fmt.Printf("  ! %s: %v\n", fan.Name, err)
				}
				break
			}
			blurbs, err := crawl.ParseListing(body)
			if err != nil || len(blurbs) == 0 {
				break
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			for _, b := range blurbs {
				if annotated[b.ID] {
					continue
				}
				var one int
				if err := hasWork.QueryRowContext(ctx, b.ID).Scan(&one); err == sql.ErrNoRows {
					continue // not in the mirror: nothing to retype
				} else if err != nil {
					tx.Rollback()
					return err
				}
				annotated[b.ID] = true
				for _, tag := range b.Tags {
					if tag.Type == "" {
						continue
					}
					res, err := tx.StmtContext(ctx, updateType).ExecContext(ctx, tag.Type, b.ID, tag.Name)
					if err != nil {
						tx.Rollback()
						return fmt.Errorf("work %d tag %q: %w", b.ID, tag.Name, err)
					}
					if n, _ := res.RowsAffected(); n > 0 {
						retagged += int(n)
					}
				}
			}
			if err := tx.Commit(); err != nil {
				return err
			}
		}
		if (i+1)%10 == 0 || i == 0 {
			rate := time.Since(start).Seconds() / float64(pages)
			left := ""
			if target > 0 && len(annotated) > 0 {
				perPage := float64(len(annotated)) / float64(pages)
				if perPage > 0 {
					left = fmt.Sprintf(", ~%.0f page(s) to go", float64(target-len(annotated))/perPage)
				}
			}
			fmt.Printf("  %-40.40s works %6d  retagged %7d  pages %4d  %.1fs/page%s\n",
				fan.Name, len(annotated), retagged, pages, rate, left)
		}
	}
	fmt.Printf("\nbackfill: %d work(s) annotated, %d tag row(s) retyped, %d page(s) in %s\n",
		len(annotated), retagged, pages, time.Since(start).Round(time.Second))
	return nil
}

// backfillFromCLI is the entry point the flag table calls.
func backfillFromCLI(ctx context.Context, args []string) error {
	fs := newFlagSet("harvest")
	c := bindConfig(fs)
	var (
		catalog   = fs.String("catalog", "harvest-catalog.json", "fandom catalogue")
		maxPages  = fs.Int("max-pages", 5, "listing pages per fandom")
		maxFandom = fs.Int("max-fandoms", 0, "stop after N fandoms (0 = all)")
		target    = fs.Int("target-works", 112953, "stop once this many works are annotated")
		delay     = fs.Duration("crawl-delay", 0, "delay between requests (0 = robots.txt, else 30s)")
		pauseAf   = fs.Int("pause-after", 8, "consecutive failures before a pause")
		pauseFor  = fs.Duration("pause", 5*time.Minute, "how long to pause")
		backfill  = fs.Bool("backfill-types", false, "retype the mirror's flattened tag categories")
	)
	if _, err := finishConfig(c, fs, args); err != nil {
		return err
	}
	_ = *backfill // the flag exists so `harvest -backfill-types` parses; this function IS that mode

	base := strings.TrimRight(os.Getenv("KINDRED_AO3_BASE"), "/")
	if base == "" {
		base = "https://archiveofourown.org"
	}
	cat, err := loadCatalogue(*catalog)
	if err != nil {
		return err
	}
	client := crawl.NewHTTPClient(base)
	if *delay > 0 {
		client = client.WithDelay(*delay)
	}
	defer client.Close()
	f := &patientFetch{
		inner: crawlerFetch{crawl.New(client)}, limit: *pauseAf, pause: *pauseFor,
		logf: func(format string, a ...any) { fmt.Printf(format+"\n", a...) },
	}
	db, err := sql.Open("sqlite", "file:"+c.CorpusDB+"?_pragma=busy_timeout(30000)")
	if err != nil {
		return fmt.Errorf("open corpus: %w", err)
	}
	defer db.Close()
	return runBackfillTypes(ctx, f, db, cat, base, *maxPages, *maxFandom, *target)
}

// loadCatalogueOrNil loads the fandom index for the server, and says why /media is off when it
// cannot: an empty index rendered as a page reads as "the archive has no fandoms".
func loadCatalogueOrNil(logger *slog.Logger, path string) *harvest.Catalogue {
	if strings.TrimSpace(path) == "" {
		logger.Info("no fandom catalogue loaded; /media will answer 503",
			"hint", "pass --catalogue /path/to/harvest-catalog.json")
		return nil
	}
	cat, err := loadCatalogue(path)
	if err != nil {
		logger.Warn("fandom catalogue unavailable; /media will answer 503", "path", path, "err", err)
		return nil
	}
	logger.Info("fandom catalogue loaded",
		"media", len(cat.Media), "fandoms", len(cat.Fandoms), "path", path)
	return cat
}

// loadCatalogue reads the catalogue file written by -catalog-fetch.
func loadCatalogue(path string) (*harvest.Catalogue, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("catalogue %s: %w (run -catalog-fetch first)", path, err)
	}
	var cat harvest.Catalogue
	if err := json.Unmarshal(body, &cat); err != nil {
		return nil, fmt.Errorf("catalogue %s: %w", path, err)
	}
	if len(cat.Fandoms) == 0 {
		return nil, fmt.Errorf("catalogue %s holds no fandoms", path)
	}
	return &cat, nil
}
