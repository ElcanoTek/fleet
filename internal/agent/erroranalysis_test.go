package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// TestErrorAnalysisSchemaCompiles guards that the fixed taxonomy schema is a
// valid draft-07 schema — if it ever breaks, every diagnosis would fail
// validation and silently produce no analysis.
func TestErrorAnalysisSchemaCompiles(t *testing.T) {
	if err := structuredoutput.ValidateSchema(json.RawMessage(errorAnalysisSchema)); err != nil {
		t.Fatalf("errorAnalysisSchema does not compile: %v", err)
	}
}

func TestErrorAnalysisSchemaValidation(t *testing.T) {
	schema := json.RawMessage(errorAnalysisSchema)

	// A conforming diagnosis (even wrapped in prose/fences — ValidateOutput
	// extracts the JSON) validates and round-trips.
	good := "Here is the analysis:\n```json\n" +
		`{"category":"credentials","summary":"the API key was rejected","remediation":["rotate the key","check the secret name"]}` +
		"\n```"
	out, err := structuredoutput.ValidateOutput(good, schema)
	if err != nil {
		t.Fatalf("conforming output rejected: %v", err)
	}
	var parsed struct {
		Category    string   `json:"category"`
		Summary     string   `json:"summary"`
		Remediation []string `json:"remediation"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("validated output not parseable: %v", err)
	}
	if parsed.Category != "credentials" || parsed.Summary == "" || len(parsed.Remediation) != 2 {
		t.Fatalf("round-tripped analysis wrong: %+v", parsed)
	}

	// An out-of-taxonomy category is rejected.
	if _, err := structuredoutput.ValidateOutput(`{"category":"banana","summary":"x"}`, schema); err == nil {
		t.Error("expected rejection of an out-of-taxonomy category")
	}
	// Missing required `summary` is rejected.
	if _, err := structuredoutput.ValidateOutput(`{"category":"network"}`, schema); err == nil {
		t.Error("expected rejection when required summary is missing")
	}
	// More than 3 remediation steps is rejected (maxItems).
	if _, err := structuredoutput.ValidateOutput(`{"category":"timeout","summary":"x","remediation":["a","b","c","d"]}`, schema); err == nil {
		t.Error("expected rejection of >3 remediation steps")
	}
	// Non-JSON is rejected.
	if _, err := structuredoutput.ValidateOutput("I couldn't analyze this.", schema); err == nil {
		t.Error("expected rejection of non-JSON output")
	}
}

// TestErrorAnalysisSchemaTaxonomy guards the documented category set so a typo in
// the schema (or an accidental enum change) is caught.
func TestErrorAnalysisSchemaTaxonomy(t *testing.T) {
	for _, cat := range []string{
		"configuration", "credentials", "network", "rate_limit", "timeout",
		"tool_error", "model_refusal", "resource_exhausted", "logic_error", "unknown",
	} {
		if !strings.Contains(errorAnalysisSchema, `"`+cat+`"`) {
			t.Errorf("taxonomy category %q missing from errorAnalysisSchema", cat)
		}
	}
}
