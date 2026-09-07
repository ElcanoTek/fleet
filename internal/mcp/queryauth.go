package mcp

import (
	"errors"
	"net/http"
)

// errQueryAuthRedirect is what the query-param-authenticated client returns
// for any redirect: the transport re-applies the key to EVERY request it sees,
// hops included, so following a 30x would hand the key to whatever host the
// server named. Refusing is the only safe policy for a URL-borne credential.
var errQueryAuthRedirect = errors.New("redirects are disabled for query-parameter-authenticated MCP connections")

// WithQueryParam returns a shallow copy of base whose transport appends
// name=value to every request's query string. It exists for vendors whose
// hosted MCP servers authenticate with an API key in the URL (Browserbase's
// ?browserbaseApiKey=…): attaching the credential per-request keeps it out of
// the registered server URL, so stored rows, log lines, and error strings that
// embed the URL never carry the key.
//
// The copy also refuses redirects, whatever base did. The stdlib strips
// Authorization/Cookie on a cross-domain hop, but a query-string credential is
// not a header — the RoundTripper below would re-attach it to the redirected
// request, so a 30x from the vendor (or a compromised endpoint) would relay the
// key to an arbitrary origin. Every caller today hands in a client that already
// refuses redirects (mcpoauth.SafeHTTPClient); this pins the property to the
// credential rather than to the caller's choice of base.
func WithQueryParam(base *http.Client, name, value string) *http.Client {
	c := *base
	inner := c.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	c.Transport = &queryParamTransport{inner: inner, name: name, value: value}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return errQueryAuthRedirect }
	return &c
}

type queryParamTransport struct {
	inner http.RoundTripper
	name  string
	value string
}

func (t *queryParamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before mutating: RoundTrippers must not modify the caller's request.
	r2 := req.Clone(req.Context())
	q := r2.URL.Query()
	q.Set(t.name, t.value)
	r2.URL.RawQuery = q.Encode()
	return t.inner.RoundTrip(r2)
}

// queryParamName reports whether s is safe to use as a query-parameter name:
// unreserved URL characters only, so a catalog entry can never smuggle
// delimiters or encoded structure into the request line.
func QueryParamNameOK(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}
