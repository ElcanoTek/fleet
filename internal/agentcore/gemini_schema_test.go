package agentcore

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
)

func TestGeminiToolSchema(t *testing.T) {
	str := map[string]any{"type": "string"}
	cases := []struct {
		name   string
		schema map[string]any
		want   map[string]any // nil: unchanged
	}{
		{
			name: "acceptable schema",
			schema: map[string]any{"type": "object", "required": []string{"tags"}, "properties": map[string]any{
				"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"opts": map[string]any{"type": "object"},
			}},
		},
		{
			name:   "array without items",
			schema: map[string]any{"type": "object", "properties": map[string]any{"tags": map[string]any{"type": "array", "description": "d"}}},
			want:   map[string]any{"type": "object", "properties": map[string]any{"tags": map[string]any{"type": "array", "description": "d", "items": str}}},
		},
		{
			name:   "items true",
			schema: map[string]any{"type": "object", "properties": map[string]any{"tags": map[string]any{"type": "array", "items": true}}},
			want:   map[string]any{"type": "object", "properties": map[string]any{"tags": map[string]any{"type": "array", "items": str}}},
		},
		{
			name:   "tuple rewritten to prefixItems",
			schema: map[string]any{"type": "object", "properties": map[string]any{"pair": map[string]any{"type": "array", "prefixItems": []any{str, str}}}},
			want:   map[string]any{"type": "object", "properties": map[string]any{"pair": map[string]any{"type": "array", "prefixItems": []any{str, str}, "items": str}}},
		},
		{
			name: "nested in an object and in an array",
			schema: map[string]any{"type": "object", "properties": map[string]any{
				"filter": map[string]any{"type": "object", "properties": map[string]any{"ids": map[string]any{"type": "array"}}},
				"grid":   map[string]any{"type": "array", "items": map[string]any{"type": "array"}},
			}},
			want: map[string]any{"type": "object", "properties": map[string]any{
				"filter": map[string]any{"type": "object", "properties": map[string]any{"ids": map[string]any{"type": "array", "items": str}}},
				"grid":   map[string]any{"type": "array", "items": map[string]any{"type": "array", "items": str}},
			}},
		},
		{
			name:   "required name with no property",
			schema: map[string]any{"type": "object", "required": []string{"a", "ghost"}, "properties": map[string]any{"a": str}},
			want:   map[string]any{"type": "object", "required": []string{"a"}, "properties": map[string]any{"a": str}},
		},
		{
			name:   "no required name has a property",
			schema: map[string]any{"type": "object", "required": []string{"a"}, "additionalProperties": str},
			want:   map[string]any{"type": "object", "additionalProperties": str},
		},
		{
			// fantasy sends only a []string required list; this one never reaches Gemini.
			name:   "decoded required list",
			schema: map[string]any{"type": "object", "required": []any{"ghost"}, "properties": map[string]any{"a": str}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := copyJSONValue(tc.schema)
			got, changed := geminiToolSchema(tc.schema)
			if !reflect.DeepEqual(tc.schema, before) {
				t.Fatalf("the caller's schema was modified: %#v", tc.schema)
			}
			if tc.want == nil {
				if changed || reflect.ValueOf(got).Pointer() != reflect.ValueOf(tc.schema).Pointer() {
					t.Fatalf("changed = %v, got %#v; want the schema itself", changed, got)
				}
				return
			}
			if !changed || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v (changed %v), want %#v", got, changed, tc.want)
			}
		})
	}
}

// A google entry sends Gemini a tool whose array has no items with string
// items, and only the required names that have a property, on both paths;
// Gemini answers a 400 for either as written. The caller's tool is unchanged.
func TestGoogleToolSchemasAreOnesGeminiAccepts(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		reply := `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP","index":0}],` +
			`"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+reply+"\r\n\r\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()
	p, err := buildProvider(ProviderConfig{Name: "gemini", Type: ProviderTypeGoogle, APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}
	lm, err := p.LanguageModel(context.Background(), "gemini-3.8-flash")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	schema := map[string]any{"type": "object", "required": []string{"tags", "ghost"}, "properties": map[string]any{
		"tags": map[string]any{"type": "array"},
	}}
	before := copyJSONValue(schema)
	call := fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("ping")},
		Tools:  []fantasy.Tool{fantasy.FunctionTool{Name: "mcp_tool", InputSchema: schema}},
	}
	if _, err := lm.Generate(context.Background(), call); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	stream, err := lm.Stream(context.Background(), call)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range stream {
	}
	if !reflect.DeepEqual(schema, before) {
		t.Fatalf("the caller's tool schema was modified: %#v", schema)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	for i, body := range bodies {
		var req struct {
			Tools []struct {
				FunctionDeclarations []struct {
					Parameters struct {
						Required   []string `json:"required"`
						Properties map[string]struct {
							Items *struct {
								Type string `json:"type"`
							} `json:"items"`
						} `json:"properties"`
					} `json:"parameters"`
				} `json:"functionDeclarations"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(body, &req); err != nil || len(req.Tools) == 0 || len(req.Tools[0].FunctionDeclarations) == 0 {
			t.Fatalf("request %d body %s: %v", i, body, err)
		}
		params := req.Tools[0].FunctionDeclarations[0].Parameters
		if items := params.Properties["tags"].Items; items == nil || items.Type != "STRING" {
			t.Errorf("request %d: tags items = %+v, want STRING", i, items)
		}
		if strings.Join(params.Required, ",") != "tags" {
			t.Errorf("request %d: required = %v, want [tags]", i, params.Required)
		}
	}
}
