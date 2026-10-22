package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/engine"
	"git.polarisocial.xyz/kindred/kindred/internal/rank"
	"git.polarisocial.xyz/kindred/kindred/internal/store"
)

// The /algo page: two algorithms, one seed, side by side.
//
// An algorithm is a named tune. That is not a convenience — it is the only
// definition this deployment can honestly offer. The ranking IS a weighted
// sum over the signals, the tune table stores those weights by name, and
// inventing a second notion of "algorithm" would create a registry that can
// disagree with the one the engine actually uses.
func (d Deps) renderAlgo(w http.ResponseWriter, r *http.Request) {
	base := d.baseOf(r, "Compare algorithms", "Compare algorithms")
	q := r.URL.Query()
	page := AlgoPage{
		Base:   base,
		Seed:   strings.TrimSpace(q.Get("seed")),
		A:      strings.TrimSpace(q.Get("a")),
		B:      strings.TrimSpace(q.Get("b")),
		Judged: strings.TrimSpace(q.Get("judged")),
	}
	if page.A == "" {
		page.A = "default"
	}

	// The registry, so both selects are populated from what exists.
	if d.Algo != nil {
		if names, err := d.Algo.TuneNames(r.Context()); err == nil {
			page.Choices = append([]string{"default"}, names...)
		}
	}
	if len(page.Choices) == 0 {
		page.Choices = []string{"default"}
	}

	// No seed yet: the untouched form. Not a 404 — the page is in the nav
	// and a reader who clicks it first must not be told it does not exist.
	if page.Seed == "" {
		d.page(w, "algo.html", page, http.StatusOK)
		return
	}

	if d.Engine == nil || d.Engine.Corpus == nil {
		page.Err = "no corpus is loaded on this server"
		d.page(w, "algo.html", page, http.StatusServiceUnavailable)
		return
	}
	seed, err := engine.ParseSeed(page.Seed)
	if err != nil {
		page.Err = err.Error()
		d.page(w, "algo.html", page, http.StatusBadRequest)
		return
	}
	if page.B == "" {
		// Two algorithms are the question. An empty second column is not
		// a comparison, and rendering one would invite a vote about
		// nothing.
		page.Err = "pick a second algorithm to compare with " + page.A
		d.page(w, "algo.html", page, http.StatusBadRequest)
		return
	}
	if page.A == page.B {
		page.Err = fmt.Sprintf("cannot compare %q with itself: the two lists would be identical", page.A)
		d.page(w, "algo.html", page, http.StatusBadRequest)
		return
	}
	if d.Algo == nil {
		page.Err = "this deployment has no algorithm registry (it is running without a state database)"
		d.page(w, "algo.html", page, http.StatusServiceUnavailable)
		return
	}

	// Resolve both tunes before ranking, so an unknown name is a named
	// error rather than a column ranked by the defaults.
	weightsFor := func(name string) (map[string]float64, error) {
		if name == "default" {
			return nil, nil
		}
		w, err := d.Algo.Tune(r.Context(), name)
		if err != nil {
			return nil, fmt.Errorf("no algorithm named %q", name)
		}
		return w, nil
	}
	leftW, err := weightsFor(page.A)
	if err != nil {
		page.Err = err.Error()
		d.page(w, "algo.html", page, http.StatusBadRequest)
		return
	}
	rightW, err := weightsFor(page.B)
	if err != nil {
		page.Err = err.Error()
		d.page(w, "algo.html", page, http.StatusBadRequest)
		return
	}

	// The SAME pool for both sides. That is what makes this a measurement:
	// the only thing that differs between the two columns is the weights.
	pool := intParamOr(r, "pool", 0)
	n := intParamOr(r, "n", 5)
	if n < 1 || n > 25 {
		n = 5
	}

	side := func(tune string) ([]rank.Candidate, error) {
		res, err := d.Engine.Recommend(r.Context(), engine.Request{
			Kind:     seed.Kind,
			Seeds:    []engine.Seed{seed},
			Tune:     tune,
			Exclude:  true,
			N:        n,
			PoolSize: pool,
		})
		if err != nil {
			return nil, err
		}
		return res.Items, nil
	}
	tuneName := func(name string) string {
		if name == "default" {
			return ""
		}
		return name
	}
	page.Left, err = side(tuneName(page.A))
	if err != nil {
		page.Err = err.Error()
		d.page(w, "algo.html", page, http.StatusInternalServerError)
		return
	}
	page.Right, err = side(tuneName(page.B))
	if err != nil {
		page.Err = err.Error()
		d.page(w, "algo.html", page, http.StatusInternalServerError)
		return
	}
	page.LeftWeights = leftW
	page.RightWeights = rightW
	page.WeightsDiffer = differingWeights(leftW, rightW)
	page.Shared = sharedCount(page.Left, page.Right)

	d.page(w, "algo.html", page, http.StatusOK)
}

