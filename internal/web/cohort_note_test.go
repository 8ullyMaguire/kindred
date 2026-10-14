package web

// The standings pages name the owner-cohort weighting when this deployment
// ranks with one, and say nothing when it does not.
//
// Two failures this pins, and asserting that the note exists catches neither:
//
//   - The note rendered UNCONDITIONALLY. A deployment with no owner cohort
//     would then claim a weighting that is not running -- a false statement
//     about the numbers, which is worse than a missing sentence.
//   - The note NEVER rendered, because the condition reached a field no
//     template read. That is the accepted-and-ignored shape this codebase
//     keeps finding: rank.html carried a title and a link and printed
//     neither, for as long as the file existed.
//
// Every absence assertion counts its subject first. "Zero notes" is also what
// a page that rendered nothing at all returns, so the absence is evidence only
// once the page has been shown to render its own content.

import (
	"net/http"
	"strings"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"
)

// cohortNoteMarker is the attribute the three pages carry the note in. The
// count is taken on this rather than on the sentence, so a page that merely
// mentions "the owner cohort" in prose cannot be mistaken for the note.
const cohortNoteMarker = `data-testid="cohort-note"`

func TestStandingsPagesNameTheOwnerCohortOnlyWhenOneIsRunning(t *testing.T) {
	rows := []LeaderboardRow{{Rank: 1, WorkID: 42, Title: "Top Work", Rating: 1800,
		RD: 90, Comparisons: 12, Wins: 9, Losses: 2, Draws: 1}}
	card := sampleCard("A", 1)

	cases := []struct {
		name string
		tpl  string
		// furniture is a string the page renders in BOTH variants. It is what
		// makes the absent case mean something.
		furniture string
		with      any
		without   any
	}{
		{
			name: "leaderboard.html", tpl: "leaderboard.html", furniture: "Top Work",
			with:    LeaderboardPage{Base: sweepBase, Rows: rows, CohortNote: ownerCohortNote},
			without: LeaderboardPage{Base: sweepBase, Rows: rows},
		},
		{
			name: "leaderboard.html empty", tpl: "leaderboard.html", furniture: "Nothing has been rated yet",
			// The empty board still names the weighting: the note describes how
			// this deployment computes standings, which is true before the first
			// rating exists -- and a note that vanished here would make the deploy
			// gate report drift on a correct deploy.
			with: LeaderboardPage{Base: sweepBase, Empty: true, MinCompare: 3,
				Note: "Nothing has been rated yet.", CohortNote: ownerCohortNote},
			without: LeaderboardPage{Base: sweepBase, Empty: true, MinCompare: 3,
				Note: "Nothing has been rated yet."},
		},
		{
			name: "arena.html", tpl: "arena.html", furniture: "Card A",
			with:    ArenaPage{Base: sweepBase, Cards: []WorkCard{card}, Session: "s", CohortNote: ownerCohortNote},
			without: ArenaPage{Base: sweepBase, Cards: []WorkCard{card}, Session: "s"},
		},
		{
			name: "rank.html", tpl: "rank.html", furniture: "Rated Work",
			with: RankPage{Base: sweepBase, WorkID: 42, Title: "Rated Work",
				Rating: 1620, RD: 85, Rank: 3, RankOf: 40, KindLabel: "tag",
				CohortNote: ownerCohortNote},
			without: RankPage{Base: sweepBase, WorkID: 42, Title: "Rated Work",
				Rating: 1620, RD: 85, Rank: 3, RankOf: 40, KindLabel: "tag"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/with an owner cohort", func(t *testing.T) {
			body := renderOne(t, tc.tpl, tc.with)
			if !strings.Contains(body, tc.furniture) {
				t.Fatalf("%s did not render its own content (%q missing), so the note's "+
					"presence proves nothing:\n%s", tc.tpl, tc.furniture, body)
			}
			if n := strings.Count(body, cohortNoteMarker); n != 1 {
				t.Fatalf("%s rendered the cohort note %d times, want exactly 1:\n%s", tc.tpl, n, body)
			}
			if !strings.Contains(body, "owner cohort") {
				t.Fatalf("%s rendered the marker but not the sentence:\n%s", tc.tpl, body)
			}
		})

		t.Run(tc.name+"/without an owner cohort", func(t *testing.T) {
			body := renderOne(t, tc.tpl, tc.without)
			if !strings.Contains(body, tc.furniture) {
				t.Fatalf("%s did not render its own content (%q missing), so 'no note' "+
					"would be vacuously true:\n%s", tc.tpl, tc.furniture, body)
			}
			if n := strings.Count(body, cohortNoteMarker); n != 0 {
				t.Fatalf("%s claims an owner-cohort weighting on a deployment that has "+
					"none (%d occurrences):\n%s", tc.tpl, n, body)
			}
		})
	}
}

// TestTheStandingsHandlersSetTheCohortNote runs the real routes over the
// fixture: the table above proves the templates OBEY the field, and this
// proves the handlers SET it. Each half can be right while the page is wrong,
// which is exactly how the note would ship dead.
func TestTheStandingsHandlersSetTheCohortNote(t *testing.T) {
	d, _ := judgeDeps(t)
	targets := []string{"/leaderboard", "/arena", "/rank/" + string(arena.KindTag) + "/7"}

	for _, target := range targets {
		code, body := arenaBody(t, d, target)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", target, code)
		}
		if !strings.Contains(body, "<h1>") {
			t.Fatalf("GET %s rendered no heading, so 'no note' would be vacuously true:\n%s",
				target, body)
		}
		if n := strings.Count(body, cohortNoteMarker); n != 0 {
			t.Fatalf("GET %s claims an owner-cohort weighting with OwnerCohort false "+
				"(%d occurrences)", target, n)
		}
	}

	d.OwnerCohort = true
	for _, target := range targets {
		code, body := arenaBody(t, d, target)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", target, code)
		}
		if !strings.Contains(body, "<h1>") {
			t.Fatalf("GET %s rendered no heading, so the note below would not mean the "+
				"page rendered at all:\n%s", target, body)
		}
		if n := strings.Count(body, cohortNoteMarker); n != 1 {
			t.Fatalf("GET %s rendered the cohort note %d times with OwnerCohort true, want 1:\n%s",
				target, n, body)
		}
	}
}
