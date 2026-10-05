package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/config"
	"git.polarisocial.xyz/kindred/kindred/internal/dump"
	"git.polarisocial.xyz/kindred/kindred/internal/onion"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// runDump builds an anonymised, signed snapshot into --out.
//
// The snapshot is written to disk and served by a separate onion service;
// this command never opens a listener itself. That separation is what
// makes "no clearnet route exists for a snapshot" checkable rather than
// aspirational.
func runDump(ctx context.Context, args []string) error {
	fs := newFlagSet("dump")
	c := bindConfig(fs)
	out := fs.String("out", "", "output directory for the snapshot (required)")
	stableSalt := fs.Bool("stable-salt", false, "reuse one salt across dumps (linkage, not secrecy)")
	version := fs.Int("version", 0, "snapshot version (0 = next after the newest on disk)")
	full := fs.Bool("full", false, "force a full snapshot even when a delta would do")
	keep := fs.Int("keep", dump.RetentionVersions, "versions to retain after this dump (0 = no pruning)")
	c, err := finishConfig(c, fs, args)
	if err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out is required: the snapshot goes to a directory, never to a listener")
	}

	// The version has to be resolved BEFORE the expensive work, so a refusal to
	// overwrite costs a second rather than an anonymisation pass over the whole
	// corpus.
	ver, err := dump.AllocateVersion(*out, *version)
	if err != nil {
		return err
	}
	isFull, baseVer, baseReason := dump.IsFullSnapshot(*out, ver, *full)
	if isFull {
		// A full snapshot names no base. -1 is not a valid version number, so
		// "this is a delta" is decidable from the field alone. Leaving it at the
		// zero value would make a full v0 claim v0 as its own base.
		baseVer = -1
	}
	fmt.Printf("writing a %s snapshot (%s)\n", map[bool]string{true: "FULL", false: "DELTA"}[isFull], baseReason)

	s, err := store.Open(ctx, c.DB, c.CorpusDB)
	if err != nil {
		return err
	}
	defer s.Close()

	pub, priv, err := dump.NewKey()
	if err != nil {
		return err
	}

	// A per-dump salt is the default. It is not a secret — it goes in the
	// manifest — but rotating it is what stops two snapshots being joined
	// to track one reader across time.
	salt, err := dump.NewSalt()
	if err != nil {
		return err
	}
	if *stableSalt {
		// The stable salt is stored in the state database, not derived from
		// a key: the point of the mode is that the same reader gets the
		// same pseudonym in every dump, which means the salt itself must
		// outlive any one dump. It is published in the manifest, so it is
		// linkage and not secrecy.
		salt, err = s.StableSalt(ctx)
		if err != nil {
			return err
		}
	}

	affinity, err := dump.Anonymise(ctx, s.Corpus, c.KAnon, salt, 40)
	if err != nil {
		return fmt.Errorf("anonymise: %w", err)
	}

	snapDir := filepath.Join(*out, dump.VersionDirName(ver))
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return err
	}

	m := &dump.Manifest{
		Version:         dump.ManifestVersion,
		SnapshotVersion: ver,
		BaseVersion:     baseVer,
		BuildTime:       time.Now().UTC().Format(time.RFC3339),
		KAnon:           c.KAnon,
		SaltMode:        saltMode(*stableSalt),
		Salt:            salt,
		Full:            isFull,
		Shards:          map[string]dump.Shard{},
		RowCounts:       map[string]int64{"tag_affinity": int64(len(affinity))},
	}

	// Shard the affinity rows by the low bits of the pseudonym, so the
	// bucketing does not require the raw user id in memory.
	shards := map[int][]dump.Affinity{}
	for _, row := range affinity {
		shard := dump.ShardOfPseudonym(row.Pseudonym)
		shards[shard] = append(shards[shard], row)
	}

	// A delta carries only the shards whose contents differ from the base.
	// The base is read by hashing what it published and hashing what would be
	// published now -- no row-by-row diff, and no assumption that a shard
	// "should" have changed, which is how a delta silently omits a row a peer
	// needed.
	baseDir := filepath.Join(*out, dump.VersionDirName(baseVer))
	var base *dump.Manifest
	if !isFull {
		var err error
		base, err = dump.LoadManifest(baseDir)
		if err != nil {
			return fmt.Errorf("load base manifest: %w", err)
		}
	}

	// A deleted bucket is one the base published and this dump has no rows for.
	// The peer must be told, or it keeps rows for a bucket that no longer exists
	// and its own copy stays self-consistent while being wrong.
	if base != nil {
		removed := missingShards(base, shards)
		for _, key := range removed {
			m.Shards[key] = dump.Shard{Removed: true}
			fmt.Printf("  shard %s removed (no rows in this snapshot)\n", key)
		}
	}

	for shard, rows := range shards {
		sort.Slice(rows, func(i, j int) bool { return rows[i].Pseudonym < rows[j].Pseudonym })
		blob, err := json.Marshal(rows)
		if err != nil {
			return fmt.Errorf("shard %d: %w", shard, err)
		}
		hash := dump.HashBytes(blob)
		key := fmt.Sprint(shard)
		name := dump.ShardFileName(key)

		// Delta rule: omit a shard that is byte-identical to the base's.
		//
		// The key is the shard NUMBER as a string, not the file name. This
		// lookup used the file name, so base.Shards["shard-042.json"] was always
		// absent, `ok` was always false, and every shard was written even though
		// the hashes were equal. The snapshot verified, called itself a delta,
		// and was the size of a full one.
		if base != nil {
			if prev, ok := base.Shards[key]; ok && prev.Hash == hash {
				continue // unchanged; the peer already has it
			}
		}
		// .json.zst, and the content is plain JSON.
		//
		// The first version of this wrote uncompressed JSON under a .zst
		// name, on the assumption that a compression step would be added
		// later. None ever was: the project carries no zstd dependency, on
		// purpose, because the target is a Pi and a static 12 MB binary is
		// the whole point. Nothing in the reader is fooled -- `fetch` moves
		// shards as opaque bytes and `Verify` hashes them -- but any third
		// party who sees the extension runs `zstd -d` and gets an error.
		//
		// Two honest options: add a pure-Go zstd (no cgo, but ~1 MB of
		// code and a new dependency in a project that has three on
		// purpose), or name the file for what it is. Compression here
		// saves 7.0 MB to about 2 MB on a snapshot that is fetched over
		// Tor, so it is worth doing eventually -- as its own decision with
		// its own measurement, not as a lie in a filename.
		if err := os.WriteFile(filepath.Join(snapDir, name), blob, 0o644); err != nil {
			return err
		}
		m.Shards[key] = dump.Shard{
			Hash: hash,
			Rows: int64(len(rows)),
			Path: name,
		}
	}

	// Report what the delta actually saved, rather than calling it a delta and
	// shipping the whole snapshot. With a per-dump salt every shard changes, so
	// a delta against a per-dump-salted base is a full snapshot wearing a delta's
	// name -- which is exactly what a peer would measure.
	if base != nil {
		carried, total := dump.DeltaAccounting(base, m)
		onDisk := dump.DeltaBytesOnDisk(snapDir, m)
		fmt.Printf("  delta carries %d of %d shards (%d bytes on disk, base is %s)\n",
			carried, total, onDisk, humanBytes(baseDir, base))
		if carried == total {
			fmt.Println("  NOTE: every shard changed, so this delta is the size of a")
			fmt.Println("        full snapshot. That is what --stable-salt is for.")
		}
	}

	if err := m.Sign(priv, snapDir); err != nil {
		return err
	}
	// The public key travels with the snapshot: a peer needs it to verify,
	// and the key's job is integrity, not identity — a peer who trusts the
	// key out of band gains the anonymity guarantee; one who does not still
	// gets a consistent view.
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.pub"), pub, 0o644); err != nil {
		return err
	}

	fmt.Printf("snapshot v%d written to %s\n", ver, snapDir)
	fmt.Printf("  tag_affinity rows : %d (k=%d, salt=%s)\n", len(affinity), c.KAnon, m.SaltMode)
	fmt.Printf("  shards            : %d\n", len(m.Shards))
	fmt.Printf("  public key        : %s\n", hex.EncodeToString(pub))
	if *stableSalt {
		fmt.Println("  NOTE: --stable-salt makes readers linkable across dumps. That is a")
		fmt.Println("        research affordance and a re-identification risk, not protection.")
	}

	// Retention last, so a failure above leaves the previous versions alone.
	if *keep > 0 {
		removed, err := dump.Prune(*out, *keep)
		if err != nil {
			return fmt.Errorf("retention: %w", err)
		}
		if len(removed) > 0 {
			fmt.Printf("  retention: removed v%s (keeping %d)\n",
				joinInts(removed), *keep)
		}
	}
	return nil
}

