package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestClampSearchInt(t *testing.T) {
	cases := []struct {
		raw         string
		def, lo, hi int
		want        int
	}{
		{"", 20, 1, 100, 20},
		{"50", 20, 1, 100, 50},
		{"0", 20, 1, 100, 1},
		{"999", 20, 1, 100, 100},
		{"-5", 0, 0, 100, 0},
		{"garbage", 20, 1, 100, 20},
	}
	for _, c := range cases {
		if got := clampSearchInt(c.raw, c.def, c.lo, c.hi); got != c.want {
			t.Errorf("clampSearchInt(%q,%d,%d,%d) = %d, want %d", c.raw, c.def, c.lo, c.hi, got, c.want)
		}
	}
}
