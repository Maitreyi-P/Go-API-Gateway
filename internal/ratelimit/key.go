package ratelimit

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP derives the rate-limit key for a request: the first address in
// an X-Forwarded-For header if present (as commonly set by a load
// balancer or proxy in front of the gateway), otherwise the IP from the
// direct connection's RemoteAddr.
//
// X-Forwarded-For is attacker-controlled input from the client's
// perspective, so trusting it is only appropriate when the gateway sits
// behind infrastructure that sets/overwrites the header itself rather than
// forwarding it verbatim from untrusted clients.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0]); first != "" {
			return first
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
