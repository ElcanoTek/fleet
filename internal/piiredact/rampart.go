package piiredact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Rampart engine (#450 follow-on, now shipped): the ML half of PII redaction.
// Rampart (huggingface.co/nationaldesignstudio/rampart) is a MiniLM
// token-classification ONNX model with a 35-label BIO head over 17 PII entity
// types — names, contact info, government IDs, financial numbers, and street
// addresses — far beyond what the deterministic PatternRedactor's five regexes
// can see. It runs OUT of process behind a small HTTP service (the operator
// deploys it next to fleet; scripts/rampart-service is the reference
// implementation over the official npm runtime), and this file is the
// host-side client implementing the same Redactor contract.
//
// The service contract is deliberately text-in/text-out — no character-offset
// math across the process boundary, where UTF-8/UTF-16 disagreements breed
// corruption bugs:
//
//	POST <url>  {"text": "..."}
//	200         {"text": "<redacted, e.g. My name is [GIVEN_NAME_1]>",
//	             "findings": [{"kind": "given_name", "count": 1}, ...]}
//
// Rampart's numbered placeholders ([GIVEN_NAME_1], [SSN_1], …) are kept
// verbatim in redact mode: stable numbering lets the model still refer to
// entities distinctly ("email [EMAIL_1], not [EMAIL_2]"), which the flat
// [PII:kind] markers of the pattern engine cannot express.
//
// Failure posture: a service error NEVER fails the tool call and NEVER
// silently disables protection — the call falls back to the deterministic
// PatternRedactor (the same floor a pattern-engine deployment has) and the
// degradation is logged, rate-limited so a dead service can't flood the log.
// Detection quality degrades; the control stays on.
type RampartRedactor struct {
	mode     Mode
	url      string
	client   *http.Client
	fallback *PatternRedactor
	// lastDegradedLog rate-limits the fallback log line (unix seconds).
	lastDegradedLog atomic.Int64
	// logf is the logger seam, overridable in tests to capture output and
	// assert no raw text/PII is ever written.
	logf func(format string, args ...any)
}

// rampartTimeout bounds one detection round-trip. The model is small (~7ms
// p50 on CPU per the model card) — the budget covers cold starts and chunked
// long inputs, not a hung service stalling every tool call.
const rampartTimeout = 5 * time.Second

// rampartDegradedLogInterval spaces out "service down, using pattern
// fallback" log lines.
const rampartDegradedLogInterval = 30 * time.Second

// rampartMaxResponseBytes caps the response read (a redaction of a 64KB tool
// output cannot legitimately be near this).
const rampartMaxResponseBytes = 4 << 20

// NewRampart builds the Rampart-backed redactor for mode against the service
// at url. ModeOff yields a pass-through no-op, mirroring New.
func NewRampart(mode Mode, url string) *RampartRedactor {
	return &RampartRedactor{
		mode:     mode,
		url:      strings.TrimSpace(url),
		client:   &http.Client{Timeout: rampartTimeout},
		fallback: New(mode),
		logf:     log.Printf,
	}
}

// Mode reports the configured strictness.
func (r *RampartRedactor) Mode() Mode { return r.mode }

// Redact runs the configured mode over text via the Rampart service, falling
// back to the deterministic pattern engine when the service is unreachable or
// answers garbage. Off/empty → pass-through, byte-for-byte.
func (r *RampartRedactor) Redact(text string) Result {
	if r == nil || r.mode == ModeOff || text == "" {
		return Result{Text: text}
	}
	res, err := r.detect(context.Background(), text)
	if err != nil {
		now := time.Now().Unix()
		if last := r.lastDegradedLog.Load(); now-last >= int64(rampartDegradedLogInterval/time.Second) &&
			r.lastDegradedLog.CompareAndSwap(last, now) {
			// The error names the failure mode (dial/status/decode), never the text.
			r.logf("piiredact: rampart service degraded, using pattern fallback: %v", err)
		}
		return r.fallback.Redact(text)
	}
	return r.applyMode(text, res)
}

// ProbeService performs one detection round-trip WITHOUT the pattern fallback
// — the admin "Test detection" button's seam, where a connectivity failure
// must surface as a failure, not silently degrade.
func (r *RampartRedactor) ProbeService(ctx context.Context, text string) (Result, error) {
	res, err := r.detect(ctx, text)
	if err != nil {
		return Result{}, err
	}
	return r.applyMode(text, res), nil
}

// rampartResponse is the service's answer.
type rampartResponse struct {
	Text     string `json:"text"`
	Findings []struct {
		Kind  string `json:"kind"`
		Count int    `json:"count"`
	} `json:"findings"`
}

// detect POSTs text to the service and validates the response shape.
func (r *RampartRedactor) detect(ctx context.Context, text string) (*rampartResponse, error) {
	if r.url == "" {
		return nil, fmt.Errorf("no service URL configured")
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rampart request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rampart returned status %d", resp.StatusCode)
	}
	var out rampartResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, rampartMaxResponseBytes)).Decode(&out); err != nil {
		return nil, fmt.Errorf("rampart response decode: %w", err)
	}
	// A response with findings but unchanged/empty text would mean the service
	// detected PII yet returned nothing usable to redact with — treat as
	// garbage rather than passing the original through in redact mode.
	if out.Text == "" && len(out.Findings) > 0 {
		return nil, fmt.Errorf("rampart response missing redacted text")
	}
	return &out, nil
}

