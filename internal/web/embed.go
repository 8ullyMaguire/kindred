// Package web serves kindred's HTML frontend from the binary itself, on
// the same port as the API.
//
// The target is a Raspberry Pi with 512 MB of free memory, which decides
// most of the design here. A static binary that also carries its own
// templates is 12 MB and one file; the same thing with a bundler is a
// node_modules tree, a build step that has to work on the deploy host,
// and an asset pipeline to keep alive. For four routes that trade is
// absurd.
//
// This package is also the only place kindred renders strings that came
// out of a 1.7 GB corpus into a browser, which is why it uses
// html/template and not text/template. A work called
// <script>alert(1)</script> is a real title in a real dataset.
package web

import "embed"

// assets holds the templates, the CSS and the JS. go:embed bakes them
// into the binary, so a deployed kindred has no sibling files to lose and
// no directory to get out of step with the binary next to it.
//
//go:embed assets
var assets embed.FS
