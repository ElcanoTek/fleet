package mcp

import (
	"fmt"
	"net/http"
	"strings"
)

// stripHeadersOnCrossOriginRedirect is the CheckRedirect policy for the two
// credential-bearing clients in this package (the inline-HTTP-tool client and
// the HTTP MCP transport). Both write already-resolved secrets onto the
// outbound request as plain headers (e.g. X-Api-Key, vendor auth headers), and
// Go's default policy only strips Authorization/Cookie/Www-Authenticate on a
// cross-domain hop — every other header is forwarded to whatever host a 30x
// names. That would let a redirecting (or compromised) endpoint relay a
// resolved secret to an attacker-controlled origin.
//
// Policy: follow redirects (some REST APIs legitimately 30x — trailing-slash
// canonicalisation, presigned URLs), but when a hop leaves the original
// origin, drop every header the ORIGINAL request carried — that set is exactly
// the caller-provided credential/static headers. Origin is compared as
// scheme-independent host:port, stricter than the stdlib's domain-suffix rule:
// a redirect that changes only the port (or makes a default port explicit)
// also strips, which fails safe. The stdlib's own sensitive-header rule still
// applies on top for Authorization/Cookie.
func stripHeadersOnCrossOriginRedirect(req *http.Request, via []*http.Request) error {
	// http.Client only applies its 10-hop cap through the default policy, so a
	// custom CheckRedirect must re-impose it.
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	origin := via[0]
	if !strings.EqualFold(req.URL.Host, origin.URL.Host) {
		for k := range origin.Header {
			req.Header.Del(k)
		}
	}
	return nil
}
