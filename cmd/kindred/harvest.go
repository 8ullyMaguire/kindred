package main

// `kindred harvest` -- fill the mirror from AO3's own catalogue, spread across fandoms.
//
// Three steps, each its own invocation so a long run can be inspected between them:
//
//	kindred harvest -catalog-fetch catalog.json      # 11 requests: every media, every fandom
//	kindred harvest -plan -catalog catalog.json      # print the allocation, fetch nothing
//	kindred harvest -run  -catalog catalog.json ...  # listing pages, then work pages
//
// The plan is printed before anything is fetched because the interesting failure here is a
// DISTRIBUTION failure -- 40% of the budget in one fandom -- and it is invisible in a progress
// log that only counts works.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
	"git.polarisocial.xyz/kindred/kindred/internal/harvest"
)

// crawlerFetch adapts the crawler's FetchOne (which carries the delay and the retry ladder) to
// the one-method interface the harvest needs. Going through the client directly would skip both,
// and a harvest that skipped the delay would be rude at a scale nobody notices until it is
// blocked.
type crawlerFetch struct{ c *crawl.Crawler }

func (a crawlerFetch) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	return a.c.FetchOne(ctx, rawURL)
}

// patientFetch wraps a fetcher with a circuit breaker, because AO3's failure mode is not "slow",
// it is "every request answers 525 for twenty minutes at a time" (measured 2026-10-25: the same
// URL that returned 200 in 5s an hour earlier returned 9KB of Cloudflare error for minutes on
// end, and a run that kept retrying at full speed spent that time learning nothing).
//
// After `limit` consecutive failures it pauses; the pause is the answer to "don't wait on a
// challenge indefinitely" that does not give up either -- it stops hammering, says so, and tries
// again, and every URL gets two more attempts after a pause before the caller is told it failed.
type patientFetch struct {
	inner  harvest.Fetcher
	limit  int
	pause  time.Duration
	logf   func(format string, args ...any)
	fails  int
	pauses int
}

