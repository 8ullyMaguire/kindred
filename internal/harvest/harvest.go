package harvest

// Choosing WHAT to crawl, and crawling it.
//
// The mirror already holds ~113k works, and its tag_type column is 'freeforms' for essentially
// every row because the tool that built it flattened AO3's classification. Two consequences drive
// this package:
//
//   - Fandoms cannot be read out of the mirror. They come from AO3's own catalogue
//     (/media -> /media/<media>/fandoms), which prints every fandom with its work count.
//   - The catalogue is what makes a DISTRIBUTED harvest possible. Selecting works from the
//     mirror's own tag counts selects the archive's biggest fandoms over and over; selecting from
//     the catalogue lets the budget be split by fandom before any work is fetched.
//
// The work of choosing is done on LISTING pages: a listing carries twenty works with title,
// authors, fandoms, typed tags, rating, completion and every count, for one request. Fetching
// twenty work pages to compare them costs twenty requests and the comparison is what needs
// breadth. Only the works that SURVIVE selection get a work page, and only for the fields a blurb
// does not carry (summary, language, chapter count).

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"math"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
)

// Fandom is one entry of AO3's catalogue.
type Fandom struct {
	// Name is the tag as AO3 displays it ("Haikyuu!!").
	Name string `json:"name"`
	// Path is the tag's URL segment, still percent-encoded ("Haikyuu%21%21").
	Path  string `json:"path"`
	Media string `json:"media"`
	Works int    `json:"works"`
}

// Catalogue is every fandom, by media.
type Catalogue struct {
	Media []string `json:"media"`
	// MediaPaths are the URL segments as AO3 published them ("Anime%20*a*%20Manga"). They are
	// kept verbatim rather than rebuilt from the display name: re-escaping "*" to "%2A" is
	// equivalent in theory and was refused in practice, and a catalogue is fetched once.
	MediaPaths map[string]string `json:"media_paths"`
	Fandoms    []Fandom          `json:"fandoms"`
	// FetchedAt is when the catalogue was read, so a stale plan is visible as stale.
	FetchedAt string `json:"fetched_at"`
}

// Fetcher is the one thing this package needs from the network layer.
//
// It is an interface so a test can hand in fixtures and so the harvest cannot accidentally grow
// its own retry policy: crawl.Crawler already owns the delay, the retries and the interstitial
// check, and a second HTTP stack here is a second place for those to be wrong.
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) ([]byte, error)
}

var (
	mediaLinkRe  = regexp.MustCompile(`href="/media/([^"/]+)/fandoms"`)
	fandomLinkRe = regexp.MustCompile(`(?s)<a class="tag" href="/tags/([^"]+)/works">([^<]+)</a>\s*\((\d[\d,]*)\)`)
)

// CrawlCatalogue reads AO3's media index and every media's fandom list.
//
// Eleven requests in total (measured 2026-10-25: the Anime & Manga page alone is 1.3 MB and
// lists 5,019 fandoms with counts). The media pages are NOT paginated, so there is no
// page-count guess here that could silently truncate the catalogue.
func CrawlCatalogue(ctx context.Context, f Fetcher, base string) (*Catalogue, error) {
	if f == nil {
		return nil, fmt.Errorf("harvest: no fetcher")
	}
	body, err := f.Fetch(ctx, base+"/media")
	if err != nil {
		return nil, fmt.Errorf("harvest: media index: %w", err)
	}
	seen := map[string]bool{}
	var media []string
	paths := map[string]string{}
	for _, m := range mediaLinkRe.FindAllSubmatch(body, -1) {
		raw := string(m[1])
		name, uerr := url.PathUnescape(raw)
		if uerr != nil {
			name = raw
		}
		if !seen[name] {
			seen[name] = true
			media = append(media, name)
			paths[name] = raw
		}
	}
	if len(media) == 0 {
		return nil, fmt.Errorf("harvest: the media index named no media (not the page expected?)")
	}
	sort.Strings(media)

	cat := &Catalogue{MediaPaths: paths}
	for _, m := range media {
		seg := paths[m]
		if seg == "" {
			seg = url.PathEscape(m)
		}
		mediaBody, err := f.Fetch(ctx, base+"/media/"+seg+"/fandoms")
		if err != nil {
			return nil, fmt.Errorf("harvest: media %q: %w", m, err)
		}
		fandoms, err := ParseFandomList(mediaBody, m)
		if err != nil {
			return nil, fmt.Errorf("harvest: media %q: %w", m, err)
		}
		cat.Media = append(cat.Media, m)
		cat.Fandoms = append(cat.Fandoms, fandoms...)
	}
	return cat, nil
}

