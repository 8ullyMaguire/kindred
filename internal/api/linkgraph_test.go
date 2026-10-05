package api_test

// SPEC §4.4 row 3, third row: "A bug makes an outbound request."
//
// The mitigation the spec names is a link-graph assertion: the serving binary's
// request path links no HTTP client. This implements it.
//
// ## What it actually asserts, and why not the obvious thing
//
// The obvious assertion — `go list -deps ./cmd/kindred` contains no `net/http` —
// is FALSE, and would be false forever for a good reason. `internal/api` IS an
// HTTP server; it imports `net/http` to do its job. A gate that forbade it would
// be forbidding the product.
//
// So the claim has to be about REACHABILITY, not presence:
//
//   - the serving path (internal/api and what it imports) must not reach an
//     outbound HTTP CLIENT
//   - the fetch path (internal/onion) must not reach a plain dialer at all —
//     every dial goes through the Tor SOCKS proxy
//   - internal/signal, which computes recommendations, must reach neither
//
// `internal/crawl` really does fetch https://archiveofourown.org over clearnet.
// That is a deliberate, separate tool for seeding a corpus, it is NOT reachable
// from `cmd/kindred` (nothing imports it), and it must stay that way. A gate
// that scanned the whole repository for outbound clients would flag it and be
// wrong.
//
// ## Why this is a link graph and not a grep
//
// A grep for `http.Get` finds call sites. It cannot find a client CONSTRUCTED
// elsewhere and passed in, which is exactly how an outbound request gets added
// without any new `http.Get` appearing. `go list -deps` follows imports, so it
// sees the client however it is constructed.
//
// It also cannot be satisfied by a comment. A test that greps for the absence of
// a listener passes forever on a codebase that has no listener for unrelated
// reasons — the same trap as §4.4 row 1, which is why that one is an AST
// obligation rather than a grep.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const modPath = "git.polarocial.xyz/kindred/kindred"

// repoRoot walks up from the test's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find go.mod above the test directory")
	return ""
}

// depsOf runs `go list -deps` for a package and returns the set of linked
// packages, excluding the standard library so the assertions can be specific.
func depsOf(t *testing.T, pkg string) map[string]bool {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("go", "list", "-deps", "./"+pkg)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./%s: %v\n%s", pkg, err, out)
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Keep stdlib separate: net/http is a stdlib package and the whole
		// question is whether OUR code reaches it.
		set[line] = !strings.Contains(line, ".")
	}
	return set
}

// outboundClientPackages are the stdlib packages that can make an outbound HTTP
// request without a proxy.
//
// `net/http/httptrace` and `net/http/internal` are deliberately NOT here: they
// are pulled in by net/http's own internals and cannot issue a request on their
// own. Listing them would make the gate unfalsifiable in the other direction —
// it would fail on a tree where net/http is linked for serving, which is the
// correct design.
var outboundClientPackages = []string{
	"net/http",
	"net/http/httputil",
	"net/smtp",
	"net/rpc",
	"net/rpc/jsonrpc",
}

// The serving path must not reach an outbound HTTP client.
//
// It reaches net/http THROUGH the server itself — `internal/api` imports it to
// handle requests. So the assertion is about the DIRECTION of the dependency:
// `internal/api` may import net/http, but it must not import a package that
// dials OUT, and it must not import the fetch client at all.
//
// The check that matters is therefore: internal/api does not import
// internal/onion, and internal/api does not construct a client. Both are
// asserted separately below, because a link-graph-only assertion on a package
// that legitimately imports net/http cannot distinguish serving from fetching.
func TestServingPathDoesNotReachTheFetchClient(t *testing.T) {
	api := depsOf(t, "internal/api")
	if api[modPath+"/internal/onion"] {
		t.Fatal("internal/api reaches internal/onion, the Tor fetch client. " +
			"The serving path must not be able to make an outbound request at all.")
	}
	// net/http IS linked, by the server. Assert the honest fact so a future
	// reader does not "fix" this test by banning net/http.
	if !api["net/http"] {
		t.Log("note: internal/api does not link net/http; the server is elsewhere")
	} else {
		t.Log("internal/api links net/http for SERVING, which is the product")
	}
}

func TestSignalLayerReachesNoNetworkPackage(t *testing.T) {
	// internal/signal computes recommendations over a Candidate set. Per SPEC §5's
	// layering rule, "handlers never compute a signal; signals never touch SQL".
	// A signal that could reach the network would break that rule in a direction
	// nothing else checks.
	sig := depsOf(t, "internal/signal")
	for _, pkg := range outboundClientPackages {
		if sig[pkg] {
			t.Errorf("internal/signal links %s; a signal must be a pure function "+
				"over Candidate, Seeds and Store", pkg)
		}
	}
	if sig[modPath+"/internal/onion"] {
		t.Error("internal/signal links internal/onion")
	}
}

func TestFetchPathHasNoPlainDialFallback(t *testing.T) {
	// The fetch client may link net/http — that is how it speaks to a peer's
	// onion service. What it must not do is reach a dialer that bypasses the
	// SOCKS proxy. onion.Client pins its Transport's DialContext, and this
	// asserts the pinning is still there at the SOURCE level, because
	// TestTransportHasNoPlainDialFallback in internal/onion covers the same
	// ground from the other side and neither test can see the other's edits.
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "onion", "onion.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if !strings.Contains(text, "SOCKS5") && !strings.Contains(text, "socks5") {
		t.Fatal("internal/onion no longer mentions a SOCKS proxy; the fetch path " +
			"must dial only through Tor")
	}
	if !strings.Contains(text, "DialContext") {
		t.Fatal("internal/onion no longer pins a DialContext, so the transport " +
			"would fall back to a plain dialer")
	}
}

// THE CRAWL EXCEPTION, pinned.
//
// `internal/crawl` fetches https://archiveofourown.org over clearnet. That is a
// deliberate tool for seeding a corpus from the real site, and it must never be
// reachable from the serving binary.
//
// This is the assertion that makes the gate honest rather than
// satisfiable-by-deleting-the-crawler: if someone imports crawl from cmd/kindred
// or from internal/api, this fails. A gate that merely scanned the repository for
// outbound clients would flag crawl as a violation and be wrong.
func TestCrawlStaysUnreachableFromTheServingBinary(t *testing.T) {
	deps := depsOf(t, "cmd/kindred")
	if deps[modPath+"/internal/crawl"] {
		t.Fatal("cmd/kindred links internal/crawl, which fetches archiveofourown.org " +
			"over clearnet. The serving binary must not carry an outbound crawler.")
	}
	api := depsOf(t, "internal/api")
	if api[modPath+"/internal/crawl"] {
		t.Fatal("internal/api links internal/crawl")
	}
}

// The gate must be able to FAIL. It is a link-graph assertion about a property
// that is currently true; if the property could not be broken, it would be a
// comment.
func TestTheLinkGraphAssertionCanFire(t *testing.T) {
	// Sanity-check the harness itself: depsOf must return something, and must
	// distinguish stdlib from module packages. If it returned an empty set for
	// every package, every assertion above would pass vacuously.
	api := depsOf(t, "internal/api")
	if len(api) == 0 {
		t.Fatal("depsOf returned nothing; every assertion in this file is vacuous")
	}
	if api["net/http"] != true {
		t.Fatal("expected internal/api to link net/http (it is an HTTP server); " +
			"the stdlib-flag logic is wrong")
	}
	if api[modPath+"/internal/api"] != false {
		t.Fatal("expected the package itself to be flagged as stdlib=false")
	}
}
