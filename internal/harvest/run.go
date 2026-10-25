package harvest

// Running the plan: listing pages to choose, work pages to keep.
//
// The run is resumable by FANDOM, not by URL. A crawl that resumes mid-fandom has to re-read the
// listing pages to know which works it had picked, and re-reading 10 pages to recover one work is
// a worse trade than re-doing the fandom's quota. The state file therefore records a fandom as
// done only after its quota is saved.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/crawl"
)

// RunOptions is one harvest run.
type RunOptions struct {
	Base string
	Plan Plan
	// ByName resolves a fandom name to its catalogue entry (for the tag path).
	ByName map[string]Fandom
	Taste  Taste
	Sink   crawl.Sink

	// MaxPages caps the listing pages read per fandom, because a quota of 120 with a candidate
	// multiplier of 3 would be 18 pages and the last pages of a kudos-sorted listing are not
	// where anything interesting is.
	MaxPages int
	// WorkPages fetches each selected work's page for summary, language and chapter count. Off
	// means the blurb's own fields are stored, which is a legitimate corpus (and 20x cheaper) but
	// carries no summary.
	WorkPages bool
	// Reprobe asks the mirror which works it already has. Returning true SKIPS the work: the
	// mirror's copy exists, and re-fetching it would spend a request to learn nothing.
	Probe func(ctx context.Context, id int64) (bool, error)

	State     *RunState
	StatePath string
	// Progress, when set, is called after every saved work.
	Progress func(f Fandom, done, quota int, id int64)
	// Logf receives per-fandom and per-failure notices.
	Logf func(format string, args ...any)
}

// RunState is the resume file.
type RunState struct {
	// Done[Fandom] = works saved for that fandom.
	Done map[string]int `json:"done"`
	// Picked[Fandom] = works already selected (so a resumed run does not fetch them twice).
	Picked map[string][]int64 `json:"picked"`
	// Saved counts every work written across runs.
	Saved  int `json:"saved"`
	Errors int `json:"errors"`
	// Finished[Fandom] marks a fandom whose quota is complete.
	Finished map[string]bool `json:"finished"`
}

// NewRunState returns an empty state.
func NewRunState() *RunState {
	return &RunState{Done: map[string]int{}, Picked: map[string][]int64{}, Finished: map[string]bool{}}
}

// LoadRunState reads a state file, or returns a fresh one when the file does not exist.
func LoadRunState(path string) (*RunState, error) {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return NewRunState(), nil
	}
	if err != nil {
		return nil, err
	}
	st := NewRunState()
	if err := json.Unmarshal(body, st); err != nil {
		return nil, fmt.Errorf("harvest state %s: %w", path, err)
	}
	if st.Done == nil {
		st.Done = map[string]int{}
	}
	if st.Picked == nil {
		st.Picked = map[string][]int64{}
	}
	if st.Finished == nil {
		st.Finished = map[string]bool{}
	}
	return st, nil
}

