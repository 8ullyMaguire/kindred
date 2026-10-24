package main

import "testing"

// The empty tune name means "the built-in default" on the command line, and ” in
// `eval_runs.tune` means "this run predates the column". Letting the first spelling
// through makes those two indistinguishable in the table that exists to tell them
// apart — found by reading the deployed table back and finding the default run
// labelled "(unrecorded)".
func TestTheDefaultTuneIsRecordedByNameNotAsEmpty(t *testing.T) {
	if got := tuneOr(""); got != "default" {
		t.Fatalf("empty tune recorded as %q, want \"default\": an unrecorded run and a "+
			"default-tune run would read the same", got)
	}
	if got := tuneOr("walk25"); got != "walk25" {
		t.Fatalf("named tune recorded as %q", got)
	}
}
