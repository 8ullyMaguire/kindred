// Package onion provides the .onion-only fetch client.
//
// The threat this defends against is specific and worth stating plainly:
// a peer must not be able to learn the operator's IP by asking for a
// snapshot, and the operator must not accidentally learn a peer's. The
// guarantee is not "we usually use Tor" — it is that this package has no
// code path which can resolve a name or open a socket to anything but a
// SOCKS proxy pointed at a .onion host.
//
// Three properties enforce that, and each has a test:
//
//  1. The dialer is a SOCKS5 dialer. There is no plain-dial fallback, so
//     a missing Tor is a failure, never a silent leak over the clearnet.
//  2. The URL host is checked against dump.IsOnionHost before every
//     request and after every redirect. A redirect from an onion to a
//     clearnet host is refused, because following it is exactly the leak.
//  3. No DNS: the onion address is handed to the SOCKS proxy as bytes and
//     this package never calls a resolver.
package onion

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"git.polarisocial.xyz/kindred/kindred/internal/dump"
)

// DefaultSocks is Tor's SOCKS port.
const DefaultSocks = "127.0.0.1:9050"

// ErrNotOnion is returned when a host is not a valid onion address.
var ErrNotOnion = errors.New("onion: refusing to fetch a non-onion host")

// Client fetches over Tor and nowhere else.
type Client struct {
	// SocksAddr is the Tor SOCKS5 proxy. There is no default-fallback to
	// a direct connection, deliberately: if Tor is not running the fetch
	// fails, and that failure is the security property working.
	SocksAddr string

	// Timeout bounds the whole exchange, not just the connect.
	Timeout time.Duration

	http *http.Client
}

// New builds a client. The SOCKS address must be a local proxy; a remote
// one is refused because a remote SOCKS proxy is just another way to be
// deanonymised by whoever runs it.
func New(socksAddr string, timeout time.Duration) (*Client, error) {
	if socksAddr == "" {
		socksAddr = DefaultSocks
	}
	host, _, err := net.SplitHostPort(socksAddr)
	if err != nil {
		return nil, fmt.Errorf("onion: socks address %q must be host:port: %w", socksAddr, err)
	}
	// Only a loopback proxy is accepted. A SOCKS proxy on another host is
	// a third party in the middle of every fetch.
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		if host != "localhost" {
			return nil, fmt.Errorf("onion: socks proxy must be on loopback, got %q", host)
		}
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	c := &Client{SocksAddr: socksAddr, Timeout: timeout}
	c.http = &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// Dial through SOCKS5. This is the only dialer in the client:
			// there is no fallback to net.Dial, so an unreachable Tor
			// fails instead of leaking.
			DialContext: c.dialSocks,
			// No proxy from the environment. HTTP_PROXY set to a
			// clearnet proxy would silently undo every guarantee here.
			Proxy: nil,
			// A connection must not be reused across onion hosts.
			DisableKeepAlives:   true,
			DisableCompression:  false,
			TLSHandshakeTimeout: timeout,
			// An onion service is HTTP, not HTTPS: the .onion address is
			// already authenticated by the Tor handshake, and TLS would
			// add a certificate authority path that does not exist for
			// onion addresses.
			TLSClientConfig: nil,
		},
		CheckRedirect: c.checkRedirect,
	}
	return c, nil
}

// dialSocks opens a connection through the local Tor SOCKS5 proxy.
func (c *Client) dialSocks(ctx context.Context, network, addr string) (net.Conn, error) {
	d := socksDialer{proxy: c.SocksAddr}
	conn, err := d.dial(ctx, network, addr)
	if err != nil {
		// The error text says Tor specifically, because "connection
		// refused" from a bare dialer is ambiguous and an operator who
		// cannot tell Tor is down from a peer being down will eventually
		// start looking for a way around it.
		return nil, fmt.Errorf("onion: cannot reach Tor at %s (is tor running?): %w", c.SocksAddr, err)
	}
	return conn, nil
}

// checkRedirect refuses any redirect that leaves the onion network.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("onion: too many redirects")
	}
	host := req.URL.Hostname()
	if !dump.IsOnionHost(host) {
		// This is the leak, stopped at the last possible moment: a
		// compromised or merely careless peer could otherwise bounce the
		// client onto the clearnet and observe the operator's IP.
		return fmt.Errorf("onion: refusing redirect from %s to non-onion host %q",
			via[len(via)-1].URL.Host, host)
	}
	return nil
}