// Save writes the state file atomically.
func (s *RunState) Save(path string) error {
	if path == "" {
		return nil
	}
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RunReport is what a run did.
type RunReport struct {
	Fandoms     int `json:"fandoms"`
	Works       int `json:"works"`
	Saved       int `json:"saved"`
	AlreadyHeld int `json:"already_held"`
	// Empty is fandoms whose listing produced no candidate at all (a tag that lists nothing, or a
	// page that failed). Reported because an empty fandom is a hole in a "distributed" harvest.
	Empty  []string `json:"empty"`
	Failed int      `json:"failed"`
	// ListingPages and WorkPages are the request counts, which is what a rate limit answers to.
	ListingPages int `json:"listing_pages"`
	WorkPages    int `json:"work_pages"`
}

// Run executes the plan.
func Run(ctx context.Context, f Fetcher, o RunOptions) (*RunReport, error) {
	if f == nil {
		return nil, fmt.Errorf("harvest: no fetcher")
	}
	if o.Sink == nil {
		return nil, fmt.Errorf("harvest: no sink")
	}
	if o.State == nil {
		o.State = NewRunState()
	}
	if o.MaxPages <= 0 {
		o.MaxPages = 10
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	names := make([]string, 0, len(o.Plan.Quota))
	for name := range o.Plan.Quota {
		names = append(names, name)
	}
	// Biggest quota first: a failure early costs the least, and the report shows the fandom a
	// human would check first at the top.
	sort.Slice(names, func(i, j int) bool {
		if o.Plan.Quota[names[i]] != o.Plan.Quota[names[j]] {
			return o.Plan.Quota[names[i]] > o.Plan.Quota[names[j]]
		}
		return names[i] < names[j]
	})

	rep := &RunReport{}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if o.State.Finished[name] {
			continue
		}
		fan, ok := o.ByName[name]
		if !ok {
			continue
		}
		rep.Fandoms++
		quota := o.Plan.Quota[name]
		need := quota - o.State.Done[name]
		if need <= 0 {
			o.State.Finished[name] = true
			continue
		}

		// Pages are read INCREMENTALLY and the pool is re-ranked after each one, because the
		// fandom's most-kudos'd works are usually already in the mirror: a run that read its
		// pages up front and then discovered the top of the list was held would have spent the
		// pages and still be short. Here a page is only read when the quota is still unmet.
		picked := o.State.Picked[name]
		taken := map[int64]bool{}
		for _, id := range picked {
			taken[id] = true
		}
		saved := o.State.Done[name]
		var pool []crawl.Blurb
		for page := 1; page <= o.MaxPages && saved < quota; page++ {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			body, ferr := f.Fetch(ctx, listingURL(o.Base, fan.Path, page))
			rep.ListingPages++
			if ferr != nil {
				if page == 1 {
					logf("listing failed for %q: %v", name, ferr)
					rep.Failed++
				}
				break
			}
			blurbs, perr := crawl.ParseListing(body)
			if perr != nil {
				if page == 1 {
					logf("listing unreadable for %q: %v", name, perr)
					rep.Failed++
				}
				break
			}
			if len(blurbs) == 0 {
				break
			}
			pool = append(pool, blurbs...)
			sort.SliceStable(pool, func(i, j int) bool {
				si, sj := Score(pool[i], o.Taste), Score(pool[j], o.Taste)
				if si != sj {
					return si > sj
				}
				return pool[i].ID < pool[j].ID
			})
			for _, b := range pool {
				if saved >= quota {
					break
				}
				if taken[b.ID] {
					continue
				}
				if o.Probe != nil {
					held, herr := o.Probe(ctx, b.ID)
					if herr != nil {
						logf("probe %d: %v", b.ID, herr)
						continue
					}
					if held {
						rep.AlreadyHeld++
						taken[b.ID] = true
						continue
					}
				}
				w, werr := workFor(ctx, f, o, b)
				if werr != nil {
					logf("work %d: %v", b.ID, werr)
					rep.Failed++
					taken[b.ID] = true
					continue
				}
				if serr := o.Sink.Save(ctx, w); serr != nil {
					logf("save %d: %v", b.ID, serr)
					rep.Failed++
					taken[b.ID] = true
					continue
				}
				if o.WorkPages {
					rep.WorkPages++
				}
				saved++
				rep.Saved++
				taken[b.ID] = true
				o.State.Picked[name] = append(o.State.Picked[name], b.ID)
				o.State.Done[name] = saved
				o.State.Saved++
				if o.Progress != nil {
					o.Progress(fan, saved, quota, b.ID)
				}
				// Checkpoint every work: a harvest is measured in hours and a state file that
				// only lands at a fandom boundary is a state file that loses an hour of manners.
				if err := o.State.Save(o.StatePath); err != nil {
					logf("state: %v", err)
				}
			}
		}
		if len(pool) == 0 {
			// No candidates at all is a hole in a distributed harvest; nothing saved because
			// everything on the pages was already held is not, and saying so would make the
			// report look like a broken fandom when it is a complete one.
			rep.Empty = append(rep.Empty, name)
		}
		if saved >= quota {
			o.State.Finished[name] = true
			if err := o.State.Save(o.StatePath); err != nil {
				logf("state: %v", err)
			}
		} else {
			logf("%q reached %d/%d from %d page(s)", name, saved, quota, o.MaxPages)
		}
		rep.Works += saved
	}
	return rep, nil
}

// listingURL is AO3's works listing for a tag, sorted by kudos.
//
// The sort parameters are load-bearing and were measured: `sort_column=kudos` answers a syntax
// error page, the site's own value is `kudos_count`, and sending sort_column WITHOUT
// sort_direction gets a 525 from Cloudflare. With both, twenty blurbs.
func listingURL(base, path string, page int) string {
	v := url.Values{}
	v.Set("work_search[sort_column]", "kudos_count")
	v.Set("work_search[sort_direction]", "desc")
	v.Set("page", fmt.Sprint(page))
	return base + "/tags/" + path + "/works?" + v.Encode()
}

// workFor returns the work to store: the page's own parse when WorkPages is set, otherwise the
// blurb's fields, which is a real (if summary-less) row.
func workFor(ctx context.Context, f Fetcher, o RunOptions, b crawl.Blurb) (*crawl.ParsedWork, error) {
	if !o.WorkPages {
		return blurbWork(b, o.Base), nil
	}
	body, err := f.Fetch(ctx, fmt.Sprintf("%s/works/%d", o.Base, b.ID))
	if err != nil {
		return nil, err
	}
	w, err := crawl.ParseWork(body, fmt.Sprintf("%s/works/%d", o.Base, b.ID))
	if err != nil {
		return nil, err
	}
	// The page and the blurb disagree occasionally (a work edited between the two requests).
	// The PAGE wins for every field it carries; the blurb fills the ones it does not.
	if w.Title == "" {
		w.Title = b.Title
	}
	if w.Kudos == 0 && b.Kudos > 0 {
		w.Kudos = b.Kudos
	}
	if w.Bookmarks == nil {
		w.Bookmarks = b.Bookmarks
	}
	return w, nil
}

// blurbWork turns a blurb into a work row, for the summary-less fast path.
func blurbWork(b crawl.Blurb, base string) *crawl.ParsedWork {
	w := &crawl.ParsedWork{
		ID:         b.ID,
		Title:      b.Title,
		Authors:    joinNames(b.Authors),
		URL:        fmt.Sprintf("%s/works/%d", base, b.ID),
		WordCount:  b.Words,
		Kudos:      b.Kudos,
		Bookmarks:  b.Bookmarks,
		Hits:       deref(b.Hits),
		Complete:   b.Complete,
		Rating:     b.Rating,
		UpdateDate: isoDate(b.Updated),
		Tags:       b.Tags,
	}
	return w
}

// isoDate turns a blurb's "28 Oct 2015" into "2015-10-28".
//
// The work page's parser yields ISO and the blurb yields the display form; storing both in one
// column would make every date comparison in the recommender wrong for half the corpus.
func isoDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range []string{"2 Jan 2006", "02 Jan 2006", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return s
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// CatalogueAge is how stale a catalogue is, for the plan's own honesty line.
func CatalogueAge(fetchedAt string) string {
	t, err := time.Parse(time.RFC3339, fetchedAt)
	if err != nil {
		return "unknown"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Minute).String()
}
