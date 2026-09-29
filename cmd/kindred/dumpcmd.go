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
	c, err := finishConfig(c, fs, args)
	if err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out is required: the snapshot goes to a directory, never to a listener")
	}

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

	snapDir := filepath.Join(*out, fmt.Sprintf("v%d", *version))
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return err
	}

	m := &dump.Manifest{
		Version:   dump.ManifestVersion,
		BuildTime: time.Now().UTC().Format(time.RFC3339),
		KAnon:     c.KAnon,
		SaltMode:  saltMode(*stableSalt),
		Salt:      salt,
		Full:      true,
		Shards:    map[string]dump.Shard{},
		RowCounts: map[string]int64{"tag_affinity": int64(len(affinity))},
	}

	// Shard the affinity rows by the low bits of the pseudonym, so the
	// bucketing does not require the raw user id in memory.
	shards := map[int][]dump.Affinity{}
	for _, row := range affinity {
		shard := dump.ShardOfPseudonym(row.Pseudonym)
		shards[shard] = append(shards[shard], row)
	}
	for shard, rows := range shards {
		sort.Slice(rows, func(i, j int) bool { return rows[i].Pseudonym < rows[j].Pseudonym })
		blob, err := json.Marshal(rows)
		if err != nil {
			return fmt.Errorf("shard %d: %w", shard, err)
		}
		hash := dump.HashBytes(blob)
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
		name := fmt.Sprintf("shard-%03d.json", shard)
		if err := os.WriteFile(filepath.Join(snapDir, name), blob, 0o644); err != nil {
			return err
		}
		m.Shards[fmt.Sprint(shard)] = dump.Shard{
			Hash: hash,
			Rows: int64(len(rows)),
			Path: name,
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

	fmt.Printf("snapshot v%d written to %s\n", *version, snapDir)
	fmt.Printf("  tag_affinity rows : %d (k=%d, salt=%s)\n", len(affinity), c.KAnon, m.SaltMode)
	fmt.Printf("  shards            : %d\n", len(m.Shards))
	fmt.Printf("  public key        : %s\n", hex.EncodeToString(pub))
	if *stableSalt {
		fmt.Println("  NOTE: --stable-salt makes readers linkable across dumps. That is a")
		fmt.Println("        research affordance and a re-identification risk, not protection.")
	}
	return nil
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
