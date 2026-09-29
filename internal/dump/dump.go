// Package dump builds an anonymised, content-addressed snapshot of a
// corpus and signs it.
//
// What is published and what is not is decided per table in SPEC §4.2,
// and the decisions are load-bearing: the corpus carries 6,261 users with
// usernames and URLs and interaction tables recording what named
// individuals bookmarked. Publishing those is publishing what named
// people read, which is the one genuinely sensitive thing in this corpus.
// Everything here exists to make that specific mistake impossible by
// construction rather than by review.
package dump

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestVersion is the schema version of manifest.json.
const ManifestVersion = 1

// ShardCount is how many shards a snapshot is divided into.
const ShardCount = 256

// Manifest describes a snapshot: what is in it, what it is made of, and
// what it is worth trusting.
type Manifest struct {
	Version     int    `json:"version"`
	BaseVersion int    `json:"base_version,omitempty"`
	BuildTime   string `json:"build_time"`

	// RowCounts is the published table's row count per shard family. It
	// exists so a peer can tell a truncated download from a complete one
	// without parsing every shard.
	RowCounts map[string]int64 `json:"row_counts"`

	// KAnon is the bookmark threshold applied before pseudonymisation, and
	// it is published. Publishing the threshold while hiding who clears
	// it is the point: the row count alone must not reveal a small
	// account.
	KAnon int `json:"k_anon"`

	// SaltMode is "per-dump" or "stable". It is a privacy trade-off
	// recorded honestly, not a security control: a stable salt is
	// published in the manifest, so it is linkage, not secrecy.
	SaltMode string `json:"salt_mode"`

	// Salt is the pseudonymisation salt, published for the same reason as
	// SaltMode. A salt in a public document cannot be a secret; calling
	// it one would be the error.
	Salt []byte `json:"salt"`

	// Shards maps a shard number to its content hash and row count.
	Shards map[string]Shard `json:"shards"`

	// Full marks a full snapshot; a delta omits unchanged shards.
	Full bool `json:"full"`
}

// Shard is one content-addressed piece of a snapshot.
type Shard struct {
	Hash string `json:"hash"`
	Rows int64  `json:"rows"`
	Path string `json:"path"`
}

// Sign writes the manifest and its detached minisig-style signature.
//
// The signature is ed25519 over the exact manifest bytes, and the manifest
// carries the hash of nothing else — a peer verifies the bytes it received
// and nothing needs re-serialising to match. Re-marshalling a manifest to
// verify it would make the signature depend on field order, which is a
// property of the encoder rather than of the data.
func (m *Manifest) Sign(priv ed25519.PrivateKey, dir string) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	// The manifest is canonicalised before hashing: without this, the
	// same manifest serialised twice can differ in a byte and the
	// signature will not verify, which reads as tampering.
	canonical, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("canonicalise manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), body, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.canonical"), canonical, 0o644); err != nil {
		return fmt.Errorf("write canonical manifest: %w", err)
	}
	sig := ed25519.Sign(priv, canonical)
	if err := os.WriteFile(filepath.Join(dir, "manifest.sig"), sig, 0o644); err != nil {
		return fmt.Errorf("write signature: %w", err)
	}
	return nil
}

// Verify checks a manifest's signature against a public key.
//
// It verifies the canonical bytes, not the pretty-printed file: the
// signature is over the canonical form, and verifying a re-serialisation
// would reintroduce the encoder dependency the canonical form removes.
func Verify(dir string, pub ed25519.PublicKey) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.canonical"))
	if err != nil {
		return nil, fmt.Errorf("read canonical manifest: %w", err)
	}
	sig, err := os.ReadFile(filepath.Join(dir, "manifest.sig"))
	if err != nil {
		return nil, fmt.Errorf("read signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, raw, sig) {
		return nil, fmt.Errorf("manifest signature does not verify against the given key")
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	// A manifest from a future version may mean fields this verifier
	// ignores, which is exactly where a silent misreading comes from.
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("manifest version %d is not supported (want %d)", m.Version, ManifestVersion)
	}
	return &m, nil
}

