package web

// The themes pages (#47): the work clusters, for a reader.
//
//	/themes          what themes exist, biggest first, with three works to check each label
//	/themes/{id}     one theme's works
//
// The clustering happens offline (`kindred worktree --write`) and this page reads the tables.
// That is deliberate: a cluster over 60,000 works takes a minute, and a page that took a minute
// would be a page nobody opens twice.
//
// The list page states what a theme IS in its own voice, because "Theme 24 · 1,281 works" reads
// like a category somebody defined, and what this actually is, is where 1,281 works landed when
// their tags were averaged. The distinction matters to a reader deciding whether to trust it.

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ThemesPage is the list of themes.
type ThemesPage struct {
	Base
	Themes []ThemeCard
	// Built is false when this deployment has never clustered works: an empty list and an
	// unbuilt tree look identical to a reader and are different facts.
	Built bool
}

// ThemeCard is one theme in the list.
type ThemeCard struct {
	ID     int
	Label  string
	Size   int
	Sample []ThemeWork
}

// ThemeWork is one work in a theme.
type ThemeWork struct {
	ID    int64
	Title string
}

// ThemePage is one theme's works.
type ThemePage struct {
	Base
	ID    int
	Label string
	Size  int
	Works []ThemeWork
	// Truncated is true when the theme holds more works than the page shows.
	Truncated bool
	Shown     int
}

// themeQuery is the read side of work_family, shared by both pages.
type themeStore struct {
	db *sql.DB
}

func (t themeStore) families(r *http.Request) ([]ThemeCard, bool, error) {
	rows, err := t.db.QueryContext(r.Context(),
		`SELECT id, label, size FROM work_family WHERE parent IS NULL ORDER BY size DESC, id`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("themes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ThemeCard
	for rows.Next() {
		var c ThemeCard
		if err := rows.Scan(&c.ID, &c.Label, &c.Size); err != nil {
			return nil, false, fmt.Errorf("themes: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("themes: %w", err)
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	return out, true, nil
}

// works lists a family's works, its subfamilies included.
//
// A subfamily is how a big theme is divided internally; a reader who opens theme 24 wants the
// theme, not the sixth of it that happens to be a subfamily.
func (t themeStore) works(r *http.Request, family int, limit int) ([]int64, int, error) {
	rows, err := t.db.QueryContext(r.Context(), `
		SELECT work_id FROM work_family_member
		 WHERE family = ? OR family IN (SELECT id FROM work_family WHERE parent = ?)`,
		family, family)
	if err != nil {
		return nil, 0, fmt.Errorf("theme works: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, 0, fmt.Errorf("theme works: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("theme works: %w", err)
	}
	total := len(ids)
	// Stable output: ids ascending. The table has no natural order, and a page whose rows move
	// between requests is a page a reader cannot finish reading.
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, total, nil
}

func (d Deps) renderThemes(w http.ResponseWriter, r *http.Request) {
	base := d.baseOf(r, "Themes", "Themes")
	if d.Engine == nil || d.Engine.Store == nil || d.Engine.Store.DB == nil {
		d.fail(w, r, http.StatusServiceUnavailable, errors.New("no state database"))
		return
	}
	ts := themeStore{db: d.Engine.Store.DB}
	cards, built, err := ts.families(r)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	page := ThemesPage{Base: base, Built: built}
	if !built {
		d.page(w, "themes.html", page, http.StatusOK)
		return
	}
	limit := 40
	if n, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && n > 0 {
		limit = n
	}
	if limit > 200 {
		limit = 200
	}
	if len(cards) > limit {
		cards = cards[:limit]
	}
	for i := range cards {
		works, err := d.themeWorks(r, cards[i].ID, 3)
		if err != nil {
			d.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		cards[i].Sample = works
	}
	page.Themes = cards
	d.page(w, "themes.html", page, http.StatusOK)
}

func (d Deps) renderTheme(w http.ResponseWriter, r *http.Request, arg string) {
	id, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil {
		d.fail(w, r, http.StatusNotFound, fmt.Errorf("theme %q is not a number", arg))
		return
	}
	base := d.baseOf(r, fmt.Sprintf("Theme %d", id), fmt.Sprintf("Theme %d", id))
	if d.Engine == nil || d.Engine.Store == nil || d.Engine.Store.DB == nil {
		d.fail(w, r, http.StatusServiceUnavailable, errors.New("no state database"))
		return
	}
	ts := themeStore{db: d.Engine.Store.DB}
	cards, built, err := ts.families(r)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if !built {
		d.fail(w, r, http.StatusNotFound,
			errors.New("no theme tree on this deployment yet: run 'kindred worktree --write'"))
		return
	}
	var card *ThemeCard
	for i := range cards {
		if cards[i].ID == id {
			card = &cards[i]
			break
		}
	}
	if card == nil {
		d.fail(w, r, http.StatusNotFound, fmt.Errorf("no theme %d", id))
		return
	}
	limit := 60
	if n, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && n > 0 {
		limit = n
	}
	if limit > 300 {
		limit = 300
	}
	works, total, err := d.themeWorksWithTotal(r, id, limit)
	if err != nil {
		d.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	page := ThemePage{
		Base: base, ID: card.ID, Label: card.Label, Size: card.Size,
		Works: works, Shown: len(works), Truncated: total > len(works),
	}
	d.page(w, "theme.html", page, http.StatusOK)
}

// workTitles reads titles in chunks: a theme can hold thousands of works and one IN (...) with
// thousands of placeholders is past SQLite's variable limit.
func (d Deps) workTitles(r *http.Request, ids []int64) (map[int64]string, error) {
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
		rows, err := d.Engine.Corpus.DB.QueryContext(r.Context(),
			`SELECT id, title FROM works WHERE id IN (`+qmList(len(part))+`)`, args...)
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

func qmList(n int) string {
	if n == 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// themeWorksWithTotal is themeWorks plus the size before truncation, which is what tells a
// reader whether they are looking at a theme or at the top of one.
func (d Deps) themeWorksWithTotal(r *http.Request, family, limit int) ([]ThemeWork, int, error) {
	ts := themeStore{db: d.Engine.Store.DB}
	ids, total, err := ts.works(r, family, limit)
	if err != nil {
		return nil, 0, err
	}
	if len(ids) == 0 || d.Engine.Corpus == nil {
		out := make([]ThemeWork, 0, len(ids))
		for _, id := range ids {
			out = append(out, ThemeWork{ID: id})
		}
		return out, total, nil
	}
	titles, err := d.workTitles(r, ids)
	if err != nil {
		return nil, 0, err
	}
	out := make([]ThemeWork, 0, len(ids))
	for _, id := range ids {
		out = append(out, ThemeWork{ID: id, Title: titles[id]})
	}
	return out, total, nil
}

// themeWorks resolves ids to titles through the corpus.
func (d Deps) themeWorks(r *http.Request, family, limit int) ([]ThemeWork, error) {
	ts := themeStore{db: d.Engine.Store.DB}
	ids, _, err := ts.works(r, family, limit)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 || d.Engine == nil || d.Engine.Corpus == nil {
		// The ids are known even without titles; a page that dropped the works because it could
		// not name them would report an empty theme.
		out := make([]ThemeWork, 0, len(ids))
		for _, id := range ids {
			out = append(out, ThemeWork{ID: id})
		}
		return out, nil
	}
	titles, err := d.workTitles(r, ids)
	if err != nil {
		return nil, err
	}
	out := make([]ThemeWork, 0, len(ids))
	for _, id := range ids {
		out = append(out, ThemeWork{ID: id, Title: titles[id]})
	}
	return out, nil
}
