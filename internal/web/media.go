package web

// /media/{media}/fandoms -- AO3's fandom index, per media.
//
// AO3 publishes the archive's whole fandom catalogue under /media: eleven media, each with the
// fandoms that live in it and how many works each holds. This server has that catalogue on disk
// because the harvest reads it (77,514 rows, 75,574 distinct fandoms), and this page is the reader's
// side of the same data -- until now the only way to see a fandom's size was to ask the corpus,
// which knows a work's tags but not which MEDIA its fandom belongs to.
//
// Two deliberate choices:
//
//   - The page is a BROWSE, not a ranking. /fandoms ranks fandoms by how much of a reader's taste
//     they match; this one is the archive's own index, which is a different question and must not
//     borrow the ranking's vocabulary or it will read as a recommendation.
//   - A fandom links to /search?q=<name>, not to its tag page: tag pages are keyed by numeric id,
//     and resolving five thousand names through the corpus to build one list would make this page
//     as slow as the ranking while telling the reader nothing more. Search by name finds the works
//     either way.

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/harvest"
)

// MediaFandom is one row of the index.
type MediaFandom struct {
	Name    string
	Works   int
	Search  string // the /search URL for this fandom's name
	Percent float64
}

// MediaPage is the page's data.
type MediaPage struct {
	Base
	// Media is the display name of the current media ("Anime & Manga"), or "" on the index
	// itself (which lists every media with its fandom count).
	Media string
	// Rows are the fandoms of the current media, biggest first.
	Rows []MediaFandom
	// MediaRows lists every media, for the sidebar and for the index.
	MediaRows []MediaLink
	// TotalFandoms and TotalWorks are the corpus catalogue's own totals.
	TotalFandoms int
	TotalWorks   int
	// Truncated says the list was cut at the page's limit rather than being short.
	Truncated bool
	// Limit is the cap that produced Truncated, so the page can say the number.
	Limit int
}

// MediaLink is one media in the index.
type MediaLink struct {
	Name    string
	Path    string
	Fandoms int
	Works   int
	Current bool
}

// mediaPageLimit caps the rows rendered. A media holds up to 14,297 fandoms (Books); printing all
// of them is a 400 KB page nobody scrolls, and the template says the cap was reached rather than
// pretending the list ended.
const mediaPageLimit = 500

// renderMedia serves /media/ (the index) and /media/{media}/fandoms.
func (d Deps) renderMedia(w http.ResponseWriter, r *http.Request, arg string) {
	cat := d.Catalogue
	if cat == nil {
		d.fail(w, r, http.StatusServiceUnavailable,
			errors.New("no fandom catalogue is loaded on this server"))
		return
	}

	mediaName, wantFandoms := splitMediaArg(arg)
	page := MediaPage{
		MediaRows:    mediaLinks(cat, mediaName),
		TotalFandoms: len(cat.Fandoms),
	}
	for _, f := range cat.Fandoms {
		page.TotalWorks += f.Works
	}

	if wantFandoms && mediaName != "" {
		if canon, ok := resolveMedia(cat, mediaName); ok {
			mediaName = canon
			page.Media = canon
			page.MediaRows = mediaLinks(cat, canon)
		} else {
			// An unknown media is a 404: the URL names something the archive does not have, and
			// an empty index with a 200 would read as "this media has no fandoms".
			d.notFound(w, r)
			return
		}
	}

	// Rows for the current media, or every fandom when the index is asked with no media.
	var rows []MediaFandom
	biggest := 0
	for _, f := range cat.Fandoms {
		if page.Media != "" && !strings.EqualFold(f.Media, page.Media) {
			continue
		}
		rows = append(rows, MediaFandom{Name: f.Name, Works: f.Works,
			Search: "/search?q=" + url.QueryEscape(f.Name)})
		if f.Works > biggest {
			biggest = f.Works
		}
	}
	// A fandom listed under several media would otherwise appear twice in the index.
	rows = dedupeFandoms(rows)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Works != rows[j].Works {
			return rows[i].Works > rows[j].Works
		}
		return rows[i].Name < rows[j].Name
	})
	if len(rows) > mediaPageLimit {
		rows = rows[:mediaPageLimit]
		page.Truncated = true
		page.Limit = mediaPageLimit
	}
	if biggest > 0 {
		for i := range rows {
			rows[i].Percent = 100 * float64(rows[i].Works) / float64(biggest)
		}
	}
	page.Rows = rows

	title := "Media"
	if page.Media != "" {
		title = displayMedia(page.Media)
	}
	page.Base = d.baseOf(r, title, title)
	d.page(w, "media.html", page, http.StatusOK)
}

