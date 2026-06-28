package agent

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
)

// SuggestTitle generates a short, sidebar-friendly title for a freshly-created
// conversation by summarizing the first exchange against the operator-configured
// titling model (config.TitleModel — a fast, cheap model). Returns "" on any
// failure; callers treat that as "keep the current title".
func (m *Manager) SuggestTitle(ctx context.Context, userMessage, assistantReply string) string {
	if strings.TrimSpace(assistantReply) == "" {
		return ""
	}
	titleModel := m.config.TitleModel
	model, err := m.resolver.Resolve(ctx, titleModel)
	if err != nil {
		log.Printf("SuggestTitle: resolve title model %q: %v", titleModel, err)
		return ""
	}
	sys := "You write concise chat titles for a sidebar. Given a short exchange between a user and an assistant, " +
		"output ONLY a natural 4-6 word title phrase. Prefer compact noun phrases over imperative sentences. " +
		"Do not copy the user's request verbatim. Omit filler like 'help', 'please', 'find', 'show me', 'latest', or 'email from' unless essential to meaning. " +
		"No quotes, no trailing punctuation, no prefix like 'Title:'."

	ag := fantasy.NewAgent(model,
		fantasy.WithSystemPrompt(sys),
		fantasy.WithTemperature(0.2),
		fantasy.WithMaxOutputTokens(titleMaxOutputTokens),
	)

	prompt := fmt.Sprintf("User: %s\n\nAssistant: %s", truncate(userMessage, 600), truncate(assistantReply, 600))

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	maxTokens := int64(titleMaxOutputTokens)
	result, err := ag.Generate(ctx, fantasy.AgentCall{
		Messages:        []fantasy.Message{fantasy.NewUserMessage(prompt)},
		MaxOutputTokens: &maxTokens,
	})
	if err != nil {
		log.Printf("SuggestTitle: %v", err)
		return ""
	}

	var out strings.Builder
	for _, c := range result.Response.Content {
		if tc, ok := c.(fantasy.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	return normalizeTitle(out.String())
}

const titleMaxOutputTokens = 512
const maxTitleLen = 60

var (
	thinkTagRegex       = regexp.MustCompile(`(?s)<think>.*?</think>`)
	orphanThinkTagRegex = regexp.MustCompile(`</?think>`)
)

// heuristicFillerPrefix strips conversational lead-ins so the instant title
// (#302) reads as a noun phrase, not a verbatim request.
var heuristicFillerPrefix = regexp.MustCompile(`(?i)^(can you|could you|would you|please|help me|i need to|i need|i want to|i want|i'd like to|i'd like|what is|what are|what's|how (do|can|should|to) (i|you|we)?|how to|tell me about|tell me|give me|show me|let's|lets)\s+`)

// HeuristicTitle derives a sidebar title from the first user message with ZERO
// I/O (#302), so a new conversation shows a real name within milliseconds
// instead of a placeholder while the LLM titler runs. Strips a leading filler
// phrase, title-cases the remainder, and truncates to ~50 chars on a word
// boundary. Returns "New conversation" when nothing meaningful remains. The
// async LLM titler may later upgrade this (unless the user has locked the title).
func HeuristicTitle(msg string) string {
	s := strings.TrimSpace(msg)
	// Strip stacked lead-ins ("can you help me ..." → "..."), bounded so a
	// pathological all-filler message can't loop forever.
	for i := 0; i < 4; i++ {
		stripped := heuristicFillerPrefix.ReplaceAllString(s, "")
		if stripped == s {
			break
		}
		s = strings.TrimSpace(stripped)
	}
	// Single-line, collapse internal whitespace.
	s = strings.Join(strings.Fields(s), " ")
	s = truncateTitle(s, 50)
	s = strings.TrimRight(s, " .,;:!?-")
	if utf8.RuneCountInString(s) < 3 {
		return "New conversation"
	}
	return titleCase(s)
}

// titleCase upper-cases the first letter of each word, leaving the rest as-is
// (so acronyms like "Go", "API", "SQL" and mixed-case tokens survive). Small
// connector words stay lowercase unless they lead.
func titleCase(s string) string {
	small := map[string]bool{
		"a": true, "an": true, "the": true, "and": true, "or": true, "but": true,
		"of": true, "to": true, "in": true, "on": true, "for": true, "with": true,
		"at": true, "by": true, "from": true, "as": true, "is": true,
	}
	words := strings.Fields(s)
	for i, w := range words {
		lower := strings.ToLower(w)
		if i > 0 && small[lower] {
			words[i] = lower
			continue
		}
		r := []rune(w)
		r[0] = []rune(strings.ToUpper(string(r[0])))[0]
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

// normalizeTitle cleans a raw model response into a sidebar title.
func normalizeTitle(raw string) string {
	title := thinkTagRegex.ReplaceAllString(raw, "")
	title = orphanThinkTagRegex.ReplaceAllString(title, "")
	title = strings.TrimSpace(title)
	title = strings.Trim(title, "\"'.,;:")
	title = strings.Join(strings.Fields(title), " ")
	title = truncateTitle(title, maxTitleLen)
	return strings.Trim(title, "\"'.,;:!?")
}

// truncateTitle bounds s to at most maxBytes on a word boundary, rune-safe.
func truncateTitle(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		return cut[:i]
	}
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
