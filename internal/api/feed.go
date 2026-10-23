package api

// Feeds: the mirror, subscribable (#61) and discoverable (#62).
//
// ## Why RSS 2.0 and not Atom
//
// The roadmap row says RSS, and every reader that matters speaks it. Atom's
// advantages are typed dates and stable identity — the second of which the
// corpus cannot supply: a work has no revision history here, only an
// update_date, so "updated" means "the mirror saw a new date", not "the author
// revised it". A format that invites a precision the data does not have is the
// wrong format.
//
// ## What a feed entry may claim
//
// An entry is dated by the WORK, never by when kindred computed it. The tag
// feed is therefore honest about the only question a subscriber has: has
// anything arrived under this tag since I last looked. The seed feed recomputes
// its list per request, so it has no arrival date at all; it reports the works'
// own dates and says in the channel description that the list is computed on
// demand. Inventing a timestamp is an assertion the mirror cannot support.

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/corpus"
	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// rssDoc is RSS 2.0. The struct is the whole format this server emits: no
// namespaces, no extensions, because a client that has to understand an
// extension to read the feed is a client that will not.
type rssDoc struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	Language      string    `xml:"language"`
	LastBuildDate string    `xml:"lastBuildDate"`
	Items         []rssItem `xml:"item"`
}

type rssItem struct {
	Title       string  `xml:"title"`
	Link        string  `xml:"link"`
	GUID        rssGUID `xml:"guid"`
	PubDate     string  `xml:"pubDate"`
	Description string  `xml:"description"`
}

// rssGUID carries isPermaLink as an ATTRIBUTE, which is what makes the guid a
// URL a client can resolve rather than an opaque string it invents. A relative
// guid is invalid RSS and clients respond by minting their own identity per
// fetch, so every refresh looks like new works.
type rssGUID struct {
	Value       string `xml:",chardata"`
	IsPermaLink bool   `xml:"isPermaLink,attr"`
}

// feedItemLimit is the default and the ceiling for entries per feed.
const (
	feedDefaultItems = 50
	feedMaxItems     = 200
)

// absoluteBase derives the scheme and host a feed's links must carry.
//
// RSS requires absolute URLs, and this deployment has no configured public
// address: it sits behind a tunnel, and the hostname the reader typed is the
// only one that is correct. So the base comes from the request, preferring the
// forwarded headers a reverse proxy sets. Host with a port is preserved; a
// missing scheme defaults to http, which is what a loopback request is.
func absoluteBase(r *http.Request) string {
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.TrimSpace(strings.Split(p, ",")[0])
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0])
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// feedRequestedItems parses ?n= with the same bounds every list surface uses.
func feedRequestedItems(r *http.Request) int {
	n := atoiDefault(r.URL.Query().Get("n"), feedDefaultItems)
	if n < 1 {
		n = 1
	}
	if n > feedMaxItems {
		n = feedMaxItems
	}
	return n
}

// handleFeedTag serves works carrying a tag, most recently updated first.
//
// The order is corpus.ChronologicalUpdateDateDesc, not `update_date DESC`. The
// mirror stores AO3's "31 Oct 2025" form, so a string sort is dominated by the
// day-of-month: measured on the live corpus, the naive order names 31 Oct 2025
// the most recent work while the true one is 26 Jun 2026. A feed that claims
// "recent" in the wrong order is worse than no feed, because a subscriber
// unsubscribes over it and never learns why.
func (s *Server) handleFeedTag(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/feed/tag/"), ".xml")
	id, err := strconv.ParseInt(strings.TrimSuffix(raw, "/"), 10, 64)
	if err != nil {
		writeTextErr(w, http.StatusNotFound, "that is not a tag id")
		return
	}
	ctx := r.Context()
	var name string
	if err := s.Engine.Corpus.DB.QueryRowContext(ctx,
		`SELECT name FROM tags WHERE id = ?`, id).Scan(&name); err != nil {
		// An unknown tag is a 404 with a body, not a feed with no items: an
		// empty feed reads as "nothing new", which is a claim about the corpus
		// rather than about the URL.
		writeTextErr(w, http.StatusNotFound, fmt.Sprintf("no tag with id %d", id))
		return
	}
	n := feedRequestedItems(r)

	// Blocked tags are the site's standing defaults, not a reader's: a feed is
	// anonymous by construction, and there is no session to read a block list
	// from. The site-wide list still applies, because a deployment that blocks
	// a tag means it everywhere.
	args := []any{id}
	q := `SELECT w.id, w.title, w.authors, w.word_count, w.kudos, w.rating,
	             w.update_date, w.url
	      FROM works w JOIN work_tags wt ON wt.work_id = w.id
	      WHERE wt.tag_id = ?`
	q += feedBlockSQL(s.DefaultBlockedTagIDs, &args)
	q += ` ORDER BY ` + corpus.ChronologicalUpdateDateDesc + `, w.id DESC LIMIT ?`
	args = append(args, n)

	rows, err := s.Engine.Corpus.DB.QueryContext(ctx, q, args...)
	if err != nil {
		writeTextErr(w, http.StatusInternalServerError, "the corpus could not be read")
		s.Log.Error("feed query failed", "tag", id, "err", err)
		return
	}
	defer rows.Close()
	base := absoluteBase(r)
	items := make([]rssItem, 0, n)
	last := time.Time{}
	for rows.Next() {
		var (
			workID        int64
			title, author string
			words, kudos  int64
			rating, upd   string
			url           string
		)
		if err := rows.Scan(&workID, &title, &author, &words, &kudos, &rating, &upd, &url); err != nil {
			writeTextErr(w, http.StatusInternalServerError, "the corpus could not be read")
			s.Log.Error("feed scan failed", "tag", id, "err", err)
			return
		}
		if url == "" {
			url = fmt.Sprintf("%s/work/%d", base, workID)
		}
		pub := ""
		if t, ok := corpus.ParseDate(upd); ok {
			pub = t.UTC().Format(time.RFC1123Z)
			if t.After(last) {
				last = t
			}
		}
		items = append(items, rssItem{
			Title:       title,
			Link:        url,
			GUID:        rssGUID{Value: url, IsPermaLink: true},
			PubDate:     pub,
			Description: feedDescription(author, words, kudos, rating),
		})
	}
	if err := rows.Err(); err != nil {
		s.Log.Error("feed rows", "tag", id, "err", err)
	}

	lastBuild := time.Now().UTC()
	if !last.IsZero() {
		// The channel's lastBuildDate is about the DATA, not about this
		// process: a client uses it to decide whether to re-read.
		lastBuild = last
	}
	doc := rssDoc{Version: "2.0", Channel: rssChannel{
		Title:         "kindred — " + name,
		Link:          base + "/tag/" + strconv.FormatInt(id, 10),
		Description:   fmt.Sprintf("Works carrying the tag %q, most recently updated first.", name),
		Language:      "en",
		LastBuildDate: lastBuild.Format(time.RFC1123Z),
		Items:         items,
	}}
	writeRSS(w, doc)
}

