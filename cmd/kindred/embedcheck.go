package main

// kindred embed-check (#76) -- fingerprint the embedding table WITHOUT rebuilding.
//
// The deploy path already fingerprints after every `kindred embed`, which
// answers the drift question whenever somebody rebuilds. This command is for the
// other two moments:
//
//   - BEFORE a rebuild, so there is a baseline to compare against even if the
//     last build predates the fingerprints (a new deployment has none);
//   - when nothing is being rebuilt and an operator wants to know whether the
//     table they are serving moved -- after a restore, a manual copy, or a
//     suspicious score change.
//
// It writes a fingerprint row, so it is not a read-only check: two invocations
// in a row compare a table with itself, which is what makes it usable as a
// wiring test and useless as a drift test. The output says so.

import (
	"context"
	"flag"
	"fmt"

	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

func runEmbedCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("embed-check", flag.ContinueOnError)
	db := fs.String("db", "", "path to the kindred state database, where the embeddings and the fingerprints live (required)")
	sample := fs.Int("sample", 64, "how many tag vectors to keep for per-tag comparison (max 512)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *db == "" {
		return fmt.Errorf("embed-check needs -db: the fingerprints describe the embeddings table in the state database")
	}
	s, err := store.Open(ctx, *db, "")
	if err != nil {
		return err
	}
	defer s.Close()

	run, err := s.RecordEmbedRun(ctx, *sample)
	if err != nil {
		return err
	}
	fmt.Printf("fingerprint #%d: %d rows, dim %d, mean norm %.4f, %d sampled vectors, %s\n",
		run.ID, run.Rows, run.Dim, run.MeanNorm, len(run.Sample), run.BuiltAt)

	runs, err := s.EmbedRuns(ctx, 2)
	if err != nil {
		return err
	}
	if len(runs) < 2 {
		fmt.Println("no previous fingerprint on this deployment, so there is nothing to compare: " +
			"this is a baseline, not a result")
		return nil
	}
	d := store.DriftBetween(runs[1], runs[0])
	fmt.Printf("against #%d (%s):\n", d.FromID, d.FromAt)
	fmt.Printf("  sampled tags %d, per-tag cosine %.4f, centroid %.4f, scale %.3f\n",
		d.SampleN, d.SampleCosine, d.CentroidCosine, d.MeanNormRatio)
	fmt.Printf("  rows %d -> %d, width %d -> %d\n", d.RowsBefore, d.RowsAfter, d.DimBefore, d.DimAfter)
	fmt.Printf("  verdict: %s\n", d.Verdict)
	if d.SampleCosine >= store.DriftFloor && d.DimBefore == d.DimAfter {
		fmt.Println("  note: this compares the table with itself unless a rebuild happened between " +
			"the two fingerprints -- running this twice proves the wiring, not the rebuild")
	}
	return nil
}
