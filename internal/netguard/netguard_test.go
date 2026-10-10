package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestPolicyBlocksPrivateRanges(t *testing.T) {
	policy := Policy{}
	for _, address := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.9", "192.168.1.1", "169.254.169.254", "::1", "fd00::1", "100.64.0.1"} {
		if policy.Permits(netip.MustParseAddr(address)) {
			t.Errorf("%s permitted, want blocked", address)
		}
	}
	for _, address := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111"} {
		if !policy.Permits(netip.MustParseAddr(address)) {
			t.Errorf("%s blocked, want permitted", address)
		}
	}
	allowed := Policy{Allow: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}}
	if !allowed.Permits(netip.MustParseAddr("10.0.0.7")) || allowed.Permits(netip.MustParseAddr("10.0.1.7")) {
		t.Fatal("allow list not applied exactly")
	}
}

func TestClientRefusesLoopbackAtDialTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer server.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	_, err := Policy{}.Client(5 * time.Second).Do(req)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if _, err := (Policy{AllowPrivate: true}).Client(5 * time.Second).Do(req.Clone(context.Background())); err != nil {
		t.Fatalf("AllowPrivate request failed: %v", err)
	}
}
