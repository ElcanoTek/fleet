package httpapi

import (
	"context"
	"net/http"
)

type GuardrailProbeResult struct {
	OK        bool    `json:"ok"`
	Mode      string  `json:"mode"`
	Profile   string  `json:"profile"`
	Flagged   bool    `json:"flagged"`
	Score     float64 `json:"score,omitempty"`
	Detail    string  `json:"detail,omitempty"`
	LatencyMS int64   `json:"latency_ms"`
}

func WithGuardrailProbe(fn func(context.Context) GuardrailProbeResult) Option {
	return func(s *Server) { s.guardrailProbe = fn }
}

func (s *Server) handleAdminGuardrailProbe(w http.ResponseWriter, r *http.Request) {
	if s.guardrailProbe == nil {
		http.Error(w, "guardrail probe unavailable", http.StatusNotImplemented)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.guardrailProbe(r.Context()))
}