// handleFeedSeed serves the recommendations for one seed work.
//
// The list is computed per request, which is why the channel description says
// so: nothing here has an arrival time, and the entries are dated by the works
// themselves.
func (s *Server) handleFeedSeed(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/feed/seed/"), ".xml")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeTextErr(w, http.StatusNotFound,
			"a seed feed is /feed/seed/{kind}/{id}, for example /feed/seed/ao3_work/17249318")
		return
	}
	kind, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		writeTextErr(w, http.StatusNotFound, "that is not a work id")
		return
	}
	n := feedRequestedItems(r)
	if n > 50 {
		// A seed feed runs a full ranking; 200 of those per poll is a way to
		// turn a subscriber into a load test.
		n = 50
	}
	res, err := s.Engine.Recommend(r.Context(), engine.Request{
		Kind:  parts[0],
		Seeds: []engine.Seed{{Kind: parts[0], ID: kind}},
		N:     n,
	})
	if errors.Is(err, corpus.ErrNotFound) || errors.Is(err, store.ErrNotFound) {
		writeTextErr(w, http.StatusNotFound, fmt.Sprintf("no %s with id %d", parts[0], kind))
		return
	}
	if err != nil {
		writeTextErr(w, http.StatusInternalServerError, "this seed could not be ranked")
		s.Log.Error("seed feed failed", "seed", parts[1], "err", err)
		return
	}
	ctx := r.Context()
	base := absoluteBase(r)
	// The ranked item carries a score and a summary, not the fields a feed
	// entry shows. They are one query away in the corpus, and reading them there
	// keeps the ranking's ordering while giving the entry the work's own facts —
	// rather than inventing a description out of a score nobody asked about.
	meta := workMetaByID(ctx, s, res.Items)
	items := make([]rssItem, 0, len(res.Items))
	for _, it := range res.Items {
		link := it.URL
		if link == "" {
			link = fmt.Sprintf("%s/work/%d", base, it.ID)
		}
		m := meta[it.ID]
		pub := ""
		if t, ok := corpus.ParseDate(m.updateDate); ok {
			pub = t.UTC().Format(time.RFC1123Z)
		}
		items = append(items, rssItem{
			Title:       it.Title,
			Link:        link,
			GUID:        rssGUID{Value: link, IsPermaLink: true},
			PubDate:     pub,
			Description: feedDescription(m.authors, m.words, m.kudos, m.rating),
		})
	}
	title := parts[0] + " " + parts[1]
	if len(res.Items) > 0 && res.Items[0].Title != "" {
		title = res.Items[0].Title
	}
	// The seed's own title, when the corpus has it: "like <the seed>" reads as
	// a promise the feed keeps; "like ao3_work 17249318" does not.
	if seed, err := seedTitle(ctx, s, parts[0], kind); err == nil && seed != "" {
		title = seed
	}
	doc := rssDoc{Version: "2.0", Channel: rssChannel{
		Title: "kindred — like " + title,
		Link:  base + "/work/" + strconv.FormatInt(kind, 10),
		Description: "Works ranked from this seed. The list is computed when it is " +
			"requested, so its entries carry the works' own dates; a different " +
			"request can rank a different set.",
		Language:      "en",
		LastBuildDate: time.Now().UTC().Format(time.RFC1123Z),
		Items:         items,
	}}
	writeRSS(w, doc)
}