// applyMode maps the service response onto the Redactor result contract,
// with the deterministic pattern engine as a SECOND pass over Rampart's
// output — belt and suspenders, not either/or. Observed live: the model can
// miss a structured shape ((415) 555-0134 sailed through) that the regexes
// catch trivially; values Rampart already placeholdered no longer match the
// patterns, so the sweep only ever adds. The rampart engine is therefore a
// strict superset of the pattern engine's floor.
func (r *RampartRedactor) applyMode(original string, res *rampartResponse) Result {
	findings := r.groupFindings(res.Findings)
	switch r.mode {
	case ModeObserve:
		pat := r.fallback.Redact(original) // observe fallback: findings only, text untouched
		findings = mergeFindings(findings, pat.Findings)
		if len(findings) == 0 {
			return Result{Text: original}
		}
		return Result{Text: original, Findings: findings}
	case ModeBlock:
		pat := r.fallback.Redact(original)
		findings = mergeFindings(findings, pat.Findings)
		if len(findings) == 0 {
			return Result{Text: original}
		}
		return Result{
			Text:     fmt.Sprintf("[BLOCKED: content withheld — PII detected (%s)]", summarize(findings)),
			Findings: findings,
			Blocked:  true,
		}
	default: // ModeRedact — the service's placeholder text + the pattern sweep over it.
		pat := r.fallback.Redact(res.Text)
		findings = mergeFindings(findings, pat.Findings)
		if len(findings) == 0 {
			return Result{Text: original}
		}
		return Result{Text: pat.Text, Findings: findings}
	}
}

// mergeFindings sums two finding sets by kind (distinct spans, so counts add).
func mergeFindings(a, b []Finding) []Finding {
	if len(b) == 0 {
		return a
	}
	counts := map[Kind]int{}
	for _, f := range a {
		counts[f.Kind] += f.Count
	}
	for _, f := range b {
		counts[f.Kind] += f.Count
	}
	return findingsFrom(counts)
}

// groupFindings normalizes Rampart's 17 entity labels into fleet's audit
// kinds: name parts fold into "name", street-address components into
// "address", IP_ADDRESS aligns with the pattern engine's "ip"; everything
// else keeps its (lowercased) label. Grouping keeps the audit line readable
// ("name×2, address×1") without losing the distinct high-signal kinds
// (ssn, credit_card, passport, …).
func (r *RampartRedactor) groupFindings(in []struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}) []Finding {
	counts := map[Kind]int{}
	for _, f := range in {
		if f.Count <= 0 {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(f.Kind))
		switch kind {
		case "":
			continue
		case "given_name", "surname":
			kind = "name"
		case "building_number", "street_name", "secondary_address", "city", "state", "zip_code":
			kind = "address"
		case "ip_address":
			kind = string(KindIP)
		}
		counts[Kind(kind)] += f.Count
	}
	out := findingsFrom(counts)
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}
