package api

// Themes (#47): the work clusters, readable.
//
//	GET /api/v1/themes            the themes, biggest first, with their labels and a sample
//	GET /api/v1/themes/{id}       the works in one theme
//
// The tree is built offline by `kindred worktree --write` and lives in work_family /
// work_family_member, so these routes are a read of a table, not a clustering per request. An
// empty table is answered as `built: false` rather than as an empty list: a reader who asks for
// themes on a deployment that has never clustered them should be told why there are none.

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
)

// themeRow is one theme in the list.
type themeRow struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
	Size  int    `json:"size"`
	// Sample is a few of the theme's highest-support works, so the label can be checked against
	// what is actually in the cluster rather than taken on trust.
	Sample []themeSample `json:"sample,omitempty"`
}

type themeSample struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

func (s *Server) handleThemes(w http.ResponseWriter, r *http.Request) {
	rows, built, err := s.themesFromState(r)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if !built {
		writeJSON(w, http.StatusOK, map[string]any{
			"built":  false,
			"themes": []themeRow{},
			"note":   "no theme tree on this deployment yet: run 'kindred worktree --write'",
		})
		return
	}
	// Biggest first, then id: a theme of 40 works is a curiosity, one of 3,000 is a way in.
	sort.Slice(rows, func(a, b int) bool {
		if rows[a].Size != rows[b].Size {
			return rows[a].Size > rows[b].Size
		}
		return rows[a].ID < rows[b].ID
	})
	if n := clampLimit(intParam(r, "limit", 60), 500); n < len(rows) {
		rows = rows[:n]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"built":  true,
		"count":  len(rows),
		"themes": rows,
	})
}

func (s *Server) handleThemeWorks(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("theme id must be a number, got %q", r.PathValue("id")))
		return
	}
	all, built, err := s.themesFromState(r)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	if !built {
		writeErr(w, http.StatusNotFound,
			errors.New("no theme tree on this deployment yet: run 'kindred worktree --write'"))
		return
	}
	var found *themeRow
	for i := range all {
		if all[i].ID == id {
			found = &all[i]
			break
		}
	}
	if found == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no theme %d", id))
		return
	}
	works, err := s.themeWorks(r, id, clampLimit(intParam(r, "limit", 40), 200))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    found.ID,
		"label": found.Label,
		"size":  found.Size,
		"works": works,
	})
}

// themesFromState reads the family table, its subfamilies folded into their parents.
//
// A reader asking "what themes are there" means the top level; the subfamilies are how a big
// theme is divided internally, and listing 2,300 of them would bury the 200 that matter.
func (s *Server) themesFromState(r *http.Request) ([]themeRow, bool, error) {
	if s.Store == nil || s.Store.DB == nil {
		return nil, false, errors.New("no state database")
	}
	rows, err := s.Store.DB.QueryContext(r.Context(), `
		SELECT id, label, size FROM work_family WHERE parent IS NULL`)
	if err != nil {
		if isMissingTableErr(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("themes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []themeRow
	for rows.Next() {
		var t themeRow
		if err := rows.Scan(&t.ID, &t.Label, &t.Size); err != nil {
			return nil, false, fmt.Errorf("themes: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("themes: %w", err)
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	for i := range out {
		sample, err := s.themeWorks(r, out[i].ID, 3)
		if err != nil {
			return nil, false, err
		}
		out[i].Sample = sample
	}
	return out, true, nil
}

// themeWorks lists a family's works, highest kudos first.
//
// Title and kudos come from the CORPUS, the membership from the STATE database: two databases,
// one answer, and the join happens here because the corpus is read-only and often on another
// filesystem.
func (s *Server) themeWorks(r *http.Request, family int, limit int) ([]themeSample, error) {
	rows, err := s.Store.DB.QueryContext(r.Context(), `
		SELECT work_id FROM work_family_member
		 WHERE family = ? OR family IN (SELECT id FROM work_family WHERE parent = ?)
		 LIMIT 5000`, family, family)
	if err != nil {
		if isMissingTableErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("theme works: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("theme works: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("theme works: %w", err)
	}
	if len(ids) == 0 || s.Engine == nil || s.Engine.Corpus == nil {
		return nil, nil
	}
	titles, err := corpusTitles(r, s.Engine.Corpus, ids)
	if err != nil {
		return nil, err
	}
	out := make([]themeSample, 0, len(ids))
	for _, id := range ids {
		if t, ok := titles[id]; ok {
			out = append(out, themeSample{ID: id, Title: t})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	if limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// corpusTitles reads work titles in chunks, because a theme can hold 5,000 works and one
// IN (...) with 5,000 placeholders is past SQLite's variable limit.
func corpusTitles(r *http.Request, c *corpus.AO3, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	const chunk = 400
	for i := 0; i < len(ids); i += chunk {
		j := i + chunk
		if j > len(ids) {
			j = len(ids)
		}
		part := ids[i:j]
		args := make([]any, len(part))
		for k, id := range part {
			args[k] = id
		}
		rows, err := c.DB.QueryContext(r.Context(),
			`SELECT id, title FROM works WHERE id IN (`+questionMarkList(len(part))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("theme titles: %w", err)
		}
		for rows.Next() {
			var id int64
			var title string
			if err := rows.Scan(&id, &title); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("theme titles: %w", err)
			}
			out[id] = title
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("theme titles: %w", err)
		}
		_ = rows.Close()
	}
	return out, nil
}

func questionMarkList(n int) string {
	if n == 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func clampLimit(n, max int) int {
	if n > max {
		return max
	}
	if n < 1 {
		return 1
	}
	return n
}

func isMissingTableErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}