// missingShards returns the KEYS of the base's shards that this dump has no rows
// for. The key is the shard number as a string, because that is what
// Manifest.Shards is keyed by -- returning file names here put entries into the
// new manifest under a key no reader would look up.
func missingShards(base *dump.Manifest, shards map[int][]dump.Affinity) []string {
	var out []string
	for key := range base.Shards {
		n, err := strconv.Atoi(key)
		if err != nil {
			continue
		}
		if _, ok := shards[n]; !ok {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// humanBytes is the size of everything the BASE published, so the log line can
// show the saving as a ratio rather than asking the reader to divide.
func humanBytes(baseDir string, base *dump.Manifest) string {
	var n int64
	for _, sh := range base.Shards {
		st, err := os.Stat(filepath.Join(baseDir, sh.Path))
		if err != nil {
			continue
		}
		n += st.Size()
	}
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func joinInts(xs []int) string {
	parts := make([]string, len(xs))
	for i, v := range xs {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, " ")
}

func saltMode(stable bool) string {
	if stable {
		return "stable"
	}
	return "per-dump"
}

// runVerify checks a snapshot's signature and shard hashes.
func runVerify(ctx context.Context, args []string) error {
	fs := newFlagSet("verify")
	dir := fs.String("dir", "", "snapshot directory (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("--dir is required")
	}
	pub, err := os.ReadFile(filepath.Join(*dir, "manifest.pub"))
	if err != nil {
		return fmt.Errorf("read public key: %w", err)
	}
	m, err := dump.Verify(*dir, ed25519.PublicKey(pub))
	if err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	fmt.Printf("manifest v%d verified (k=%d, salt=%s, full=%v)\n",
		m.Version, m.KAnon, m.SaltMode, m.Full)

	// Hash every shard and compare against the manifest. A shard whose
	// hash differs is refused, and so is the whole snapshot: a partially
	// valid snapshot is not a snapshot.
	var checked, bad int
	for _, sh := range m.Shards {
		blob, err := os.ReadFile(filepath.Join(*dir, sh.Path))
		if err != nil {
			return fmt.Errorf("shard %s: %w", sh.Path, err)
		}
		if got := dump.HashBytes(blob); got != sh.Hash {
			fmt.Printf("  BAD  %s: hash %s does not match manifest %s\n", sh.Path, got, sh.Hash)
			bad++
			continue
		}
		checked++
	}
	if bad > 0 {
		return fmt.Errorf("%d of %d shards failed their hash", bad, len(m.Shards))
	}
	fmt.Printf("  %d shards verified by content hash\n", checked)
	return nil
}

// runFetch pulls a snapshot from a peer's onion service.
//
// It accepts only a .onion base URL, and the client it builds has no
// clearnet path, so there is nothing to fall back to.
func runFetch(ctx context.Context, args []string) error {
	fs := newFlagSet("fetch")
	base := fs.String("from", "", "peer's onion base URL, e.g. http://<56 chars>.onion")
	into := fs.String("into", "", "local directory to write into")
	timeout := fs.Duration("timeout", 60*time.Second, "overall timeout")
	socks := fs.String("socks", onion.DefaultSocks, "Tor SOCKS address (loopback only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *base == "" || *into == "" {
		return errors.New("--from and --into are required")
	}
	if err := onion.CheckBase(*base); err != nil {
		return err
	}
	client, err := onion.New(*socks, *timeout)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*into, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"manifest.json", "manifest.canonical", "manifest.sig", "manifest.pub"} {
		blob, err := client.Get(ctx, *base, name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*into, name), blob, 0o644); err != nil {
			return err
		}
	}
	// Verify what arrived before trusting it.
	m, err := dump.Verify(*into, mustPub(*into))
	if err != nil {
		return fmt.Errorf("fetched manifest does not verify: %w", err)
	}
	fmt.Printf("fetched snapshot v%d from %s\n", m.Version, *base)
	fmt.Printf("  k=%d salt=%s shards=%d\n", m.KAnon, m.SaltMode, len(m.Shards))
	names := make([]string, 0, len(m.Shards))
	for _, sh := range m.Shards {
		names = append(names, sh.Path)
	}
	sort.Strings(names)
	for _, name := range names {
		blob, err := client.Get(ctx, *base, name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*into, name), blob, 0o644); err != nil {
			return err
		}
	}
	return runVerify(ctx, []string{"--dir", *into})
}

func mustPub(dir string) ed25519.PublicKey {
	blob, err := os.ReadFile(filepath.Join(dir, "manifest.pub"))
	if err != nil {
		return nil
	}
	return ed25519.PublicKey(blob)
}

var _ = config.Load
