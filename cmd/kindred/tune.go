package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/config"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
)

// runTune inspects or sets the signal weights.
//
// The tune is a named weight vector over signals (SPEC §3.4), stored in the
// state database so it survives a restart and can be changed without a
// rebuild. `kindred tune` is what the DefaultTune comment promises when it
// says peer_rating's weight "should earn its place".
//
// ## What is validated, and why it is not more
//
// Weights must be non-negative and sum to 1. Non-negative because rank.Score
// renormalises by the sum of the weights actually in force, so a negative
// weight silently inverts that signal's contribution rather than reducing it.
// Sum-to-1 because the sum IS the scale: a vector summing to 4.0 produces the
// same ranking as one summing to 1.0, so storing it unsummed would let two
// byte-different rows mean the same thing.
//
// Unknown signal names are refused rather than ignored. An ignored typo leaves
// the reader believing they have re-weighted neighbourhood when they have
// re-weighted nothing, which is the same class of bug as the silent no-op
// diversity cap.
func runTune(ctx context.Context, args []string) error {
	fs := newFlagSet("tune")
	var (
		name    = fs.String("name", "default", "tune name")
		set     = fs.String("set", "", "signal=weight pairs, comma separated")
		listAll = fs.Bool("list", false, "list every stored tune")
		asJSON  = fs.Bool("json", false, "emit JSON")
	)
	c := bindConfig(fs)
	if _, err := finishConfig(c, fs, args); err != nil {
		return err
	}

	db, err := openStateDB(c)
	if err != nil {
		return err
	}
	defer db.Close()

	if *listAll {
		return tuneList(ctx, db)
	}
	if *set != "" {
		return tuneSet(ctx, db, *name, *set)
	}
	return tuneShow(ctx, db, *name, *asJSON)
}

func tuneList(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT name FROM tunes ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("tune: none stored; `kindred tune --set tag_overlap=0.3,neighbourhood=0.4,...` writes one")
		return nil
	}
	for _, n := range names {
		fmt.Println(n)
	}
	return nil
}

func tuneShow(ctx context.Context, db *sql.DB, name string, asJSON bool) error {
	t, stored, err := loadTune(ctx, db, name)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"name": t.Name, "weights": t.Weights, "stored": stored,
		})
	}
	if !stored {
		fmt.Printf("tune %q (built-in defaults; nothing stored)\n", t.Name)
	} else {
		fmt.Printf("tune %q (stored)\n", t.Name)
	}
	printWeights(t.Weights)
	return nil
}

// printWeights prints heaviest first, which is the order that tells a reader
// what is actually driving the ranking.
func printWeights(w map[string]float64) {
	keys := make([]string, 0, len(w))
	for k := range w {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return w[keys[i]] > w[keys[j]] })
	for _, k := range keys {
		fmt.Printf("  %-14s %.4f\n", k, w[k])
	}
}

// tuneSet parses and writes a weight vector.
func tuneSet(ctx context.Context, db *sql.DB, name, spec string) error {
	weights, err := parseTuneSpec(spec)
	if err != nil {
		return err
	}
	if err := validateTune(weights); err != nil {
		return err
	}
	blob, err := json.Marshal(weights)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tunes(name, weights) VALUES(?,?)
		 ON CONFLICT(name) DO UPDATE SET weights=excluded.weights`,
		name, string(blob)); err != nil {
		return fmt.Errorf("tune: write %q: %w", name, err)
	}
	fmt.Printf("tune %q saved:\n", name)
	printWeights(weights)
	return nil
}

// parseTuneSpec reads "a=0.3,b=0.7" into a weight map.
func parseTuneSpec(spec string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("%q is not signal=weight", part)
		}
		k = strings.TrimSpace(k)
		w, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil, fmt.Errorf("weight for %q is not a number: %w", k, err)
		}
		out[k] = w
	}
	if len(out) == 0 {
		return nil, errors.New("no weights given")
	}
	return out, nil
}

// validateTune checks the weight vector against the signals this build knows.
//
// It validates against DefaultTune's key set rather than against the stored
// row, so a tune written by a build with an extra signal is refused here
// rather than silently losing that signal at score time.
func validateTune(w map[string]float64) error {
	known := engine.DefaultTune().Weights
	sum := 0.0
	for k, v := range w {
		if _, ok := known[k]; !ok {
			names := make([]string, 0, len(known))
			for n := range known {
				names = append(names, n)
			}
			sort.Strings(names)
			return fmt.Errorf("unknown signal %q; this build has: %s",
				k, strings.Join(names, ", "))
		}
		if v < 0 {
			return fmt.Errorf("weight for %q is negative (%v); rank.Score renormalises "+
				"by the sum, so a negative weight inverts the signal rather than "+
				"reducing it", k, v)
		}
		sum += v
	}
	// A tolerance rather than == 0: the caller wrote decimals, and the shipped
	// defaults do not sum to exactly 1.0 in binary floating point. Being
	// pedantic here would reject them.
	if diff := sum - 1.0; diff > 1e-6 || diff < -1e-6 {
		return fmt.Errorf("weights sum to %.6f, want 1.0; the sum IS the scale, so a "+
			"vector summing to something else stores a value that means the same "+
			"thing as a different one", sum)
	}
	return nil
}

// loadTune reads a stored tune, falling back to the built-in defaults.
//
// The fallback is reported in the second return value so the CLI can print
// "nothing stored" instead of presenting an unstored default as if it had been
// persisted.
func loadTune(ctx context.Context, db *sql.DB, name string) (rank.Tune, bool, error) {
	var blob string
	err := db.QueryRowContext(ctx, `SELECT weights FROM tunes WHERE name = ?`, name).
		Scan(&blob)
	if err == sql.ErrNoRows {
		d := engine.DefaultTune()
		if name != "" && name != d.Name {
			// An unknown name must not silently get the default's weights: a
			// typo'd tune name that resolves to a working tune is worse than
			// an error.
			return rank.Tune{}, false, fmt.Errorf(
				"no tune named %q (stored: %s; the built-in default is %q)",
				name, storedNames(ctx, db), d.Name)
		}
		return d, false, nil
	}
	if err != nil {
		return rank.Tune{}, false, fmt.Errorf("tune: read %q: %w", name, err)
	}
	var w map[string]float64
	if err := json.Unmarshal([]byte(blob), &w); err != nil {
		return rank.Tune{}, false, fmt.Errorf("tune %q is corrupt: %w", name, err)
	}
	return rank.Tune{Name: name, Weights: w}, true, nil
}

func storedNames(ctx context.Context, db *sql.DB) string {
	rows, err := db.QueryContext(ctx, `SELECT name FROM tunes ORDER BY name`)
	if err != nil {
		return "none"
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return "none"
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

// openStateDB opens kindred's own state database.
//
// It ensures the tunes table rather than trusting the schema: a state file
// written by a build predating the table would otherwise fail on the first
// write with "no such table", which reads as corruption rather than as a
// version gap.
func openStateDB(c *config.Config) (*sql.DB, error) {
	db, err := sql.Open("sqlite", c.DB)
	if err != nil {
		return nil, fmt.Errorf("open state db %s: %w", c.DB, err)
	}
	if _, err := db.ExecContext(context.Background(),
		`CREATE TABLE IF NOT EXISTS tunes(
			name TEXT PRIMARY KEY,
			weights TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		db.Close()
		return nil, fmt.Errorf("tune: ensure schema: %w", err)
	}
	return db, nil
}
