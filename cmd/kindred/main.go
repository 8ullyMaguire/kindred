// Command kindred indexes a corpus, recommends from it, serves a
// read-only API over it, and publishes anonymised snapshots of it.
//
// Every subcommand is offline by construction: nothing in the request
// path can make a network call (SPEC §3.2.1), and the only outbound
// dialing in this binary is the onion fetch client, which refuses any
// non-.onion host.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"git.polarisocial.xyz/kindred/kindred/internal/config"
)

const usage = `kindred — a general entity recommender over a local mirror

usage: kindred <command> [flags]

commands:
  serve     run the HTTP API (the read-only mirror, incl. the unofficial AO3 surface)
  ingest    measure the corpus and build the index
  recommend recommend entities from seeds
  stats     print corpus and index statistics
  dump      write an anonymised, signed snapshot for peers
  verify    verify a snapshot's signature and hashes
  fetch     pull a snapshot from a peer's .onion service
  tune      inspect or set signal weights

run "kindred <command> -h" for command flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	// SIGINT/SIGTERM cancel the context so in-flight requests finish and
	// the store closes cleanly. A killed process can leave a -wal file
	// that the next start has to recover.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "serve":
		err = runServe(ctx, args)
	case "ingest":
		err = runIngest(ctx, args)
	case "embed":
		err = runEmbed(ctx, args)
	case "recommend":
		err = runRecommend(ctx, args)
	case "stats":
		err = runStats(ctx, args)
	case "dump":
		err = runDump(ctx, args)
	case "verify":
		err = runVerify(ctx, args)
	case "fetch":
		err = runFetch(ctx, args)
	case "tune":
		err = runTune(ctx, args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "kindred %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

// newFlagSet builds a flag set that reports errors rather than exiting, so
// main can return them.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return fs
}

// loadConfig binds the config flags and parses. It does NOT parse.
//
// The first version of this bound the config flags, parsed, and returned —
// so every command that registered its own flags afterwards had them
// rejected as "flag provided but not defined". `kindred dump --out` was
// unusable, and the failure mode was silent in the worst way: `-k-anon`
// was already bound by the config, so passing it appeared to work while
// `dump`'s own -out did not.
//
// Commands now follow one order: register every flag, then call
// finishConfig to parse and validate.
func bindConfig(fs *flag.FlagSet) *config.Config {
	c := config.Load()
	c.Bind(fs)
	return c
}

// finishConfig parses a flag set whose flags are all registered, and
// checks the one setting every command needs.
func finishConfig(c *config.Config, fs *flag.FlagSet, args []string) (*config.Config, error) {
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if c.CorpusDB == "" {
		return nil, fmt.Errorf("--corpus is required (the read-only mirror to serve from)")
	}
	return c, nil
}
