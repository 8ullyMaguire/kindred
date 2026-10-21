package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/tagfamily"
)

// runBrowse answers the general "fandom family x concept family" question.
//
// It replaces the bespoke worm-isekai command: the families are named
// (--universe worm --concept isekai), and the families themselves live in
// internal/tagfamily/families.json, so asking about a different fandom or
// a different idea needs no code change.
func runBrowse(ctx context.Context, corpusPath string, args []string) error {
	fs := newFlagSet("browse")
	universe := fs.String("universe", "", "fandom family key (e.g. worm)")
	concept := fs.String("concept", "", "concept family key (e.g. isekai)")
	recent := fs.Int("recent", tagfamily.DefaultRecentDays, "recency window in days")
	limit := fs.Int("n", tagfamily.DefaultLimit, "number of results")
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	out := fs.String("out", "", "write the JSON payload to this path as well")
	c := bindConfig(fs)
	if _, err := finishConfig(c, fs, args); err != nil {
		return err
	}
	if *universe == "" && *concept == "" {
		return fmt.Errorf("browse: --universe and --concept are both empty; name at least one family")
	}

	db, closeDB, err := openCorpusForSubcommand(c.CorpusDB, args)
	if err != nil {
		return err
	}
	defer closeDB()

	fams, err := tagfamily.LoadFamilies()
	if err != nil {
		return err
	}
	var universeTags, conceptTags []string
	if *universe != "" {
		u, ok := fams.Universe(*universe)
		if !ok {
			return fmt.Errorf("browse: unknown universe %q; known: %s",
				*universe, strings.Join(fams.UniverseNames(), ", "))
		}
		universeTags = u.Spellings
	}
	if *concept != "" {
		cn, ok := fams.Concept(*concept)
		if !ok {
			return fmt.Errorf("browse: unknown concept %q; known: %s",
				*concept, strings.Join(fams.ConceptNames(), ", "))
		}
		conceptTags = cn.Tags
	}

	res, err := tagfamily.Query(ctx, db, tagfamily.QueryOptions{
		UniverseTags: universeTags,
		ConceptTags:  conceptTags,
		RecentDays:   *recent,
		Limit:        *limit,
	})
	if err != nil {
		return err
	}

	payload, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, append(payload, '\n'), 0o644); err != nil {
			return fmt.Errorf("browse: write %s: %w", *out, err)
		}
	}
	if *asJSON {
		fmt.Println(string(payload))
		return nil
	}
	printBrowseTable(res)
	return nil
}

// printBrowseTable prints the human-readable view.
func printBrowseTable(res tagfamily.Result) {
	fmt.Printf("%d matched (showing %d) | universe=%d concept=%d window=%dd\n\n",
		res.Meta.Matched, res.Meta.Returned, res.Meta.UniverseWorks,
		res.Meta.ConceptWorks, res.Meta.RecentDays)
	for _, it := range res.Items {
		extra := ""
		if it.OtherFandom != "" {
			extra = " -> " + it.OtherFandom
		}
		if len(it.MatchedTags) > 0 {
			extra += " [" + strings.Join(it.MatchedTags, ", ") + "]"
		}
		fmt.Printf("%-9d %5dd %5dk %7dw  %s%s\n",
			it.ID, it.AgeDays, it.Kudos, it.Words, it.Title, extra)
	}
}
