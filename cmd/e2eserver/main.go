// Package e2eserver boots a real kindred over a fixture corpus for the
// browser tests.
//
// ## Why a helper binary rather than a shell script
//
// The Playwright suite needs a server that is genuinely the product: the real
// binary, the real handlers, the real templates, the real store. Starting one
// means ingest -> embed -> serve against a corpus, and doing that from a shell
// script means re-deriving the flag set in a second language every time a flag
// changes. Doing it in Go means the harness is compiled against the same
// command package the binary uses, so a rename breaks the build instead of
// producing a confusing 404 at test time.
//
// ## Why a fixture corpus and not the real one
//
// The real mirror is 1.7 GB and lives only on thinkcentre, so an E2E suite
// against it could not run in CI and would take minutes per run. The fixture
// is 40 works, deterministic, and the same `internal/testcorpus` corpus the Go
// tests use — so the browser asserts against the same data shape the unit
// tests do.
//
// It is still NOT a mock: this process runs the real ingest, the real index
// build, and the real HTTP server. Only the size is fake.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/testcorpus"
)

func main() {
	var (
		dir     = flag.String("dir", "", "scratch directory (required)")
		corpus  = flag.Int("works", 40, "fixture corpus size")
		listen  = flag.String("listen", "127.0.0.1:0", "listen address; :0 picks a free port")
		ready   = flag.String("ready-file", "", "write the chosen base URL here once serving")
		keepFor = flag.Duration("up", 0, "exit after this long (0 = until signalled)")
	)
	flag.Parse()

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "e2eserver: -dir is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "e2eserver: %v\n", err)
		os.Exit(1)
	}

	corpusPath := filepath.Join(*dir, "corpus.db")
	if _, err := os.Stat(corpusPath); os.IsNotExist(err) {
		if _, err := testcorpus.New(*corpus).Write(corpusPath); err != nil {
			fmt.Fprintf(os.Stderr, "e2eserver: build fixture corpus: %v\n", err)
			os.Exit(1)
		}
	}

	base, err := start(context.Background(), *dir, *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2eserver: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("e2eserver: serving on %s\n", base)
	if *ready != "" {
		if err := os.WriteFile(*ready, []byte(base), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "e2eserver: write ready file: %v\n", err)
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *keepFor > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(*keepFor):
		}
		return
	}
	<-ctx.Done()
	fmt.Println("e2eserver: shutting down")
}
