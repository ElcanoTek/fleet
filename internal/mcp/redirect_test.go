package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The redirect policy exists so a 30x from a tool's configured endpoint cannot
// relay its resolved credential headers (X-Api-Key and friends — which the
// stdlib does NOT strip, unlike Authorization/Cookie) to a different origin.

func TestStripHeadersOnCrossOriginRedirect_Policy(t *testing.T) {
	mkReq := func(rawURL string, hdr map[string]string) *http.Request {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse %q: %v", rawURL, err)
		}
		req := &http.Request{URL: u, Header: http.Header{}}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		return req
	}
	creds := map[string]string{"X-Api-Key": "sekrit-placeholder", "Accept": "application/json"}

	t.Run("same origin keeps headers", func(t *testing.T) {
		origin := mkReq("https://api.example.com:8443/a", creds)
		next := mkReq("https://api.example.com:8443/b", creds)
		if err := stripHeadersOnCrossOriginRedirect(next, []*http.Request{origin}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if next.Header.Get("X-Api-Key") == "" {
			t.Fatal("same-origin redirect must keep the credential header")
		}
	})

	t.Run("cross-host strips every original header", func(t *testing.T) {
		origin := mkReq("https://api.example.com/a", creds)
		next := mkReq("https://evil.example.net/b", creds)
		if err := stripHeadersOnCrossOriginRedirect(next, []*http.Request{origin}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := next.Header.Get("X-Api-Key"); got != "" {
			t.Fatalf("credential header forwarded cross-origin: %q", got)
		}
		if got := next.Header.Get("Accept"); got != "" {
			t.Fatalf("original header forwarded cross-origin: %q", got)
		}
	})

	t.Run("port change is a different origin", func(t *testing.T) {
		origin := mkReq("http://127.0.0.1:1000/a", creds)
		next := mkReq("http://127.0.0.1:2000/b", creds)
		if err := stripHeadersOnCrossOriginRedirect(next, []*http.Request{origin}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if next.Header.Get("X-Api-Key") != "" {
			t.Fatal("credential header forwarded to a different port")
		}
	})

	t.Run("caps the chain at 10 hops", func(t *testing.T) {
		origin := mkReq("https://api.example.com/a", nil)
		via := make([]*http.Request, 10)
		for i := range via {
			via[i] = origin
		}
		if err := stripHeadersOnCrossOriginRedirect(mkReq("https://api.example.com/b", nil), via); err == nil {
			t.Fatal("expected an error after 10 redirects")
		}
	})
}

// recordingServer returns a server that records the headers of the last request
// it saw on /final.
func recordingServer(t *testing.T) (*httptest.Server, *http.Header) {
	t.Helper()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestExecuteHTTPTool_RedirectHeaderHandling(t *testing.T) {
	transport := &httpToolTransport{}
	client := transport.httpClient()

	t.Run("cross-origin redirect strips the resolved credential header", func(t *testing.T) {
		target, got := recordingServer(t)
		// A second httptest server is a different port on 127.0.0.1 — a
		// different origin under the policy.
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/final", http.StatusFound)
		}))
		defer redirector.Close()

		spec := HTTPToolSpec{
			Name:    "probe",
			Method:  http.MethodGet,
			URL:     redirector.URL + "/start",
			Headers: map[string]string{"X-Api-Key": "sekrit-placeholder"},
		}
		res, err := executeHTTPTool(context.Background(), client, spec, nil)
		if err != nil {
			t.Fatalf("executeHTTPTool: %v", err)
		}
		if res.IsError {
			t.Fatalf("unexpected tool error: %+v", res)
		}
		if key := got.Get("X-Api-Key"); key != "" {
			t.Fatalf("credential header leaked across origins: %q", key)
		}
	})

	t.Run("same-origin redirect keeps the credential header", func(t *testing.T) {
		var got http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/start" {
				http.Redirect(w, r, "/final", http.StatusFound)
				return
			}
			got = r.Header.Clone()
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer srv.Close()

		spec := HTTPToolSpec{
			Name:    "probe",
			Method:  http.MethodGet,
			URL:     srv.URL + "/start",
			Headers: map[string]string{"X-Api-Key": "sekrit-placeholder"},
		}
		if _, err := executeHTTPTool(context.Background(), client, spec, nil); err != nil {
			t.Fatalf("executeHTTPTool: %v", err)
		}
		if got.Get("X-Api-Key") != "sekrit-placeholder" {
			t.Fatal("same-origin redirect must keep the credential header")
		}
	})
}

func TestHTTPTransport_RedirectStripsBearer(t *testing.T) {
	target, got := recordingServer(t)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/final", http.StatusFound)
	}))
	defer redirector.Close()

	tr := NewHTTPTransportWithHeaders(redirector.URL, map[string]string{
		"X-Api-Key": "sekrit-placeholder",
	})
	// The recording server answers plain JSON, not a JSON-RPC envelope; a
	// decode error is fine — the outbound request is what's under test.
	if _, err := tr.Call(context.Background(), "tools/list", map[string]interface{}{}); err != nil {
		t.Logf("transport call (expected decode noise): %v", err)
	}
	if len(*got) == 0 {
		t.Fatal("redirect target never received the request")
	}
	if key := got.Get("X-Api-Key"); key != "" {
		t.Fatalf("MCP transport forwarded the credential header cross-origin: %q", key)
	}
}
