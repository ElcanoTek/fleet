package guardrail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPDetector(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(Verdict{Flagged: true, Score: .97, Reason: "injection"})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	v, err := d.Check(context.Background(), "prompt-injection", "user_message", "ignore instructions")
	if err != nil || !v.Flagged {
		t.Fatalf("verdict=%+v err=%v", v, err)
	}
	if got["profile"] != "prompt-injection" || got["source"] != "user_message" || got["text"] != "ignore instructions" {
		t.Fatalf("request=%v", got)
	}
}

func TestParseMode(t *testing.T) {
	if got, err := ParseMode(" BLOCK "); err != nil || got != ModeBlock {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := ParseMode("redact"); err == nil {
		t.Fatal("expected invalid mode")
	}
}

// TestNewHTTPDetectorRequiresHTTPSOffLoopback: the request body is raw
// untrusted text, so a plaintext hop is only allowed to a loopback classifier.
func TestNewHTTPDetectorRequiresHTTPSOffLoopback(t *testing.T) {
	ok := []string{
		"http://127.0.0.1:8790/v1/check",
		"http://localhost:8790/v1/check",
		"http://[::1]:8790/v1/check",
		"https://guard.internal.example/v1/check",
	}
	for _, u := range ok {
		if _, err := NewHTTPDetector(u); err != nil {
			t.Errorf("%s: unexpected error %v", u, err)
		}
	}
	bad := []string{
		"http://guard.internal.example/v1/check",
		"http://10.0.0.5:8790/v1/check",
		"ftp://127.0.0.1/check",
		"127.0.0.1:8790",
	}
	for _, u := range bad {
		if _, err := NewHTTPDetector(u); err == nil {
			t.Errorf("%s: expected rejection", u)
		}
	}
}

// TestHTTPDetectorRejectsOversizedBody: an unbounded caller must get a fast,
// explicit error rather than a 5 s timeout that reads as a detector outage.
func TestHTTPDetectorRejectsOversizedBody(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(Verdict{})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Check(context.Background(), "p", "tool_output", strings.Repeat("a", MaxDetectorBody+1))
	if !errors.Is(err, ErrTextTooLarge) {
		t.Fatalf("err=%v", err)
	}
	if called {
		t.Fatal("oversized body reached the detector")
	}
}
