package store

// The velocity gate (#48). Two tests matter: one proves it says NO on a thin deployment, one
// proves it can say YES. A gate that can only ever refuse is a constant with a sentence
// attached, and this project has shipped a check that could not fail before.

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// velocityFixture writes seen_works rows: owner -> marks per reader-day.
func velocityFixture(t *testing.T, owners map[string][]int) *Store {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE seen_works(
		owner_key TEXT NOT NULL,
		work_id INTEGER NOT NULL,
		shown_at TEXT,
		read_at TEXT,
		PRIMARY KEY (owner_key, work_id))`); err != nil {
		t.Fatal(err)
	}
	id := 0
	for owner, perDay := range owners {
		for day, n := range perDay {
			for k := 0; k < n; k++ {
				id++
				// Day numbers are spread out so each is a distinct calendar day.
				stamp := fmt.Sprintf("2026-10-%02d 12:00:00", day+1)
				if _, err := db.Exec(`INSERT INTO seen_works (owner_key, work_id, shown_at, read_at)
					VALUES (?, ?, ?, ?)`, owner, id, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return &Store{DB: db}
}

func TestVelocityGateRefusesAThinDeployment(t *testing.T) {
	// The live state database on 2026-10-24: 8 owners, 11 marks, 8 reader-days, max 3 in one
	// day. Nothing here could carry a weight.
	rep, err := velocityFixture(t, map[string][]int{
		"a": {3},
		"b": {2},
		"c": {1},
		"d": {1},
		"e": {1},
		"f": {1},
		"g": {1},
		"h": {1},
	}).Velocity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Owners != 8 || rep.Marks != 11 || rep.ReaderDays != 8 {
		t.Fatalf("report = %d owners / %d marks / %d days, want 8 / 11 / 8",
			rep.Owners, rep.Marks, rep.ReaderDays)
	}
	if rep.MeetsGate() {
		t.Fatalf("the gate passed on 8 owners with one reader-day each: %s", rep.Why)
	}
	// The refusal must name the COUNT that would change its mind, or the next session has to
	// re-derive the threshold by hand -- which is what this command exists to stop.
	if rep.WithFiveDays != 0 {
		t.Fatalf("%d owners have five reader-days in a one-day-each fixture", rep.WithFiveDays)
	}
	if want := "need 2"; !strings.Contains(rep.Why, want) {
		t.Fatalf("the refusal does not name the threshold (%q): %s", want, rep.Why)
	}
}

func TestVelocityGatePassesOnARealDistribution(t *testing.T) {
	// Four owners with five reader-days each, at very different paces: p90 well above 2x the
	// median. This is the shape the row is waiting for.
	rep, err := velocityFixture(t, map[string][]int{
		"fast":   {4, 5, 6, 5, 4}, // 24 marks over 5 days = 4.8/day
		"median": {1, 1, 2, 1, 1}, // 6 over 5 = 1.2
		"slow":   {1, 1, 1, 1, 1}, // 5 over 5 = 1.0
		"steady": {2, 2, 2, 2, 2}, // 10 over 5 = 2.0
	}).Velocity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.WithFiveDays != 4 {
		t.Fatalf("%d owners have five reader-days, want 4", rep.WithFiveDays)
	}
	if !rep.MeetsGate() {
		t.Fatalf("the gate refused a fixture built to pass it: %s", rep.Why)
	}
	// The percentiles are pinned to the fixture's arithmetic, not merely compared: a
	// nearest-rank percentile off by one still satisfies "p90 > p50" and would ship a
	// distribution that no reader has. Rates here are 4.8, 2.0, 1.2 and 1.0 marks/day.
	if rep.P50 != 1.2 {
		t.Fatalf("p50 = %v, want 1.2 (the second of four sorted rates)", rep.P50)
	}
	if rep.P90 != 4.8 {
		t.Fatalf("p90 = %v, want 4.8 (the fastest of four)", rep.P90)
	}
}

// A deployment whose readers all move at the same pace is refused even when it is thick: the
// weight would be a constant. This is the half of the threshold nobody would think to test.
func TestVelocityGateRefusesAFlatDistribution(t *testing.T) {
	same := []int{2, 2, 2, 2, 2}
	rep, err := velocityFixture(t, map[string][]int{
		"a": same, "b": same, "c": same, "d": same, "e": same,
	}).Velocity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.WithFiveDays != 5 {
		t.Fatalf("%d owners with five days, want 5", rep.WithFiveDays)
	}
	if rep.MeetsGate() {
		t.Fatalf("the gate passed on a flat distribution: %s", rep.Why)
	}
	if !strings.Contains(rep.Why, "flat") {
		t.Fatalf("the refusal does not name the reason: %s", rep.Why)
	}
}
