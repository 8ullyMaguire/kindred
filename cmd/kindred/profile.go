package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/config"
	"git.polarisocial.xyz/kindred/kindred/internal/corpusquery"
	"git.polarisocial.xyz/kindred/kindred/internal/profile"
)

// runProfile builds, lists, shows and rates taste profiles.
//
// The subverbs are one command because they share a store and a corpus handle,
// and a reader's workflow is "build once, then nudge it" — splitting them into
// separate binaries would mean opening the same database three times.
func runProfile(ctx context.Context, corpusPath string, args []string) error {
	if len(args) == 0 {
		return errors.New(`profile: expected a subcommand
  list                     every stored profile
  show   NAME              one profile's weights
  build  NAME  --works A,B build a profile from liked work ids
  rate   NAME  --work N [--like|--dislike]
                          fold one verdict into a profile`)
	}
	sub, rest := args[0], args[1:]

	// Each subverb parses its OWN flags. A shared flag set would make
	// `profile show reader` reject --works as an unknown flag, and the
	// shared handle it opened would be the read-only mirror (see
	// openProfileStore) which cannot hold a writable profile at all.
	switch sub {
	case "list":
		fs := newFlagSet("profile list")
		c := bindConfig(fs)
		if _, err := finishConfig(c, fs, rest); err != nil {
			return err
		}
		ps, _, closeAll, err := openProfileStore(c)
		if err != nil {
			return err
		}
		defer closeAll()
		names, err := ps.List(ctx)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("profile: none stored; build one with " +
				"`kindred profile build --name me --works 1,2,3`")
			return nil
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil

	case "show":
		fs := newFlagSet("profile show")
		c := bindConfig(fs)
		if _, err := finishConfig(c, fs, rest); err != nil {
			return err
		}
		// Flags must precede the name. Go's flag package stops parsing at the
		// first non-flag argument, so `show reader --corpus X` silently drops
		// --corpus and then fails with "--corpus is required" -- an error that
		// names the wrong problem. Taking the name from the trailing
		// positionals accepts both orders, so the caller is not left
		// guessing which one this subcommand wants.
		name, err := firstPositional(fs.Args(),
			"profile show: expected a profile name")
		if err != nil {
			return err
		}
		ps, _, closeAll, err := openProfileStore(c)
		if err != nil {
			return err
		}
		defer closeAll()
		p, err := ps.Load(ctx, name)
		if err != nil {
			return err
		}
		fmt.Println(p.String())
		return nil

	case "build":
		return profileBuild(ctx, rest)
	case "rate":
		return profileRate(ctx, rest)

	default:
		return fmt.Errorf("profile: unknown subcommand %q", sub)
	}
}

// profileBuild derives a profile from a list of work ids.
func profileBuild(ctx context.Context, args []string) error {
	fs := newFlagSet("profile build")
	name := fs.String("name", "", "profile name (required)")
	ids := fs.String("works", "", "comma-separated liked work ids (required)")
	c := bindConfig(fs)
	if _, err := finishConfig(c, fs, args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("profile build: --name is required")
	}
	if *ids == "" {
		return errors.New("profile build: --works is required")
	}
	workIDs, err := parseInt64List(*ids)
	if err != nil {
		return fmt.Errorf("profile build: --works: %w", err)
	}
	ps, corpus, closeAll, err := openProfileStore(c)
	if err != nil {
		return err
	}
	defer closeAll()

	p, err := profile.BuildFromWorks(ctx, corpus, workIDs, *name)
	if err != nil {
		return err
	}
	if err := ps.Save(ctx, p); err != nil {
		return err
	}
	fmt.Println(p.String())
	return nil
}

// profileRate folds one verdict into a stored profile.
//
// The update is returned to the caller rather than written silently, and the
// caller can pass --save to persist it. A profile that rewrites itself on
// every click with no record of the change is one nobody can debug when the
// recommendations shift.
func profileRate(ctx context.Context, args []string) error {
	fs := newFlagSet("profile rate")
	name := fs.String("name", "", "profile name (required)")
	work := fs.Int64("work", 0, "work id being rated (required)")
	like := fs.Bool("like", false, "this work is a like")
	dislike := fs.Bool("dislike", false, "this work is a dismissal")
	save := fs.Bool("save", false, "persist the updated profile")
	c := bindConfig(fs)
	if _, err := finishConfig(c, fs, args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("profile rate: --name is required")
	}
	if *work == 0 {
		return errors.New("profile rate: --work is required")
	}
	if *like && *dislike {
		return errors.New("profile rate: --like and --dislike are mutually exclusive")
	}
	if !*like && !*dislike {
		// Default to a like: `rate NAME --work 7` reading as "I liked it" is
		// the common case, and requiring a second flag for it is friction.
		*like = true
	}

	ps, corpus, closeAll, err := openProfileStore(c)
	if err != nil {
		return err
	}
	defer closeAll()

	// The work's own tags are the feedback: a verdict on a work is a
	// statement about its tags, and there is no other signal available.
	tags, err := tagsOfWork(ctx, corpus, *work)
	if err != nil {
		return fmt.Errorf("profile rate: %w", err)
	}
	if len(tags) == 0 {
		return fmt.Errorf("profile rate: work %d has no tags in this corpus; nothing to learn", *work)
	}

	p, err := ps.Load(ctx, *name)
	if err != nil {
		if !errors.Is(err, profile.ErrNotFound) {
			return err
		}
		// Rating into a profile that does not exist is a legitimate way to
		// start one, and refusing it would force a build step for someone who
		// only wants to nudge a reader who already knows what they like.
		p = &profile.Profile{Name: *name, Tags: map[int32]float64{}}
	}

	p = profile.Apply(p, profile.Feedback{
		WorkID: *work, Liked: *like, TagIDs: tags,
	})

	if *save {
		if err := ps.Save(ctx, p); err != nil {
			return err
		}
		fmt.Printf("profile %q updated and saved\n", *name)
	} else {
		fmt.Printf("profile %q updated in memory; pass --save to persist\n", *name)
	}
	for _, id := range tags {
		fmt.Printf("  tag %d: %.3f\n", id, p.Tags[id])
	}
	return nil
}

