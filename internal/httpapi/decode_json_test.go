package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecodeJSONBodyHelpers pins the one 400 contract every JSON-body handler
// now shares (#1949): a malformed body is "bad json: …" for both helpers, an
// empty body is a 400 only when the body is required, and a valid body decodes.
func TestDecodeJSONBodyHelpers(t *testing.T) {
	t.Parallel()

	type payload struct {
		Name string `json:"name"`
	}
	cases := []struct {
		name     string
		body     string
		optional bool
		wantOK   bool
	}{
		{"required valid", `{"name":"a"}`, false, true},
		{"required malformed", `{`, false, false},
		{"required empty", ``, false, false},
		{"optional valid", `{"name":"a"}`, true, true},
		{"optional malformed", `{`, true, false},
		{"optional empty", ``, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			var dst payload
			decode := decodeJSONBody
			if tc.optional {
				decode = decodeOptionalJSONBody
			}
			if got := decode(w, r, &dst); got != tc.wantOK {
				t.Fatalf("ok = %v, want %v", got, tc.wantOK)
			}
			if tc.wantOK {
				if w.Code != http.StatusOK {
					t.Errorf("a successful decode wrote status %d", w.Code)
				}
				return
			}
			if w.Code != http.StatusBadRequest || !strings.HasPrefix(w.Body.String(), "bad json: ") {
				t.Errorf("got %d %q, want 400 starting with \"bad json: \"", w.Code, w.Body.String())
			}
		})
	}
}
