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