// runRate is an alias for `profile rate`, matching the sibling's verb so a
// muscle-memory `kindred rate` works.
func runRate(ctx context.Context, corpusPath string, args []string) error {
	return runProfile(ctx, corpusPath, append([]string{"rate"}, args...))
}

// ---------------------------------------------------------------- corpus-query

// runCorpusQuery answers a question about the corpus.
//
// This is the `--corpus-query` family from the sibling tool, kept as its own
// subcommand rather than a flag on `recommend`: none of these modes rank
// candidates for a reader, so making them share a code path with ranking would
// mean carrying the seed machinery into queries that have no seeds.
func runCorpusQuery(ctx context.Context, corpusPath string, args []string) error {
	fs := newFlagSet("corpus-query")
	// NOT --mode: config.Bind already registers -mode for lite|full.
	// Registering it twice panics in flag.Var, and the panic surfaces as a
	// stack trace inside a test rather than as a duplicate-flag error.
	queryMode := fs.String("query", string(corpusquery.ModeFandomRanking), "query mode")
	tag := fs.String("tag", "", "tag name, for tag-neighbours")
	profileName := fs.String("profile", "", "taste profile to weight by, for fandom-ranking")
	limit := fs.Int("limit", 100, "maximum rows")
	minCo := fs.Int("min-co-works", corpusquery.DefaultMinCoWorksToRank,
		"evidence gate for fandom-ranking (negative disables)")
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	c := bindConfig(fs)
	if _, err := finishConfig(c, fs, args); err != nil {
		return err
	}

	m, err := corpusquery.ParseMode(*queryMode)
	if err != nil {
		return fmt.Errorf("corpus-query: %w", err)
	}

	// args, not the flag-set-parsed value: the corpus is opened after
	// finishConfig, so the parsed value is authoritative here, but passing
	// args keeps the same resolution rule as the profile subverbs.
	db, closeDB, err := openCorpusForSubcommand(c.CorpusDB, args)
	if err != nil {
		return err
	}
	defer closeDB()

	r := corpusquery.NewRunner(db)
	opts := corpusquery.Options{
		Limit: *limit, MinCoWorks: *minCo, ProfileName: *profileName,
	}

	var res corpusquery.Result
	switch m {
	case corpusquery.ModeFandomRanking:
		var seedTags []int32
		if *profileName != "" {
			// The profile store is the WRITABLE state database, not the
			// read-only mirror this query reads from. Opening the mirror here
			// made --profile fail with "attempt to write a readonly database"
			// on the very call it existed to serve.
			stateDB, err := sql.Open("sqlite", c.DB)
			if err != nil {
				return fmt.Errorf("corpus-query: open state db: %w", err)
			}
			defer stateDB.Close()
			ps := profile.NewStore(stateDB)
			if err := ps.EnsureSchema(ctx); err != nil {
				return err
			}
			seedTags, err = ps.TagIDs(ctx, *profileName, 200)
			if err != nil {
				return err
			}
		}
		res, err = r.FandomRanking(ctx, seedTags, opts)
	case corpusquery.ModeUnderrated:
		res, err = r.Underrated(ctx, opts)
	case corpusquery.ModeTagNeighbours:
		if *tag == "" {
			return errors.New("corpus-query: --tag is required for tag-neighbours")
		}
		res, err = r.TagNeighbours(ctx, *tag, opts)
	default:
		// Reached for a DECLARED-but-unimplemented mode, because ParseMode now
		// accepts those (see corpusquery.ParseMode). The message names the
		// working modes rather than just refusing, because the caller has a
		// correct mode name and wants to know what to use instead.
		working := make([]string, 0, len(corpusquery.Modes()))
		for _, w := range corpusquery.Modes() {
			working = append(working, string(w))
		}
		return fmt.Errorf(
			"corpus-query: mode %q is declared but not implemented; working modes: %s",
			m, strings.Join(working, ", "))
	}
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	if len(res.Rows) == 0 {
		fmt.Printf("corpus-query %s: no rows\n", res.Mode)
	}
	for i, row := range res.Rows {
		fmt.Printf("%3d  %-38.38s  %8.4f  works=%-6d co=%-6d %s\n",
			i+1, row.Label, row.Score, row.Works, row.CoWorks, row.Note)
	}
	for _, n := range res.Notes {
		fmt.Fprintf(os.Stderr, "note: %s\n", n)
	}
	if res.Truncated {
		fmt.Fprintf(os.Stderr, "note: truncated at %d rows\n", len(res.Rows))
	}
	return nil
}

