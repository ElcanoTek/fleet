package agent

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// memoryExtractMaxFacts caps how many durable facts one turn may yield, bounding
// both the model's output and the number of proposals a turn can spawn. Keep it
// in sync with the schema's "maxItems" below.
const memoryExtractMaxFacts = 5

// memoryExtractionSchema is the draft-07 JSON Schema the memory auto-indexer
// (#234) constrains the extraction model to. Each fact is an object (#515):
// content (the fact), an optional kind from the closed memory-type set, and an
// optional `replaces` — the 1-based NUMBER of a KNOWN memory this new fact
// directly contradicts/outdates (a contradiction CANDIDATE; a human confirms
// before anything is retired). additionalProperties:false keeps the model from
// padding the objects; maxItems bounds the batch.
const memoryExtractionSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["facts"],
  "properties": {
    "facts": {
      "type": "array",
      "maxItems": 5,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["content"],
        "properties": {
          "content": { "type": "string" },
          "kind": {
            "type": "string",
            "enum": ["fact", "preference", "identity", "constraint", "context"]
          },
          "replaces": { "type": "integer", "minimum": 1 }
        }
      }
    }
  }
}`

// ExtractedFact is one durable fact mined from a completed exchange. Replaces,
// when non-zero, is the 1-based index into the caller's `known` list of the
// memory this fact claims to supersede — the caller maps it to a STABLE memory
// id immediately (a positional index must never outlive the snapshot it
// indexes into).
type ExtractedFact struct {
	Content  string
	Kind     string
	Replaces int
}

// ExtractMemories mines one completed exchange for DURABLE, reusable facts worth
// remembering across future conversations (#234) — stable preferences,
// environment/config facts, standing instructions. It is the read-only
// extraction half; the caller decides what to do with the facts (fleet surfaces
// them as memory PROPOSALS the user confirms). `known` is the user's existing
// ACTIVE memories in a stable order: the model must not re-propose them, and it
// may flag that a new fact directly contradicts/outdates known memory N via
// `replaces: N` (#515 stage 2 — a candidate, never an automatic retirement).
//
// It mirrors SuggestTitle: a short-lived fantasy.NewAgent call through the
// host-side resolver against the cheap config.MemoryModel, with a tight prompt,
// low temperature, a hard timeout, and structured-output validation. It is
// best-effort — any failure (resolve, generate, non-conforming JSON) returns nil
// and never affects the turn. An out-of-range `replaces` drops the CLAIM, not
// the fact.
func (m *Manager) ExtractMemories(ctx context.Context, userMessage, assistantReply string, known []string) []ExtractedFact {
	if strings.TrimSpace(userMessage) == "" {
		return nil
	}
	model, err := m.resolver.Resolve(ctx, m.config.MemoryModel)
	if err != nil {
		log.Printf("ExtractMemories: resolve memory model %q: %v", m.config.MemoryModel, err)
		return nil
	}

	sys := "You extract DURABLE, REUSABLE facts from a chat exchange — things worth remembering about the USER or their environment across FUTURE, unrelated conversations. " +
		"Good: stable preferences, standing instructions, and environment/config facts (e.g. \"uses ruff for Python linting\", \"prod database host is db.prod.internal\", \"always wants staged commits before pushing\"). " +
		"Do NOT extract: one-off task details, questions, ephemeral or time-bound facts, restatements of the assistant's reply, or anything already in the KNOWN list. " +
		"Each fact must be a single short third-person declarative sentence, self-contained (no pronouns referring to this chat). " +
		"Set `kind` when it clearly fits: preference | identity | constraint | context (default is fact). " +
		"If — and ONLY if — a new fact DIRECTLY contradicts or outdates one numbered KNOWN memory (same subject, incompatible claims, the exchange shows the new one is current), set `replaces` to that memory's number; the user will review it. Related-but-compatible facts are NOT contradictions. " +
		"When in doubt, omit — an empty list is the correct answer for most turns. " +
		structuredoutput.PromptAugmentation(json.RawMessage(memoryExtractionSchema))

	var b strings.Builder
	if len(known) > 0 {
		b.WriteString("KNOWN (do not repeat; reference by number in `replaces` ONLY for a direct contradiction):\n")
		for i, k := range known {
			if s := strings.TrimSpace(k); s != "" {
				b.WriteString(itoa(i+1) + ". ")
				b.WriteString(truncate(s, 200))
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("EXCHANGE:\nUser: ")
	b.WriteString(truncate(userMessage, 4000))
	b.WriteString("\n\nAssistant: ")
	b.WriteString(truncate(assistantReply, 4000))

	ag := fantasy.NewAgent(model,
		fantasy.WithSystemPrompt(sys),
		fantasy.WithTemperature(0.1),
		fantasy.WithMaxOutputTokens(768),
	)
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	maxTokens := int64(768)
	result, err := ag.Generate(ctx, fantasy.AgentCall{
		Messages:        []fantasy.Message{fantasy.NewUserMessage(b.String())},
		MaxOutputTokens: &maxTokens,
	})
	if err != nil {
		log.Printf("ExtractMemories: %v", err)
		return nil
	}

	var out strings.Builder
	for _, c := range result.Response.Content {
		if tc, ok := c.(fantasy.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	validated, err := structuredoutput.ValidateOutput(out.String(), json.RawMessage(memoryExtractionSchema))
	if err != nil {
		// Deliberately do NOT log the validation error verbatim: it can echo the
		// model's non-conforming output, which is derived from conversation
		// content. The bare failure is enough to diagnose.
		log.Printf("ExtractMemories: output failed schema validation; skipping")
		return nil
	}
	var parsed struct {
		Facts []struct {
			Content  string `json:"content"`
			Kind     string `json:"kind"`
			Replaces int    `json:"replaces"`
		} `json:"facts"`
	}
	if err := json.Unmarshal(validated, &parsed); err != nil {
		return nil
	}

	facts := make([]ExtractedFact, 0, len(parsed.Facts))
	for _, f := range parsed.Facts {
		content := strings.TrimSpace(f.Content)
		if content == "" {
			continue
		}
		replaces := f.Replaces
		if replaces < 0 || replaces > len(known) {
			// A number pointing outside the snapshot is a hallucinated claim:
			// keep the fact, drop the supersede candidate.
			replaces = 0
		}
		facts = append(facts, ExtractedFact{Content: content, Kind: f.Kind, Replaces: replaces})
		if len(facts) >= memoryExtractMaxFacts {
			break
		}
	}
	return facts
}
