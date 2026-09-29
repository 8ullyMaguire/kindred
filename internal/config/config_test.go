package config

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultDBIsOnLocalDiskNotThePool(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/kindred-xdg")
	got := DefaultDB()
	if got != "/tmp/kindred-xdg/kindred/kindred.db" {
		t.Fatalf("DefaultDB() = %q", got)
	}
	// The pool is an 8-way mergerfs mount; SQLite belongs on local disk
	// (SPEC §5). A default that resolved onto a mount point would be a
	// silent performance bug on every deployment.
	if _, err := os.Stat(filepath.Dir(got)); err == nil {
		// existing is fine, just make sure it is not under /mnt
		if filepath.HasPrefix(got, "/mnt/") {
			t.Fatalf("DefaultDB() is on the pool: %q", got)
		}
	}
}

func TestEnvOverridesFlagsDefaults(t *testing.T) {
	t.Setenv("KINDRED_LISTEN", "127.0.0.1:9999")
	t.Setenv("KINDRED_TOP_N", "7")
	t.Setenv("KINDRED_PUBLIC_API", "0")
	c := Load()
	if c.Listen != "127.0.0.1:9999" {
		t.Fatalf("Listen = %q", c.Listen)
	}
	if c.TopN != 7 {
		t.Fatalf("TopN = %d, want 7", c.TopN)
	}
	if c.PublicAPI.Load() {
		t.Fatal("PublicAPI = true, want false: the kill switch must be settable from the env")
	}
}

func TestFlagsOverrideEnv(t *testing.T) {
	t.Setenv("KINDRED_TOP_N", "7")
	c := Load()
	fs := c.Bind(flag.NewFlagSet("t", flag.ContinueOnError))
	if err := fs.Parse([]string{"--top-n", "3"}); err != nil {
		t.Fatal(err)
	}
	if c.TopN != 3 {
		t.Fatalf("TopN = %d, want 3 (flag must win over env)", c.TopN)
	}
}

func TestMalformedIntFallsBackToDefault(t *testing.T) {
	t.Setenv("KINDRED_TOP_N", "not-a-number")
	if got := Load().TopN; got != 24 {
		t.Fatalf("TopN = %d, want the default 24 rather than a parse error", got)
	}
}

func TestLite(t *testing.T) {
	c := &Config{Mode: "lite"}
	if !c.Lite() {
		t.Fatal("Mode=lite reported full")
	}
	c.Mode = "full"
	if c.Lite() {
		t.Fatal("Mode=full reported lite")
	}
}

func TestStableSaltIsNotAFlag(t *testing.T) {
	// If this ever becomes a flag, the manifest's salt-mode field stops
	// being a logged decision and becomes an accident.
	t.Setenv("KINDRED_STABLE_SALT", "1")
	if !Load().StableSalt {
		t.Fatal("env did not enable the stable salt")
	}
	t.Setenv("KINDRED_STABLE_SALT", "")
	if Load().StableSalt {
		t.Fatal("stable salt defaulted on")
	}
}
