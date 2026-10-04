package crawl

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrOfflineMode is returned by an Offline client's Fetch.
//
// It is ErrOffline's own value rather than a new sentinel: callers switch on
// ErrOffline with errors.Is, and having two spellings of "you asked me not to
// use the network" is how one of them gets left unchecked.
var ErrOfflineMode = ErrOffline

// Offline is a Client that refuses every fetch.
//
// It exists because offline mode as a *flag* is easy to get wrong: the sibling
// tool's offline mode still called fetch_robots_delay(), which makes a real
// HTTP request to robots.txt, and broke the mode's one promise — no HTTP at
// all — in exactly the Cloudflare-blocked situation that prompted the flag in
// the first place.
//
// Making it a TYPE rather than a boolean means the refusal is structural. There
// is no code path in which an Offline client performs I/O, because it has no
// client to perform it with.
type Offline struct {
	// Reason is reported so an operator can see WHY nothing was fetched, rather
	// than finding an empty mirror and no explanation.
	Reason string
}

// NewOffline returns an offline client.
func NewOffline(reason string) *Offline { return &Offline{Reason: reason} }

// Fetch always fails with ErrOffline.
func (o *Offline) Fetch(context.Context, string) ([]byte, error) {
	if o.Reason != "" {
		return nil, fmt.Errorf("%w (%s)", ErrOffline, o.Reason)
	}
	return nil, ErrOffline
}

// Delay reports zero, because there is no request to space out.
func (o *Offline) Delay() time.Duration { return 0 }

// Close is a no-op.
func (o *Offline) Close() error { return nil }

// LoadRobotsDelay returns the default without any network call.
//
// This is the method the sibling got wrong, and the comment there is the reason
// it is spelled out here: reading robots.txt IS an HTTP request, so an offline
// client that implemented this by fetching would violate the mode it exists to
// guarantee.
func (o *Offline) LoadRobotsDelay(context.Context) (time.Duration, error) {
	return DefaultCrawlDelay, nil
}

// ---------------------------------------------------------------- sink

// Sink persists parsed works into the mirror.
//
// The interface exists because the crawl must be usable against a throwaway
// fixture database in tests and the real 1.7 GB mirror in production, and
// because the write path is where this estate's bugs have actually lived: the
// sibling tool had three defects in its batch write path, none of which were
// arithmetic.
type Sink interface {
	// Save writes or updates one work and its tags. It must be idempotent:
	// re-crawling a work must not duplicate its tag rows.
	Save(ctx context.Context, w *ParsedWork) error
	// Close flushes.
	Close() error
}

// SQLSink writes to a SQLite mirror.
type SQLSink struct {
	db *sql.DB
	// Now is injected so `first_seen`/`last_updated` are reproducible in tests.
	Now func() time.Time
}

// NewSQLSink wraps a database handle.
func NewSQLSink(db *sql.DB) *SQLSink {
	return &SQLSink{db: db, Now: func() time.Time { return time.Now().UTC() }}
}

// Save writes a work and its tags.
//
// Idempotent by construction: works are upserted on id, and tags are deleted
// then re-inserted for that work. Re-crawling a work whose tags changed must
// leave one row per tag, not two, and an INSERT that only adds is how a mirror
// accumulates 3.9M rows where 3.8M distinct pairs exist.
func (s *SQLSink) Save(ctx context.Context, w *ParsedWork) error {
	if w == nil {
		return errors.New("crawl: nil work")
	}
	if w.ID <= 0 {
		return fmt.Errorf("crawl: work %q has no id", w.Title)
	}
	now := s.Now().Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("crawl: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `INSERT INTO works
		(id,url,title,authors,summary,rating,word_count,hits,kudos,bookmarks,
		 chapters,language,complete,update_date,first_seen,last_updated)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			url=excluded.url, title=excluded.title, authors=excluded.authors,
			summary=excluded.summary, rating=excluded.rating,
			word_count=excluded.word_count, hits=excluded.hits,
			kudos=excluded.kudos,
			-- bookmarks is only overwritten when the page carried the meta tag.
			-- COALESCE keeps a previously-known count instead of nulling it,
			-- because a page that omits the tag means "unknown", not "zero".
			bookmarks=COALESCE(excluded.bookmarks, works.bookmarks),
			chapters=excluded.chapters, language=excluded.language,
			complete=excluded.complete, update_date=excluded.update_date,
			last_updated=excluded.last_updated`,
		w.ID, w.URL, w.Title, w.Authors, w.Summary, w.Rating, w.WordCount,
		w.Hits, w.Kudos, w.Bookmarks, w.Chapters, w.Language, boolInt(w.Complete),
		w.UpdateDate, now, now); err != nil {
		return fmt.Errorf("crawl: upsert work %d: %w", w.ID, err)
	}

	// Replace this work's tags wholesale, so a re-crawl cannot double them.
	if _, err := tx.ExecContext(ctx, `DELETE FROM work_tags WHERE work_id = ?`, w.ID); err != nil {
		return fmt.Errorf("crawl: clear tags %d: %w", w.ID, err)
	}
	for _, t := range w.Tags {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tags(name) VALUES(?) ON CONFLICT(name) DO NOTHING`, t.Name); err != nil {
			return fmt.Errorf("crawl: tag %q: %w", t.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO work_tags(work_id,tag_id,tag_type)
			 SELECT ?, id, ? FROM tags WHERE name = ?`,
			w.ID, t.Type, t.Name); err != nil {
			return fmt.Errorf("crawl: link tag %q: %w", t.Name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("crawl: commit work %d: %w", w.ID, err)
	}
	return nil
}

// Close is a no-op; the caller owns the database handle.
func (s *SQLSink) Close() error { return nil }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SaveAll writes every work, stopping at the first failure but reporting which.
//
// One work's malformed tags must not cost the other 689, so the loop continues
// past a failure and the caller gets the full list of what did not land.
func SaveAll(ctx context.Context, sink Sink, works []*ParsedWork) (saved int, failed map[string]error) {
	failed = map[string]error{}
	for _, w := range works {
		if err := ctx.Err(); err != nil {
			failed[w.URL] = err
			return saved, failed
		}
		if err := sink.Save(ctx, w); err != nil {
			key := w.URL
			if key == "" {
				key = w.Title
			}
			failed[key] = err
			continue
		}
		saved++
	}
	return saved, failed
}

// TagTypeIsFandom reports whether an AO3 page tag category is a fandom.
//
// It exists so a caller does not hand-roll the string comparison and get
// `Fandom` vs `fandoms` wrong, which is the kind of mismatch that silently
// classifies nothing and reports success.
func TagTypeIsFandom(t string) bool {
	return strings.EqualFold(strings.TrimSpace(t), "fandom") ||
		strings.EqualFold(strings.TrimSpace(t), "fandoms")
}
