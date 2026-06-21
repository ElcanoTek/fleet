package httpapi

import (
	"mime"
	"strings"
	"testing"
)

// TestOfficeMimeTypes_Registered: the init() in mime.go registers
// OOXML + legacy office types on import. Without this, .pptx files
// produced by the Gamma MCP and served via the workspace endpoint
// fall back to application/zip sniffing on hosts that don't ship
// /etc/mime.types (minimal containers, alpine).
func TestOfficeMimeTypes_Registered(t *testing.T) {
	cases := []struct {
		ext      string
		wantPart string // a substring the registered Content-Type must contain
	}{
		{".pptx", "presentationml.presentation"},
		{".xlsx", "spreadsheetml.sheet"},
		{".docx", "wordprocessingml.document"},
		{".ppt", "ms-powerpoint"},
		{".xls", "ms-excel"},
		{".doc", "msword"},
	}
	for _, c := range cases {
		t.Run(c.ext, func(t *testing.T) {
			got := mime.TypeByExtension(c.ext)
			if !strings.Contains(got, c.wantPart) {
				t.Errorf("mime.TypeByExtension(%q) = %q, want substring %q", c.ext, got, c.wantPart)
			}
		})
	}
}
