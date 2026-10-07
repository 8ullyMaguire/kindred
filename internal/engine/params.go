package engine

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ParseFilter reads ranking filters out of a query string.
//
// It lives here, beside Filter and filterSQL, rather than in the api or web
// package, for one reason: the three must not be able to disagree. A filter
// with a parser in each HTTP layer is a filter whose meaning is whatever the
// layer in front of it decided, and the two layers here have already shipped
// with different accepted spellings of the same control.
//
// It errors on every value it does not recognise rather than ignoring it.
// A filter that parses and then does nothing is the accepted-and-ignored
// shape this project has found repeatedly, and it is worst in a form: the
// reader sets "in progress only", sees unfiltered results, and concludes the
// engine is broken rather than that the control is inert.
func ParseFilter(q url.Values) (Filter, error) {
	var f Filter
	get := func(name string) string { return strings.TrimSpace(q.Get(name)) }

	// Bounded integer parse shared by the three numeric filters. The bound
	// is what stops a typo'd `min_words=99999999999999999999` becoming an
	// overflowing value that silently matches everything or nothing.
	num := func(name string) (int64, error) {
		v := get(name)
		if v == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be a whole number, got %q", name, v)
		}
		if n < 0 {
			return 0, fmt.Errorf("%s must not be negative, got %d", name, n)
		}
		return n, nil
	}

	var err error
	if f.MinWords, err = num("min_words"); err != nil {
		return f, err
	}
	if f.MaxWords, err = num("max_words"); err != nil {
		return f, err
	}
	if f.MinKudos, err = num("min_kudos"); err != nil {
		return f, err
	}
	// A reversed range is a reader mistake worth naming: it can only ever
	// match nothing, and an empty result set with no error reads as "this
	// corpus has nothing", which is a much stronger and wrong claim.
	if f.MinWords > 0 && f.MaxWords > 0 && f.MinWords > f.MaxWords {
		return f, fmt.Errorf("min_words (%d) is above max_words (%d), which can only match nothing",
			f.MinWords, f.MaxWords)
	}

	// The spellings are a superset on purpose: a hand-written query string
	// says `complete=1`, and a form's select says `complete=in-progress`.
	// Rejecting one of them would make the same control behave differently
	// depending on how the reader spelled it.
	//
	// `true` means COMPLETE ONLY, not "no filter". That reads like a
	// vacuous truth in isolation, but a tri-state select has three
	// positions and the boolean truthy spelling most naturally pairs with
	// "the work is complete". Mapping it to `any` would make
	// `complete=true` and `complete=false` the SAME request, which is
	// indefensible for a control whose whole job is to distinguish them.
	switch strings.ToLower(get("complete")) {
	case "", "any", "all":
		f.Complete = CompleteAny
	case "only", "complete", "completed", "true", "1", "yes":
		f.Complete = CompleteOnly
	case "no", "false", "0", "wip", "in-progress", "incomplete":
		f.Complete = CompleteWIP
	default:
		return f, fmt.Errorf(
			"complete must be any, complete or in-progress, got %q", q.Get("complete"))
	}

	// Ratings and languages are multi-valued and accept both repeated
	// parameters and a comma-separated list, because the form posts repeated
	// fields and a query string is easier to write by hand with commas.
	for _, spec := range []struct {
		name string
		dst  *[]string
	}{{"rating", &f.Ratings}, {"lang", &f.Languages}, {"language", &f.Languages}} {
		if len(q[spec.name]) == 0 {
			continue
		}
		var out []string
		for _, raw := range q[spec.name] {
			for _, part := range strings.Split(raw, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
		}
		*spec.dst = append(*spec.dst, out...)
	}
	return f, nil
}
