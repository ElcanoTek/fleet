package tools

import (
	"context"

	"charm.land/fantasy"
)

// ProposeSkillParams are the typed parameters for the propose_skill tool.
type ProposeSkillParams struct {
	Name        string `json:"name" description:"Skill name: lowercase kebab-case (a-z, 0-9, hyphens), max 64 chars, e.g. deal-sheet-check."`
	Description string `json:"description" description:"ONE line: what the skill does AND when to use it — this is how future runs decide the skill applies."`
	Body        string `json:"body" description:"The full markdown instructions (concrete, imperative steps). Not a diff."`
	Reason      string `json:"reason" description:"Why this workflow is worth saving as a skill."`
}

// NewProposeSkillTool creates the propose_skill native tool (docs/SKILLS.md
// phase 3, "save from run"). Like propose_memory/propose_note the Run body is
// a stub: the call is intercepted by checkSkillProposal before it executes and
// staged through the SkillProposer seam as a PROPOSED user skill the owner
// reviews in Settings → Skills. Registered in BOTH modes.
func NewProposeSkillTool() fantasy.AgentTool {
	description := `Propose saving a new Agent Skill OR staging an edit/customization to an existing skill (` + "`skills/<name>/SKILL.md`" + ` or ` + "`user-skills/<name>/SKILL.md`" + `) for your user.

Use this when:
1. The user asks you to edit, update, customize, or clone any skill they can see or use (first read its current ` + "`user-skills/<name>/SKILL.md`" + ` or ` + "`skills/<name>/SKILL.md`" + `, apply the requested changes to the full markdown body, and call ` + "`propose_skill`" + ` with the skill's name).
2. You've just executed a repeatable, multi-step workflow the user is likely to want again.

The proposal is staged on their **Settings → Skills** page where they can click **Approve** or **Approve & share** (to publish it to the whole workspace) in one click. If the user already has a skill with that ` + "`name`" + `, ` + "`propose_skill`" + ` stages an update to that skill in place.

- name: lowercase-kebab, stable, descriptive (e.g. campaign-optimization-log or weekly-pacing-report).
- description: one line covering what it does and when it applies — future runs match on this.
- body: the complete markdown instructions, written for a future agent with no memory of this conversation.
- Do NOT include secrets, credentials, or transient per-conversation details.`

	return fantasy.NewAgentTool("propose_skill", description,
		func(_ context.Context, params ProposeSkillParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Name == "" || params.Body == "" {
				return fantasy.NewTextErrorResponse("propose_skill requires a non-empty name and body."), nil
			}
			return fantasy.NewTextResponse("Skill proposal received."), nil
		})
}
