// Command recallprobe is a THROWAWAY diagnostic: it builds the exact candidate
// set the shipped pool query builds, for the exact split `kindred eval` uses,
// and reports pool recall@1000 under several ORDER BY choices.
//
// It answers one question with evidence: the pool keeps 1000 of the ~116,000
// works that share a tag with a reader's seeds. Is the ordering that decides
// which 1000 the reason the ceiling is 0.21, and would a different ordering
// keep more of the works the reader actually wanted?
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"

	"git.polarisocial.xyz/kindred/kindred/internal/eval"

	_ "modernc.org/sqlite"
)

type res struct {
	user       int64
	held       int
	reach      int
	medShared  int
	a, b, c, d int
}

func main() {
	corpusPath := flag.String("corpus", "", "corpus sqlite path")
	users := flag.Int("users", 38, "evaluate the first N readers by id")
	pool := flag.Int("pool", 1000, "candidate pool depth")
	flag.Parse()

	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+*corpusPath+"?mode=ro&_pragma=busy_timeout(20000)")
	check(err)
	check(db.PingContext(ctx))

	byUser, readers, err := eval.LoadBookmarks(ctx, db)
	check(err)
	ids := make([]int64, 0, len(byUser))
	for id := range byUser {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > *users {
		ids = ids[:*users]
	}
	kept := make(map[int64][]int64, len(ids))
	for _, id := range ids {
		kept[id] = byUser[id]
	}
	split := eval.SplitBookmarks(kept, eval.DefaultRatio)
	fmt.Printf("corpus: %d readers, %d held out over %d readers\n", readers, totalHeld(split), len(split.Test))

	nWorks := 0
	check(db.QueryRowContext(ctx, `SELECT COUNT(*) FROM works`).Scan(&nWorks))
	_, err = db.ExecContext(ctx, `CREATE TEMP TABLE tdf AS SELECT tag_id, COUNT(*) df FROM work_tags GROUP BY tag_id`)
	check(err)
	// A candidate's own tag count, computed once for the whole corpus rather
	// than per row: the correlated version cost ~40s per reader on its own.
	_, err = db.ExecContext(ctx, `CREATE TEMP TABLE tcnt AS SELECT work_id, COUNT(*) n FROM work_tags GROUP BY work_id`)
	check(err)

	out := make([]res, 0, len(ids))
	for _, u := range ids {
		test := split.Test[u]
		if len(test) == 0 || len(split.Train[u]) == 0 {
			continue
		}
		r, err := probe(ctx, db, u, split.Train[u], test, nWorks, *pool)
		if err != nil {
			fmt.Fprintf(os.Stderr, "user %d: %v\n", u, err)
			continue
		}
		fmt.Printf("user %d: %d held, reach %d, median shared %d -> A %d B %d C %d D %d\n",
			r.user, r.held, r.reach, r.medShared, r.a, r.b, r.c, r.d)
		out = append(out, r)
	}

	sumA, sumB, sumC, sumD, sumHeld := 0, 0, 0, 0, 0
	fmt.Printf("\n%-6s %-5s %-7s %-6s %-6s %-6s %-6s\n", "user", "held", "reach", "A:cnt", "B:idf", "C:norm", "D:type")
	for _, r := range out {
		fmt.Printf("%-6d %-5d %-7d %-6d %-6d %-6d %-6d\n", r.user, r.held, r.reach, r.a, r.b, r.c, r.d)
		sumA += r.a
		sumB += r.b
		sumC += r.c
		sumD += r.d
		sumHeld += r.held
	}
	fmt.Printf("\nheld out %d works over %d readers\n", sumHeld, len(out))
	fmt.Printf("mean per-user recall@%d  A count         %.4f\n", *pool, meanRecall(out, func(r res) int { return r.a }))
	fmt.Printf("mean per-user recall@%d  B idf           %.4f\n", *pool, meanRecall(out, func(r res) int { return r.b }))
	fmt.Printf("mean per-user recall@%d  C normalised    %.4f\n", *pool, meanRecall(out, func(r res) int { return r.c }))
	fmt.Printf("mean per-user recall@%d  D type-weighted %.4f\n", *pool, meanRecall(out, func(r res) int { return r.d }))
	fmt.Printf("micro (hits/held)  A %.4f  B %.4f  C %.4f  D %.4f\n",
		float64(sumA)/float64(sumHeld), float64(sumB)/float64(sumHeld),
		float64(sumC)/float64(sumHeld), float64(sumD)/float64(sumHeld))
}

