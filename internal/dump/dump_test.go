package dump

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// newCorpus builds a corpus with the user tables the anonymiser reads, so
// the tests exercise the real schema rather than a mock of it.
func newCorpus(t *testing.T, users, bookmarksPerUser int) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corpus.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	stmts := []string{
		`CREATE TABLE users(id INTEGER PRIMARY KEY, username TEXT, url TEXT)`,
		`CREATE TABLE user_work_interactions(user_id INTEGER, work_id INTEGER,
			interaction_type TEXT, seed_work_id INTEGER, timestamp TEXT)`,
		`CREATE TABLE works(id INTEGER PRIMARY KEY, title TEXT)`,
		`CREATE TABLE tags(id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE work_tags(work_id INTEGER, tag_id INTEGER)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	// Tags 1..5.
	for i := 1; i <= 5; i++ {
		if _, err := db.Exec(`INSERT INTO tags(id,name) VALUES(?,?)`, i, "tag"+string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
	}
	for u := 1; u <= users; u++ {
		if _, err := db.Exec(`INSERT INTO users(id,username,url) VALUES(?,?,?)`,
			u, "user"+string(rune('0'+u)), "https://example.com/u"+string(rune('0'+u))); err != nil {
			t.Fatal(err)
		}
		for b := 0; b < bookmarksPerUser; b++ {
			work := u*100 + b
			if _, err := db.Exec(`INSERT INTO works(id,title) VALUES(?,?)`, work, "w"); err != nil {
				t.Fatal(err)
			}
			// The real table has one row per (user, work, type) and the
			// same work appears more than once per user. Every second
			// bookmark is inserted twice so a COUNT(*)-based threshold
			// would disagree with a COUNT(DISTINCT) one.
			reps := 1
			if b%2 == 0 {
				reps = 2
			}
			for r := 0; r < reps; r++ {
				if _, err := db.Exec(
					`INSERT INTO user_work_interactions(user_id,work_id,interaction_type,timestamp)
					 VALUES(?,?,'bookmarked','2026-01-01')`, u, work); err != nil {
					t.Fatal(err)
				}
			}
			// Each work carries two tags.
			if _, err := db.Exec(`INSERT INTO work_tags(work_id,tag_id) VALUES(?,?)`, work, (b%5)+1); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO work_tags(work_id,tag_id) VALUES(?,?)`, work, ((b+1)%5)+1); err != nil {
				t.Fatal(err)
			}
		}
	}
	return db
}

