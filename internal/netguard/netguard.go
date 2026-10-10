// Package netguard builds network clients that refuse private destinations.
// Workflow authors control URLs, so every outbound connection made on their
// behalf is an SSRF vector (02 §4.1): loopback, RFC 1918, link-local (cloud
// metadata at 169.254.169.254), CGNAT and unique-local ranges are blocked
// unless explicitly allowed. The check runs on the resolved IP at dial time,
// so DNS rebinding cannot bypass it.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

var ErrBlocked = errors.New("destination address is not allowed")

// Policy decides which resolved addresses may be dialled.
type Policy struct {
	// AllowPrivate permits every address; for local development only.
	AllowPrivate bool
	// Allow lists extra prefixes, e.g. a private database subnet.
	Allow []netip.Prefix
}

var blocked = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"224.0.0.0/4", "240.0.0.0/4", "::1/128", "::/128", "fc00::/7", "fe80::/10", "ff00::/8",
)

func mustPrefixes(values ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(values))
	for i, value := range values {
		out[i] = netip.MustParsePrefix(value)
	}
	return out
}

// ParseAllow reads a comma-separated CIDR list such as EGRESS_ALLOW_CIDRS.
func ParseAllow(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(item)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", item)
		}
		out = append(out, prefix)
	}
	return out, nil
}

// Permits reports whether addr may be dialled.
func (p Policy) Permits(addr netip.Addr) bool {
	if p.AllowPrivate {
		return true
	}
	addr = addr.Unmap()
	for _, prefix := range p.Allow {
		if prefix.Contains(addr) {
			return true
		}
	}
	for _, prefix := range blocked {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// Dialer returns a dialer whose Control hook rejects blocked addresses after
// DNS resolution.
func (p Policy) Dialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			addr, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("%w: unresolved address", ErrBlocked)
			}
			if !p.Permits(addr) {
				return fmt.Errorf("%w: %s", ErrBlocked, addr)
			}
			return nil
		},
	}
}

// DialContext is the policy-checked dial function for non-HTTP clients.
func (p Policy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return p.Dialer().DialContext(ctx, network, address)
}

// Client returns an HTTP client that enforces the policy on every connection,
// including redirects, and follows at most five redirects.
func (p Policy) Client(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy:                 nil, // a proxy would hide the real destination from the check
		DialContext:           p.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			return nil
		},
	}
}
