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

func TestSearch_TasksTypeReturnsEmpty(t *testing.T) {
	// type=tasks is accepted but not yet indexed — must return an empty set, never
	// silently fall through to conversation hits.
	s := New(&config.Config{SearchEnabled: true}, &fakeEngine{}, nil)
	rr := httptest.NewRecorder()
	s.search(rr, httptest.NewRequest(http.MethodGet, "/search?q=anything&type=tasks", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("tasks search: code=%d, want 200", rr.Code)
	}
	var resp searchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 0 || len(resp.Results) != 0 {
		t.Errorf("tasks search: total=%d len=%d, want 0/0", resp.Total, len(resp.Results))
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