// TestAnonymiseDropsReadersBelowK is the core privacy property: a reader
// with fewer than K bookmarks must not appear at all.
func TestAnonymiseDropsReadersBelowK(t *testing.T) {
	db := newCorpus(t, 10, 25) // all 10 have 25 bookmarks
	salt, _ := NewSalt()
	rows, err := anonymise(context.Background(), db, 20, salt, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 10 {
		t.Fatalf("got %d rows, want 10 (every user has 25 >= 20)", len(rows))
	}

	// Now a mix: some below K.
	db2 := newCorpus(t, 5, 25)
	if _, err := db2.Exec(`DELETE FROM user_work_interactions WHERE user_id > 2`); err != nil {
		t.Fatal(err)
	}
	rows2, err := anonymise(context.Background(), db2, 20, salt, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows2) != 2 {
		t.Fatalf("got %d rows, want 2; the filter is applied before pseudonymisation", len(rows2))
	}
}

// tagWork gives a work two tags, so a reader who bookmarked it has a
// publishable weight vector. A reader with bookmarks but no resolvable
// tags is correctly dropped by anonymise — there is nothing to publish.
func tagWork(t *testing.T, db *sql.DB, work int, tag1, tag2 int) {
	t.Helper()
	for _, tag := range []int{tag1, tag2} {
		if _, err := db.Exec(`INSERT INTO work_tags(work_id,tag_id) VALUES(?,?)`, work, tag); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAnonymiseCountsDistinctWorksNotRows is the fixture-shaped regression
// for the schema bug: the same work bookmarked repeatedly must count once,
// or a user with four works and seven rows clears a threshold of six.
func TestAnonymiseCountsDistinctWorksNotRows(t *testing.T) {
	db := newCorpus(t, 1, 0)
	// One user, 4 distinct works, 7 rows (three duplicated).
	for i, reps := range []int{2, 2, 2, 1} {
		work := 500 + i
		if _, err := db.Exec(`INSERT INTO works(id,title) VALUES(?,?)`, work, "w"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(
			`INSERT INTO user_work_interactions(user_id,work_id,interaction_type,timestamp)
			 VALUES(1,?,'bookmarked','2026-01-01')`, work); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(
			`INSERT INTO user_work_interactions(user_id,work_id,interaction_type,timestamp)
			 VALUES(1,?,'bookmarked','2026-01-02')`, work); err != nil {
			t.Fatal(err)
		}
		for r := 1; r < reps; r++ {
			if _, err := db.Exec(
				`INSERT INTO user_work_interactions(user_id,work_id,interaction_type,timestamp)
				 VALUES(1,?,'bookmarked','2026-01-03')`, work); err != nil {
				t.Fatal(err)
			}
		}
		// The reader needs a publishable tag vector, or anonymise drops
		// them for having nothing to say rather than for being too small.
		tagWork(t, db, work, 1, 2)
	}
	salt, _ := NewSalt()
	// k=4 clears on distinct works.
	rows, err := anonymise(context.Background(), db, 4, salt, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("k=4 returned %d rows, want 1 (4 distinct works)", len(rows))
	}
	// k=5 must not clear on row count: there are only 4 works.
	rows, err = anonymise(context.Background(), db, 5, salt, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("k=5 returned %d rows, want 0: 7 interaction rows must not count as 7 works", len(rows))
	}
}

// TestAnonymiseIgnoresTheWrongInteractionType: 'bookmarker' rows are about
// a work, not a reader's own action, and must not be counted.
func TestAnonymiseIgnoresTheWrongInteractionType(t *testing.T) {
	db := newCorpus(t, 1, 0)
	salt, _ := NewSalt()
	// 10 rows of type 'bookmarker' and 2 of type 'bookmarked'.
	for i := 0; i < 10; i++ {
		work := 900 + i
		if _, err := db.Exec(`INSERT INTO works(id,title) VALUES(?,?)`, work, "w"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(
			`INSERT INTO user_work_interactions(user_id,work_id,interaction_type,timestamp)
			 VALUES(1,?,'bookmarker','2026-01-01')`, work); err != nil {
			t.Fatal(err)
		}
		tagWork(t, db, work, 1, 2)
	}
	for i := 0; i < 2; i++ {
		work := 950 + i
		if _, err := db.Exec(`INSERT INTO works(id,title) VALUES(?,?)`, work, "w"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(
			`INSERT INTO user_work_interactions(user_id,work_id,interaction_type,timestamp)
			 VALUES(1,?,'bookmarked','2026-01-01')`, work); err != nil {
			t.Fatal(err)
		}
		tagWork(t, db, work, 1, 2)
	}
	rows, err := anonymise(context.Background(), db, 3, salt, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("k=3 returned %d rows; 'bookmarker' rows must not count toward a reader's bookmarks", len(rows))
	}
}

// TestAnonymiseRefusesKZero: a threshold of zero would publish every
// reader's taste, including accounts with a single bookmark.
func TestAnonymiseRefusesKZero(t *testing.T) {
	db := newCorpus(t, 3, 1)
	salt, _ := NewSalt()
	if _, err := anonymise(context.Background(), db, 0, salt, 40); err == nil {
		t.Fatal("k=0 was accepted; that publishes every reader including singletons")
	}
	if _, err := anonymise(context.Background(), db, -1, salt, 40); err == nil {
		t.Fatal("k=-1 was accepted")
	}
}

// TestAnonymiseEmitsNoWorkIDs: the published row must carry tag weights,
// never a work id. A work id is reading history.
func TestAnonymiseEmitsNoWorkIDs(t *testing.T) {
	db := newCorpus(t, 3, 25)
	salt, _ := NewSalt()
	rows, err := anonymise(context.Background(), db, 20, salt, 40)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		blob, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		// A work id would be a bare number in the JSON. The pseudonym is
		// hex and the weights are fractions, so any large integer is a
		// leaked work reference.
		var m map[string]any
		if err := json.Unmarshal(blob, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["work_ids"]; ok {
			t.Fatal("the affinity row carries work_ids")
		}
		weights, ok := m["w"].(map[string]any)
		if !ok {
			t.Fatal("no weight map")
		}
		for k, v := range weights {
			f, isNum := v.(float64)
			if !isNum {
				t.Fatalf("weight %q is %T, want a number", k, v)
			}
			if f <= 0 || f > 1 {
				t.Fatalf("weight %q = %v, want a normalised fraction in (0,1]", k, f)
			}
			if len(k) > 2 && k[0] == 'w' { // "w..." would be a work id masquerading
				t.Fatalf("weight key %q looks like a work reference", k)
			}
		}
	}
}

// TestPseudonymRotatesWithSalt: the default is a per-dump salt, so the
// same reader gets a different pseudonym in two dumps and cannot be
// tracked. This is the property the default exists for.
func TestPseudonymRotatesWithSalt(t *testing.T) {
	s1, _ := NewSalt()
	s2, _ := NewSalt()
	p1 := pseudonym(42, s1)
	p2 := pseudonym(42, s2)
	if p1 == p2 {
		t.Fatal("two salts produced the same pseudonym; a reader could be tracked across dumps")
	}
	// The same salt must be deterministic, or a single dump would be
	// internally inconsistent.
	if pseudonym(42, s1) != p1 {
		t.Fatal("the same salt produced two pseudonyms for one reader")
	}
}

// TestPseudonymDoesNotRevealTheID: the pseudonym must not be a
// reversible or guessable encoding of the user id.
func TestPseudonymDoesNotRevealTheID(t *testing.T) {
	salt, _ := NewSalt()
	p := pseudonym(12345, salt)
	if p == "12345" || p == "12345.onion" {
		t.Fatalf("pseudonym %q is the raw id", p)
	}
	// It must be hex of a truncated hash, so 32 chars.
	if len(p) != 32 {
		t.Fatalf("pseudonym is %d chars, want 32", len(p))
	}
}

func TestSignAndVerifyRoundTrip(t *testing.T) {
	pub, priv, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m := &Manifest{
		Version:   ManifestVersion,
		Full:      true,
		KAnon:     20,
		SaltMode:  "per-dump",
		Salt:      []byte("salt"),
		Shards:    map[string]Shard{"0": {Hash: "abc", Rows: 10}},
		RowCounts: map[string]int64{"works": 100},
	}
	if err := m.Sign(priv, dir); err != nil {
		t.Fatal(err)
	}
	got, err := Verify(dir, pub)
	if err != nil {
		t.Fatal(err)
	}
	if got.KAnon != 20 || got.SaltMode != "per-dump" {
		t.Fatalf("manifest round-tripped wrong: %+v", got)
	}
}

func TestVerifyRejectsAWrongKey(t *testing.T) {
	_, priv, _ := NewKey()
	otherPub, _, _ := NewKey()
	dir := t.TempDir()
	m := &Manifest{Version: ManifestVersion, KAnon: 20, SaltMode: "per-dump", Shards: map[string]Shard{}}
	if err := m.Sign(priv, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir, otherPub); err == nil {
		t.Fatal("a manifest verified against the wrong key")
	}
}

func TestVerifyRejectsATamperedManifest(t *testing.T) {
	pub, priv, _ := NewKey()
	dir := t.TempDir()
	m := &Manifest{Version: ManifestVersion, KAnon: 20, SaltMode: "per-dump", Shards: map[string]Shard{}}
	if err := m.Sign(priv, dir); err != nil {
		t.Fatal(err)
	}
	// Change the canonical form: lower K so more readers are published.
	path := filepath.Join(dir, "manifest.canonical")
	raw, _ := os.ReadFile(path)
	var mm Manifest
	if err := json.Unmarshal(raw, &mm); err != nil {
		t.Fatal(err)
	}
	mm.KAnon = 1
	tampered, _ := json.Marshal(mm)
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir, pub); err == nil {
		t.Fatal("a tampered manifest verified; the signature is not covering the content")
	}
}

func TestVerifyRejectsAFutureVersion(t *testing.T) {
	pub, priv, _ := NewKey()
	dir := t.TempDir()
	m := &Manifest{Version: ManifestVersion + 1, KAnon: 20, SaltMode: "per-dump", Shards: map[string]Shard{}}
	if err := m.Sign(priv, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir, pub); err == nil {
		t.Fatal("a future manifest version verified; unknown fields would be ignored silently")
	}
}

func TestVerifyRejectsABadSignatureLength(t *testing.T) {
	pub, priv, _ := NewKey()
	dir := t.TempDir()
	m := &Manifest{Version: ManifestVersion, KAnon: 20, SaltMode: "per-dump", Shards: map[string]Shard{}}
	if err := m.Sign(priv, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.sig"), []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir, pub); err == nil {
		t.Fatal("a short signature verified")
	}
}

// TestIsOnionHost pins the host-shape rule the fetch client depends on.
func TestIsOnionHost(t *testing.T) {
	valid := "abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrstuvwx.onion"
	for _, tc := range []struct {
		host string
		want bool
	}{
		{valid, true},
		{valid + ":8080", true}, // a port is part of the target
		{"example.com", false},
		{"127.0.0.1", false},
		{"127.0.0.1:8080", false},
		{"localhost", false},
		{"archiveofourown.org", false},
		{"short.onion", false},
		{valid + ".evil.com", false}, // suffix must be terminal
		{"evil.com/" + valid, false},
		{"", false},
		{".onion", false},
		{strings.Repeat("a", 56) + ".onion", true},  // all one char, still base32
		{strings.Repeat("1", 56) + ".onion", false}, // 1 is not base32
		{strings.Repeat("a", 55) + ".onion", false}, // one short
		{strings.Repeat("a", 57) + ".onion", false}, // one long
	} {
		if got := IsOnionHost(tc.host); got != tc.want {
			t.Errorf("IsOnionHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestShardOfIsDeterministicAndSpread(t *testing.T) {
	// Deterministic: content addressing requires the same id to land in
	// the same shard every time.
	for i := 0; i < 100; i++ {
		id := int64(i * 7919)
		if shardOf(id) != shardOf(id) {
			t.Fatalf("shardOf(%d) is not deterministic", id)
		}
	}
	// Spread: consecutive ids must not all land in one shard.
	counts := map[int]int{}
	for i := 0; i < 10000; i++ {
		counts[shardOf(int64(i))]++
	}
	if len(counts) < 200 {
		t.Fatalf("10000 ids landed in only %d shards; a skewed shard map makes deltas useless", len(counts))
	}
	for shard, n := range counts {
		if n > 200 {
			t.Fatalf("shard %d got %d of 10000 ids; distribution is too skewed", shard, n)
		}
	}
}

var _ = ed25519.SignatureSize
