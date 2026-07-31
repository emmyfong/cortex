package parse

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// The web parser fetches URLs supplied by the caller, which makes it a
// server-side request forgery (SSRF) surface: without controls, a caller could
// use it to reach Postgres on localhost, hosts on the local network, or a cloud
// metadata endpoint, and read the response back through the ingested document.
//
// Two layers guard against that:
//
//  1. validateURL rejects non-HTTP schemes before any request is made.
//  2. guardAddress inspects the IP actually being dialled, on every connection.
//
// The second layer is the one that matters. Checking only the hostname up front
// is defeated by DNS rebinding (a name that resolves to a public IP once and
// 127.0.0.1 the next time) and by redirects to internal hosts. Because the
// dialler runs for every hop, both are covered.

const (
	dialTimeout    = 10 * time.Second
	requestTimeout = 30 * time.Second
	maxRedirects   = 5
)

// validateURL parses a URL and confirms it is a fetchable HTTP(S) address.
func validateURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	switch parsed.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("unsupported URL scheme %q: only http and https are allowed", parsed.Scheme)
	}

	if parsed.Host == "" {
		return nil, fmt.Errorf("URL has no host")
	}
	return parsed, nil
}

// isBlockedIP reports whether an address is in non-public space that the
// fetcher must never reach. It fails closed on a nil address.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}

	// Judge v4-mapped v6 addresses (::ffff:127.0.0.1) on their embedded v4
	// address, otherwise they slip past every v4 check below.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	switch {
	case ip.IsLoopback(),
		ip.IsPrivate(),
		ip.IsUnspecified(),
		ip.IsLinkLocalUnicast(), // includes 169.254.169.254, the cloud metadata endpoint
		ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast():
		return true
	}

	// IPv6 unique local addresses (fc00::/7) — the v6 equivalent of RFC1918.
	if len(ip) == net.IPv6len && ip.To4() == nil && ip[0]&0xfe == 0xfc {
		return true
	}
	return false
}

// guardAddress is installed as net.Dialer.Control and runs immediately before
// each connection is established, with the resolved address in hand.
func guardAddress(network, address string, _ syscall.RawConn) error {
	if !strings.HasPrefix(network, "tcp") {
		return fmt.Errorf("blocked non-tcp network %q", network)
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("cannot parse dial address %q: %w", address, err)
	}

	ip := net.ParseIP(host)
	if isBlockedIP(ip) {
		// Deliberately vague: echoing back which internal ranges exist would
		// let a caller map the network by probing.
		return fmt.Errorf("blocked request to non-public address")
	}
	return nil
}

// newSafeHTTPClient builds a client that cannot be steered at internal hosts.
func newSafeHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: dialTimeout,
		Control: guardAddress,
	}

	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: requestTimeout,
		DisableKeepAlives:     true,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   requestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			// Each hop still dials through guardAddress, so a redirect to an
			// internal host fails at connect time; this only bounds the chain.
			if _, err := validateURL(req.URL.String()); err != nil {
				return fmt.Errorf("blocked redirect: %w", err)
			}
			return nil
		},
	}
}
