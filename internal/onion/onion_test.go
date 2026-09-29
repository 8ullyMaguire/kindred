package onion

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/dump"
)

// dumpIsOnion is a thin alias so the test reads against the same rule the
// client enforces.
func dumpIsOnion(host string) bool { return dump.IsOnionHost(host) }

// These tests are the ones SPEC §4.4 names. Each maps to a row of that
// table, and each is a refusal test: what matters is that the bad case
// fails, not that the good case works.

// TestGetRefusesAClearnetHost is the "fetch client falls back to
// clearnet" threat. The client is pointed at an ordinary local HTTP
// server — the exact thing a peer would use to deanonymise the operator —
// and must refuse before dialling.
func TestGetRefusesAClearnetHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the request reached a clearnet server; the refusal is not happening")
		w.Write([]byte("leaked"))
	}))
	defer srv.Close()

	c, err := New(DefaultSocks, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(context.Background(), srv.URL, "manifest.json")
	if err == nil {
		t.Fatal("fetching a clearnet host succeeded; the client's only job is to prevent this")
	}
	if !errors.Is(err, ErrNotOnion) {
		t.Fatalf("err = %v, want ErrNotOnion so the reason is unambiguous", err)
	}
}

// TestGetRefusesLoopbackByName covers the variant an attacker would
// actually try: a URL that looks local but bypasses a naive string check.
func TestGetRefusesLoopbackByName(t *testing.T) {
	for _, base := range []string{
		"http://127.0.0.1:8080",
		"http://localhost:9999",
		"http://[::1]:8080",
		"http://192.168.1.1",
		"http://example.com",
		"https://archiveofourown.org",
		"http://user:pass@evil.onionx.com",
		"ftp://foo.onion",
	} {
		c, err := New(DefaultSocks, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Get(context.Background(), base, "x"); !errors.Is(err, ErrNotOnion) {
			t.Errorf("Get(%q) = %v, want ErrNotOnion", base, err)
		}
	}
}

// TestCheckRedirectRefusesLeavingTheOnion is the redirect leak: an onion
// that redirects to a clearnet host would otherwise carry the operator's
// IP with it.
func TestCheckRedirectRefusesLeavingTheOnion(t *testing.T) {
	c, err := New(DefaultSocks, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	onion := "http://" + validOnion() + "/"

	from := mustReq(t, onion)
	via := []*http.Request{from}

	// Staying on the onion is allowed.
	stay := mustReq(t, "http://"+validOnion2()+"/next")
	if err := c.checkRedirect(stay, via); err != nil {
		t.Fatalf("a redirect to another onion was refused: %v", err)
	}

	// Leaving is not.
	leak := mustReq(t, "http://example.com/track")
	err = c.checkRedirect(leak, append(via, stay))
	if err == nil {
		t.Fatal("a redirect from an onion to a clearnet host was allowed; that is the IP leak")
	}
	if !strings.Contains(err.Error(), "non-onion") {
		t.Fatalf("err = %v, want it to name the reason", err)
	}
}

func TestCheckRedirectBoundsHops(t *testing.T) {
	c, _ := New(DefaultSocks, time.Second)
	req := mustReq(t, "http://"+validOnion()+"/")
	many := make([]*http.Request, 5)
	for i := range many {
		many[i] = req
	}
	if err := c.checkRedirect(req, many); err == nil {
		t.Fatal("five redirects were allowed; the bound exists to stop a loop")
	}
}

// TestNewRefusesANonLoopbackProxy: a SOCKS proxy on another host is a
// third party in the middle of every fetch, and would see every onion
// address the operator contacts.
func TestNewRefusesANonLoopbackProxy(t *testing.T) {
	for _, addr := range []string{
		"socks.example.com:9050",
		"10.0.0.1:1080",
		"8.8.8.8:9050",
		"[2001:db8::1]:9050",
	} {
		if _, err := New(addr, time.Second); err == nil {
			t.Errorf("New(%q) succeeded; only a loopback Tor proxy is acceptable", addr)
		}
	}
}

func TestNewAcceptsLoopbackForms(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:9050", "localhost:9050", "[::1]:9050"} {
		if _, err := New(addr, time.Second); err != nil {
			t.Errorf("New(%q) = %v, want success", addr, err)
		}
	}
}

func TestNewDefaultsAndRejectsBadAddresses(t *testing.T) {
	c, err := New("", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if c.SocksAddr != DefaultSocks {
		t.Fatalf("default socks = %q, want %q", c.SocksAddr, DefaultSocks)
	}
	if _, err := New("no-port", time.Second); err == nil {
		t.Fatal("an address without a port was accepted")
	}
	// A zero timeout must become a real one, not an instant deadline.
	c, err = New("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout <= 0 {
		t.Fatal("a zero timeout was left as zero; every request would fail instantly")
	}
}

// TestTransportHasNoPlainDialFallback is the structural check: the
// transport's only dialer is the SOCKS one. If someone later adds a
// fallback to net.Dial, this fails.
func TestTransportHasNoPlainDialFallback(t *testing.T) {
	c, err := New(DefaultSocks, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.http.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("no dialer set: the transport would use its default, which is a plain dial")
	}
	if tr.Dial != nil {
		t.Fatal("a deprecated plain Dial is set alongside DialContext")
	}
	// Proxy must be nil: an HTTP_PROXY in the environment would otherwise
	// route onion traffic through a clearnet proxy and undo everything.
	if tr.Proxy != nil {
		t.Fatal("a proxy function is set; environment proxies must not apply")
	}
}

// TestGetFailsLoudlyWhenTorIsDown: with no Tor, the fetch must fail and
// say so. It must not fall back to a direct connection, which is the
// failure mode this whole package exists to prevent.
func TestGetFailsLoudlyWhenTorIsDown(t *testing.T) {
	// Point at a closed loopback port: nothing is listening, which is what
	// a stopped Tor looks like.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // now closed: connections are refused

	c, err := New(addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(context.Background(), "http://"+validOnion()+"/", "manifest.json")
	if err == nil {
		t.Fatal("a fetch succeeded with no Tor running")
	}
	if !strings.Contains(err.Error(), "Tor") {
		t.Fatalf("err = %v; the message must say Tor is unreachable, or an "+
			"operator will look for a way around it", err)
	}
}

// TestGetRejectsABadOnionShape: not every .onion string is an address, and
// a malformed one should be refused before it reaches a socket.
func TestGetRejectsABadOnionShape(t *testing.T) {
	c, _ := New(DefaultSocks, time.Second)
	for _, base := range []string{
		"http://short.onion",
		"http://" + strings.Repeat("a", 55) + ".onion", // 55, not 56
		"http://" + strings.Repeat("a", 57) + ".onion", // 57
		"http://" + strings.Repeat("1", 56) + ".onion", // 1 is not base32
		"http://.onion",
		"http://",
	} {
		if _, err := c.Get(context.Background(), base, "m"); !errors.Is(err, ErrNotOnion) {
			t.Errorf("Get(%q) = %v, want ErrNotOnion", base, err)
		}
	}
}

// --- helpers --------------------------------------------------------------

// validOnion builds a syntactically valid v3 onion address: exactly 56
// base32 characters before the .onion suffix.
//
// The length is asserted by TestValidOnionHelpersAreValid, because a
// helper that is one character short produces a test that fails for the
// wrong reason — which is how a real regression would hide behind a
// broken fixture.
func validOnion() string {
	return "abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrstuvwx.onion"
}

func validOnion2() string {
	return "765432zyxwvutsrqponmlkjihgfedcba765432zyxwvutsrqponmlkji.onion"
}

func TestValidOnionHelpersAreValid(t *testing.T) {
	for name, host := range map[string]string{"validOnion": validOnion(), "validOnion2": validOnion2()} {
		base := strings.TrimSuffix(host, ".onion")
		if len(base) != 56 {
			t.Errorf("%s() has a %d-char base, want 56: a v3 onion address is 56 "+
				"base32 characters, and a short one fails every test that uses it", name, len(base))
		}
		if !dumpIsOnion(host) {
			t.Errorf("%s() (%q) is not accepted as an onion address", name, host)
		}
	}
}

func mustReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