func totalHeld(s eval.Split) int {
	n := 0
	for _, v := range s.Test {
		n += len(v)
	}
	return n
}

func meanRecall(rows []res, f func(res) int) float64 {
	if len(rows) == 0 {
		return 0
	}
	s := 0.0
	for _, r := range rows {
		s += float64(f(r)) / float64(r.held)
	}
	return s / float64(len(rows))
}

func probe(ctx context.Context, db *sql.DB, u int64, train, test []int64, nWorks, pool int) (res, error) {
	var r res
	r.user, r.held = u, len(test)
	conn, err := db.Conn(ctx)
	if err != nil {
		return r, err
	}
	defer conn.Close()
	ex := func(stmt string, args ...any) error {
		_, err := conn.ExecContext(ctx, stmt, args...)
		return err
	}
	for _, t := range []string{"tr", "ho", "stag", "cand"} {
		if err := ex("DROP TABLE IF EXISTS temp." + t); err != nil {
			return r, err
		}
	}
	if err := ex(`CREATE TEMP TABLE tr (id INTEGER PRIMARY KEY)`); err != nil {
		return r, err
	}
	if err := ex(`CREATE TEMP TABLE ho (id INTEGER PRIMARY KEY)`); err != nil {
		return r, err
	}
	for _, id := range train {
		if err := ex(`INSERT OR IGNORE INTO tr VALUES (?)`, id); err != nil {
			return r, err
		}
	}
	for _, id := range test {
		if err := ex(`INSERT OR IGNORE INTO ho VALUES (?)`, id); err != nil {
			return r, err
		}
	}
	// The seed's tags, carrying corpus document frequency and a representative
	// tag_type (a tag id can arrive under more than one type in this mirror).
	if err := ex(`CREATE TEMP TABLE stag AS
		SELECT wt.tag_id AS tag_id, t.df AS df, MIN(wt.tag_type) AS tt
		FROM work_tags wt
		JOIN tr ON tr.id = wt.work_id
		JOIN tdf t ON t.tag_id = wt.tag_id
		GROUP BY wt.tag_id`); err != nil {
		return r, err
	}
	// One pass builds every arm's score. COUNT(*) is the shipped ordering;
	// idfw is inverse-document-frequency weighted overlap; typew weights
	// fandom/character/relationship above freeform; ntags lets the normalised
	// arm divide out a candidate's own tag bulk.
	if err := ex(fmt.Sprintf(`CREATE TEMP TABLE cand AS
		SELECT wt.work_id AS id,
		       COUNT(*) AS shared,
		       SUM(ln(1.0 + %d.0/st.df)) AS idfw,
		       SUM(CASE st.tt WHEN 'fandom' THEN 2.0 WHEN 'character' THEN 2.0
		                      WHEN 'relationship' THEN 1.5 ELSE 1.0 END) AS typew,
		       COALESCE(tc.n, 1) AS ntags
		FROM work_tags wt
		JOIN stag st ON st.tag_id = wt.tag_id
		LEFT JOIN tcnt tc ON tc.work_id = wt.work_id
		WHERE wt.work_id NOT IN (SELECT id FROM tr)
		GROUP BY wt.work_id`, nWorks)); err != nil {
		return r, err
	}

	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM cand`).Scan(&r.reach); err != nil {
		return r, err
	}
	_ = conn.QueryRowContext(ctx, `SELECT shared FROM (SELECT shared, ROW_NUMBER() OVER (ORDER BY shared) rn, COUNT(*) OVER () tot FROM cand) WHERE rn = tot/2`).Scan(&r.medShared)

	arms := []struct {
		order string
		dst   *int
	}{
		{"shared DESC, id ASC", &r.a},
		{"idfw DESC, id ASC", &r.b},
		{"shared*1.0/(ntags+1) DESC, id ASC", &r.c},
		{"typew DESC, id ASC", &r.d},
	}
	for _, arm := range arms {
		var hits int
		stmt := fmt.Sprintf(`SELECT COUNT(*) FROM (SELECT id FROM cand ORDER BY %s LIMIT %d) WHERE id IN (SELECT id FROM ho)`, arm.order, pool)
		if err := conn.QueryRowContext(ctx, stmt).Scan(&hits); err != nil {
			return r, err
		}
		*arm.dst = hits
	}
	return r, nil
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