// ParseFandomList reads one media page's fandom rows.
func ParseFandomList(body []byte, media string) ([]Fandom, error) {
	ms := fandomLinkRe.FindAllSubmatch(body, -1)
	if len(ms) == 0 {
		// A media with no fandoms would be odd but possible; a page that is not a fandom list is
		// not. Distinguish by the container the site always prints.
		if !strings.Contains(string(body), "index group") {
			return nil, fmt.Errorf("not a fandom list page")
		}
		return nil, nil
	}
	out := make([]Fandom, 0, len(ms))
	for _, m := range ms {
		name := html.UnescapeString(strings.TrimSpace(string(m[2])))
		if name == "" {
			continue
		}
		out = append(out, Fandom{
			Name:  name,
			Path:  canonTagPath(string(m[1])),
			Media: media,
			Works: atoiComma(string(m[3])),
		})
	}
	return out, nil
}

// canonTagPath turns a catalogue href segment into the tag's own text.
//
// The href is not a URL-encoded tag: AO3 writes HTML entities into it, so a fandom with an
// apostrophe appears as `James%20Cameron&#39;s%20Avatar%20-%20All%20Media%20Types`. Using that
// segment verbatim requests a tag literally named "Cameron&#39;s" and answers 404 -- which a
// harvest reports as one failed fandom and moves on, so the loss is a silently missing fandom
// rather than an error. Unescape the entity, then the percent-encoding, and keep the plain text;
// listingURL re-encodes it.
func canonTagPath(seg string) string {
	s := html.UnescapeString(seg)
	if dec, err := url.PathUnescape(s); err == nil {
		s = dec
	}
	return s
}

// Plan is the allocation: how many works each fandom may contribute.
type Plan struct {
	Target int            `json:"target"`
	Floor  int            `json:"floor"`
	Cap    int            `json:"cap"`
	Quota  map[string]int `json:"quota"` // fandom name -> works
	Total  int            `json:"total"`
	// Skipped counts catalogue entries too small to supply the floor. They are reported rather
	// than dropped silently: "1,200 fandoms" means nothing without "of 61,000".
	Skipped  int `json:"skipped"`
	Coverage struct {
		FandomsInCatalogue int `json:"fandoms_in_catalogue"`
		FandomsPlanned     int `json:"fandoms_planned"`
		WorksInCatalogue   int `json:"works_in_catalogue"`
	} `json:"coverage"`
}

