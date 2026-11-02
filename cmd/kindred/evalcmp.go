package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/eval"
	"git.polarisocial.xyz/kindred/kindred/internal/stats"
)

// loadDump reads a --dump-users file into user-id keyed records.
func loadDump(path string) (map[int64]eval.UserResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[int64]eval.UserResult{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var u eval.UserResult
		if err := json.Unmarshal(sc.Bytes(), &u); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out[u.User] = u
	}
	return out, sc.Err()
}

// runEvalCmp compares two --dump-users files, reader by reader.
//
//	kindred evalcmp runs/base.jsonl runs/npmi_12.jsonl
//
// Printed as the mean difference (arm - baseline) with a 95% percentile bootstrap
// interval. An interval that straddles zero is "NO VERDICT", and the output says so
// rather than printing a decimal that reads like one: at 37 readers almost every
// difference this evaluator can produce is noise, and the instrument must say that
// instead of ranking noise.
func runEvalCmp(_ context.Context, args []string) error {
	// The first argument may be an existing subcommand's flag string if the caller
	// wrote `kindred evalcmp -a ...`; refuse rather than open a file named "-a".
	for _, a := range args {
		if len(a) > 0 && a[0] == '-' {
			return errors.New("usage: kindred evalcmp <baseline.jsonl> <arm.jsonl> (no flags)")
		}
	}
	if len(args) != 2 {
		return errors.New("usage: kindred evalcmp <baseline.jsonl> <arm.jsonl>")
	}
	base, err := loadDump(args[0])
	if err != nil {
		return fmt.Errorf("evalcmp: %w", err)
	}
	arm, err := loadDump(args[1])
	if err != nil {
		return fmt.Errorf("evalcmp: %w", err)
	}

	// The intersection, in a FIXED order. Pairing by map iteration would draw a
	// different bootstrap sample on every run, and two reports of the same data
	// would disagree.
	ids := make([]int64, 0, len(base))
	for id := range base {
		if _, ok := arm[id]; ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) == 0 {
		return errors.New("evalcmp: the two dumps share no reader — they scored different cohorts and cannot be paired")
	}

	fmt.Printf("paired on %d readers\n", len(ids))
	fmt.Printf("%-7s %-11s %-11s %-9s %-21s %-4s %s\n",
		"metric", "base", "arm", "diff", "95% CI", "n", "verdict")

	metrics := []struct {
		name string
		get  func(eval.UserResult) float64
	}{
		{"recall", func(u eval.UserResult) float64 { return u.Recall }},
		{"ndcg", func(u eval.UserResult) float64 { return u.NDCG }},
		{"hits", func(u eval.UserResult) float64 { return float64(u.Hits) }},
	}
	for _, m := range metrics {
		a := make([]float64, len(ids))
		b := make([]float64, len(ids))
		var sa, sb float64
		for i, id := range ids {
			a[i] = m.get(base[id])
			b[i] = m.get(arm[id])
			sa += a[i]
			sb += b[i]
		}
		mean, lo, hi, n := stats.BootstrapDiff(b, a, 2000, 12345)
		verdict := "NO VERDICT"
		switch {
		case lo > 0:
			verdict = "BETTER"
		case hi < 0:
			verdict = "WORSE"
		}
		fmt.Printf("%-7s %-11.4f %-11.4f %+-9.4f [%+.4f, %+.4f] %-4d %s\n",
			m.name, sa/float64(len(ids)), sb/float64(len(ids)), mean, lo, hi, n, verdict)
	}
	return nil
}
