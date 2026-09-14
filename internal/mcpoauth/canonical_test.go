package mcpoauth

import "testing"

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
		// IPv6 literals keep their brackets: Hostname() strips them, and
		// re-joining host and port without them makes the boundary ambiguous.
		{"https://[2001:db8::1]/mcp", "https://[2001:db8::1]/mcp"},
		{"https://[2001:DB8::1]:8443/mcp", "https://[2001:db8::1]:8443/mcp"},
		{"https://[2001:db8::1]:443/mcp", "https://[2001:db8::1]/mcp"}, // default port still dropped
		{"https://[2001:db8::1:8443]/mcp", "https://[2001:db8::1:8443]/mcp"},
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

// TestCanonicalResourceURIKeepsIPv6IdentitiesApart is the property this
// function exists for, on the one host form that used to break it. Its doc
// comment says treating URL-ish strings as interchangeable "is the classic way
// to leak a bearer to the wrong resource" — and because Hostname() strips an
// IPv6 literal's brackets, https://[2001:db8::1]:8443 and
// https://[2001:db8::1:8443] (a different server) both canonicalized to
// https://2001:db8::1:8443. One canonical string means one DB key, one
// encryption AAD and one RFC 8707 audience, so the collision pointed two
// servers at a single identity.
func TestCanonicalResourceURIKeepsIPv6IdentitiesApart(t *testing.T) {
	a, aerr := CanonicalResourceURI("https://[2001:db8::1]:8443/mcp")
	b, berr := CanonicalResourceURI("https://[2001:db8::1:8443]/mcp")
	if aerr != nil || berr != nil {
		t.Fatalf("canonicalize: %v / %v", aerr, berr)
	}
	if a == b {
		t.Errorf("two distinct IPv6 servers share one canonical identity: %q", a)
	}
	// The canonical form must be a URL that parses back to the same host, or
	// everything downstream that re-parses the stored identity is broken.
	for _, canon := range []string{a, b} {
		round, err := CanonicalResourceURI(canon)
		if err != nil {
			t.Errorf("canonical form %q does not re-parse: %v", canon, err)
			continue
		}
		if round != canon {
			t.Errorf("canonical form %q is not stable: re-canonicalized to %q", canon, round)
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
