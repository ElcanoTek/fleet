// Admin PII-redaction probe: the Features panel's "Test detection" button.
// POST /admin/pii-redaction/test runs the CURRENT process-wide redactor —
// exactly what tool calls go through — over a fixed synthetic sample and
// returns the detected kinds, the redacted preview, and latency. For the
// rampart engine a dead detection service reports as a failure (surfacing
// connectivity is the point); tool calls themselves fall back to the pattern
// engine. The sample is synthetic — no operator data is involved.

package httpapi

import (
	"context"
	"net/http"
)

// PIIProbeResult is the probe's admin-facing outcome. Redacted carries the
// synthetic sample post-redaction so the admin sees the marker style the
// model will see ([PII:email] vs [GIVEN_NAME_1]) — never real data.
type PIIProbeResult struct {
	OK        bool   `json:"ok"`
	Engine    string `json:"engine"`
	Mode      string `json:"mode"`
	Detail    string `json:"detail"`
	Redacted  string `json:"redacted,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
}

// WithPIIRedactionProbe injects the probe (wired by cmd/fleet from the PII
// redactor state). Omitted (tests, mock mode), the endpoint answers 501.
func WithPIIRedactionProbe(fn func(ctx context.Context) PIIProbeResult) Option {
	return func(s *Server) { s.piiProbe = fn }
}

// handleAdminPIIProbe serves POST /admin/pii-redaction/test.
func (s *Server) handleAdminPIIProbe(w http.ResponseWriter, r *http.Request) {
	if s.piiProbe == nil {
		http.Error(w, "PII redaction probe unavailable", http.StatusNotImplemented)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.piiProbe(r.Context()))
}
