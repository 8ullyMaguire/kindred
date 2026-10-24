package main

// `kindred velocity` -- #48's own gate, as a command.
//
// The row refuses to fit a weight to a distribution that does not exist, and that refusal is
// right. What it left behind is a measurement somebody has to redo by hand every session, from
// a sentence in a markdown file. This prints the same numbers with the same threshold and a
// verdict, so the row's next step is a command's output rather than an archaeology exercise.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

func runVelocity(ctx context.Context, args []string) error {
	fs := newFlagSet("velocity")
	c := bindConfig(fs)
	var (
		asJSON = fs.Bool("json", false, "print the report as JSON")
		top    = fs.Int("owners", 10, "print this many owners, fastest first")
	)
	c, err := finishConfig(c, fs, args)
	if err != nil {
		return err
	}

	s, err := store.Open(ctx, c.DB, c.CorpusDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	rep, err := s.Velocity(ctx)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"owners":         rep.Owners,
			"marks":          rep.Marks,
			"reader_days":    rep.ReaderDays,
			"owners_with_5":  rep.WithFiveDays,
			"p50_per_day":    rep.P50,
			"p90_per_day":    rep.P90,
			"meets_gate":     rep.MeetsGate(),
			"why":            rep.Why,
			"owners_by_pace": rep.OwnerRows,
		})
	}

	fmt.Printf("reader velocity (#48): %d owner(s), %d marked work(s), %d reader-day(s)\n\n",
		rep.Owners, rep.Marks, rep.ReaderDays)
	if rep.Owners == 0 {
		fmt.Println(rep.Why)
		return nil
	}
	fmt.Printf("  owners with 5+ reader-days   %d of %d\n", rep.WithFiveDays, rep.Owners)
	fmt.Printf("  marks per reader-day  p50    %.2f\n", rep.P50)
	fmt.Printf("  marks per reader-day  p90    %.2f\n", rep.P90)
	fmt.Println()
	if rep.MeetsGate() {
		fmt.Println("GATE PASSED:", rep.Why)
	} else {
		fmt.Println("GATE NOT MET:", rep.Why)
	}

	if *top > 0 && len(rep.OwnerRows) > 0 {
		n := *top
		if n > len(rep.OwnerRows) {
			n = len(rep.OwnerRows)
		}
		fmt.Printf("\nfastest %d:\n", n)
		fmt.Printf("  %-16s %6s %7s %8s\n", "owner", "marks", "days", "per day")
		for _, o := range rep.OwnerRows[:n] {
			// The owner key is a hash. Eight characters is enough to tell two rows apart in a
			// report and not enough to be a usable identifier on its own.
			short := o.Owner
			if len(short) > 8 {
				short = short[:8]
			}
			fmt.Printf("  %-16s %6d %7d %8.2f\n", short, o.Marks, o.ReaderDays, o.PerDay)
		}
	}
	return nil
}
