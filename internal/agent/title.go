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
