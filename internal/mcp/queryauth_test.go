package mcp

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The Browserbase catalog entry's URL carries its own query parameter
// (?keepAlive=true, which is what lets a hosted session outlive one fleet turn
// so a human can complete a login). The API key is attached per-request on top
// of that URL, so the two must coexist: if key injection replaced the query
// string instead of adding to it, keep-alive would silently switch off and the
// browserbase skill's handoff would lose the user's session. q.Set is what makes
// that safe — this pins it.
func TestWithQueryParamPreservesExistingQuery(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
	}))
	defer srv.Close()

	client := WithQueryParam(srv.Client(), "browserbaseApiKey", "bb-test-key-not-real")
	resp, err := client.Get(srv.URL + "/mcp?keepAlive=true&proxies=false")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if got := gotQuery.Get("browserbaseApiKey"); got != "bb-test-key-not-real" {
		t.Errorf("browserbaseApiKey = %q, want the injected key", got)
	}
	if got := gotQuery.Get("keepAlive"); got != "true" {
		t.Errorf("keepAlive = %q, want it preserved as \"true\"", got)
	}
	if got := gotQuery.Get("proxies"); got != "false" {
		t.Errorf("proxies = %q, want it preserved as \"false\"", got)
	}
}

// The credential must not leak into the caller's request: RoundTrip clones
// before mutating, so the URL the caller still holds has no key in it. That is
// what keeps the key out of stored rows and log lines that echo a request URL.
func TestWithQueryParamDoesNotMutateCallerRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/mcp?keepAlive=true", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	before := req.URL.String()

	resp, err := WithQueryParam(srv.Client(), "browserbaseApiKey", "bb-test-key-not-real").Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if after := req.URL.String(); after != before {
		t.Errorf("caller request URL was mutated: %q -> %q", before, after)
	}
}

// A nil Transport on the base client must fall back to http.DefaultTransport
// rather than panicking — the overlay builds clients both ways.
func TestWithQueryParamHandlesNilTransport(t *testing.T) {
	if got := WithQueryParam(&http.Client{}, "k", "v"); got.Transport == nil {
		t.Fatal("transport must be wrapped, not left nil")
	}
}

func TestQueryParamNameOK(t *testing.T) {
	for _, ok := range []string{"browserbaseApiKey", "api-key", "api_key", "k", "A1"} {
		if !QueryParamNameOK(ok) {
			t.Errorf("QueryParamNameOK(%q) = false, want true", ok)
		}
	}
	// Anything that could smuggle delimiters or structure into the request
	// line must be refused.
	for _, bad := range []string{"", "has space", "a=b", "a&b", "a?b", "a/b", "a%20b", "a.b", string(make([]byte, 65))} {
		if QueryParamNameOK(bad) {
			t.Errorf("QueryParamNameOK(%q) = true, want false", bad)
		}
	}
}