// Allocate splits `target` works across fandoms, biggest first, with a floor and a cap.
//
// The rule is stated in one place because a quota that drifts between the plan and the run is
// how a "distributed" harvest ends up 40% Naruto.
//
// Three constraints shape it, and each has a reason:
//
//   - BREADTH: how many fandoms to cover. With 22,600 works and a floor of 10 there is room for
//     about 1,100 fandoms at TWICE the floor on average; taking the arithmetic maximum (2,260
//     fandoms at exactly 10 each) would leave nothing to weight with, so every fandom would look
//     identical and the biggest fandom in the archive would get the same ten works as a fandom a
//     twentieth its size.
//   - FLOOR: no covered fandom gets fewer than `floor` works, because a fandom with three works
//     in the mirror is a row of dead links rather than coverage.
//   - CAP: no fandom takes more than `cap`, so one giant cannot flatten the distribution no
//     matter how large it is.
//
// Within those, the remainder is split by sqrt(size) rather than size: sqrt keeps the giants
// ahead without letting a 400k-work fandom take a third of the budget, which is exactly the
// failure this whole package exists to avoid.
func Allocate(fs []Fandom, target, floor, cap, breadth int) Plan {
	p := Plan{Target: target, Floor: floor, Cap: cap, Quota: map[string]int{}}
	// AO3 lists a fandom under every media it belongs to: "James Cameron's Avatar - All Media
	// Types" appears five times in the catalogue. Keyed by name, the quota map would collapse
	// those to one entry while Total added five, and the plan would claim 22,600 works while
	// allocating fewer -- so the duplicates are merged here, once, before any arithmetic.
	merged := make(map[string]Fandom, len(fs))
	for _, f := range fs {
		if prev, ok := merged[f.Name]; ok {
			if f.Works > prev.Works {
				prev.Works = f.Works
				merged[f.Name] = prev
			}
			continue
		}
		merged[f.Name] = f
	}
	p.Coverage.FandomsInCatalogue = len(merged)
	eligible := make([]Fandom, 0, len(merged))
	for _, f := range merged {
		p.Coverage.WorksInCatalogue += f.Works
		if f.Works >= floor {
			eligible = append(eligible, f)
		} else {
			p.Skipped++
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Works != eligible[j].Works {
			return eligible[i].Works > eligible[j].Works
		}
		return eligible[i].Name < eligible[j].Name
	})

	want := breadth
	if want <= 0 {
		// Half the arithmetic maximum: the other half is what the sqrt split has to work with.
		want = target / (2 * floor)
	}
	if want > len(eligible) {
		want = len(eligible)
	}
	chosen := eligible[:want]
	if len(chosen) == 0 {
		return p
	}
	p.Coverage.FandomsPlanned = len(chosen)

	var sumSqrt float64
	for _, f := range chosen {
		sumSqrt += math.Sqrt(float64(f.Works))
	}
	rest := target - len(chosen)*floor
	if rest < 0 {
		rest = 0
	}
	for _, f := range chosen {
		q := floor
		if sumSqrt > 0 {
			q += int(math.Round(float64(rest) * math.Sqrt(float64(f.Works)) / sumSqrt))
		}
		if cap > 0 && q > cap {
			q = cap
		}
		if q > f.Works {
			q = f.Works
		}
		p.Quota[f.Name] = q
		p.Total += q
	}
	return p
}

// Taste is the affinity model: tag name -> weight.
//
// It is deliberately a plain map rather than a profile object. The selection needs ONE question
// answered ("how much does this work look like something the reader wants"), and a map of the
// tags that answer it can be written by hand, loaded from a profile, or rebuilt from bookmarks
// without this package knowing which.
type Taste map[string]float64

// LoadTasteFile reads a taste map from JSON.
func LoadTasteFile(path string) (Taste, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Taste
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("taste file %s: %w", path, err)
	}
	// Normalise the keys ONCE: AO3 tag names differ by case and the blurb text is whatever the
	// author typed, so a lookup that assumed exact case would score every tag as unknown and
	// silently rank by kudos alone.
	out := make(Taste, len(t))
	for k, v := range t {
		out[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return out, nil
}

// Score ranks a candidate: quality first, then how much of it the reader's taste recognises.
//
// Both halves are bounded so neither swallows the other. A log of the counts is the standard
// move -- kudos run to five figures and a raw sum would let one blockbuster outrank a work that
// matches four favourite tags -- and the affinity is an average over the work's own tags, so a
// work tagged with everything cannot win by volume.
func Score(b crawl.Blurb, t Taste) float64 {
	quality := math.Log10(1+float64(b.Kudos))*1.0 +
		math.Log10(1+float64(deref(b.Bookmarks)))*1.5 +
		math.Log10(1+float64(deref(b.Hits)))*0.25

	if len(t) == 0 {
		return quality
	}
	var sum, hits float64
	for _, tag := range b.Tags {
		hits++
		if w, ok := t[strings.ToLower(strings.TrimSpace(tag.Name))]; ok {
			sum += w
		}
	}
	// Capped at 3 so a strong taste match beats a popular work but does not make the harvest a
	// single-tag search: the whole point is a corpus the recommender can learn across.
	affinity := 0.0
	if hits > 0 {
		affinity = math.Min(3, 3*sum/hits*float64(len(t))/4)
	}
	return quality + affinity
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func atoiComma(s string) int {
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
		}
	}
	return n
}
