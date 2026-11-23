package web

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientIP returns the caller's address. X-Forwarded-For is only read when
// the direct peer is inside trusted; it is then walked right to left and the
// first address outside trusted wins (the leftmost entry if all are trusted).
// An entry that does not parse ends the walk with the last good address.
func clientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	peer, _ := netip.ParseAddr(host)
	peer = peer.Unmap()
	if !inPrefixes(peer, trusted) {
		return peer
	}
	last := peer
	parts := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			return last
		}
		a = a.Unmap()
		last = a
		if !inPrefixes(a, trusted) {
			return a
		}
	}
	return last
}
