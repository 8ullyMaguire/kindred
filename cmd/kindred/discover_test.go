package main

// Tests for the discover seed shorthand. Pure functions, no network.

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseSeedBuildsTheURLsAO3ActuallyUses(t *testing.T) {
	const base = "https://archiveofourown.org"
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"tag:Slow Burn", base + "/tags/Slow%20Burn/works"},
		// AO3 rewrites a slash inside a tag name to *s*, so the pairing tag
		// "Kirk/Spock" lives at /tags/Kirk*s*Spock/works. Escaping it the
		// obvious way (%2F) 404s, and a 404 here looks like "no works for this
		// tag" rather than "wrong URL".
		{"tag:Kirk/Spock", base + "/tags/Kirk%2As%2ASpock/works"},
		{"user:someone", base + "/users/someone/works"},
		{"bookmarks:someone", base + "/users/someone/bookmarks"},
		{"collection:foo", base + "/collections/foo/works"},
		{"series:12345", base + "/series/12345"},
		{"work:99", base + "/works/99"},
		// A full URL passes through: the shorthand exists so the common cases do
		// not need AO3's URL shapes, not to forbid naming one.
		{"https://archiveofourown.org/tags/Dark/works?page=3", base + "/tags/Dark/works?page=3"},
	} {
		got, err := parseSeed(tc.in, base)
		if err != nil {
			t.Errorf("parseSeed(%q) error = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseSeed(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseSeedSearchSortsByKudos(t *testing.T) {
	// A search that is not sorted by something stable returns a different first
	// page on every request, so the walk would gather twenty works at random
	// per page instead of the twenty best. The sort is part of the seed, not a
	// nicety.
	got, err := parseSeed("search:dragons", "https://archiveofourown.org")
	if err != nil {
		t.Fatalf("parseSeed: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("the built URL does not parse: %v", err)
	}
	q := u.Query()
	if q.Get("work_search[query]") != "dragons" {
		t.Errorf("query = %q; want dragons", q.Get("work_search[query]"))
	}
	if q.Get("work_search[sort_column]") != "kudos" {
		t.Errorf("sort_column = %q; want kudos", q.Get("work_search[sort_column]"))
	}
}

func TestParseSeedRejectsWhatItCannotBuild(t *testing.T) {
	const base = "https://archiveofourown.org"
	for _, in := range []string{
		"",
		"   ",
		"no-colon-here",
		"weird:value",
		"tag:",
		"series:notanid",
		"work:notanid",
	} {
		if got, err := parseSeed(in, base); err == nil {
			t.Errorf("parseSeed(%q) = %q with no error; want an error", in, got)
		}
	}
}

func TestParseKindsValidatesAgainstTheClassifier(t *testing.T) {
	// A typo must fail here rather than produce a walk that quietly follows
	// nothing: an empty frontier looks exactly like a seed with no works.
	got, err := parseKinds("tag, user-bookmarks")
	if err != nil {
		t.Fatalf("parseKinds: %v", err)
	}
	if len(got) != 2 || got[0] != "tag" || got[1] != "user-bookmarks" {
		t.Errorf("parseKinds = %v; want [tag user-bookmarks]", got)
	}

	if got, err := parseKinds("none"); err != nil || len(got) != 0 {
		t.Errorf("parseKinds(none) = %v, %v; want empty and no error", got, err)
	}
	if got, err := parseKinds(""); err != nil || len(got) != 0 {
		t.Errorf("parseKinds(\"\") = %v, %v; want empty and no error", got, err)
	}
	if _, err := parseKinds("tags"); err == nil {
		t.Error(`parseKinds("tags") accepted a value the classifier never returns; want an error`)
	}
	if _, err := parseKinds("tag,bogus"); err == nil {
		t.Error("parseKinds accepted one good and one bad kind; want an error")
	}
}

func TestParseKindsErrorNamesTheValidOnes(t *testing.T) {
	// The message is the documentation for anyone who mistypes, so it has to
	// list what would have worked.
	_, err := parseKinds("bogus")
	if err == nil {
		t.Fatal("parseKinds(bogus) returned no error")
	}
	for _, want := range []string{"tag", "user-bookmarks", "series"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not list %q", err, want)
		}
	}
}
