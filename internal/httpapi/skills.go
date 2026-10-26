package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/store"
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
	// Source distinguishes bundle-authored skills from an Agent Plugin's and
	// from fleet's built-in pack ("bundle" | "plugin" | "builtin") so the
	// library UI can badge provenance. Plugin names the source plugin when
	// Source is "plugin" (docs/AGENT-PLUGINS.md).
	Source   string `json:"source"`
	Plugin   string `json:"plugin,omitempty"`
	SharedBy string `json:"shared_by,omitempty"`
	// DeclaredAllowedTools is the skill's frontmatter `allowed-tools` (nil when
	// absent), surfaced for REVIEW/observability — the library UI shows it so an
	// operator can see a skill's declared tool contract. NOT an authorization
	// boundary (see clientconfig.Skill.DeclaredAllowedTools / docs/SKILLS.md).
	DeclaredAllowedTools []string `json:"declared_allowed_tools,omitempty"`
}

type skillsResponse struct {
	Skills []skillEntry `json:"skills"`
	Shared []skillEntry `json:"shared_skills,omitempty"`
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

// listSkills serves GET /skills: the bundle's skill roster in `skills` (used
// by composer "/" autocomplete) plus active workspace-shared teammate skills
// in `shared_skills` (used by Settings → Skills → Workspace pack for 1-click
// opt-in installation). Member-gated like /personas.
func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	skills := s.bundleSkills()
	sharedByName := map[string]store.UserSkill{}
	var sharedList []store.UserSkill
	if s.store != nil {
		if shared, err := s.store.ListWorkspaceSharedUserSkills(r.Context()); err == nil {
			sharedList = shared
			for _, sk := range shared {
				sharedByName[sk.Name] = sk
			}
		}
	}
	bundleNames := make(map[string]bool, len(skills))
	entries := make([]skillEntry, 0, len(skills))
	for _, sk := range skills {
		bundleNames[sk.Name] = true
		origin := s.skillOrigin(sk.Name)
		entry := skillEntry{
			Name: sk.Name, Description: sk.Description, Source: origin.Source, Plugin: origin.Plugin,
			DeclaredAllowedTools: sk.DeclaredAllowedTools,
		}
		if sharedSk, ok := sharedByName[sk.Name]; ok {
			entry.Description = sharedSk.Description
			entry.Source = "workspace"
			entry.SharedBy = sharedSk.SharedBy
			entry.Plugin = ""
			entry.DeclaredAllowedTools = nil
		}
		entries = append(entries, entry)
	}
	var sharedEntries []skillEntry
	for _, sk := range sharedList {
		if bundleNames[sk.Name] {
			continue
		}
		sharedEntries = append(sharedEntries, skillEntry{
			Name:        sk.Name,
			Description: sk.Description,
			Source:      "workspace",
			SharedBy:    sk.SharedBy,
		})
	}
	writeJSON(w, skillsResponse{Skills: entries, Shared: sharedEntries})
}

// skillOrigin reports where a merged-roster skill came from — the bundle's
// own skills/, an Agent Plugin, or the built-in pack — with the same
// precedence the merged tree was built with (clientconfig.Bundle.SkillOrigin).
func (s *Server) skillOrigin(name string) clientconfig.SkillOrigin {
	if s.clientConfig == nil {
		return clientconfig.SkillOrigin{Source: "bundle"}
	}
	return s.clientConfig.SkillOrigin(name)
}

// skillByName serves GET /skills/{name}: the skill's full SKILL.md body for
// the library UI's read view. The name is matched against the loaded roster
// (never used as a raw path), so traversal is structurally impossible.
func (s *Server) skillByName(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/skills/"), "/")
	if s.store != nil {
		if shared, err := s.store.ListWorkspaceSharedUserSkills(r.Context()); err == nil {
			for _, sk := range shared {
				if sk.Name != name {
					continue
				}
				resp := map[string]any{
					"name":        sk.Name,
					"description": sk.Description,
					"source":      "workspace",
					"shared_by":   sk.SharedBy,
					"body":        sk.Body,
					"content":     store.RenderUserSkillMarkdown(&sk),
				}
				writeJSON(w, resp)
				return
			}
		}
	}
	for _, sk := range s.bundleSkills() {
		if sk.Name != name {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.clientConfig.SkillsDir, sk.Dir, "SKILL.md")) // #nosec G304 — path from the validated roster, not the request.
		if err != nil {
			http.Error(w, "skill unreadable", http.StatusInternalServerError)
			return
		}
		origin := s.skillOrigin(sk.Name)
		resp := map[string]any{
			"name":                   sk.Name,
			"description":            sk.Description,
			"source":                 origin.Source,
			"declared_allowed_tools": sk.DeclaredAllowedTools,
			"content":                string(data),
		}
		if origin.Plugin != "" {
			resp["plugin"] = origin.Plugin
		}
		writeJSON(w, resp)
		return
	}
	http.Error(w, "unknown skill", http.StatusNotFound)
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
func (s *Server) applySkillInvocation(ctx context.Context, user, userMessage, rawMessage string) string {
	var block string
	if s.store != nil {
		// A user's active skill (including a customized override of a bundle
		// skill) takes precedence over the static bundle copy; otherwise fall
		// back to the bundle/built-in roster. Best-effort — a store error just
		// falls back to the bundle roster.
		if userSkills, err := s.effectiveUserSkills(ctx, user); err == nil {
			block = matchUserSkillInvocation(rawMessage, userSkills)
		}
	}
	if block == "" {
		block, _ = matchSkillInvocation(rawMessage, s.bundleSkills())
	}
	return userMessage + block
}