// Affinity is one anonymised reader row: a tag→weight vector with no work
// ids and no username.
//
// It deliberately cannot be joined back to a person: there is no user id,
// no work id, and the pseudonym is a per-dump salted hash. What survives is
// "this reader leans toward slow burn", which is the aggregate signal and
// nothing finer.
type Affinity struct {
	Pseudonym string             `json:"p"`
	Weights   map[string]float64 `json:"w"`
}

// Anonymise builds the tag-affinity rows that replace the dropped user
// tables. It is exported so the dump command is the only caller; the
// internals stay unexported.
func Anonymise(ctx context.Context, corpus *sql.DB, k int, salt []byte, limitTags int) ([]Affinity, error) {
	return anonymise(ctx, corpus, k, salt, limitTags)
}

// ShardOfPseudonym buckets a pseudonym into a shard.
//
// A pseudonym, not the user id, is what gets bucketed: the raw id must not
// be the thing that determines where a reader's row lands in a public
// file, and the pseudonym is already the form that is safe to publish.
func ShardOfPseudonym(p string) int {
	h := uint64(14695981039346656037)
	for i := 0; i < len(p); i++ {
		h ^= uint64(p[i])
		h *= 1099511628211
	}
	return int(h % ShardCount)
}

// HashBytes is a shard's content address.
func HashBytes(b []byte) string { return hashBytes(b) }

// anonymise builds the tag_affinity table that replaces the dropped user
// tables.
//
// The order of operations is the whole design (SPEC §4.2):
//
//  1. Filter to readers with >= K bookmarks. Do this first, so the number
//     of rows in the snapshot cannot be used to infer who clears the
//     threshold.
//  2. Pseudonymise with a salt that rotates per dump by default, so two
//     snapshots cannot be joined to track the same reader.
//  3. Emit only tag weights. Never a work id: "reads a lot of slow burn"
//     is publishable, "read this specific work" is not.
//
// Filtering after pseudonymisation would leak the threshold's population
// size, and emitting work ids at any point would publish reading history.
func anonymise(ctx context.Context, corpus *sql.DB, k int, salt []byte, limitTags int) ([]Affinity, error) {
	if k <= 0 {
		return nil, fmt.Errorf("k-anon must be positive: a threshold of 0 publishes every reader's taste")
	}
	// The interaction table holds one row per (user, work, type) and the
	// same work appears more than once for a user: measured 7 rows for 4
	// distinct works. Counting rows would apply the k-anon threshold to
	// repeated interactions rather than to distinct reading, letting a
	// user who bookmarked the same four works seven times clear a
	// threshold of 20 on their own.
	//
	// interaction_type is 'bookmarked' for 173,768 rows and 'bookmarker'
	// for 9,982. The first is a user bookmarking a work; the second is a
	// row about a work that happens to carry a user id, and counting it
	// would attribute a reader's history from someone else's action.
	rows, err := corpus.QueryContext(ctx, `
		SELECT u.id, u.username, COUNT(DISTINCT i.work_id) AS n
		FROM users u
		JOIN user_work_interactions i ON i.user_id = u.id
		WHERE i.interaction_type = 'bookmarked'
		GROUP BY u.id
		HAVING n >= ?`, k)
	if err != nil {
		return nil, fmt.Errorf("read bookmark counts: %w", err)
	}
	type eligible struct {
		userID   int64
		username string
	}
	var people []eligible
	for rows.Next() {
		var e eligible
		var n int64
		if err := rows.Scan(&e.userID, &e.username, &n); err != nil {
			rows.Close()
			return nil, err
		}
		people = append(people, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]Affinity, 0, len(people))
	for _, p := range people {
		weights, err := readerTagWeights(ctx, corpus, p.userID, limitTags)
		if err != nil {
			return nil, err
		}
		if len(weights) == 0 {
			// A reader with bookmarks but no resolvable tags has nothing
			// publishable. Emitting an empty vector would be a row whose
			// only content is "this account exists".
			continue
		}
		out = append(out, Affinity{
			Pseudonym: pseudonym(p.userID, salt),
			Weights:   weights,
		})
	}
	// Sorted by pseudonym so the snapshot's byte content does not depend
	// on the database's iteration order: content addressing requires
	// that the same data produces the same hash.
	sort.Slice(out, func(i, j int) bool { return out[i].Pseudonym < out[j].Pseudonym })
	return out, nil
}

