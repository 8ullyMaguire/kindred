package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/wormisekai"
)

// runWormIsekai answers one reader question over the corpus mirror: recent
// works in the Worm universe that match an isekai taste, plus Worm stories
// that cross into another universe.
//
// It is a subcommand rather than a corpus-query mode because the question is
// an intersection of two tag families with a recency window, and the answer
// needs to say WHICH family matched. None of the corpus-query modes express
// that join, and bolting a flag onto one of them would make it a
// different question in every mode.
func runWormIsekai(ctx context.Context, corpusPath string, args []string) error {
	fs := newFlagSet("worm-isekai")
	limit := fs.Int("n", wormisekai.DefaultLimit, "number of recommendations")
	recent := fs.Int("recent", wormisekai.DefaultRecentDays, "recency window in days")
	concept := fs.String("concept", "", "comma-separated concept tags (default: the isekai family)")
	strict := fs.Bool("strict", false, "use only the strict isekai/transmigration concept tags")
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	out := fs.String("out", "", "write the JSON payload to this path as well")
	c := bindConfig(fs)
	if _, err := finishConfig(c, fs, args); err != nil {
		return err
	}

	db, closeDB, err := openCorpusForSubcommand(c.CorpusDB, args)
	if err != nil {
		return err
	}
	defer closeDB()

	opts := wormisekai.Options{
		Limit:      *limit,
		RecentDays: *recent,
	}
	if *strict {
		opts.ConceptTags = wormisekai.StrictConceptTags
	}
	if *concept != "" {
		opts.ConceptTags = splitTags(*concept)
	}

	res, err := wormisekai.NewRunner(db).Run(ctx, opts)
	if err != nil {
		return err
	}

	payload, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, append(payload, '\n'), 0o644); err != nil {
			return fmt.Errorf("worm-isekai: write %s: %w", *out, err)
		}
	}
	if *asJSON {
		fmt.Println(string(payload))
		return nil
	}
	printWormTable(res)
	return nil
}

// printWormTable prints the human-readable view.
func printWormTable(res wormisekai.Result) {
	fmt.Printf("worm x isekai: %d matched (showing %d) | universe=%d inside=%d into=%d window=%dd\n\n",
		res.Meta.Matched, res.Meta.Returned, res.Meta.UniverseWorks,
		res.Meta.InsideCandidates, res.Meta.IntoCandidates, res.Meta.RecentDays)
	for _, w := range res.Items {
		extra := ""
		if w.Direction == wormisekai.DirectionInto {
			extra = " -> " + w.OtherFandom
		} else if len(w.ConceptTags) > 0 {
			extra = " [" + strings.Join(w.ConceptTags, ", ") + "]"
		}
		fmt.Printf("%-9d %-6s %5dd %5dk %7dw  %s%s\n",
			w.ID, w.Direction, w.AgeDays, w.Kudos, w.Words, w.Title, extra)
	}
}

// splitTags splits a comma-separated tag list, trimming spaces.
func splitTags(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}
