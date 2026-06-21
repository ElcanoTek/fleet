package tools

import (
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