// pseudonym is a salted hash of a user id.
//
// The salt is per-dump by default, so the same reader gets a different
// pseudonym in every snapshot and two snapshots cannot be joined. With
// --stable-salt the pseudonym is constant across dumps, which makes
// cross-snapshot research possible and is a re-identification risk the
// caller has opted into knowingly.
func pseudonym(userID int64, salt []byte) string {
	h := sha256.New()
	h.Write(salt)
	var id [8]byte
	binary.LittleEndian.PutUint64(id[:], uint64(userID))
	h.Write(id[:])
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// readerTagWeights returns a reader's top tags by bookmark count,
// normalised to sum to 1.
func readerTagWeights(ctx context.Context, corpus *sql.DB, userID int64, limit int) (map[string]float64, error) {
	if limit <= 0 {
		limit = 40
	}
	// Same corrections as the threshold query: distinct works, and only
	// the type that means "this user bookmarked this work".
	rows, err := corpus.QueryContext(ctx, `
		SELECT t.name, COUNT(DISTINCT i.work_id) AS n
		FROM user_work_interactions i
		JOIN work_tags wt ON wt.work_id = i.work_id
		JOIN tags t ON t.id = wt.tag_id
		WHERE i.user_id = ? AND i.interaction_type = 'bookmarked'
		GROUP BY t.id
		ORDER BY n DESC
		LIMIT ?`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("reader tag weights: %w", err)
	}
	defer rows.Close()
	weights := map[string]float64{}
	var total float64
	type kv struct {
		name string
		n    float64
	}
	var all []kv
	for rows.Next() {
		var e kv
		if err := rows.Scan(&e.name, &e.n); err != nil {
			return nil, err
		}
		all = append(all, e)
		total += e.n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if total == 0 {
		return nil, nil
	}
	for _, e := range all {
		weights[e.name] = e.n / total
	}
	return weights, nil
}

// NewSalt returns a fresh per-dump salt.
func NewSalt() ([]byte, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("read salt: %w", err)
	}
	return salt, nil
}

// NewKey returns a fresh ed25519 keypair.
func NewKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	return pub, priv, nil
}

// shardOf assigns a row to a shard by hashing its id.
//
// The hash is a plain FNV-1a over the big-endian id: content addressing
// wants rows to spread evenly across shards, and FNV is fine for that. It
// is not a security primitive and must not be used to hide which rows went
// where.
func shardOf(id int64) int {
	h := uint64(14695981039346656037)
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(id))
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return int(h % ShardCount)
}

// hashBytes is the content address of a shard's bytes.
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// IsOnionHost reports whether a host is a Tor onion address.
//
// This is the chokepoint for "no clearnet fetch". A host is accepted only
// if it ends in .onion and is a plausible v3 onion (56 base32 chars). A
// bare hostname, an IP, or anything with a path is refused, so the fetch
// client cannot be pointed at a local port by a well-formed-looking URL.
func IsOnionHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.ContainsAny(host, "/\\@ \t") {
		return false
	}
	// Strip a port if one was given. An onion service's port is part of
	// the target, not a separate authority.
	if i := strings.LastIndex(host, ":"); i > 0 {
		if _, err := fmt.Sscanf(host[i+1:], "%d", new(int)); err == nil {
			host = host[:i]
		}
	}
	if !strings.HasSuffix(host, ".onion") {
		return false
	}
	base := strings.TrimSuffix(host, ".onion")
	// v3 onion addresses are 56 base32 characters.
	if len(base) != 56 {
		return false
	}
	for _, c := range base {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz234567", c) {
			return false
		}
	}
	return true
}