// handleOpenSearch serves the descriptor a browser reads to add this site to
// its search-engine list (#62).
//
// One entry, HTML output: the reader lands on /search, which is the page that
// renders results. Advertising the JSON API here would put a URL in the
// browser's address bar that renders raw JSON.
func (s *Server) handleOpenSearch(w http.ResponseWriter, r *http.Request) {
	base := absoluteBase(r)
	w.Header().Set("Content-Type", "application/opensearchdescription+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
  <ShortName>kindred</ShortName>
  <Description>Search this mirror of AO3 by tag, title or byline.</Description>
  <InputEncoding>UTF-8</InputEncoding>
  <Url type="text/html" method="get" template="%s/search?q={searchTerms}"/>
  <Url type="application/rss+xml" method="get" template="%s/search?q={searchTerms}&amp;format=rss"/>
</OpenSearchDescription>
`, base, base)
}

// feedDescription is the entry body: the facts a reader decides on, in the
// order they decide on them.
func feedDescription(author string, words, kudos int64, rating string) string {
	parts := make([]string, 0, 4)
	if author != "" {
		parts = append(parts, "by "+author)
	}
	if words > 0 {
		parts = append(parts, fmt.Sprintf("%s words", commas(words)))
	}
	if kudos > 0 {
		parts = append(parts, fmt.Sprintf("%s kudos", commas(kudos)))
	}
	if rating != "" {
		parts = append(parts, rating)
	}
	return strings.Join(parts, " · ")
}

// commas formats an integer with thousands separators.
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// writeRSS emits the document with the content type a reader requires.
//
// The type is the reason this exists rather than a generic JSON writer: a feed
// served as text/html is offered to the reader as a download or rendered as
// text, which looks like a broken feed rather than the wrong header.
func writeRSS(w http.ResponseWriter, doc rssDoc) {
	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		writeTextErr(w, http.StatusInternalServerError, "the feed could not be encoded")
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
	_, _ = w.Write([]byte("\n"))
}

// writeTextErr answers a feed request in text/plain: a client asking for XML
// that gets JSON has to parse an error in a format it never asked for.
func writeTextErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n"))
}

// feedsByDate is used by tests and by any future "all recent items" channel.
// It exists so the ordering rule has one home in Go as well as in SQL.
func feedsByDate(items []rssItem) {
	sort.SliceStable(items, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC1123Z, items[i].PubDate)
		tj, _ := time.Parse(time.RFC1123Z, items[j].PubDate)
		return ti.After(tj)
	})
}

// workMeta is the handful of corpus columns a feed entry renders.
type workMeta struct {
	authors, rating, updateDate, url string
	words, kudos                     int64
}

// workMetaByID reads those columns for a set of ranked ids.
//
// Chunked at 400 bind markers, which is the limit the rest of this tree already
// respects: SQLite answers a 36k-id IN list with a failure that reads like "no
// tags" rather than "too many parameters".
func workMetaByID(ctx context.Context, s *Server, items []rank.Candidate) map[int64]workMeta {
	out := make(map[int64]workMeta, len(items))
	if len(items) == 0 {
		return out
	}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	for start := 0; start < len(ids); start += 400 {
		end := start + 400
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.Engine.Corpus.DB.QueryContext(ctx,
			`SELECT id, authors, word_count, kudos, rating, update_date, url
			 FROM works WHERE id IN (`+corpus.Placeholders(len(chunk))+`)`, args...)
		if err != nil {
			s.Log.Warn("feed: work metadata unavailable", "err", err)
			return out
		}
		for rows.Next() {
			var (
				id                        int64
				authors, rating, upd, url string
				words, kudos              sql.NullInt64
			)
			if err := rows.Scan(&id, &authors, &words, &kudos, &rating, &upd, &url); err != nil {
				rows.Close()
				s.Log.Warn("feed: work metadata scan", "err", err)
				return out
			}
			out[id] = workMeta{
				authors: authors, rating: rating, updateDate: upd, url: url,
				words: words.Int64, kudos: kudos.Int64,
			}
		}
		rows.Close()
	}
	return out
}

// seedTitle reads one work's title, so a seed feed can name its seed.
func seedTitle(ctx context.Context, s *Server, kind string, id int64) (string, error) {
	if kind != "ao3_work" {
		return "", nil
	}
	var title string
	err := s.Engine.Corpus.DB.QueryRowContext(ctx,
		`SELECT title FROM works WHERE id = ?`, id).Scan(&title)
	return title, err
}

// feedBlockSQL excludes the site-wide blocked tags, in the same shape the
// ranking uses: a feed is anonymous, so it carries no reader's block list, but
// a tag the DEPLOYMENT blocks is blocked everywhere.
func feedBlockSQL(blocked map[int64]bool, args *[]any) string {
	if len(blocked) == 0 {
		return ""
	}
	ids := make([]int64, 0, len(blocked))
	for id := range blocked {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		*args = append(*args, id)
	}
	return " AND wt.tag_id NOT IN (" + corpus.Placeholders(len(ids)) + ")"
}
