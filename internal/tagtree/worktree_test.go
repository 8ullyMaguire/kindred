package tagtree

// The work-vector and theme-label tests (#47).
//
// The weighting is the whole argument of the file, so the fixture is built to fail if the
// weighting is flat: two works share a RARE tag and nothing else, two others share only the
// archive's most common tag. With inverse-sqrt weighting the rare pair is closer; with a flat
// mean the common pair is, because the common tag contributes the same to both.

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// workFixture writes just enough of both databases: tag embeddings, tag support, works.
func workFixture(t *testing.T, tagVecs map[int64][]float32, support map[int64]int, works map[int64][]int64, kudos map[int64]int) (*sql.DB, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	state, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	corpus, err := sql.Open("sqlite", filepath.Join(dir, "corpus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { corpus.Close() })

	if _, err := state.Exec(`CREATE TABLE embeddings (kind TEXT, entity_id INTEGER PRIMARY KEY, dim INTEGER, vec BLOB)`); err != nil {
		t.Fatal(err)
	}
	for id, v := range tagVecs {
		blob := make([]byte, len(v)*4)
		for i, f := range v {
			bits := math.Float32bits(f)
			blob[i*4] = byte(bits)
			blob[i*4+1] = byte(bits >> 8)
			blob[i*4+2] = byte(bits >> 16)
			blob[i*4+3] = byte(bits >> 24)
		}
		if _, err := state.Exec(`INSERT INTO embeddings (kind, entity_id, dim, vec) VALUES ('tag', ?, ?, ?)`,
			id, len(v), blob); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := corpus.Exec(`CREATE TABLE works (id INTEGER PRIMARY KEY, title TEXT, kudos INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.Exec(`CREATE TABLE work_tags (work_id INTEGER, tag_id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	// Support rows: the corpus counts them, and LoadWorks reads the same table for support.
	for tid, n := range support {
		for i := 0; i < n; i++ {
			// Dummy works carry the support; they have no embeddings, so they never cluster.
			if _, err := corpus.Exec(`INSERT INTO work_tags (work_id, tag_id) VALUES (?, ?)`, 1_000_000+int64(tid)*1000+int64(i), tid); err != nil {
				t.Fatal(err)
			}
		}
	}
	for wid, tags := range works {
		if _, err := corpus.Exec(`INSERT INTO works (id, title, kudos) VALUES (?, ?, ?)`, wid, "w", kudos[wid]); err != nil {
			t.Fatal(err)
		}
		for _, tid := range tags {
			if _, err := corpus.Exec(`INSERT INTO work_tags (work_id, tag_id) VALUES (?, ?)`, wid, tid); err != nil {
				t.Fatal(err)
			}
		}
	}
	return state, corpus
}

func unit(v ...float32) []float32 {
	var n float64
	for _, f := range v {
		n += float64(f) * float64(f)
	}
	n = math.Sqrt(n)
	out := make([]float32, len(v))
	for i := range v {
		out[i] = float32(float64(v[i]) / n)
	}
	return out
}

func TestRareTagsMoveAWorkMoreThanCommonOnes(t *testing.T) {
	// Three tags in three orthogonal directions: 1 is common (500 works), 2 and 3 are rare (4
	// each). Work A carries the common tag and 2; B carries the common tag and 3; C carries both
	// rare tags. Under inverse-sqrt weighting A is much closer to C (they share a rare tag) than
	// to B (they share only the common one). Under a FLAT mean the two similarities are equal by
	// symmetry -- which is exactly the failure this fixture is shaped to catch.
	state, corpus := workFixture(t,
		map[int64][]float32{
			1: unit(1, 0, 0),
			2: unit(0, 1, 0),
			3: unit(0, 0, 1),
		},
		map[int64]int{1: 500, 2: 4, 3: 4},
		map[int64][]int64{
			10: {1, 2},
			11: {1, 3},
			12: {2, 3},
		},
		map[int64]int{10: 10, 11: 10, 12: 10},
	)
	o := DefaultWorkOpts()
	o.MinTags = 2
	ws, err := LoadWorks(context.Background(), state, corpus, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 3 {
		t.Fatalf("loaded %d works, want 3", len(ws))
	}
	byID := map[int64]WorkVec{}
	for _, w := range ws {
		byID[w.ID] = w
		// Unit length is the documented contract: Build is called with Center=false for works
		// (the shared popularity direction IS the theme), so nothing downstream normalises. A
		// work left unnormalised would make Euclidean distance depend on how MANY tags it has.
		var n float64
		for _, f := range w.V {
			n += float64(f) * float64(f)
		}
		if math.Abs(math.Sqrt(n)-1) > 1e-4 {
			t.Fatalf("work %d has vector length %.6f, want 1", w.ID, math.Sqrt(n))
		}
	}
	shareRare := Cosine(byID[10].V, byID[12].V)
	shareCommon := Cosine(byID[10].V, byID[11].V)
	if shareRare <= shareCommon+0.2 {
		t.Fatalf("sharing a RARE tag scored %.4f against %.4f for sharing the common one: the "+
			"weighting is flat (a flat mean makes these equal)", shareRare, shareCommon)
	}
}

func TestLoadWorksDropsWorksBelowMinTags(t *testing.T) {
	state, corpus := workFixture(t,
		map[int64][]float32{1: unit(1, 0), 2: unit(0, 1)},
		map[int64]int{1: 10, 2: 10},
		map[int64][]int64{10: {1}, 11: {1, 2}},
		map[int64]int{10: 5, 11: 5},
	)
	o := DefaultWorkOpts()
	o.MinTags = 2 // work 10 carries one tag, work 11 carries two
	ws, err := LoadWorks(context.Background(), state, corpus, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].ID != 11 {
		t.Fatalf("loaded %+v, want only work 11 (one tag has no direction)", ws)
	}
}

func TestThemeLabelsUseLiftNotFrequency(t *testing.T) {
	// Three clusters. "fluff" is in EVERY work of every cluster -- the archive's water.
	// "time loop" is in 8 of 10 works of cluster 0 and 1 of 10 in the others.
	tagged := map[int64][]int64{}
	families := make([][]int64, 3)
	for c := 0; c < 3; c++ {
		for i := 0; i < 10; i++ {
			wid := int64(c*100 + i)
			families[c] = append(families[c], wid)
			tags := []int64{1} // fluff, everywhere
			if c == 0 && i < 8 {
				tags = append(tags, 2) // time loop, concentrated
			}
			if c == 0 && i == 9 {
				// A tag on ONE work: a huge lift over a 10-work cluster and no evidence at all.
				// Only the share floor keeps it out of the label.
				tags = append(tags, 4)
			}
			if c != 0 && i < 1 {
				tags = append(tags, 2)
			}
			tagged[wid] = tags
		}
	}
	names := map[int64]string{1: "fluff", 2: "time loop", 4: "one work only"}
	got := LabelThemes(families, names, tagged, WorkOpts{LabelTags: 3, MinLabelShare: 0.25, MinLift: 2})

	if len(got[0]) == 0 {
		t.Fatalf("cluster 0 got no label at all")
	}
	if got[0][0].Name != "time loop" {
		t.Fatalf("cluster 0 led with %q, want the concentrated tag", got[0][0].Name)
	}
	for _, l := range got[0] {
		if l.Name == "one work only" {
			t.Fatalf("the label took a tag on ONE work of ten (share 0.1): the share floor is not applied")
		}
	}
	for _, l := range got[0] {
		if l.Name == "fluff" {
			t.Fatalf("the label kept 'fluff', which is in every work of every cluster")
		}
	}
	// A cluster with NO concentrated tag gets NO label, rather than the archive's water.
	for c := 1; c < 3; c++ {
		for _, l := range got[c] {
			if l.Name == "fluff" {
				t.Fatalf("cluster %d was labelled with the water tag %q", c, l.Name)
			}
		}
	}
}

func TestThemeNameShortensLongTags(t *testing.T) {
	long := "haruno sakura & uchiha sasuke & uzumaki naruto & hatake kakashi"
	got := ThemeName([]ThemeLabel{{TagID: 1, Name: long}, {TagID: 2, Name: "short"}})
	if len([]rune(got)) > 60 {
		t.Fatalf("label %q is %d runes: it does not fit the column it is drawn in", got, len([]rune(got)))
	}
	if got[len(got)-5:] != "short" {
		t.Fatalf("label %q lost its second tag", got)
	}
}

func TestShortTagCutsAtAWordBoundary(t *testing.T) {
	got := ShortTag("haruno sakura & uchiha sasuke & uzumaki naruto")
	if []rune(got)[len([]rune(got))-1] != '…' {
		t.Fatalf("ShortTag(%q) = %q, want a trailing ellipsis", got, got)
	}
	if len([]rune(got)) > 27 {
		t.Fatalf("ShortTag returned %d runes, want at most 27", len([]rune(got)))
	}
	if ShortTag("short") != "short" {
		t.Fatalf("a short name was changed")
	}
}
