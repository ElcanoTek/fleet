package tools

import (
	"context"

	"charm.land/fantasy"
)

// ProposeNoteParams are the typed parameters for the propose_note tool.
type ProposeNoteParams struct {
	Slug   string `json:"slug" description:"Target note id; an existing slug edits that note, a new slug proposes a new note. Must match ^[a-z0-9_-]{1,128}$."`
	Title  string `json:"title" description:"Short human-readable title for the note."`
	Body   string `json:"body" description:"Full proposed markdown body (not a diff)."`
	Reason string `json:"reason" description:"Why this change is warranted."`
}

// NewProposeNoteTool creates the propose_note native tool. Like propose_memory,
// the actual staging (DB persistence) happens in the orchestration layer via
// the NoteProposer seam — the call is intercepted by checkNoteProposal before
// this Run body executes — so this body only provides the schema + a clean
// fallback result. It is registered in BOTH modes (unlike propose_memory).
func NewProposeNoteTool() fantasy.AgentTool {
	description := `Propose a new shared note or an edit to an existing one in the admin-curated knowledge base.

Use this when you discover durable, cross-agent information — a corrected rate limit, a new API quirk, a reusable playbook step. The proposal is reviewed by an admin before going live; it does NOT take effect immediately and you must NOT assume your change is applied.

- slug: an existing slug edits that note; a new slug proposes a new note.
- body: the FULL proposed markdown body (not a diff).
- Do NOT put secrets, credentials, or per-conversation/transient details in a note.`

	return fantasy.NewAgentTool("propose_note", description,
		func(_ context.Context, params ProposeNoteParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Slug == "" || params.Body == "" {
				return fantasy.NewTextErrorResponse("propose_note requires a non-empty slug and body."), nil
			}
			return fantasy.NewTextResponse("Note proposal received."), nil
		})
}
