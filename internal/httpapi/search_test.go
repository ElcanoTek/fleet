package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

func TestSearch_DisabledReturns404(t *testing.T) {
	s := New(&config.Config{SearchEnabled: false}, &fakeEngine{}, nil)
	rr := httptest.NewRecorder()
	s.search(rr, httptest.NewRequest(http.MethodGet, "/search?q=hello", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("disabled search: code=%d, want 404", rr.Code)
	}
}

func TestSearch_MethodNotAllowed(t *testing.T) {
	s := New(&config.Config{SearchEnabled: true}, &fakeEngine{}, nil)
	rr := httptest.NewRecorder()
	s.search(rr, httptest.NewRequest(http.MethodPost, "/search", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /search: code=%d, want 405", rr.Code)
	}
}

func TestSearch_EmptyQueryReturnsEmpty(t *testing.T) {
	// Empty q short-circuits before touching the store, so a nil store is fine.
	s := New(&config.Config{SearchEnabled: true}, &fakeEngine{}, nil)
	rr := httptest.NewRecorder()
	s.search(rr, httptest.NewRequest(http.MethodGet, "/search?q=", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("empty query: code=%d, want 200", rr.Code)
	}
	var resp searchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 0 || len(resp.Results) != 0 {
		t.Errorf("empty query: total=%d len=%d, want 0/0", resp.Total, len(resp.Results))
	}
	if resp.Results == nil {
		t.Error("Results must serialize as [] not null")
	}
}

func TestSearch_UnknownTypeRejected(t *testing.T) {
	// Search is conversations-only. The retired "tasks" stub (and its "all"
	// alias) used to answer 200 with a lying empty set (#1076); any type other
	// than "conversations" must now be an honest 400.
	s := New(&config.Config{SearchEnabled: true}, &fakeEngine{}, nil)
	for _, typ := range []string{"tasks", "all", "garbage"} {
		rr := httptest.NewRecorder()
		s.search(rr, httptest.NewRequest(http.MethodGet, "/search?q=anything&type="+typ, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("type=%s: code=%d, want 400", typ, rr.Code)
		}
	}
	// The one indexed surface stays accepted, spelled out or defaulted.
	for _, target := range []string{"/search?q=&type=conversations", "/search?q="} {
		rr := httptest.NewRecorder()
		s.search(rr, httptest.NewRequest(http.MethodGet, target, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("%s: code=%d, want 200", target, rr.Code)
		}
	}
}

// Paging follows the scheduler's contract: absent → default, present must be
// well-formed and in range, else an error naming the range. The old clamp
// answered `?limit=abc` with the default page and `?limit=999` with 100 rows.
func TestParseSearchLimit(t *testing.T) {
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", searchDefaultLimit, false},
		{"50", 50, false},
		{"1", 1, false},
		{"100", 100, false},
		{"0", 0, true},
		{"-5", 0, true},
		{"101", 0, true},
		{"999", 0, true},
		{"garbage", 0, true},
		{"12abc", 0, true},
	}
	for _, c := range cases {
		got, err := parseSearchLimit(c.raw)
		if (err != nil) != c.wantErr {
			t.Errorf("parseSearchLimit(%q) err = %v, wantErr %v", c.raw, err, c.wantErr)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), "1 and 100") {
			t.Errorf("parseSearchLimit(%q) error should name the accepted range, got %q", c.raw, err)
		}
		if got != c.want {
			t.Errorf("parseSearchLimit(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestParseSearchOffset(t *testing.T) {
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"40", 40, false},
		{"-1", 0, true},
		{"garbage", 0, true},
	}
	for _, c := range cases {
		got, err := parseSearchOffset(c.raw)
		if (err != nil) != c.wantErr {
			t.Errorf("parseSearchOffset(%q) err = %v, wantErr %v", c.raw, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("parseSearchOffset(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// Over the wire: a malformed page parameter is a 400 that names the range,
// never a 200 with a page the client did not ask for.
func TestSearch_BadPagingIs400(t *testing.T) {
	s := New(&config.Config{SearchEnabled: true}, &fakeEngine{}, nil)
	for _, target := range []string{
		"/search?q=x&limit=abc", "/search?q=x&limit=0", "/search?q=x&limit=101",
		"/search?q=x&offset=-1", "/search?q=x&offset=abc",
	} {
		// The store is nil: the 400 must be written BEFORE any search runs.
		rr := httptest.NewRecorder()
		s.search(rr, httptest.NewRequest(http.MethodGet, target, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: code=%d body=%q, want 400", target, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "must be") {
			t.Errorf("%s: body should describe the accepted range, got %q", target, rr.Body.String())
		}
	}
}