func (p *patientFetch) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		body, err := p.inner.Fetch(ctx, rawURL)
		if err == nil {
			p.fails = 0
			return body, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		p.fails++
		if attempt >= 2 {
			return nil, err
		}
		if p.limit > 0 && p.fails >= p.limit {
			p.pauses++
			p.logf("  .. %d consecutive fetch failures; pausing %s before retrying (%d pause(s) so far)",
				p.fails, p.pause, p.pauses)
			if err := sleepCtx(ctx, p.pause); err != nil {
				return nil, err
			}
			p.fails = 0
			continue
		}
		if err := sleepCtx(ctx, 3*time.Second); err != nil {
			return nil, err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func runHarvest(ctx context.Context, args []string) error {
	fs := newFlagSet("harvest")
	c := bindConfig(fs)
	var (
		catalogPath = fs.String("catalog", "harvest-catalog.json", "fandom catalogue (written by -catalog-fetch)")
		fetchCat    = fs.Bool("catalog-fetch", false, "read AO3's media + fandom lists into -catalog")
		planOnly    = fs.Bool("plan", false, "print the allocation and exit without fetching")
		run         = fs.Bool("run", false, "execute the plan")
		target      = fs.Int("target", 22600, "works to add (20% of the current mirror)")
		floor       = fs.Int("floor", 25, "fewest works per selected fandom")
		capPer      = fs.Int("cap", 200, "most works a single fandom may contribute")
		breadth     = fs.Int("breadth", 0, "fandoms to cover (0 = target / (2 x floor))")
		tastePath   = fs.String("taste", "", "JSON map of tag -> weight; empty ranks by quality alone")
		statePath   = fs.String("state", "harvest-state.json", "resume file")
		media       = fs.String("media", "", "only fandoms in this media (substring, case-insensitive)")
		limit       = fs.Int("limit-fandoms", 0, "process at most N fandoms this run (smoke tests)")
		maxPages    = fs.Int("max-pages", 10, "listing pages per fandom")
		workPages   = fs.Bool("work-pages", true, "fetch each selected work's page (summary, language, chapters)")
		delay       = fs.Duration("crawl-delay", 0, "delay between requests (0 = robots.txt, else 30s)")
		quiet       = fs.Bool("quiet", false, "log only per-fandom summaries")
		enrich      = fs.Bool("enrich", false, "fetch work pages for rows that have no summary (a blurb-only pass leaves these)")
		enrichMax   = fs.Int("enrich-max", 0, "stop after N enrichments (0 = no limit)")
		pauseAfter  = fs.Int("pause-after", 8, "consecutive fetch failures before a pause (0 = never)")
		pauseFor    = fs.Duration("pause", 5*time.Minute, "how long to pause once -pause-after is reached")
		abortAfter  = fs.Int("abort-after", 10, "stop after N fandoms in a row produce nothing (0 = never)")
	)
	c, err := finishConfig(c, fs, args)
	if err != nil {
		return err
	}

	base := strings.TrimRight(os.Getenv("KINDRED_AO3_BASE"), "/")
	if base == "" {
		base = "https://archiveofourown.org"
	}
	// Large catalogue pages need more than the default 60s: measured, one media page (1.3 MB)
	// exceeded it and took the whole catalogue read with it.
	client := crawl.NewHTTPClient(base).WithTimeout(3 * time.Minute)
	if *delay > 0 {
		client = client.WithDelay(*delay)
	}
	defer client.Close()
	fetcher := harvest.Fetcher(crawlerFetch{crawl.New(client)})
	fetcher = &patientFetch{inner: fetcher, limit: *pauseAfter, pause: *pauseFor,
		logf: func(f string, a ...any) { fmt.Printf(f+"\n", a...) }}

	if *fetchCat {
		fmt.Printf("reading %s/media ...\n", base)
		cat, err := harvest.CrawlCatalogue(ctx, fetcher, base)
		if err != nil {
			return err
		}
		cat.FetchedAt = time.Now().UTC().Format(time.RFC3339)
		if err := writeJSON(*catalogPath, cat); err != nil {
			return err
		}
		fmt.Printf("wrote %s: %d media, %d fandoms\n", *catalogPath, len(cat.Media), len(cat.Fandoms))
		printMediaSummary(cat)
		return nil
	}

	body, err := os.ReadFile(*catalogPath)
	if err != nil {
		return fmt.Errorf("catalogue %s: %w (run -catalog-fetch first)", *catalogPath, err)
	}
	var cat harvest.Catalogue
	if err := json.Unmarshal(body, &cat); err != nil {
		return fmt.Errorf("catalogue %s: %w", *catalogPath, err)
	}
	fandoms := filterMedia(cat.Fandoms, *media)
	if len(fandoms) == 0 {
		return fmt.Errorf("no fandoms in the catalogue match media %q", *media)
	}

	if *enrich {
		corpus, err := sql.Open("sqlite", "file:"+c.CorpusDB+"?_pragma=busy_timeout(30000)")
		if err != nil {
			return fmt.Errorf("open corpus: %w", err)
		}
		defer corpus.Close()
		return enrichRows(ctx, fetcher, crawl.NewSQLSink(corpus), corpus, base, *enrichMax)
	}

	plan := harvest.Allocate(fandoms, *target, *floor, *capPer, *breadth)
	printPlan(plan, cat, *media)

	if *planOnly || !*run {
		if !*run {
			fmt.Println("\n(no -run: nothing fetched. Add -run to execute.)")
		}
		return nil
	}

	taste := harvest.Taste{}
	if *tastePath != "" {
		taste, err = harvest.LoadTasteFile(*tastePath)
		if err != nil {
			return err
		}
		fmt.Printf("\ntaste: %d tag(s) from %s\n", len(taste), *tastePath)
	}

	corpus, err := sql.Open("sqlite", "file:"+c.CorpusDB+"?_pragma=busy_timeout(30000)")
	if err != nil {
		return fmt.Errorf("open corpus: %w", err)
	}
	defer corpus.Close()
	sink := crawl.NewSQLSink(corpus)

	st, err := harvest.LoadRunState(*statePath)
	if err != nil {
		return err
	}

	byName := make(map[string]harvest.Fandom, len(fandoms))
	for _, f := range fandoms {
		byName[f.Name] = f
	}
	if *limit > 0 {
		plan = trimPlan(plan, *limit)
	}

	opts := harvest.RunOptions{
		Base: base, Plan: plan, ByName: byName, Taste: taste, Sink: sink,
		MaxPages:  *maxPages,
		WorkPages: *workPages, State: st, StatePath: *statePath, AbortAfter: *abortAfter,
		Probe: func(ctx context.Context, id int64) (bool, error) {
			var n int
			err := corpus.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM works WHERE id = ? AND title <> ''`, id).Scan(&n)
			return n > 0, err
		},
		Logf: func(format string, a ...any) { fmt.Printf("  ! "+format+"\n", a...) },
	}
	lastFan := ""
	opts.Progress = func(f harvest.Fandom, done, quota int, id int64) {
		if *quiet && f.Name == lastFan {
			return
		}
		lastFan = f.Name
		fmt.Printf("  %-40.40s %3d/%-3d  work %d  (saved %d)\n", f.Name, done, quota, id, st.Saved)
	}

	fmt.Printf("\nrunning: %d fandom(s), %d work(s) planned, work pages %v\n",
		len(plan.Quota), plan.Total, *workPages)
	start := time.Now()
	rep, runErr := harvest.Run(ctx, fetcher, opts)
	elapsed := time.Since(start)

	if err := writeJSON(*statePath+".report.json", rep); err != nil {
		fmt.Printf("  ! report: %v\n", err)
	}
	fmt.Printf("\ndone in %s: %d saved, %d already held, %d failed, %d listing pages, %d work pages\n",
		elapsed.Round(time.Second), rep.Saved, rep.AlreadyHeld, rep.Failed, rep.ListingPages, rep.WorkPages)
	fmt.Printf("state: %s (cumulative saved %d)\n", *statePath, st.Saved)
	if len(rep.Empty) > 0 {
		fmt.Printf("empty: %d fandom(s), e.g. %s\n", len(rep.Empty), strings.Join(head(rep.Empty, 5), ", "))
	}
	return runErr
}

// enrichRows fills in what a blurb cannot carry: summary, language, chapter count.
//
// The breadth pass stores twenty works per request, which is what makes covering 1,100 fandoms
// affordable; the price is that a blurb has no summary. Those rows are completed here, one work
// page at a time, and the query IS the resume state: a row that has its summary is not selected,
// so an interrupted run continues from wherever it stopped without a bookkeeping file to trust.
func enrichRows(ctx context.Context, f harvest.Fetcher, sink crawl.Sink, db *sql.DB, base string, limit int) error {
	const q = `SELECT id FROM works WHERE (summary IS NULL OR summary = '') ORDER BY id`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	fmt.Printf("enrich: %d work(s) without a summary\n", len(ids))

	start := time.Now()
	done, failed, already := 0, 0, 0
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, err := f.Fetch(ctx, fmt.Sprintf("%s/works/%d", base, id))
		if err != nil {
			failed++
			fmt.Printf("  ! %d: %v\n", id, err)
			continue
		}
		w, err := crawl.ParseWork(body, fmt.Sprintf("%s/works/%d", base, id))
		if err != nil {
			failed++
			fmt.Printf("  ! %d: parse: %v\n", id, err)
			continue
		}
		if w.Title == "" {
			// A page that parsed but carries no title is not this work; leave the row alone rather
			// than overwrite a real title with emptiness.
			already++
			continue
		}
		if err := sink.Save(ctx, w); err != nil {
			failed++
			fmt.Printf("  ! %d: save: %v\n", id, err)
			continue
		}
		done++
		if done%10 == 0 {
			rate := time.Since(start) / time.Duration(done)
			fmt.Printf("  enriched %d/%d (%d failed) %.1fs/work\n", i+1, len(ids), failed, rate.Seconds())
		}
	}
	fmt.Printf("enrich done in %s: %d enriched, %d failed, %d skipped\n",
		time.Since(start).Round(time.Second), done, failed, already)
	return nil
}

// printPlan shows the allocation the way a person checks it: the total, the coverage, and enough
// of the top and the tail to see whether it is a distribution or a stampede.
func printPlan(p harvest.Plan, cat harvest.Catalogue, media string) {
	fmt.Printf("\nplan: %d works across %d fandom(s) (floor %d, cap %d)",
		p.Total, p.Coverage.FandomsPlanned, p.Floor, p.Cap)
	if media != "" {
		fmt.Printf(" in media %q", media)
	}
	fmt.Println()
	fmt.Printf("catalogue: %d fandoms, %d works", p.Coverage.FandomsInCatalogue, p.Coverage.WorksInCatalogue)
	if cat.FetchedAt != "" {
		fmt.Printf(" (read %s ago)", harvest.CatalogueAge(cat.FetchedAt))
	}
	fmt.Printf("\nskipped: %d fandom(s) with fewer than %d works\n", p.Skipped, p.Floor)

	type row struct {
		name string
		n    int
	}
	rows := make([]row, 0, len(p.Quota))
	for k, v := range p.Quota {
		rows = append(rows, row{k, v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].name < rows[j].name
	})
	if len(rows) == 0 {
		fmt.Println("\nno fandom can supply the floor at this target")
		return
	}
	fmt.Println("\nlargest quotas:")
	for i, r := range rows {
		if i == 8 {
			break
		}
		fmt.Printf("  %-46.46s %4d\n", r.name, r.n)
	}
	if len(rows) > 12 {
		fmt.Println("smallest quotas:")
		for i := len(rows) - 4; i < len(rows); i++ {
			fmt.Printf("  %-46.46s %4d\n", rows[i].name, rows[i].n)
		}
	}
	// The share the biggest fandom takes is the number a "distributed" claim has to answer to.
	if p.Total > 0 {
		fmt.Printf("\nlargest fandom share: %.1f%% of the harvest\n", 100*float64(rows[0].n)/float64(p.Total))
	}
}

func printMediaSummary(cat *harvest.Catalogue) {
	type agg struct {
		fandoms int
		works   int
	}
	byMedia := map[string]agg{}
	for _, f := range cat.Fandoms {
		a := byMedia[f.Media]
		a.fandoms++
		a.works += f.Works
		byMedia[f.Media] = a
	}
	names := make([]string, 0, len(byMedia))
	for m := range byMedia {
		names = append(names, m)
	}
	sort.Strings(names)
	fmt.Println()
	for _, m := range names {
		a := byMedia[m]
		fmt.Printf("  %-46.46s %5d fandoms %10d works\n", m, a.fandoms, a.works)
	}
}

func filterMedia(fs []harvest.Fandom, media string) []harvest.Fandom {
	if strings.TrimSpace(media) == "" {
		return fs
	}
	want := strings.ToLower(strings.TrimSpace(media))
	out := make([]harvest.Fandom, 0, len(fs))
	for _, f := range fs {
		if strings.Contains(strings.ToLower(f.Media), want) {
			out = append(out, f)
		}
	}
	return out
}

// trimPlan keeps the N largest quotas, for a smoke run.
func trimPlan(p harvest.Plan, n int) harvest.Plan {
	type row struct {
		name string
		n    int
	}
	rows := make([]row, 0, len(p.Quota))
	for k, v := range p.Quota {
		rows = append(rows, row{k, v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].name < rows[j].name
	})
	if n > len(rows) {
		n = len(rows)
	}
	out := harvest.Plan{Target: p.Target, Floor: p.Floor, Cap: p.Cap, Quota: map[string]int{}}
	for _, r := range rows[:n] {
		out.Quota[r.name] = r.n
		out.Total += r.n
	}
	out.Coverage = p.Coverage
	out.Coverage.FandomsPlanned = n
	return out
}

func head(ss []string, n int) []string {
	if len(ss) < n {
		return ss
	}
	return ss[:n]
}

func writeJSON(path string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
