package web

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, c := range []struct {
		name    string
		peer    string
		xff     string
		trusted []netip.Prefix
		want    string
	}{
		{"NoTrustedUsesRemoteAddr", "192.0.2.1:1234", "6.6.6.6", nil, "192.0.2.1"},
		{"TrustedPeerUsesXFF", "10.0.0.5:99", "203.0.113.7, 10.0.0.9", trusted, "203.0.113.7"},
		{"SpoofedLeftmostIgnored", "10.0.0.5:99", "6.6.6.6, 203.0.113.7", trusted, "203.0.113.7"},
		{"UntrustedPeerXFFIgnored", "192.0.2.1:1234", "203.0.113.7", trusted, "192.0.2.1"},
		{"AllTrustedLeftmost", "10.0.0.5:99", "10.1.1.1, 10.0.0.9", trusted, "10.1.1.1"},
		{"GarbageEndsWalk", "10.0.0.5:99", "6.6.6.6, junk, 10.0.0.9", trusted, "10.0.0.9"},
		{"TrustedNoXFF", "10.0.0.5:99", "", trusted, "10.0.0.5"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.peer
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := clientIP(r, c.trusted); got != netip.MustParseAddr(c.want) {
				t.Fatalf("got %v want %s", got, c.want)
			}
		})
	}
}
