package tools

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// TestHasClass is ported from cutlass; it exercises the shared CSS-class
// matcher used by the chat-base web_search.go DuckDuckGo parser.
func TestHasClass(t *testing.T) {
	tests := []struct {
		name      string
		classAttr string
		target    string
		want      bool
	}{
		{"Exact match", "foo", "foo", true},
		{"Multiple classes start", "foo bar baz", "foo", true},
		{"Multiple classes middle", "foo bar baz", "bar", true},
		{"Multiple classes end", "foo bar baz", "baz", true},
		{"Partial match start", "foobar", "foo", false},
		{"Partial match end", "foobar", "bar", false},
		{"Partial match middle", "foobar", "oba", false},
		{"Surrounded by spaces", "  foo  ", "foo", true},
		{"Tabs and newlines", "foo\tbar\nbaz", "bar", true},
		{"No class attribute", "", "foo", false},
		{"Empty class attribute", "", "foo", false},
		{"Case sensitive", "Foo", "foo", false},
		{"Target in longer string", "class-foo", "foo", false},
		{"Target contains hyphen", "btn-primary", "btn-primary", true},
		{"Mixed whitespace", " \t\n\r\fclass1 \f\r\n\t class2", "class2", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &html.Node{
				Type: html.ElementNode,
				Data: "div",
				Attr: []html.Attribute{
					{Key: "class", Val: tt.classAttr},
				},
			}
			if tt.classAttr == "" && tt.name == "No class attribute" {
				node.Attr = nil
			}

			if got := hasClass(node, tt.target); got != tt.want {
				t.Errorf("hasClass() = %v, want %v (attr: %q, target: %q)", got, tt.want, tt.classAttr, tt.target)
			}
		})
	}
}

// TestSearchDuckDuckGo_TransparentGzip pins the fix for every search reporting
// "No results": the request must leave Accept-Encoding to Go's Transport, which
// then decompresses a gzip SERP before html.Parse sees it. Setting the header
// by hand disables that decompression, so the compressed bytes parsed to zero
// div.result nodes. The server here gzips whenever gzip is offered, exactly as
// html.duckduckgo.com does.
func TestSearchDuckDuckGo_TransparentGzip(t *testing.T) {
	const serp = `<html><body>
<div class="result results_links">
  <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fdoc&amp;rut=abc">Example Doc</a>
  <a class="result__snippet" href="https://example.com/doc">A snippet.</a>
</div></body></html>`
	var gotEncoding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		if !strings.Contains(gotEncoding, "gzip") {
			t.Errorf("Accept-Encoding = %q; the Transport should offer gzip", gotEncoding)
			_, _ = io.WriteString(w, serp)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, serp)
		_ = gz.Close()
	}))
	defer srv.Close()

	tool := &WebSearchTool{
		client:      srv.Client(),
		rateLimiter: newRateLimiter(0),
		endpoint:    srv.URL,
	}
	results, err := tool.searchDuckDuckGo(context.Background(), "example", 5)
	if err != nil {
		t.Fatalf("searchDuckDuckGo: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1 (compressed SERP was not decoded before parsing)", len(results))
	}
	if results[0].Link != "https://example.com/doc" || results[0].Title != "Example Doc" {
		t.Errorf("result = %+v", results[0])
	}
	// The Transport's own offer is exactly "gzip"; a hand-set "gzip, deflate"
	// is the signature of the bug.
	if gotEncoding != "gzip" {
		t.Errorf("Accept-Encoding on the wire = %q, want the Transport-managed \"gzip\"", gotEncoding)
	}
}