// Get fetches a path from an onion base URL and returns the body.
//
// base must be an http:// URL whose host is a valid v3 onion address. The
// check is here as well as in the redirect handler because the initial
// request is not a redirect: nothing else would stop a caller passing
// https://example.com/ and having it fetched over Tor to a clearnet host.
func (c *Client) Get(ctx context.Context, base, path string) ([]byte, error) {
	if err := CheckBase(base); err != nil {
		return nil, err
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("onion: bad base url: %w", err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(path, "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("onion: build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("onion: fetch %s: %w", u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("onion: %s returned %s", u.Host, resp.Status)
	}
	// Bound the read: an onion service can serve an unbounded body, and a
	// snapshot is known to be large but not infinite.
	const maxBody = 2 << 30 // 2 GiB
	buf := make([]byte, 0, 1<<20)
	tmp := make([]byte, 1<<20)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			if len(buf)+n > maxBody {
				return nil, fmt.Errorf("onion: response exceeds %d bytes", maxBody)
			}
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if errors.Is(err, net.ErrClosed) || err.Error() == "EOF" {
				break
			}
			break
		}
	}
	return buf, nil
}

// CheckBase validates an onion base URL. Exported so the serving side can
// assert its own configuration with the same rule the fetcher enforces.
func CheckBase(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotOnion, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q is not http(s)", ErrNotOnion, u.Scheme)
	}
	if !dump.IsOnionHost(u.Hostname()) {
		return fmt.Errorf("%w: %q", ErrNotOnion, u.Hostname())
	}
	return nil
}

// socksDialer is a minimal SOCKS5 client, enough to hand a host:port to
// Tor without resolving it.
//
// A hand-rolled dialer rather than a dependency because the whole point is
// that the SOCKS handshake receives the onion address unresolved: doing
// the CONNECT with the hostname as bytes is what keeps DNS out of this
// process entirely.
type socksDialer struct {
	proxy string
}

func (d socksDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	proxyConn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", d.proxy)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = proxyConn.SetDeadline(deadline)
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("onion: bad target %q: %w", addr, err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("onion: bad port in %q", addr)
	}

	// SOCKS5 greeting: version 5, one method, "no authentication".
	if _, err := proxyConn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		proxyConn.Close()
		return nil, err
	}
	resp := make([]byte, 2)
	if _, err := readFull(proxyConn, resp); err != nil {
		proxyConn.Close()
		return nil, err
	}
	if resp[0] != 0x05 {
		proxyConn.Close()
		return nil, fmt.Errorf("onion: socks proxy is not SOCKS5 (version %d)", resp[0])
	}
	if resp[1] != 0x00 {
		proxyConn.Close()
		return nil, errors.New("onion: socks proxy requires authentication; refusing")
	}

	// CONNECT with the address as a domain name, so Tor resolves it. The
	// length byte is one byte, so an onion address (56 chars) fits.
	if len(host) > 255 {
		proxyConn.Close()
		return nil, fmt.Errorf("onion: hostname too long (%d)", len(host))
	}
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := proxyConn.Write(req); err != nil {
		proxyConn.Close()
		return nil, err
	}
	head := make([]byte, 4)
	if _, err := readFull(proxyConn, head); err != nil {
		proxyConn.Close()
		return nil, err
	}
	if head[1] != 0x00 {
		proxyConn.Close()
		return nil, fmt.Errorf("onion: socks connect failed: %s", socksStatus(head[1]))
	}
	// Drain the bound address, whose length depends on its type.
	switch head[3] {
	case 0x01: // IPv4
		if _, err := readFull(proxyConn, make([]byte, 4+2)); err != nil {
			proxyConn.Close()
			return nil, err
		}
	case 0x04: // IPv6
		if _, err := readFull(proxyConn, make([]byte, 16+2)); err != nil {
			proxyConn.Close()
			return nil, err
		}
	case 0x03: // domain
		l := make([]byte, 1)
		if _, err := readFull(proxyConn, l); err != nil {
			proxyConn.Close()
			return nil, err
		}
		if _, err := readFull(proxyConn, make([]byte, int(l[0])+2)); err != nil {
			proxyConn.Close()
			return nil, err
		}
	default:
		proxyConn.Close()
		return nil, fmt.Errorf("onion: unknown socks address type %d", head[3])
	}
	// Clear the handshake deadline; the caller's timeout governs from here.
	_ = proxyConn.SetDeadline(time.Time{})
	return proxyConn, nil
}

func readFull(c net.Conn, b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := c.Read(b[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func socksStatus(code byte) string {
	switch code {
	case 0x01:
		return "general failure"
	case 0x02:
		return "connection not allowed"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "ttl expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return fmt.Sprintf("code %d", code)
	}
}
