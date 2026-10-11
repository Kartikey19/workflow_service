package netguard

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

// M3 acceptance: the metadata address, every RFC 1918 range and loopback are
// blocked by default, each checked explicitly, and only an allow entry opens one.
func TestDefaultPolicyBlocksMetadataPrivateAndLoopback(t *testing.T) {
	cases := map[string]string{
		"metadata":       "169.254.169.254",
		"rfc1918 10/8":   "10.1.2.3",
		"rfc1918 172":    "172.16.0.1",
		"rfc1918 172.31": "172.31.255.254",
		"rfc1918 192":    "192.168.1.10",
		"loopback v4":    "127.0.0.1",
		"loopback v6":    "::1",
		"mapped v4":      "::ffff:10.0.0.1",
	}
	for name, address := range cases {
		addr := netip.MustParseAddr(address)
		if (Policy{}).Permits(addr) {
			t.Errorf("%s (%s) is permitted by default", name, address)
		}
		_, err := (Policy{}).DialContext(context.Background(), "tcp", netip.AddrPortFrom(addr, 80).String())
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: dial err = %v, want ErrBlocked", name, err)
		}
	}
	if !(Policy{}).Permits(netip.MustParseAddr("93.184.216.34")) {
		t.Error("public address blocked")
	}
	allow := Policy{Allow: []netip.Prefix{netip.MustParsePrefix("10.20.0.0/16")}}
	if !allow.Permits(netip.MustParseAddr("10.20.1.1")) || allow.Permits(netip.MustParseAddr("10.21.1.1")) {
		t.Error("EGRESS_ALLOW_CIDRS entry not applied precisely")
	}
}
