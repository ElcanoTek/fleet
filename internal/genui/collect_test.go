package genui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCollectInitialSubmit(t *testing.T) {
	t.Run("eligible prefilled card with sole submit button", func(t *testing.T) {
		spec := `{
			"title": "Clone Deal",
			"components": [
				{"type": "text_input", "id": "deal_name", "label": "Deal name", "required": true, "value": "Q4 PMP"},
				{"type": "number", "id": "floor_cpm", "label": "Floor", "required": true, "min": 0.5, "value": 2.25},
				{"type": "text_input", "id": "hidden_note", "label": "Note", "required": true, "visible_if": "floor_cpm > 100"}
			],
			"actions": [
				{"id": "submit_clone", "label": "Clone deal"},
				{"id": "copy_json", "label": "Copy", "kind": "copy", "message": "{{deal_name}}"}
			]
		}`
		actID, valsJSON, ok := CollectInitialSubmit([]byte(spec))
		if !ok {
			t.Fatal("expected eligible = true")
		}
		if actID != "submit_clone" {
			t.Fatalf("actionID = %q, want submit_clone", actID)
		}
		var got map[string]any
		if err := json.Unmarshal(valsJSON, &got); err != nil {
			t.Fatalf("unmarshal values: %v", err)
		}
		if got["deal_name"] != "Q4 PMP" || got["floor_cpm"] != 2.25 {
			t.Fatalf("values = %v", got)
		}
		if _, exists := got["hidden_note"]; exists {
			t.Fatalf("hidden_note should not be collected when visible_if is false: %v", got)
		}
	})

	t.Run("eligible when single primary submit coexists with secondary submit", func(t *testing.T) {
		spec := `{
			"title": "Publish Page",
			"components": [
				{"type": "select", "id": "env", "label": "Env", "options": ["staging", "prod"], "value": "staging", "required": true}
			],
			"actions": [
				{"id": "publish", "label": "Publish", "style": "primary"},
				{"id": "save_draft", "label": "Save draft", "style": "secondary"}
			]
		}`
		actID, _, ok := CollectInitialSubmit([]byte(spec))
		if !ok || actID != "publish" {
			t.Fatalf("got (%q, %v), want (publish, true)", actID, ok)
		}
	})

	ineligibleCases := []struct {
		name string
		spec string
	}{
		{
			name: "auto_submit explicitly false",
			spec: `{
				"title": "Form",
				"auto_submit": false,
				"components": [{"type": "text_input", "id": "x", "label": "X", "value": "ok"}],
				"actions": [{"id": "go", "label": "Go"}]
			}`,
		},
		{
			name: "dashboard panel set",
			spec: `{
				"title": "Metrics Panel",
				"panel": "kpis",
				"components": [{"type": "text_input", "id": "x", "label": "X", "value": "ok"}],
				"actions": [{"id": "go", "label": "Go"}]
			}`,
		},
		{
			name: "field_errors non-empty",
			spec: `{
				"title": "Retry Form",
				"field_errors": [{"field": "x", "message": "Deal ID already exists"}],
				"components": [{"type": "text_input", "id": "x", "label": "X", "value": "ok"}],
				"actions": [{"id": "go", "label": "Go"}]
			}`,
		},
		{
			name: "password input present",
			spec: `{
				"title": "Credentials",
				"components": [
					{"type": "text_input", "id": "api_key", "label": "API Key", "format": "password", "value": "secret"}
				],
				"actions": [{"id": "go", "label": "Save"}]
			}`,
		},
		{
			name: "password input nested in repeater",
			spec: `{
				"title": "Nested Credentials",
				"components": [{
					"type": "repeater",
					"id": "seats",
					"label": "Seats",
					"fields": [{"type": "text_input", "id": "pw", "label": "PW", "format": "password", "value": "abc"}]
				}],
				"actions": [{"id": "go", "label": "Save"}]
			}`,
		},
		{
			name: "missing visible required field",
			spec: `{
				"title": "Incomplete Form",
				"components": [
					{"type": "text_input", "id": "buyer", "label": "Buyer", "required": true}
				],
				"actions": [{"id": "go", "label": "Go"}]
			}`,
		},
		{
			name: "invalid email format on prefilled field",
			spec: `{
				"title": "Bad Email",
				"components": [
					{"type": "text_input", "id": "email", "label": "Email", "format": "email", "value": "not-an-email"}
				],
				"actions": [{"id": "go", "label": "Go"}]
			}`,
		},
		{
			name: "submit button has confirm prompt",
			spec: `{
				"title": "Destructive Confirm",
				"components": [{"type": "text_input", "id": "x", "label": "X", "value": "ok"}],
				"actions": [{"id": "go", "label": "Delete", "confirm": "Are you sure?"}]
			}`,
		},
		{
			name: "submit button has validate false",
			spec: `{
				"title": "Skip Validation",
				"components": [{"type": "text_input", "id": "x", "label": "X", "value": "ok"}],
				"actions": [{"id": "go", "label": "Save rough draft", "validate": false}]
			}`,
		},
		{
			name: "submit button disabled by disabled_if",
			spec: `{
				"title": "Disabled Submit",
				"components": [{"type": "toggle", "id": "ready", "label": "Ready", "value": false}],
				"actions": [{"id": "go", "label": "Go", "disabled_if": "!ready"}]
			}`,
		},
		{
			name: "ambiguous multiple primary submit buttons",
			spec: `{
				"title": "Choose Path",
				"components": [{"type": "text_input", "id": "x", "label": "X", "value": "ok"}],
				"actions": [
					{"id": "opt_a", "label": "Approve A"},
					{"id": "opt_b", "label": "Approve B"}
				]
			}`,
		},
		{
			name: "ambiguous two secondary submit buttons",
			spec: `{
				"title": "Choose Secondary",
				"components": [{"type": "text_input", "id": "x", "label": "X", "value": "ok"}],
				"actions": [
					{"id": "opt_a", "label": "Option A", "style": "secondary"},
					{"id": "opt_b", "label": "Option B", "style": "secondary"}
				]
			}`,
		},
		{
			name: "danger-styled submit button is never auto-submitted",
			spec: `{
				"title": "Danger Action",
				"components": [{"type": "text_input", "id": "x", "label": "X", "value": "ok"}],
				"actions": [{"id": "nuke", "label": "Delete Deal", "style": "danger"}]
			}`,
		},
		{
			name: "zero-input confirmation card is ineligible",
			spec: `{
				"title": "Confirm Action",
				"components": [{"type": "text", "content": "Proceed with deployment?"}],
				"actions": [{"id": "approve", "label": "Approve"}]
			}`,
		},
		{
			name: "all inputs hidden via visible_if is ineligible",
			spec: `{
				"title": "Hidden Inputs Only",
				"components": [{"type": "text_input", "id": "x", "label": "X", "value": "ok", "visible_if": "false"}],
				"actions": [{"id": "go", "label": "Go"}]
			}`,
		},
		{
			name: "all inputs disabled is ineligible",
			spec: `{
				"title": "Disabled Inputs Only",
				"components": [{"type": "text_input", "id": "x", "label": "X", "value": "ok", "disabled": true}],
				"actions": [{"id": "go", "label": "Go"}]
			}`,
		},
		{
			name: "url input with invalid port > 65535",
			spec: `{
				"title": "Bad Port URL",
				"components": [{"type": "text_input", "id": "u", "label": "URL", "format": "url", "value": "https://example.com:70000/a"}],
				"actions": [{"id": "go", "label": "Go"}]
			}`,
		},
		{
			name: "url input with port 0",
			spec: `{
				"title": "Zero Port URL",
				"components": [{"type": "text_input", "id": "u", "label": "URL", "format": "url", "value": "https://example.com:0/a"}],
				"actions": [{"id": "go", "label": "Go"}]
			}`,
		},
		{
			name: "url input with angle brackets in host",
			spec: `{
				"title": "Bad Host URL",
				"components": [{"type": "text_input", "id": "u", "label": "URL", "format": "url", "value": "https://exa<mple>.com/a"}],
				"actions": [{"id": "go", "label": "Go"}]
			}`,
		},
	}

	for _, tc := range ineligibleCases {
		t.Run(tc.name, func(t *testing.T) {
			if actID, _, ok := CollectInitialSubmit([]byte(tc.spec)); ok {
				t.Fatalf("expected eligible = false, got eligible = true (actionID=%q)", actID)
			}
		})
	}

	t.Run("oversize submission exceeding MaxSubmissionBytes is ineligible", func(t *testing.T) {
		lines := make([]string, 4000)
		for i := range lines {
			lines[i] = fmt.Sprintf(`"item-%05d-%s"`, i, strings.Repeat("a", 80))
		}
		spec := fmt.Sprintf(`{
			"title": "Huge List",
			"components": [
				{"type": "list_input", "id": "domains", "label": "Domains", "value": [%s]}
			],
			"actions": [{"id": "go", "label": "Submit"}]
		}`, strings.Join(lines, ","))
		if _, _, ok := CollectInitialSubmit([]byte(spec)); ok {
			t.Fatal("expected oversize submission to be ineligible")
		}
	})

	t.Run("valid IPv6 URL and JS number string coercion parity", func(t *testing.T) {
		spec := `{
			"title": "Endpoint Config",
			"components": [
				{"type": "text_input", "id": "u", "label": "URL", "format": "url", "required": true, "value": "https://[::1]:8443/v1"}
			],
			"actions": [{"id": "save", "label": "Save"}]
		}`
		if actID, _, ok := CollectInitialSubmit([]byte(spec)); !ok || actID != "save" {
			t.Fatalf("expected valid IPv6 URL card eligible, got (%q, %v)", actID, ok)
		}

		// Go-only underscores ("1_000") and hex floats ("0x1p4") must evaluate to NaN/null like JS Number().
		for _, badNum := range []string{"1_000", "0x1p4", "-0x10", "Infinity", "inf", "NaN"} {
			got := EvaluateSafe("n - 1", map[string]any{"n": badNum}, nil)
			if got != nil {
				t.Fatalf("EvaluateSafe(n - 1) for %q = %v, want nil (NaN parity with JS)", badNum, got)
			}
		}
		// Previously strconv.ParseFloat treated "1_000" as 1000 (making "1_000" > 500 true!).
		if cmp := EvaluateSafe("n > 500", map[string]any{"n": "1_000"}, true); cmp != false {
			t.Fatalf("EvaluateSafe(\"1_000\" > 500) = %v, want false", cmp)
		}
		if got := EvaluateSafe("n - 1", map[string]any{"n": "0x10"}, nil); got != float64(15) {
			t.Fatalf("EvaluateSafe(0x10 - 1) = %v, want 15", got)
		}
		if got := EvaluateSafe("n - 1", map[string]any{"n": "1.5e2"}, nil); got != float64(149) {
			t.Fatalf("EvaluateSafe(1.5e2 - 1) = %v, want 149", got)
		}
	})
}
