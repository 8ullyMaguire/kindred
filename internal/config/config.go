// Package config resolves kindred's settings from env and flags.
//
// Every setting has an env form so one tree runs in dev and prod. The
// public-API kill switch is an atomic rather than a field because it must
// be flippable while requests are in flight (SPEC §3.2.4).
package config

import (
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
)

type Config struct {
	Listen   string
	DB       string
	CorpusDB string
	Version  string
	Mode     string // "lite" or "full"
	TopN     int
	EmbedDim int
	KAnon    int

	// StableSalt is env-only on purpose. It is a privacy trade-off, and
	// the spec says enabling it is a logged decision (SPEC §4.2), so it
	// must leave a trace rather than being a flag anyone can pass casually.
	StableSalt bool

	// PublicAPI is the kill switch for the unofficial surface.
	PublicAPI atomic.Bool
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// DefaultDB is the kindred state path. It is always on local disk and
// never on the NFS pool: SQLite on an 8-way mergerfs mount is slow and
// the pool is shared (SPEC §5).
func DefaultDB() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "kindred", "kindred.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "kindred.db"
	}
	return filepath.Join(home, ".local", "share", "kindred", "kindred.db")
}

func Load() *Config {
	c := &Config{
		Listen:     env("KINDRED_LISTEN", "127.0.0.1:8010"),
		DB:         env("KINDRED_DB", DefaultDB()),
		CorpusDB:   env("KINDRED_CORPUS_DB", ""),
		Version:    env("KINDRED_VERSION", "dev"),
		Mode:       env("KINDRED_MODE", "full"),
		TopN:       envInt("KINDRED_TOP_N", 24),
		EmbedDim:   envInt("KINDRED_EMBED_DIM", 32),
		KAnon:      envInt("KINDRED_K_ANON", 20),
		StableSalt: os.Getenv("KINDRED_STABLE_SALT") == "1",
	}
	c.PublicAPI.Store(env("KINDRED_PUBLIC_API", "1") == "1")
	return c
}

// Bind attaches flags to a FlagSet, returning it so main can Parse it.
func (c *Config) Bind(fs *flag.FlagSet) *flag.FlagSet {
	fs.StringVar(&c.Listen, "listen", c.Listen, "listen address")
	fs.StringVar(&c.DB, "db", c.DB, "kindred.db path (local disk, never the pool)")
	fs.StringVar(&c.CorpusDB, "corpus", c.CorpusDB, "read-only corpus SQLite path")
	fs.StringVar(&c.Mode, "mode", c.Mode, "lite|full")
	fs.IntVar(&c.TopN, "top-n", c.TopN, "neighbours retained per tag")
	fs.IntVar(&c.EmbedDim, "embed-dim", c.EmbedDim, "SVD dimensions")
	fs.IntVar(&c.KAnon, "k-anon", c.KAnon, "min bookmarks for a tag_affinity row")
	return fs
}

// Lite reports whether the process is running in the reduced-memory mode
// that drops the embedder and keeps only top-N neighbours per tag.
func (c *Config) Lite() bool { return c.Mode == "lite" }

// StoreDir is where dumps and their secret key live.
func (c *Config) StoreDir() string { return filepath.Dir(c.DB) }