// postAlgoJudge records a vote and redirects back to the comparison.
//
// It is a PAGE endpoint, like /arena/judge, so the control is a plain form
// and works with scripting off. A fetch-based control would be the second
// thing on this page that silently does nothing when JS fails.
func (d Deps) postAlgoJudge(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		d.fail(w, r, http.StatusBadRequest, err)
		return
	}
	seedStr := strings.TrimSpace(r.PostFormValue("seed"))
	a := strings.TrimSpace(r.PostFormValue("algo_a"))
	b := strings.TrimSpace(r.PostFormValue("algo_b"))
	choice := strings.TrimSpace(r.PostFormValue("choice"))
	if a == "" {
		a = "default"
	}

	back := "/algo?seed=" + urlQuery(seedStr) + "&a=" + urlQuery(a) + "&b=" + urlQuery(b)
	seed, err := engine.ParseSeed(seedStr)
	if err != nil {
		d.fail(w, r, http.StatusBadRequest, err)
		return
	}
	switch choice {
	case "a", "b", "neither":
	default:
		d.fail(w, r, http.StatusBadRequest,
			fmt.Errorf("choice must be a, b or neither, got %q", choice))
		return
	}
	if d.Algo == nil {
		d.fail(w, r, http.StatusServiceUnavailable,
			fmt.Errorf("this deployment has no algorithm registry"))
		return
	}

	// The reader's owner key when they have a session. A vote with no owner
	// is still recorded: the arena on this deployment has never required an
	// account, and a standings table scoped to nobody would be empty.
	ownerKey := ""
	if c, err := r.Cookie(ArenaCookieName); err == nil && c.Value != "" {
		if key, err := d.Algo.OwnerKey(r.Context(), c.Value); err == nil {
			ownerKey = key
		}
	}

	if err := d.Algo.RecordAlgoComparison(r.Context(), ownerKey, store.AlgoComparison{
		SeedKind: seed.Kind,
		SeedID:   seed.ID,
		AlgoA:    a,
		AlgoB:    b,
		Winner:   choice,
	}); err != nil {
		d.fail(w, r, http.StatusBadRequest, err)
		return
	}
	http.Redirect(w, r, back+"&judged="+urlQuery(choice), http.StatusSeeOther)
}

// differingWeights names the signals whose weights are not equal, sorted so
// the page is stable between reloads.
//
// A nil and a zero weight are treated as equal: both mean "this signal does
// not contribute", and naming them as a difference would tell a reader the
// two algorithms disagree about a signal that neither uses.
func differingWeights(a, b map[string]float64) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	var out []string
	for k := range seen {
		if a[k] != b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// sharedCount is how many ids appear in both lists.
//
// It is shown because two columns can disagree about ORDER while agreeing
// about membership, and a reader who cannot tell those apart will read
// "same works, different order" as "the algorithms are identical".
func sharedCount(a, b []rank.Candidate) int {
	seen := make(map[int64]bool, len(a))
	for _, c := range a {
		seen[c.ID] = true
	}
	n := 0
	for _, c := range b {
		if seen[c.ID] {
			n++
		}
	}
	return n
}

// intParamOr reads a query integer with a default.
func intParamOr(r *http.Request, name string, def int) int {
	v := strings.TrimSpace(r.URL.Query().Get(name))
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}

// urlQuery escapes a value for a redirect target.
func urlQuery(s string) string { return url.QueryEscape(s) }

// AlgoStandingsContext is exposed for tests that want to read standings
// without going through a page.
func AlgoStandingsContext(ctx context.Context, s AlgoStore, ownerKey string) ([]store.AlgoStanding, error) {
	if s == nil {
		return nil, fmt.Errorf("no algorithm registry")
	}
	return s.AlgoStandings(ctx, ownerKey)
}
