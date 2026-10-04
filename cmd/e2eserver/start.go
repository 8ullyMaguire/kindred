package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/api"
	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/graph"
	"git.polarisocial.xyz/kindred/kindred/internal/profile"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// start builds the index over the fixture corpus and serves it, returning the
// base URL.
//
// This mirrors `runServe` deliberately rather than shelling out to it: the
// browser tests must exercise the same wiring (graph load, arena ratings,
// tolerated embedding absence) because the failures worth catching live in
// that wiring, not in the HTTP layer. A shell script drifts from it the first
// time a flag changes.
//
// The listener opens BEFORE the index is built, so the caller learns the port
// immediately and a slow build does not look like a failed start.
func start(ctx context.Context, dir, listen string) (string, error) {
	corpusPath := filepath.Join(dir, "corpus.db")
	statePath := filepath.Join(dir, "state.db")

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return "", fmt.Errorf("listen %s: %w", listen, err)
	}
	base := "http://" + ln.Addr().String()

	s, err := store.Open(ctx, statePath, corpusPath)
	if err != nil {
		ln.Close()
		return "", fmt.Errorf("open store: %w", err)
	}
	// NOTE: no `defer s.Close()` here. `start` returns while the server keeps
	// serving, so a deferred close would close the database out from under every
	// in-flight request -- measured, and the symptom is a 500 of
	// "load seeds: sql: database is closed" on a server that is otherwise
	// healthy and whose home page still renders.
	//
	// The store is closed by the shutdown goroutine below, once the listener
	// has actually stopped.

	logger := slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelWarn}))

	// Build the co-occurrence index if it is missing, the same way
	// `kindred ingest` does. A fixture corpus makes this fast; against the
	// real 1.7 GB mirror it is ~100s, which is why E2E uses a fixture.
	if !graph.IndexExists(statePath) {
		b := &graph.Builder{
			Corpus: s.Corpus, Store: execAdapter{s.DB}, TopN: 24, DBPath: statePath,
		}
		if _, err := b.Build(ctx); err != nil {
			ln.Close()
			return "", fmt.Errorf("build index: %w", err)
		}
	}

	// The graph is loaded WITH its frequency and name callbacks. Passing nil is
	// the bug serve.go's comment describes: the graph loads and answers every
	// tag query with an empty list, because PMI with zero marginals is zero and
	// the handler drops every non-positive pair — a correct output for a graph
	// with no frequencies that looks exactly like an empty corpus.
	g, gerr := graph.LoadCSR(ctx, statePath, s.Corpus,
		func() (map[int32]int64, error) { return graph.TagFrequencies(ctx, s.Corpus) },
		func() ([]string, error) {
			n, err := s.MetaInt(ctx, "node_count")
			if err != nil {
				return nil, err
			}
			return graph.TagNames(ctx, s.Corpus, n)
		},
	)
	if gerr != nil {
		// Not fatal: the read surface needs no graph. Logged so the suite can
		// assert the degradation happened rather than discover it as a 503.
		logger.Warn("no co-occurrence index; tag-similarity endpoints will answer 503",
			"err", gerr)
		g = nil
	}

	// An arena with no ratings is the normal state on a fresh fixture. The
	// signal then reports itself degraded, which is the point: an inert signal
	// must SAY it is inert rather than vanish from the weights.
	ratings, median, rerr := s.EffectiveRatings(ctx)
	if rerr != nil {
		logger.Warn("arena ratings unavailable; peer_rating will report degraded",
			"err", rerr)
		ratings, median = map[int64]float64{}, 0
	}

	eng := &engine.Engine{
		Store:    s,
		Corpus:   corpus.NewAO3(s.Corpus),
		Graph:    g,
		PoolSize: engine.DefaultPoolSize,
		TopN:     24,
		EmbedDim: 32,
	}
	eng.SetArenaRatings(ratings, median)

	srv := &api.Server{
		Engine:    eng,
		Store:     s,
		Log:       logger,
		Version:   "e2e",
		StartedAt: time.Now(),
	}

	// The profile store is a SEPARATE handle on the state db, not a use of the
	// corpus, and the browser suite needs it wired or /profiles and /profile
	// would 503 for the whole run.
	//
	// It cannot be `s`: profile.Store is a distinct type over its own schema,
	// and reusing the store's handle would couple the two lifetimes -- the
	// same class of bug the "no defer s.Close()" note above exists to prevent.
	// It is closed by the shutdown goroutine, after Shutdown returns.
	profDB, err := sql.Open("sqlite", statePath)
	if err != nil {
		ln.Close()
		return "", fmt.Errorf("open profile state db: %w", err)
	}
	profs := profile.NewStore(profDB)
	if err := profs.EnsureSchema(ctx); err != nil {
		profDB.Close()
		ln.Close()
		return "", fmt.Errorf("profile schema: %w", err)
	}
	srv.Profiles = profs

	httpSrv := &http.Server{
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error("serve", "err", err)
		}
	}()
	// Clean shutdown is not optional: the state database is opened read-write,
	// and a killed process leaves a -wal file the next open must recover,
	// which in CI is a flaky test rather than an incident.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		// After Shutdown returns, no handler is running, so this is the first
		// moment closing the store cannot break an in-flight request.
		s.Close()
		profDB.Close()
	}()

	return base, nil
}

// execAdapter bridges *sql.DB to graph.Executor, mirroring cmd/kindred's own
// adapter. The interface is a single Exec method, not database/sql's full
// surface, so an adapter that forwards ExecContext does not satisfy it.
type execAdapter struct{ DB *sql.DB }

func (e execAdapter) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return e.DB.ExecContext(ctx, q, args...)
}
