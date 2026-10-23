package web

// Pages for the per-kind algorithm surface: choosing an algorithm, and the
// related list it produces.
//
// # Why the choice is a form and not only an API call
//
// The site is server-rendered and has no JavaScript (SPEC §1.1). A settings
// surface that exists only as a JSON endpoint is a settings surface most readers
// cannot reach, so the POST comes from a form and the page renders the result of
// the write it just made — one request, one truth.

import (
	"net/http"
	"strconv"
	"strings"

	"git.polarisocial.xyz/kindred/kindred/internal/algo"
	"git.polarisocial.xyz/kindred/kindred/internal/arena"
)

// renderChoose draws the per-kind picker and the live standings.
//
// On a POST it stores (or clears) the choice for the submitted kind, then renders
// the page for that kind so the reader sees the state they just set. It does not
// redirect: a redirect would lose the "saved" confirmation and, on a page with a
// session cookie, a second cookie-less request could land in a different arm.
func (d Deps) renderChoose(w http.ResponseWriter, r *http.Request) {
	base := d.baseOf(r, "Choose your algorithm", "Choose your algorithm")
	page := ChoosePage{Base: base, Kinds: algo.List()}
	// The kind with focus: the one just saved, else the first, so the page
	// always has a primary subject rather than six equal ones.
	focus := arena.ParseKind(r.FormValue("kind"))
	if r.FormValue("kind") == "" {
		focus = arena.Kinds[0]
	}
	page.Focus = focus

	if r.Method == http.MethodPost {
		raw := strings.TrimSpace(r.FormValue("algo"))
		switch {
		case !d.hasStore():
			page.Err = "This site has no state database, so a choice cannot be stored."
		case r.FormValue("clear") == "1":
			if _, owner, err := d.arenaSession(r.Context(), w, r); err == nil {
				if err := d.Engine.Store.ClearAlgoChoice(r.Context(), owner, focus); err != nil {
					page.Err = "Could not clear the choice: " + err.Error()
				} else {
					page.Saved = "Cleared. " + focus.Label() + " follow the experiment again."
				}
			}
		default:
			id, why := algo.Parse(focus, raw)
			if why != "" {
				page.Err = why
				break
			}
			if _, owner, err := d.arenaSession(r.Context(), w, r); err == nil {
				if err := d.Engine.Store.SetAlgoChoice(r.Context(), owner, focus, string(id)); err != nil {
					page.Err = "Could not store the choice: " + err.Error()
				} else {
					page.Saved = "Saved: " + labelFor(focus, id) + " for " + focus.Label() + "."
				}
			}
		}
	}

	if d.hasStore() {
		if _, owner, err := d.arenaSession(r.Context(), w, r); err == nil {
			if choices, err := d.Engine.Store.AlgoChoices(r.Context(), owner); err == nil {
				page.Chosen = choices
			}
			if arm := algo.ArmFor(focus, owner); arm != "" {
				page.Arms = map[arena.EntityKind]algo.ID{focus: arm}
			}
		}
		stats, err := d.Engine.Store.AlgoStandingsByKind(r.Context())
		if err == nil {
			page.Standing = stats
		}
		if arms, err := d.Engine.Store.AlgoArmCounts(r.Context()); err == nil {
			page.ArmReaders = arms
		}
	}
	if page.Standing == nil {
		page.NoData = "No lists have been served yet, so the standings are empty. " +
			"Open any related list and this table starts filling."
	}
	d.page(w, "choose.html", page, http.StatusOK)
}

// renderRelatedPage draws the related list for one seed.
func (d Deps) renderRelatedPage(w http.ResponseWriter, r *http.Request) {
	base := d.baseOf(r, "Related", "Related")
	page := RelatedPage{Base: base, Items: []RelatedItemView{}}
	kind := arena.ParseKind(r.FormValue("kind"))
	page.Kind = kind
	seed, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil || seed <= 0 {
		page.Err = "?id= must be a positive entity id"
		d.page(w, "related.html", page, http.StatusBadRequest)
		return
	}
	page.Seed = seed
	page.SeedLabel = d.seedLabel(r, kind, seed)

	ownerKey := ""
	if d.hasStore() {
		if _, owner, err := d.arenaSession(r.Context(), w, r); err == nil {
			ownerKey = owner
		}
	}
	stored := ""
	if d.hasStore() && ownerKey != "" {
		if choices, err := d.Engine.Store.AlgoChoices(r.Context(), ownerKey); err == nil {
			stored = choices[kind]
		}
	}
	chosen, why := algo.Resolve(kind, r.FormValue("algo"), stored, ownerKey)
	page.Algo = chosen
	page.Refused = why
	page.Default = algo.DefaultFor(kind)
	page.Choices = algo.LiveIDs(kind)

	res, err := d.Engine.Related(r.Context(), kind, seed, chosen, 25)
	if err != nil {
		page.Err = err.Error()
		d.page(w, "related.html", page, http.StatusOK)
		return
	}
	for _, it := range res.Items {
		page.Items = append(page.Items, RelatedItemView{
			ID: it.ID, Label: it.Label, URL: it.URL, Why: it.Why, Score: it.Score,
		})
	}
	page.Note = res.Note
	page.Missing = res.Missing
	page.Millis = res.Millis

	// The impression is logged here, where the list is SERVED. The API logs its
	// own when IT serves one; a page that called the API would be two rows for
	// one list.
	if d.hasStore() && ownerKey != "" {
		arm := "default"
		switch {
		case r.FormValue("algo") != "":
			arm = "request"
		case stored != "":
			arm = "choice"
		case chosen != algo.DefaultFor(kind):
			arm = "challenger"
		}
		page.Arm = arm
		shown := make([]int64, 0, len(res.Items))
		for _, it := range res.Items {
			shown = append(shown, it.ID)
		}
		if err := d.Engine.Store.RecordAlgoImpression(r.Context(), ownerKey, kind, string(chosen), arm, seed, shown); err != nil {
			d.Log.Warn("algo impression failed", "err", err)
		}
	}
	d.page(w, "related.html", page, http.StatusOK)
}

// seedLabel names the seed for a heading: "tag/29 (jasmine)" reads; "tag/29"
// does not.
func (d Deps) seedLabel(r *http.Request, kind arena.EntityKind, id int64) string {
	if d.Engine == nil || d.Engine.Corpus == nil {
		return ""
	}
	var name string
	var err error
	switch kind {
	case arena.KindTag, arena.KindCharacter, arena.KindRelationship, arena.KindFandom:
		err = d.Engine.Corpus.DB.QueryRowContext(r.Context(),
			`SELECT name FROM tags WHERE id = ?`, id).Scan(&name)
	case arena.KindWork:
		err = d.Engine.Corpus.DB.QueryRowContext(r.Context(),
			`SELECT title FROM works WHERE id = ?`, id).Scan(&name)
	}
	if err != nil {
		return ""
	}
	return name
}

// labelFor is the human name of an algorithm id, for the "saved" line.
func labelFor(kind arena.EntityKind, id algo.ID) string {
	for _, a := range algo.For(kind) {
		if a.ID == id {
			return a.Label
		}
	}
	return string(id)
}
