package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// memoryGraphSchema is the draft-07 JSON Schema the knowledge-graph extractor
// (#523) constrains the model to: the entities a memory mentions (typed from
// the closed set) and the (subject, predicate, object) triples between them.
// `object` is either the NAME of an entity listed under entities[] (an
// entity→entity edge) or a literal value (an attribute) — the store resolves
// which by name match. additionalProperties:false + maxItems bound the output.
const memoryGraphSchema = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "additionalProperties": false,
  "required": ["entities", "relations"],
  "properties": {
    "entities": {
      "type": "array",
      "maxItems": 20,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name"],
        "properties": {
          "name": { "type": "string", "maxLength": 120 },
          "type": {
            "type": "string",
            "enum": ["person", "organization", "place", "project", "tool", "topic", "other"]
          }
        }
      }
    },
    "relations": {
      "type": "array",
      "maxItems": 30,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["subject", "predicate", "object"],
        "properties": {
          "subject": { "type": "string", "maxLength": 120 },
          "predicate": { "type": "string", "maxLength": 64 },
          "object": { "type": "string", "maxLength": 300 }
        }
      }
    }
  }
}`

const (
	memoryGraphTimeout   = 25 * time.Second
	memoryGraphMaxTokens = 1024
	// Memories are capped at 4000 runes; this just guards the prompt anyway.
	memoryGraphMaxContentChars = 4000
)

// ExtractedGraph is one memory's extracted graph fragment: the entities it
// mentions and the triples between them. Mirrors store.GraphExtraction — the
// HTTP layer maps between the two so the store package never imports agent.
type ExtractedGraph struct {
	Entities  []ExtractedGraphEntity
	Relations []ExtractedGraphRelation
}

// ExtractedGraphEntity is one named entity (Type from the closed set; the
// store normalizes unknown values to "other").
type ExtractedGraphEntity struct {
	Name string
	Type string
}

// ExtractedGraphRelation is one (subject, predicate, object) triple by name.
// Object is either an entity name from the same extraction or a literal value.
type ExtractedGraphRelation struct {
	Subject   string
	Predicate string
	Object    string
}

// ExtractMemoryGraph mines ONE memory's content for its knowledge-graph
// fragment (#523): typed entities plus (subject, predicate, object) triples.
// It mirrors SuggestTitle / AnalyzeTaskFailure — a short-lived
// fantasy.NewAgent call through the SAME host-side resolver against the cheap
// config.MemoryGraphModel, temperature 0, hard timeout, structured-output
// validation. Like AnalyzeTaskFailure it returns an error — never a partial or
// unvalidated result — on resolve/generate/validation failure; the caller
// logs it and stores nothing (the graph is best-effort derived data).
func (m *Manager) ExtractMemoryGraph(ctx context.Context, content string) (*ExtractedGraph, error) {
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("memory-graph: empty content")
	}
	modelSlug := m.config.MemoryGraphModel
	model, err := m.resolver.Resolve(ctx, modelSlug)
	if err != nil {
		return nil, fmt.Errorf("resolve memory-graph model %q: %w", modelSlug, err)
	}

	sys := "You convert ONE remembered fact about a user into knowledge-graph triples. " +
		"List the distinct real-world entities the fact mentions (people, organizations, places, projects, tools, topics) with the best-fitting type, " +
		"then express the fact as (subject, predicate, object) relations. " +
		"subject must be the name of a listed entity. predicate is a short lowercase verb phrase like \"works at\", \"prefers\", \"is based in\". " +
		"object is either the name of another listed entity, or a literal value (a date, a version, a setting) when the target is not an entity. " +
		"Extract only what the fact actually states — no inference, no world knowledge. A fact with no clear entities yields empty lists." +
		structuredoutput.PromptAugmentation(json.RawMessage(memoryGraphSchema))

	ag := fantasy.NewAgent(model,
		fantasy.WithSystemPrompt(sys),
		fantasy.WithTemperature(0),
		fantasy.WithMaxOutputTokens(memoryGraphMaxTokens),
	)

	ctx, cancel := context.WithTimeout(ctx, memoryGraphTimeout)
	defer cancel()

	maxTokens := int64(memoryGraphMaxTokens)
	result, err := ag.Generate(ctx, fantasy.AgentCall{
		Messages:        []fantasy.Message{fantasy.NewUserMessage("FACT:\n" + truncate(content, memoryGraphMaxContentChars))},
		MaxOutputTokens: &maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("memory-graph generate: %w", err)
	}

	var out strings.Builder
	for _, c := range result.Response.Content {
		if tc, ok := c.(fantasy.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	validated, err := structuredoutput.ValidateOutput(out.String(), json.RawMessage(memoryGraphSchema))
	if err != nil {
		// Do NOT echo the model output (derived from memory content) — the bare
		// failure is enough to diagnose (same posture as ExtractMemories).
		return nil, fmt.Errorf("memory-graph output failed schema validation")
	}
	return parseExtractedGraph(validated)
}

// parseExtractedGraph maps schema-validated JSON onto ExtractedGraph, dropping
// blank names/triples (the schema bounds shape, not whitespace).
func parseExtractedGraph(validated json.RawMessage) (*ExtractedGraph, error) {
	var parsed struct {
		Entities []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"entities"`
		Relations []struct {
			Subject   string `json:"subject"`
			Predicate string `json:"predicate"`
			Object    string `json:"object"`
		} `json:"relations"`
	}
	if err := json.Unmarshal(validated, &parsed); err != nil {
		return nil, fmt.Errorf("memory-graph parse: %w", err)
	}
	g := &ExtractedGraph{}
	for _, e := range parsed.Entities {
		if strings.TrimSpace(e.Name) == "" {
			continue
		}
		g.Entities = append(g.Entities, ExtractedGraphEntity{Name: e.Name, Type: e.Type})
	}
	for _, r := range parsed.Relations {
		if strings.TrimSpace(r.Subject) == "" || strings.TrimSpace(r.Predicate) == "" || strings.TrimSpace(r.Object) == "" {
			continue
		}
		g.Relations = append(g.Relations, ExtractedGraphRelation{Subject: r.Subject, Predicate: r.Predicate, Object: r.Object})
	}
	return g, nil
}
