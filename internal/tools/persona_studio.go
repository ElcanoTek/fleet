package tools

import (
	"context"
	"encoding/json"
	"strings"

	"charm.land/fantasy"
)

// PersonaStudioToolName is the canonical name of the interactive tool that
// helps a user build their own personas and digital twin
// (docs/USER-PERSONAS.md).
const PersonaStudioToolName = "persona_studio"

// Supported actions on persona_studio.
const (
	PersonaStudioActionList    = "list"
	PersonaStudioActionGet     = "get"
	PersonaStudioActionHistory = "history"
	PersonaStudioActionPropose = "propose"
)

// PersonaStudioPatrol is one suggested patrol in a persona_studio proposal.
type PersonaStudioPatrol struct {
	Title    string `json:"title" description:"Short label, e.g. 'Morning pacing sweep'."`
	Cron     string `json:"cron" description:"5-field cron expression, at least 5 minutes between runs (e.g. '0 9 * * 1-5' for 9am on weekdays, '0 */4 * * *' every 4 hours)."`
	Timezone string `json:"timezone,omitempty" description:"IANA timezone for the cron (e.g. 'America/New_York'). Defaults to UTC; ask the user or use one they mentioned."`
	Prompt   string `json:"prompt" description:"What the persona does each time the patrol fires: what to check, thresholds, and how to report."`
}

// PersonaStudioParams is the schema-generating input struct for persona_studio.
type PersonaStudioParams struct {
	Action       string                `json:"action" description:"'list' (the user's personas and the workspace personas they can build on), 'get' (one of the user's personas in full, by name), 'history' (what the user themselves typed in their recent chats — use it to learn their priorities, vocabulary and recurring work when drafting a digital twin), or 'propose' (stage a new persona, or an edit to an existing one, for the user to review and approve)."`
	Name         string                `json:"name,omitempty" description:"Persona name for 'get' and 'propose': lowercase kebab-case, max 48 chars (e.g. 'my-twin', 'deal-desk'). Proposing an existing name stages an edit to it."`
	DisplayName  string                `json:"display_name,omitempty" description:"'propose': the name shown in the persona picker, e.g. 'Brad's Twin'."`
	Description  string                `json:"description,omitempty" description:"'propose': one line saying what the persona is for."`
	Instructions string                `json:"instructions,omitempty" description:"'propose': the persona's standing instructions in markdown — role, priorities, how they decide, voice and formatting, what to watch for, what never to do. Written for a future assistant with no memory of this conversation. No secrets or credentials."`
	BasePersona  string                `json:"base_persona,omitempty" description:"'propose': the workspace persona this builds on (from 'list'); blank = the workspace default. Its expertise and tool access carry over."`
	Kind         string                `json:"kind,omitempty" description:"'propose': 'twin' for a digital twin that thinks and writes as the user would, or 'assistant' for a specialist helper. Default 'assistant'."`
	Autonomy     string                `json:"autonomy,omitempty" description:"'propose': 'advise' (analyze and suggest actions, take none — the default) or 'act' (carry out routine actions on the user's behalf). Only use 'act' if the user asked for it."`
	Patrols      []PersonaStudioPatrol `json:"patrols,omitempty" description:"'propose': up to 8 suggested recurring patrols the user can start from the persona page in one click. Suggestions only — nothing is scheduled until the user starts one."`
	Limit        int                   `json:"limit,omitempty" description:"'history': how many recent conversations to read (default 25, max 60)."`
}

// PersonaStudioHandler executes persona_studio actions for the turn's user.
// Injected onto the turn context by httpapi; it returns the JSON-ready result.
type PersonaStudioHandler func(ctx context.Context, params PersonaStudioParams) (any, error)

type personaStudioHandlerKey struct{}

// WithPersonaStudioHandler attaches a per-turn PersonaStudioHandler to ctx.
func WithPersonaStudioHandler(ctx context.Context, h PersonaStudioHandler) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, personaStudioHandlerKey{}, h)
}

func personaStudioHandlerFromContext(ctx context.Context) PersonaStudioHandler {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(personaStudioHandlerKey{}).(PersonaStudioHandler)
	return h
}

const personaStudioDescription = `Help the user build their own personas — including a digital twin that thinks, prioritizes and writes the way they do and can run their patrols.

Use it when the user asks to create, edit or improve a persona, or to set up "a version of me", "my digital twin", or an assistant that acts or suggests actions on their behalf.

How to draft a good persona:
1. 'list' to see the user's personas and the workspace personas to build on; 'get' one to edit it.
2. For a digital twin, call 'history' to read what the user has typed in recent chats (their own words only), and combine it with their memories and anything they tell you. Ask a few short questions if their role, priorities or decision rules are unclear.
3. 'propose' a draft: concrete instructions (role, goals, decision rules, thresholds, voice, what to flag, what never to do) and, where their work repeats, suggested patrols with a cron cadence and clear instructions.
4. The draft is staged for review — it does nothing until the user approves it on Settings → Personas. Tell them it is waiting there, summarize what you proposed, and mention they can start a suggested patrol from the persona's card after approving.

Never put secrets, credentials or one-off details in a persona.`

// NewPersonaStudioTool constructs the interactive persona_studio tool.
func NewPersonaStudioTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		PersonaStudioToolName,
		personaStudioDescription,
		func(ctx context.Context, params PersonaStudioParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			action := normalizePersonaStudioAction(params.Action)
			if action == "" {
				return fantasy.NewTextErrorResponse("invalid action: must be one of 'list', 'get', 'history', or 'propose'"), nil
			}
			params.Action = action
			h := personaStudioHandlerFromContext(ctx)
			if h == nil {
				return fantasy.NewTextErrorResponse("persona_studio is only available in interactive chat conversations"), nil
			}
			res, err := h(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if out, mErr := json.MarshalIndent(res, "", "  "); mErr == nil {
				return fantasy.NewTextResponse(string(out)), nil
			}
			return fantasy.NewTextErrorResponse("persona_studio: could not encode the result"), nil
		},
	)
}

func normalizePersonaStudioAction(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "list", "ls":
		return PersonaStudioActionList
	case "get", "read", "view", "show":
		return PersonaStudioActionGet
	case "history", "chats", "digest":
		return PersonaStudioActionHistory
	case "propose", "create", "draft", "update", "edit", "save":
		return PersonaStudioActionPropose
	default:
		return ""
	}
}
