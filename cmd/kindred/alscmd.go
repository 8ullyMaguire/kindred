package main

// kindred als: train implicit-feedback factors over the mirror's reader interactions
// (#89) and store them for the engine to load.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/als"
	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

func runALS(ctx context.Context, args []string) error {
	fs := newFlagSet("als")
	var (
		dbPath  = fs.String("db", "", "path to the kindred state database (required)")
		corpusP = fs.String("corpus", "", "path to the AO3 mirror (required)")
		factors = fs.Int("factors", als.DefaultParams().Factors, "latent dimensions")
		iters   = fs.Int("iters", als.DefaultParams().Iters, "sweeps over the data")
		lambda  = fs.Float64("lambda", als.DefaultParams().Lambda, "L2 regularisation")
		alpha   = fs.Float64("alpha", als.DefaultParams().Alpha, "confidence per observed interaction")
		minUser = fs.Int("min-user-interactions", 2,
			"drop readers with fewer interactions than this; 0 keeps all")
		force = fs.Bool("force", false, "retrain even if factors already exist")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return errors.New("als: --db is required")
	}
	if *corpusP == "" {
		return errors.New("als: --corpus is required")
	}
	if *factors <= 0 {
		return fmt.Errorf("als: --factors must be positive, got %d", *factors)
	}

	s, err := store.Open(ctx, *dbPath, *corpusP)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		return err
	}
	if !*force {
		n, err := s.CountWorkFactors(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			fmt.Printf("als: %d work factors already present; pass --force to retrain\n", n)
			return nil
		}
	}

	c := corpus.NewAO3(s.Corpus)
	kinds, err := c.InteractionKinds(ctx)
	if err != nil {
		return err
	}
	if len(kinds) == 0 {
		return errors.New("als: the mirror holds no reader interactions -- nothing to train on")
	}
	fmt.Printf("als: interactions by kind:")
	for k, n := range kinds {
		fmt.Printf(" %s=%d", k, n)
	}
	fmt.Println()

	raw, err := c.Interactions(ctx)
	if err != nil {
		return err
	}

	// The threshold is the caller's, printed with its effect. A reader with ONE bookmark
	// contributes a row of the model with a single observation and 112,934 implicit
	// zeros, which is mostly noise dressed as a preference; two is the smallest count
	// where a reader has said anything about co-readership. What matters is that the
	// number is stated rather than assumed.
	counts := map[int64]int{}
	for _, it := range raw {
		counts[it.UserID]++
	}
	in := make([]als.Interaction, 0, len(raw))
	dropped := 0
	for _, it := range raw {
		if *minUser > 0 && counts[it.UserID] < *minUser {
			dropped++
			continue
		}
		in = append(in, als.Interaction{User: it.UserID, Work: it.WorkID})
	}
	users := map[int64]struct{}{}
	works := map[int64]struct{}{}
	for _, it := range in {
		users[it.User] = struct{}{}
		works[it.Work] = struct{}{}
	}
	fmt.Printf("als: training on %d interactions (%d dropped under --min-user-interactions=%d): "+
		"%d readers x %d works, %d factors\n",
		len(in), dropped, *minUser, len(users), len(works), *factors)

	params := als.Params{Factors: *factors, Iters: *iters, Lambda: *lambda, Alpha: *alpha}
	start := time.Now()
	res := als.Train(in, params)
	fmt.Printf("als: trained in %s\n", time.Since(start).Round(time.Millisecond))
	if len(res.Fit) == 0 {
		return errors.New("als: training produced no sweeps")
	}
	for i := range res.Fit {
		if i == 0 || i == len(res.Fit)-1 || (i+1)%4 == 0 {
			fmt.Printf("  sweep %2d  rmse %.4f  max move %.5f\n", i+1, res.Fit[i], res.Moved[i])
		}
	}
	if last := res.Moved[len(res.Moved)-1]; last > 0.5 {
		fmt.Printf("als: WARNING the factors are still moving %.4f per sweep after %d "+
			"sweeps; the model has not settled and its similarities are a snapshot\n", last, *iters)
	}

	factorsOut := make(map[int64][]float32, len(res.WorkIDs))
	for i, id := range res.WorkIDs {
		factorsOut[id] = res.Works[i]
	}
	if err := s.SaveWorkFactors(ctx, *factors, factorsOut); err != nil {
		return err
	}
	fmt.Printf("als: stored %d work factors at dim %d in %s\n",
		len(factorsOut), *factors, *dbPath)
	if len(factorsOut) < len(works) {
		fmt.Fprintf(os.Stderr, "als: note %d works were trained but %d stored\n",
			len(works), len(factorsOut))
	}
	return nil
}
