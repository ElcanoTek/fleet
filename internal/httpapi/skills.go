package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// Skills, phase 1 of first-class skills (#513): browse the bundle's skill
// roster (GET /skills) and invoke a skill explicitly by starting a message
// with "/<skill-name>". Invocation is deterministic and cheap — the first
// token of the message's first line is compared EXACTLY against the bundle
// skill names (the same clientconfig read the prompt builder uses; no cache,
// no fuzzy matching, no model call). A matched invocation appends a block to
// the persisted user message telling the agent to read that skill's SKILL.md
// now, so the transcript itself records which skill was invoked. An unknown
// "/token" is silently ignored — a leading slash is common in normal text
// (paths like /etc/hosts), so only exact skill-name matches trigger.
//
// Authoring, save-from-run, and project scoping are phases 2/3 (see
// docs/SKILLS.md); this file is read-only over the operator-owned bundle.

// skillEntry is one skill as GET /skills returns it: the Level-1 metadata
// (name + description) the composer autocomplete renders. The SKILL.md body
// stays on disk for the agent to read on demand — it is not an API payload.
type skillEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type skillsResponse struct {
	Skills []skillEntry `json:"skills"`
}

// bundleSkills returns the client bundle's well-formed skills (nil-safe on a
// server booted without a bundle). Bundle.Skills re-reads from disk, matching
// the persona/protocol live-reload contract — an operator editing a skill in
// place is picked up without a restart.
func (s *Server) bundleSkills() []clientconfig.Skill {
	if s.clientConfig == nil {
		return nil
	}
	return s.clientConfig.Skills()
}

// listSkills serves GET /skills: the bundle's skill roster for the composer
// "/" autocomplete. Member-gated like /personas — the roster is deployment
// content, not per-user state.
func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	skills := s.bundleSkills()
	entries := make([]skillEntry, 0, len(skills))
	for _, sk := range skills {
		entries = append(entries, skillEntry{Name: sk.Name, Description: sk.Description})
	}
	writeJSON(w, skillsResponse{Skills: entries})
}

// matchSkillInvocation detects an explicit "/<skill-name>" invocation at the
// very start of message and returns the block to append plus the matched
// skill name; both are empty when nothing matches. The rule is strict on
// purpose:
//
//   - The "/" must be the message's FIRST byte (a slash mid-text never
//     triggers).
//   - The token runs to the first whitespace or the end of the message, so
//     "/skill-name trailing args" matches while "/etc/hosts" does not (its
//     token is "etc/hosts", never a skill name — names are lowercase letters,
//     digits, and hyphens).
//   - The comparison is EXACT and case-sensitive; there is no prefix or fuzzy
//     matching, so an unknown token gets no block and no error.
func matchSkillInvocation(message string, skills []clientconfig.Skill) (block, matchedName string) {
	if len(skills) == 0 || !strings.HasPrefix(message, "/") {
		return "", ""
	}
	token := message[1:]
	if i := strings.IndexFunc(token, unicode.IsSpace); i >= 0 {
		token = token[:i]
	}
	if token == "" {
		return "", ""
	}
	for _, sk := range skills {
		if sk.Name != token {
			continue
		}
		return fmt.Sprintf(
			"\n\n[Skill invoked: %[1]s]\nThe user explicitly invoked the skill %[1]q. "+
				"Read `skills/%[1]s/SKILL.md` now and follow its instructions for this request; "+
				"it may bundle scripts under `skills/%[1]s/` you run via bash/run_python.",
			sk.Name,
		), sk.Name
	}
	return "", ""
}

// applySkillInvocation appends the explicit-invocation block to userMessage
// when rawMessage (the message as the user typed it, before any server-side
// appends) starts with "/<skill-name>" for a bundle skill. A method so
// postChat stays a single statement, mirroring applyContextHandles /
// applyConnectorRecommendations.
func (s *Server) applySkillInvocation(userMessage, rawMessage string) string {
	block, _ := matchSkillInvocation(rawMessage, s.bundleSkills())
	return userMessage + block
}
