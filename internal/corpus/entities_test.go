// SPDX-License-Identifier: AGPL-3.0-only

// Tests for the entity candidate loaders: the kinds the corpus can serve, the
// evidence floor, name resolution, and the author byline normalisation.
//
// The fixture is deliberately richer than the shared one in ao3_test.go: the
// ≥3-works floor means a tag carried by two works must NOT appear, and a
// two-tag fixture cannot express that at all.

package corpus

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"git.polarisocial.xyz/kindred/kindred/internal/arena"

	_ "modernc.org/sqlite"
)

// entityFixture builds a corpus with tag counts that straddle the floor: four
// works carry tag 10, three carry tag 11, two carry tag 12 (below the floor),
// and three carry a character tag.
func entityFixture(t *testing.T) *AO3 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corpus.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(fixtureSchema); err != nil {
		t.Fatal(err)
	}
	a := NewAO3(db)

	stmts := []string{
		`INSERT INTO tags(id,name) VALUES(10,'slow burn'),(11,'time travel'),(12,'rare pair'),(20,'Hermione Granger')`,
	}
	for work := int64(1); work <= 5; work++ {
		stmts = append(stmts, `INSERT INTO works(id,title,url,summary,authors)
		 VALUES(`+itoa(work)+`,'W','https://ao3.example/`+itoa(work)+`','s','Author `+itoa(work)+`')`)
	}
	// tag 10 on works 1-4, tag 11 on 1-3, tag 12 on 1-2 (one work short).
	for work := int64(1); work <= 4; work++ {
		stmts = append(stmts, `INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(`+itoa(work)+`,10,'freeforms')`)
	}
	for work := int64(1); work <= 3; work++ {
		stmts = append(stmts, `INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(`+itoa(work)+`,11,'freeforms')`)
		stmts = append(stmts, `INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(`+itoa(work)+`,20,'characters')`)
	}
	for work := int64(1); work <= 2; work++ {
		stmts = append(stmts, `INSERT INTO work_tags(work_id,tag_id,tag_type) VALUES(`+itoa(work)+`,12,'freeforms')`)
	}
	for _, s := range stmts {
		if _, err := a.DB.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	return a
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func TestEntityCandidatesTagsUseTheEvidenceFloorAndResolveNames(t *testing.T) {
	ctx := context.Background()
	a := entityFixture(t)

	cands, err := a.EntityCandidates(ctx, arena.KindTag, 50)
	if err != nil {
		t.Fatalf("tag candidates: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d tag candidates (%+v), want 2: the tag on two works is below the floor", len(cands), cands)
	}
	// Most-carried first; the loader orders by evidence so a small limit keeps
	// the tags in play rather than an arbitrary slice.
	if cands[0].ID != 10 || cands[0].Weight != 4 {
		t.Fatalf("first candidate %+v, want tag 10 with weight 4", cands[0])
	}
	if cands[1].ID != 11 || cands[1].Weight != 3 {
		t.Fatalf("second candidate %+v, want tag 11 with weight 3", cands[1])
	}
	if cands[0].Name != "slow burn" || cands[1].Name != "time travel" {
		t.Fatalf("names %q/%q, want the corpus's names", cands[0].Name, cands[1].Name)
	}
}

func TestEntityCandidatesReachesEveryTagTypeTheCorpusHas(t *testing.T) {
	ctx := context.Background()
	a := entityFixture(t)

	chars, err := a.EntityCandidates(ctx, arena.KindCharacter, 50)
	if err != nil {
		t.Fatalf("character candidates: %v", err)
	}
	if len(chars) != 1 || chars[0].ID != 20 || chars[0].Name != "Hermione Granger" {
		t.Fatalf("character candidates %+v, want the one character tag", chars)
	}

	// A kind the mirror has almost nothing of answers "none", not an error:
	// 'characters' held 304 tag rows in the live mirror and 'fandoms' 104, so
	// an empty arena is a normal state the caller decides how to present.
	fandoms, err := a.EntityCandidates(ctx, arena.KindFandom, 50)
	if err != nil {
		t.Fatalf("fandom candidates: %v", err)
	}
	if len(fandoms) != 0 {
		t.Fatalf("fandom candidates %+v, want none", fandoms)
	}
}

func TestEntityCandidatesRefusesTheWorkKind(t *testing.T) {
	ctx := context.Background()
	a := entityFixture(t)

	// An empty list here would read as "no works matched" rather than "wrong
	// loader", and the work arena's candidates genuinely come from elsewhere.
	if _, err := a.EntityCandidates(ctx, arena.KindWork, 50); err == nil {
		t.Fatal("the work kind must be refused, not served empty")
	}
	if _, err := a.EntityCandidates(ctx, arena.EntityKind("nope"), 50); err == nil {
		t.Fatal("an unknown kind must be refused")
	}
}

func TestEntityCandidatesHonoursTheLimitAndZero(t *testing.T) {
	ctx := context.Background()
	a := entityFixture(t)

	if cands, err := a.EntityCandidates(ctx, arena.KindTag, 0); err != nil || len(cands) != 0 {
		t.Fatalf("limit 0 = %d candidates, err=%v; want none and no error", len(cands), err)
	}
	cands, err := a.EntityCandidates(ctx, arena.KindTag, 1)
	if err != nil {
		t.Fatalf("limit 1: %v", err)
	}
	if len(cands) != 1 || cands[0].ID != 10 {
		t.Fatalf("limit 1 = %+v, want the most-carried tag", cands)
	}
}

func TestEntityCandidatesAuthorsAreSplitAndNormalised(t *testing.T) {
	ctx := context.Background()
	a := entityFixture(t)

	// `authors` is a comma-space list of `pseud (username)` bylines
	// (internal/crawl/parse.go joins it that way). Two spellings of the same
	// person must be one entity — that collision is accepted and documented —
	// and two different people must not be merged.
	stmts := []string{
		`UPDATE works SET authors='Alpha (alpha_user)' WHERE id=1`,
		`UPDATE works SET authors='Alpha (alpha_user)' WHERE id=2`,
		`UPDATE works SET authors='alpha' WHERE id=3`,
		`UPDATE works SET authors='Beta (beta_user)' WHERE id=4`,
		`UPDATE works SET authors='Alpha (alpha_user), Beta (beta_user)' WHERE id=5`,
	}
	for _, s := range stmts {
		if _, err := a.DB.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}

	cands, err := a.EntityCandidates(ctx, arena.KindAuthor, 50)
	if err != nil {
		t.Fatalf("author candidates: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d author candidates (%+v), want 2", len(cands), cands)
	}
	if cands[0].ID != arena.AuthorEntityID("Alpha (alpha_user)") {
		t.Fatalf("first candidate %+v, want the alpha id", cands[0])
	}
	// Four sightings of alpha — works 1, 2, 3 and the multi-author row — and
	// two of beta. A single-author reading of the last row would have made it
	// one bogus entity instead.
	if cands[0].Weight != 4 {
		t.Fatalf("alpha weight %d, want 4 (three solo works plus the shared byline)", cands[0].Weight)
	}
	if cands[0].Name != "Alpha (alpha_user)" {
		t.Fatalf("alpha name %q, want the byline as stored", cands[0].Name)
	}
	if cands[1].Weight != 2 {
		t.Fatalf("beta weight %d, want 2", cands[1].Weight)
	}
}
