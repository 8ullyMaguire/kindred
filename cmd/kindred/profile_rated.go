package main

// The two rated-profile subcommands: import and validate.
//
// They are in one file because they are two halves of ONE question. Import
// says "here are 257 ratings, build what you can"; validate says "was the thing
// you built any good?" A build that cannot be measured is a guess with a
// confidence interval attached, and the reason this file exists rather than
// wiring validate into import is that the answer should be a deliberate
// separate run against a stored profile -- including a profile that was built
// days ago and has since collected feedback.

import (
	"context"
	"errors"
	"fmt"
	"os"

	"git.polarisocial.xyz/kindred/kindred/internal/profile"
	ratedlist "git.polarisocial.xyz/kindred/kindred/internal/ratedlist"
)

// profileImport reads a reader's external reading history and stores it as a
// rated profile.
//
// The whole command is built around one uncomfortable fact: a reader's library
// is not a list of AO3 works. On this reader's own 257-row list, 61% resolved
// against the mirror and the rest were books, fanfics from other archives, and
// works the mirror never crawled -- GnomeBob and USSExplorer have zero rows in
// a 112,935-work mirror despite holding a 10-rated and an 8-rated entry here.
//
// So the import REPORTS rather than assumes. It prints the tier breakdown, the
// unmatched rows, and the coverage per rating band, and it refuses to store
// anything below --min-confidence without saying so. A command that silently
// built a profile from 40% of a library and called it the reader's taste is the
// failure this is shaped to prevent.
func profileImport(ctx context.Context, args []string) error {
	fs := newFlagSet("profile import")
	name := fs.String("name", "", "profile name to store (required)")
	paste := fs.String("paste", "", "file holding `calibredb list` output (required)")
	minConf := fs.Float64("min-confidence", 0.5,
		"drop matches below this confidence; 1.0 keeps exact title+author matches only")
	report := fs.String("report", "", "write the match report here as well as to stdout")
	save := fs.Bool("save", false, "store the profile under --name")
	dryRun := fs.Bool("dry-run", false, "match and report without storing")
	c := bindConfig(fs)
	if _, err := finishConfig(c, fs, args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("profile import: --name is required")
	}
	if *paste == "" {
		return errors.New("profile import: --paste is required")
	}
	if *minConf < 0 || *minConf > 1 {
		return fmt.Errorf("profile import: --min-confidence %v is outside [0,1]", *minConf)
	}

	entries, err := ratedlist.LoadFromFile(*paste)
	if err != nil {
		return err
	}
	ps, corpus, closeAll, err := openProfileStore(c)
	if err != nil {
		return err
	}
	defer closeAll()

	m, err := ratedlist.MatchToCorpus(ctx, corpus, entries)
	if err != nil {
		return err
	}
	m.Summarise()
	body := m.Report()
	fmt.Print(body)
	if *report != "" {
		// The report is written as well as printed because the unmatched list
		// is the part a reader needs to correct BY HAND, and a list they have
		// to scroll a terminal to read is a list they will not correct.
		if err := os.WriteFile(*report, []byte(body), 0o644); err != nil {
			return fmt.Errorf("profile import: write report: %w", err)
		}
		fmt.Printf("report written to %s\n", *report)
	}

	usable := m.Matched(*minConf)
	if len(usable) == 0 {
		return fmt.Errorf("profile import: nothing matched at confidence >= %.2f; "+
			"the reader's library may not overlap this mirror at all", *minConf)
	}
	if *dryRun {
		fmt.Printf("dry run: %d matches usable, nothing stored\n", len(usable))
		return nil
	}
	if !*save {
		fmt.Printf("%d matches usable; pass --save to store\n", len(usable))
		return nil
	}

	works := make([]profile.RatedWork, 0, len(usable))
	for _, e := range usable {
		works = append(works, profile.RatedWork{
			WorkID:     e.WorkID,
			Rating:     e.Rating,
			Confidence: e.Confidence,
		})
	}
	p, err := profile.BuildFromRatings(ctx, corpus, works, *name)
	if err != nil {
		return err
	}
	if err := ps.Save(ctx, p); err != nil {
		return err
	}
	fmt.Printf("\nstored %q: %d works, %d tag weights\n", p.Name, p.Works, len(p.Tags))
	fmt.Println("validate it before trusting it: kindred profile validate " + p.Name)
	return nil
}

// profileValidate measures a stored rated profile against held-out ratings.
//
// It is a separate command, and takes a stored profile, because the interesting
// question is not "did the build work" but "does the profile the reader will
// actually rank with predict the ratings they withheld". Those are different
// runs against possibly different data, and conflating them would let a good
// score on the build's own training set stand in for the real one.
func profileValidate(ctx context.Context, args []string) error {
	fs := newFlagSet("profile validate")
	name := fs.String("name", "", "profile name (required)")
	paste := fs.String("paste", "", "file holding `calibredb list` output (required)")
	holdout := fs.Float64("holdout", 0.2, "fraction of works held out, in (0,1)")
	seed := fs.Int64("seed", 42, "split seed; the same seed reproduces the same split")
	c := bindConfig(fs)
	if _, err := finishConfig(c, fs, args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("profile validate: --name is required")
	}
	if *paste == "" {
		return errors.New("profile validate: --paste is required (the ratings are the ground truth)")
	}

	entries, err := ratedlist.LoadFromFile(*paste)
	if err != nil {
		return err
	}
	ps, corpus, closeAll, err := openProfileStore(c)
	if err != nil {
		return err
	}
	defer closeAll()

	// The profile must exist, or there is nothing to have been right about.
	// This is checked rather than assumed because the most likely failure here
	// is running validate before import, and a Spearman on a freshly built
	// profile would print a confident number for a profile that is not the
	// stored one.
	if _, err := ps.Load(ctx, *name); err != nil {
		return fmt.Errorf("profile validate: %w (build it first with `profile import --name %s`)", err, *name)
	}

	m, err := ratedlist.MatchToCorpus(ctx, corpus, entries)
	if err != nil {
		return err
	}
	works := make([]profile.RatedWork, 0, len(m.Entries))
	for _, e := range m.Matched(0) {
		works = append(works, profile.RatedWork{
			WorkID:     e.WorkID,
			Rating:     e.Rating,
			Confidence: e.Confidence,
		})
	}
	// Every matched work, including unrated ones, is passed: the validator
	// splits by work and drops the unrated from both halves itself, and
	// pre-filtering here would change the split population without saying so.
	res, err := profile.Validate(ctx, corpus, works, profile.ValidateOptions{
		Holdout: *holdout,
		Seed:    *seed,
	})
	if err != nil {
		return err
	}
	m.Summarise()
	fmt.Print(res.String())
	fmt.Printf("\n(matched %d of %d library rows; unmatched rows are absent from the measurement)\n",
		m.Exact+m.Fuzzy+m.TitleOnly, len(m.Entries))
	return nil
}