// ---------------------------------------------------------------- helpers

// tagsOfWork returns a work's tag ids, deduplicated.
//
// Deduplication is required, not cosmetic: work_tags has PK
// (work_id, tag_id, tag_type), so one tag can sit on a work under two types
// and a raw read hands the same id to Apply twice, doubling that tag's weight.
func tagsOfWork(ctx context.Context, db *sql.DB, workID int64) ([]int32, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT tag_id FROM work_tags WHERE work_id = ?`, workID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int32
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// openCorpusForSubcommand opens the corpus read-only.
//
// Read-only is the contract (SPEC §3.2.1): the mirror is the shared artefact
// and a peer snapshot is verified against it, so a command that could write to
// it could invalidate a signature. That is also why profiles do NOT live here --
// see openProfileStore.
//
// --corpus is re-read from args rather than trusted from the environment,
// because the environment value is a machine default and the flag is what a
// caller puts in a script. The flag has to win.
func openCorpusForSubcommand(path string, args []string) (*sql.DB, func(), error) {
	path = corpusPathFrom(args, path)
	if path == "" {
		return nil, nil, errors.New(
			"no corpus configured; set KINDRED_CORPUS_DB or pass --corpus PATH")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, nil, fmt.Errorf("open corpus %s: %w", path, err)
	}
	return db, func() { db.Close() }, nil
}

// openProfileStore returns the profile store plus the corpus handle.
//
// They are SEPARATE databases on purpose. The mirror is read-only and shared;
// a profile is per-reader, mutable, and holds tag weights, so putting it in
// the mirror would (a) fail, because that handle is mode=ro, and (b) mean a
// reader's preferences travel in a snapshot meant for peers. Both are wrong,
// and the first one is only visible as "attempt to write a readonly database".
func openProfileStore(c *config.Config) (*profile.Store, *sql.DB, func(), error) {
	statePath := c.DB
	if statePath == "" {
		return nil, nil, nil, errors.New("no state database configured; pass --db PATH")
	}
	state, err := sql.Open("sqlite", statePath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open state db %s: %w", statePath, err)
	}
	if _, err := state.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS profiles(
			name TEXT PRIMARY KEY,
			source TEXT NOT NULL DEFAULT '',
			works INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS profile_tags(
			name TEXT NOT NULL,
			tag_id INTEGER NOT NULL,
			weight REAL NOT NULL,
			PRIMARY KEY(name, tag_id))`); err != nil {
		state.Close()
		return nil, nil, nil, fmt.Errorf("profile schema: %w", err)
	}

	corpus, closeCorpus, err := openCorpusForSubcommand(c.CorpusDB, nil)
	if err != nil {
		state.Close()
		return nil, nil, nil, err
	}
	return profile.NewStore(state), corpus,
		func() { state.Close(); closeCorpus() }, nil
}

// firstPositional returns the single positional argument, erroring clearly on
// none or many. Subverbs that take a name as a bare word rather than a flag
// otherwise fail with a flag-package error about an unexpected argument, which
// does not tell the caller what was expected.
func firstPositional(args []string, usage string) (string, error) {
	switch len(args) {
	case 0:
		return "", errors.New(usage)
	case 1:
		return args[0], nil
	default:
		return "", fmt.Errorf("%s; got %d arguments", usage, len(args))
	}
}

// corpusPathFrom finds --corpus in args, accepting both --corpus=X and
// --corpus X. Falls back to the environment-derived default.
//
// It scans raw args rather than using flag.FlagSet because each subverb owns a
// different flag set and the corpus handle is needed before the subverb's own
// flags are parsed; a pre-scan avoids binding a second flag set just to read
// one string.
func corpusPathFrom(args []string, def string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--corpus" || a == "-corpus" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if v, ok := strings.CutPrefix(a, "--corpus="); ok {
			return v
		}
	}
	return def
}

func parseInt64List(s string) ([]int64, error) {
	var out []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var n int64
		if _, err := fmt.Sscanf(part, "%d", &n); err != nil {
			return nil, fmt.Errorf("%q is not a number", part)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("no ids given")
	}
	return out, nil
}
