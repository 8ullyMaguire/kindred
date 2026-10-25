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

	// AbortAfter stops the run after this many consecutive fandoms that produced nothing, which
	// is what an outage looks like from here. Without it a run walks the whole plan during an
	// outage, marks every fandom "0/78" and exits having done nothing -- the state file is intact,
	// but the operator has to notice, and a job left running overnight has done nothing at all.
	AbortAfter int

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
	consecEmpty := 0
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

		picked := o.State.Picked[name]
		taken := map[int64]bool{}
		for _, id := range picked {
			taken[id] = true
		}
		res, ferr := fillFandom(ctx, f, o, fan, quota, taken)
		rep.ListingPages += res.Pages
		rep.WorkPages += res.WorkPages
		rep.Saved += res.New
		rep.AlreadyHeld += res.Held
		saved := res.Saved
		if ferr != nil {
			logf("listing failed for %q: %v", name, ferr)
			rep.Failed++
			consecEmpty++
			if o.AbortAfter > 0 && consecEmpty >= o.AbortAfter {
				return rep, fmt.Errorf("harvest: %d fandoms in a row produced nothing (last: %v); "+
					"the archive looks unavailable -- state is saved, rerun to resume", consecEmpty, ferr)
			}
			continue
		}
		if res.Seen > 0 {
			consecEmpty = 0
		} else {
			consecEmpty++
			if o.AbortAfter > 0 && consecEmpty >= o.AbortAfter {
				o.State.Save(o.StatePath)
				return rep, fmt.Errorf("harvest: %d fandoms in a row listed nothing; "+
					"the archive looks unavailable -- state is saved, rerun to resume", consecEmpty)
			}
		}
		if saved >= quota {
			o.State.Finished[name] = true
			if err := o.State.Save(o.StatePath); err != nil {
				logf("state: %v", err)
			}
		} else {
			if res.Pages > 0 {
				logf("%q reached %d/%d after %d page(s), %d candidate(s) already held",
					name, saved, quota, res.Pages, res.Held)
			}
			if res.Seen == 0 {
				rep.Empty = append(rep.Empty, name)
			}
		}
		rep.Works += saved
	}
	return rep, nil
}

// fillFandom brings one fandom up to its quota and returns how many works it now holds.
//
// Pages are read INCREMENTALLY and the pool is re-ranked after each one, because a fandom's
// most-kudos'd works are usually already in the mirror: a run that read its pages up front and
// then discovered the top of the list was held would have spent the pages and still be short.
//
// If the kudos ordering cannot fill the quota -- which is exactly what happens in a big fandom
// the mirror already covers -- the fandom is retried under `revised_at`. That is not padding: the
// mirror was built from popular works, so the recently-updated end of a fandom is the end it does
// NOT have, and a quota left unfilled is a hole in the distribution the plan promised.
// fillResult is what one fandom's fill did, in the units the report counts.
type fillResult struct {
	// Saved is the fandom's total after the fill (cumulative across runs).
	Saved int
	// New is what THIS run added, and Held how many candidates were already in the mirror.
	New, Held int
	Pages     int
	WorkPages int
	// Seen is how many candidate blurbs the listing produced at all, which distinguishes a
	// fandom with nothing in it from one whose pages were all already held.
	Seen int
}

func fillFandom(ctx context.Context, f Fetcher, o RunOptions, fan Fandom, quota int, taken map[int64]bool) (fillResult, error) {
	var res fillResult
	saved := o.State.Done[fan.Name]
	saved = o.State.Done[fan.Name]
	var pool []crawl.Blurb
	var firstErr error
	for _, sortCol := range []string{"kudos_count", "revised_at"} {
		if saved >= quota {
			break
		}
		for page := 1; page <= o.MaxPages && saved < quota; page++ {
			if cerr := ctx.Err(); cerr != nil {
				res.Saved = saved
				return res, cerr
			}
			body, err := f.Fetch(ctx, listingURL(o.Base, fan.Path, page, sortCol))
			res.Pages++
			if err != nil {
				if page == 1 && sortCol == "kudos_count" {
					firstErr = err
				}
				break
			}
			blurbs, perr := crawl.ParseListing(body)
			if perr != nil {
				if page == 1 && sortCol == "kudos_count" {
					firstErr = perr
				}
				break
			}
			if len(blurbs) == 0 {
				break
			}
			pool = append(pool, blurbs...)
			res.Seen = len(pool)
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
						if o.Logf != nil {
							o.Logf("probe %d: %v", b.ID, herr)
						}
						continue
					}
					if held {
						res.Held++
						taken[b.ID] = true
						continue
					}
				}
				w, werr := workFor(ctx, f, o, b)
				if werr != nil {
					if o.Logf != nil {
						o.Logf("work %d: %v", b.ID, werr)
					}
					taken[b.ID] = true
					continue
				}
				if serr := o.Sink.Save(ctx, w); serr != nil {
					if o.Logf != nil {
						o.Logf("save %d: %v", b.ID, serr)
					}
					taken[b.ID] = true
					continue
				}
				if o.WorkPages {
					res.WorkPages++
				}
				saved++
				res.New++
				taken[b.ID] = true
				o.State.Picked[fan.Name] = append(o.State.Picked[fan.Name], b.ID)
				o.State.Done[fan.Name] = saved
				o.State.Saved++
				if o.Progress != nil {
					o.Progress(fan, saved, quota, b.ID)
				}
				// Checkpoint every work: a harvest is measured in hours and a state file that
				// only lands at a fandom boundary is a state file that loses an hour of manners.
				if err := o.State.Save(o.StatePath); err != nil && o.Logf != nil {
					o.Logf("state: %v", err)
				}
			}
		}
	}
	res.Saved = saved
	if firstErr != nil && saved == 0 {
		return res, firstErr
	}
	return res, nil
}

// listingURL is AO3's works listing for a tag, sorted by kudos.
//
// The sort parameters are load-bearing and were measured: `sort_column=kudos` answers a syntax
// error page, the site's own value is `kudos_count`, and sending sort_column WITHOUT
// sort_direction gets a 525 from Cloudflare. With both, twenty blurbs.
func listingURL(base, path string, page int, sortCol string) string {
	v := url.Values{}
	v.Set("work_search[sort_column]", sortCol)
	v.Set("work_search[sort_direction]", "desc")
	v.Set("page", fmt.Sprint(page))
	return base + "/tags/" + url.PathEscape(path) + "/works?" + v.Encode()
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
