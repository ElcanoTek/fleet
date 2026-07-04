package piiredact

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Rampart engine client. Load-bearing assertions: each mode maps the service
// response correctly, findings group into audit kinds, a dead/garbage service
// falls back to the deterministic pattern engine (never fail-open, never fail
// the call), the degradation log is rate-limited and text-free, and
// ProbeService surfaces connectivity errors instead of degrading.

func rampartStub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const rampartOKBody = `{
	"text": "My name is [GIVEN_NAME_1] [SURNAME_1], SSN [SSN_1], at [BUILDING_NUMBER_1] [STREET_NAME_1]",
	"findings": [
		{"kind": "GIVEN_NAME", "count": 1}, {"kind": "SURNAME", "count": 1},
		{"kind": "SSN", "count": 1},
		{"kind": "BUILDING_NUMBER", "count": 1}, {"kind": "STREET_NAME", "count": 1}
	]
}`

const rampartInput = "My name is Alex Rivera, SSN 123-45-6789, at 12 Main St"

func TestRampartRedactMode(t *testing.T) {
	srv := rampartStub(t, 200, rampartOKBody)
	r := NewRampart(ModeRedact, srv.URL)

	if r.Mode() != ModeRedact { // Redactor contract: reports its configured mode.
		t.Errorf("Mode() = %q, want redact", r.Mode())
	}
	res := r.Redact(rampartInput)
	if !strings.Contains(res.Text, "[GIVEN_NAME_1]") || strings.Contains(res.Text, "Alex") {
		t.Errorf("redact should use the service's placeholder text: %q", res.Text)
	}
	if res.Blocked {
		t.Error("redact mode must not block")
	}
	// Findings group: given_name+surname→name(2), building+street→address(2), ssn(1).
	sum := res.Summary()
	for _, want := range []string{"name×2", "address×2", "ssn×1"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary %q missing %q", sum, want)
		}
	}
}

func TestRampartObserveAndBlockModes(t *testing.T) {
	srv := rampartStub(t, 200, rampartOKBody)

	obs := NewRampart(ModeObserve, srv.URL).Redact(rampartInput)
	if obs.Text != rampartInput || !obs.Found() || obs.Blocked {
		t.Errorf("observe must pass text through with findings: %+v", obs)
	}

	blk := NewRampart(ModeBlock, srv.URL).Redact(rampartInput)
	if !blk.Blocked || !strings.HasPrefix(blk.Text, "[BLOCKED:") || strings.Contains(blk.Text, "Alex") {
		t.Errorf("block must withhold: %q", blk.Text)
	}
}

func TestRampartCleanTextPassthrough(t *testing.T) {
	srv := rampartStub(t, 200, `{"text": "the report is ready", "findings": []}`)
	res := NewRampart(ModeRedact, srv.URL).Redact("the report is ready")
	if res.Text != "the report is ready" || res.Found() {
		t.Errorf("clean text should pass through: %+v", res)
	}
}

// TestRampartFallbackOnServiceError: a dead service degrades to the pattern
// engine — the email still gets redacted (the floor), the call never errors,
// and the log line carries the failure mode but never the text.
func TestRampartFallbackOnServiceError(t *testing.T) {
	srv := rampartStub(t, 500, "boom")
	r := NewRampart(ModeRedact, srv.URL)
	var logged []string
	r.logf = func(format string, args ...any) {
		logged = append(logged, format)
		for _, a := range args {
			if s, ok := a.(error); ok && strings.Contains(s.Error(), "jane@corp.com") {
				t.Error("log must never carry the text")
			}
		}
	}

	res := r.Redact("customer email jane@corp.com")
	if strings.Contains(res.Text, "jane@corp.com") || !strings.Contains(res.Text, "[PII:email]") {
		t.Errorf("fallback must still redact via the pattern engine: %q", res.Text)
	}
	if len(logged) != 1 {
		t.Errorf("expected exactly one degradation log, got %d", len(logged))
	}
	// Rate limit: an immediate second failure does not log again.
	_ = r.Redact("another email ops@corp.com")
	if len(logged) != 1 {
		t.Errorf("degradation log should be rate-limited, got %d lines", len(logged))
	}
}

func TestRampartFallbackOnGarbageResponse(t *testing.T) {
	// Findings but no redacted text = unusable in redact mode → fallback.
	srv := rampartStub(t, 200, `{"text": "", "findings": [{"kind": "SSN", "count": 1}]}`)
	r := NewRampart(ModeRedact, srv.URL)
	r.logf = func(string, ...any) {}
	res := r.Redact("SSN 123-45-6789")
	if strings.Contains(res.Text, "123-45-6789") {
		t.Errorf("garbage response must not fail open: %q", res.Text)
	}
}

func TestRampartOffAndEmptyPassthrough(t *testing.T) {
	r := NewRampart(ModeOff, "http://127.0.0.1:1")
	if res := r.Redact("email jane@corp.com"); res.Text != "email jane@corp.com" {
		t.Error("off mode must be byte-for-byte")
	}
	r2 := NewRampart(ModeRedact, "http://127.0.0.1:1")
	if res := r2.Redact(""); res.Text != "" || res.Found() {
		t.Error("empty text must not hit the service")
	}
}

// TestRampartProbeSurfacesErrors: the admin Test button must report a dead
// service as an error, not silently fall back.
func TestRampartProbeSurfacesErrors(t *testing.T) {
	r := NewRampart(ModeRedact, "http://127.0.0.1:1") // nothing listening
	if _, err := r.ProbeService(context.Background(), "probe"); err == nil {
		t.Fatal("probe must surface connectivity errors")
	}

	srv := rampartStub(t, 200, rampartOKBody)
	ok := NewRampart(ModeRedact, srv.URL)
	res, err := ok.ProbeService(context.Background(), rampartInput)
	if err != nil || !res.Found() {
		t.Fatalf("probe against a live service: res=%+v err=%v", res, err)
	}
}

// TestRampartSecondPassSweep: a structured shape the model misses (observed
// live: a formatted phone number) is still caught by the deterministic sweep
// over Rampart's output — the rampart engine is a strict superset of the
// pattern floor, in every mode.
func TestRampartSecondPassSweep(t *testing.T) {
	// The service redacted the name but MISSED the phone number.
	srv := rampartStub(t, 200, `{
		"text": "Call [GIVEN_NAME_1] at (415) 555-0134",
		"findings": [{"kind": "GIVEN_NAME", "count": 1}]
	}`)

	red := NewRampart(ModeRedact, srv.URL).Redact("Call Alex at (415) 555-0134")
	if strings.Contains(red.Text, "555-0134") || !strings.Contains(red.Text, "[PII:phone]") {
		t.Errorf("sweep must catch the missed phone: %q", red.Text)
	}
	if !strings.Contains(red.Summary(), "name×1") || !strings.Contains(red.Summary(), "phone×1") {
		t.Errorf("findings must merge: %q", red.Summary())
	}

	// Observe: findings union, text untouched.
	obs := NewRampart(ModeObserve, srv.URL).Redact("Call Alex at (415) 555-0134")
	if obs.Text != "Call Alex at (415) 555-0134" || !strings.Contains(obs.Summary(), "phone×1") {
		t.Errorf("observe union: %+v", obs)
	}

	// Block: blocks even when ONLY the pattern pass finds something.
	srvClean := rampartStub(t, 200, `{"text": "Call me at (415) 555-0134", "findings": []}`)
	blk := NewRampart(ModeBlock, srvClean.URL).Redact("Call me at (415) 555-0134")
	if !blk.Blocked {
		t.Errorf("block must trigger on pattern-only findings: %+v", blk)
	}
}