// displayMedia turns AO3's URL spelling into the name a reader sees.
//
// The catalogue's media names come from the href, where AO3 encodes punctuation with its own
// wildcards: *a* is "&", *s* is "/", *d* is ".". Rendering the segment verbatim puts
// "Anime *a* Manga" at the top of the page, which reads as a bug rather than as a name.
func displayMedia(name string) string {
	r := strings.NewReplacer("*a*", "&", "*s*", "/", "*d*", ".")
	return r.Replace(name)
}

// splitMediaArg reads the tail of the prefix pattern: "<media>/fandoms".
func splitMediaArg(arg string) (string, bool) {
	arg = strings.Trim(arg, "/")
	if arg == "" {
		return "", false
	}
	parts := strings.Split(arg, "/")
	if len(parts) < 2 || parts[len(parts)-1] != "fandoms" {
		return "", false
	}
	name := strings.Join(parts[:len(parts)-1], "/")
	decoded, err := url.PathUnescape(name)
	if err != nil {
		return name, true
	}
	return decoded, true
}

// resolveMedia matches a URL segment against the catalogue's media, by display name or by the
// segment AO3 published (which uses *a* for "&", *d* for "." and *s* for "/").
func resolveMedia(cat *harvest.Catalogue, want string) (string, bool) {
	if cat.MediaPaths != nil {
		for name, published := range cat.MediaPaths {
			if strings.EqualFold(published, want) || strings.EqualFold(name, want) {
				return name, true
			}
		}
	}
	// Then by display name, and then by a UNIQUE first word: AO3's own URL for Books carries the
	// wildcards ("Books%20*a*%20Literature"), so /media/Books/ is not a URL the archive publishes
	// -- but a reader who types it means that media, and there is exactly one media beginning with
	// "Books". Uniqueness is the guard: with "Other Media" and nothing else starting "Other", the
	// shortcut cannot pick the wrong one, and if it ever could, it refuses instead of guessing.
	decoded := displayMedia(want)
	for _, name := range cat.Media {
		if strings.EqualFold(displayMedia(name), decoded) {
			return name, true
		}
	}
	var matches []string
	for _, name := range cat.Media {
		if strings.HasPrefix(strings.ToLower(displayMedia(name)), strings.ToLower(decoded)+" ") {
			matches = append(matches, name)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return "", false
}

// mediaLinks builds the media index with its counts.
func mediaLinks(cat *harvest.Catalogue, current string) []MediaLink {
	type agg struct {
		fandoms int
		works   int
	}
	byMedia := map[string]agg{}
	for _, f := range cat.Fandoms {
		a := byMedia[f.Media]
		a.fandoms++
		a.works += f.Works
		byMedia[f.Media] = a
	}
	out := make([]MediaLink, 0, len(byMedia))
	for name, a := range byMedia {
		path := name
		if cat.MediaPaths != nil && cat.MediaPaths[name] != "" {
			path = cat.MediaPaths[name]
		}
		out = append(out, MediaLink{
			Name: displayMedia(name), Path: path, Fandoms: a.fandoms, Works: a.works,
			Current: strings.EqualFold(name, current),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fandoms > out[j].Fandoms })
	return out
}

func dedupeFandoms(in []MediaFandom) []MediaFandom {
	seen := make(map[string]int, len(in))
	out := in[:0]
	for _, f := range in {
		if i, ok := seen[f.Name]; ok {
			if f.Works > out[i].Works {
				out[i] = f
			}
			continue
		}
		seen[f.Name] = len(out)
		out = append(out, f)
	}
	return out
}

// mediaTitle is used by the template to name the page without repeating the format string.
func mediaTitle(m MediaPage) string {
	if m.Media == "" {
		return fmt.Sprintf("Media (%d fandoms)", m.TotalFandoms)
	}
	return fmt.Sprintf("%s (%d fandoms)", m.Media, len(m.Rows))
}
