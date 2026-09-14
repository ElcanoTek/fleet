package mcpoauth

import (
	"net/url"
	"testing"
)

func TestCanonicalResourceURI(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://MCP.Example.COM", "https://mcp.example.com"},
		{"https://mcp.example.com/", "https://mcp.example.com"},
		{"https://mcp.example.com:443/", "https://mcp.example.com"},
		{"http://mcp.example.com:80/", "http://mcp.example.com"},
		{"https://mcp.example.com:8443/mcp", "https://mcp.example.com:8443/mcp"},
		{"https://mcp.example.com/path/", "https://mcp.example.com/path/"}, // non-root trailing slash preserved
		{"https://mcp.example.com/mcp#frag", "https://mcp.example.com/mcp"},
		{"HTTPS://Mcp.Example.com/MCP", "https://mcp.example.com/MCP"}, // path case preserved
		// An escaped reserved character is a different route from its decoded
		// form and is kept as typed; a default-encoding escape is normalized.
		{"https://mcp.example.com/tenant%2Fone/mcp", "https://mcp.example.com/tenant%2Fone/mcp"},
		{"https://mcp.example.com/a%3Fb/mcp", "https://mcp.example.com/a%3Fb/mcp"},
		{"https://mcp.example.com/a%23b/mcp", "https://mcp.example.com/a%23b/mcp"},
		{"https://mcp.example.com/a%20b/mcp", "https://mcp.example.com/a%20b/mcp"},
		{"https://mcp.example.com/a b/mcp", "https://mcp.example.com/a%20b/mcp"},
		{"https://mcp.example.com/tenant%2Fone/mcp?tenant=x", "https://mcp.example.com/tenant%2Fone/mcp?tenant=x"},
		{"https://mcp.example.com/%2F", "https://mcp.example.com/%2F"}, // an escaped slash is not a lone root path
	}
	for _, c := range cases {
		got, err := CanonicalResourceURI(c.in)
		if err != nil {
			t.Errorf("CanonicalResourceURI(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("CanonicalResourceURI(%q) = %q, want %q", c.in, got, c.want)
		}
		// The canonical form is a fixed point: re-canonicalizing a stored
		// identity must never change it, or every existing row would drift.
		if again, err := CanonicalResourceURI(got); err != nil || again != got {
			t.Errorf("CanonicalResourceURI(%q) is not idempotent: %q, %v", got, again, err)
		}
	}
}

// TestCanonicalResourceURIEscapedSegmentIsDistinct: the whole point of keeping
// the escape — "/tenant%2Fone/mcp" and "/tenant/one/mcp" are different
// resources and must have different identities, and the kept escape survives
// into the request path fleet dials.
func TestCanonicalResourceURIEscapedSegmentIsDistinct(t *testing.T) {
	a, err := CanonicalResourceURI("https://mcp.example.com/tenant%2Fone/mcp")
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalResourceURI("https://mcp.example.com/tenant/one/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("escaped and decoded paths canonicalize to the same identity %q", a)
	}
	u, err := url.Parse(a)
	if err != nil {
		t.Fatal(err)
	}
	if u.EscapedPath() != "/tenant%2Fone/mcp" {
		t.Errorf("request path = %q, want the escaped segment preserved", u.EscapedPath())
	}
}

func TestCanonicalResourceURIRejects(t *testing.T) {
	bad := []string{
		"",
		"not-a-url",
		"ftp://example.com",
		"/relative/path",
		"https://user:pass@example.com", // embedded credentials
		"file:///etc/passwd",
	}
	for _, in := range bad {
		if _, err := CanonicalResourceURI(in); err == nil {
			t.Errorf("CanonicalResourceURI(%q) accepted a bad URL", in)
		}
	}
}

func TestValidateServerURL(t *testing.T) {
	if _, err := ValidateServerURL("http://mcp.example.com", false); err == nil {
		t.Error("ValidateServerURL allowed http:// without allowInsecureHTTP")
	}
	if got, err := ValidateServerURL("http://mcp.example.com", true); err != nil || got != "http://mcp.example.com" {
		t.Errorf("ValidateServerURL(insecure) = %q, %v", got, err)
	}
	if got, err := ValidateServerURL("https://mcp.example.com/", false); err != nil || got != "https://mcp.example.com" {
		t.Errorf("ValidateServerURL(https) = %q, %v", got, err)
	}
}
