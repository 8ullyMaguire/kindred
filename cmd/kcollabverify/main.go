// Command kcollabverify exercises the collab index against the REAL mirror and
// prints measured facts. It is a verification tool, not a test: it needs the
// 1.7 GB corpus that only exists on the deployment host.
//
// The properties printed here are the ones a fixture cannot establish —
// whether the evidence gate leaves a usable number of pairs at corpus depth,
// and whether the signal's normalisation behaves at real magnitudes rather
// than at the handful a fixture can express.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/collab"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: kcollabverify /path/to/ao3_metadata.db [seed works]")
		os.Exit(2)
	}
	path := os.Args[1]
	seeds := []int64{17249318, 26523892, 4701869}

	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		fatal("open", err)
	}
	defer db.Close()

	t0 := time.Now()
	b := &collab.Builder{Corpus: db, Trace: func(s string) {
		fmt.Printf("  [%-14s] %s\n", s, time.Since(t0).Round(time.Millisecond))
	}}
	idx, st, err := b.Build(ctx)
	if err != nil {
		fatal("build", err)
	}
	fmt.Printf("\nBUILD  %s\n", time.Since(t0).Round(time.Millisecond))
	fmt.Printf("  gate chosen      %d  (pairs at co>=2: %d, floor %d)\n",
		b.Gate, b.PairsAtGate2, collab.DefaultMinPairsForGate2)
	fmt.Printf("  bookmark rows      %d\n", st.BookmarkRows)
	fmt.Printf("  distinct users     %d\n", st.Users)
	fmt.Printf("  pairs before gate  %d\n", st.PairsBeforeGate)
	fmt.Printf("  pairs after  gate  %d  (gate removed %.1f%%)\n",
		st.Pairs, pctDrop(st.PairsBeforeGate, st.Pairs))
	fmt.Printf("  works in graph     %d\n", st.Works)
	fmt.Printf("  memory             %s\n", peakRSS())

	// The floor is a RANKABILITY check, not an evidence check: an index with
	// fewer pairs than this cannot produce a list a reader would recognise,
	// whatever its evidence quality.
	if st.Pairs < 100 {
		fmt.Printf("\nFAIL: only %d pairs cleared the gate — the index cannot rank\n", st.Pairs)
		os.Exit(1)
	}

	// The gate must actually bind: if it removed nothing, it is not doing its
	// job and a reader cannot tell the difference between "no evidence" and
	// "no gate".
	if st.PairsBeforeGate > 0 && st.Pairs >= st.PairsBeforeGate {
		fmt.Printf("\nFAIL: the gate excluded nothing (%d before, %d after)\n",
			st.PairsBeforeGate, st.Pairs)
		os.Exit(1)
	}

	fmt.Printf("\nSEEDS\n")
	for _, s := range seeds {
		title, ok := workTitle(ctx, db, s)
		if !ok {
			fmt.Printf("  %-10d (not in this mirror)\n", s)
			continue
		}
		votes := idx.NeighboursFor([]int64{s}, collab.DefaultNeighbours, collab.Options{})
		ranked := topN(votes, 5)
		fmt.Printf("  %-10d %-44s voters=%4d neighbours=%d\n",
			s, trunc(title, 44), int(idx.Bookmarkers(s)), len(votes))
		for i, v := range ranked {
			t, _ := workTitle(ctx, db, v.id)
			fmt.Printf("      %d. %-10d vote=%6.3f  %s\n", i+1, v.id, v.vote, trunc(t, 50))
		}
		if len(votes) == 0 {
			fmt.Printf("      (no co-bookmark evidence for this seed)\n")
		}
	}

	// Depth check: how deep is the vote set for the best-evidenced seed? A
	// recommendation that only ever returns 3 items is not a
	// recommender.
	fmt.Printf("\nPOOL DEPTH\n")
	for _, s := range seeds {
		votes := idx.NeighboursFor([]int64{s}, collab.DefaultNeighbours, collab.Options{})
		if len(votes) == 0 {
			continue
		}
		var lo, hi float64
		for i, v := range sortedVotes(votes) {
			if i == 0 {
				lo = v.vote
			}
			hi = v.vote
		}
		t, _ := workTitle(ctx, db, s)
		fmt.Printf("  %-10d n=%3d  vote range %.4f..%.4f  %s\n",
			s, len(votes), lo, hi, trunc(t, 40))
	}
	fmt.Println("\nOK")
}

type vote struct {
	id   int64
	vote float64
}

func topN(votes map[int64]float64, n int) []vote {
	s := sortedVotes(votes)
	if len(s) > n {
		s = s[:n]
	}
	return s
}

func sortedVotes(votes map[int64]float64) []vote {
	out := make([]vote, 0, len(votes))
	for id, v := range votes {
		out = append(out, vote{id, v})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].vote != out[b].vote {
			return out[a].vote > out[b].vote
		}
		return out[a].id < out[b].id
	})
	return out
}

func workTitle(ctx context.Context, db *sql.DB, id int64) (string, bool) {
	var t string
	err := db.QueryRowContext(ctx, `SELECT title FROM works WHERE id = ?`, id).Scan(&t)
	return t, err == nil
}

func pctDrop(before, after int64) float64 {
	if before == 0 {
		return 0
	}
	return 100 * float64(before-after) / float64(before)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func peakRSS() string {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "unknown"
	}
	for _, line := range splitLines(string(b)) {
		if len(line) > 6 && line[:6] == "VmHWM:" {
			return line[6:]
		}
	}
	return "unknown"
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "kcollabverify: %s: %v\n", what, err)
	os.Exit(1)
}
