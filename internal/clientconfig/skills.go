package clientconfig

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// Agent Skills support for the client bundle.
//
// A skill is a self-contained folder under the bundle's skills/ dir following
// the Agent Skills standard (https://github.com/anthropics/skills):
//
//	skills/
//	  <skill-name>/
//	    SKILL.md          # YAML frontmatter (name, description) + instructions
//	    scripts/...       # OPTIONAL bundled scripts the agent runs via bash/python
//	    REFERENCE.md ...  # OPTIONAL reference files the agent reads on demand
//
// Skills are the progressive-disclosure sibling of protocols. fleet surfaces
// only each skill's name + description + path in the system prompt (Level 1,
// ~always-loaded metadata); the agent reads the full SKILL.md (Level 2) and any
// bundled scripts/resources (Level 3) on demand, by path, when a task matches —
// exactly the same read-on-demand model protocols use. A skill differs from a
// protocol in that it is a FOLDER that can bundle executable scripts and
// reference files, not a single markdown playbook.
//
// Skills run through the SAME governed surface as everything else: the skills/
// dir is bind-mounted READ-ONLY into the per-turn sandbox (alongside protocols/
// and personas/) and symlinked into the per-conversation workspace, so a SKILL.md
// that says `python skills/<name>/scripts/foo.py` resolves and runs inside the
// rootless sandbox. fleet does NOT build a bespoke skill executor — skills are
// just files the agent reads and runs with bash/run_python. A skill's optional
// frontmatter `allowed-tools` is parsed-tolerantly but NOT yet enforced as a
// hard authorization boundary; the real boundaries remain the sandbox, the MCP
// tool allowlists, and the critical-tool audit gate.
//
// SECURITY: a skill can ship code that executes in the sandbox. Treat the bundle
// as a trusted-but-reviewable supply chain — a skill is only as trustworthy as
// the bundle it ships in. Review bundled scripts the same way you review the
// bundle's mcp/ servers.

// Skill is one parsed Agent Skill discovered under a bundle's skills/ dir.
type Skill struct {
	// Dir is the skill's folder name under skills/ (e.g. "research-report").
	Dir string
	// Name is the frontmatter `name`. For a well-formed skill it equals Dir.
	Name string
	// Description is the frontmatter `description` — the single line shown in the
	// prompt roster (what the skill does + when to use it).
	Description string
	// Path is the bundle-relative path to the skill's SKILL.md, e.g.
	// "skills/research-report/SKILL.md" — the handle the agent reads on demand.
	// It resolves inside the sandbox/workspace exactly like "protocols/foo.md".
	Path string
}

// skillFrontmatter is the subset of SKILL.md YAML frontmatter fleet reads. The
// unmarshal is intentionally NON-strict: the Agent Skills standard allows
// additional fields (allowed-tools, license, metadata, …) and may add more, so
// unknown keys are ignored rather than failing the parse — a future-standard
// skill still loads.
type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// maxSkillNameLen / maxSkillDescLen mirror the Agent Skills standard's field
// limits (name ≤ 64 chars, description ≤ 1024). Exceeding them is a warning, not
// a skip — the skill still renders — so a slightly-too-long description doesn't
// silently vanish from the roster.
const (
	maxSkillNameLen = 64
	maxSkillDescLen = 1024
)

// ReadSkills walks skillsDir and parses each <sub>/SKILL.md. It returns the
// well-formed skills (sorted by Name) plus a list of human-readable problems for
// any malformed skill. An absent or empty skills/ dir is NOT a problem — a bundle
// need not ship any skills — so it returns (nil, nil).
//
// A skill is included in the returned roster only when it is well-formed: it has
// a SKILL.md with a frontmatter `name` that matches its folder and a non-empty
// `description`. That guarantees the path fleet advertises in the prompt always
// matches the name the model reasons about. Cosmetic deviations (name charset /
// length, description length) are reported as problems but do NOT exclude the
// skill. Files directly under skills/ (e.g. a README) are ignored silently.
func ReadSkills(skillsDir string) (skills []Skill, problems []string) {
	if strings.TrimSpace(skillsDir) == "" {
		return nil, nil
	}
	info, err := os.Stat(skillsDir)
	if err != nil || !info.IsDir() {
		// No skills/ dir: a bundle need not ship skills. Not a problem.
		return nil, nil
	}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil, []string{fmt.Sprintf("skills: cannot read %s: %v", skillsDir, err)}
	}
	for _, e := range entries {
		if !e.IsDir() {
			// Only folders are skills (skills/<name>/SKILL.md). A stray file at
			// the skills/ root (e.g. README.md) is allowed and ignored.
			continue
		}
		dir := e.Name()
		rel := filepath.Join("skills", dir, "SKILL.md")
		skillMd := filepath.Join(skillsDir, dir, "SKILL.md")
		raw, err := os.ReadFile(skillMd) // #nosec G304 — operator-supplied bundle path.
		if err != nil {
			problems = append(problems, fmt.Sprintf(
				"skills/%s: missing SKILL.md (each skill is a folder containing a SKILL.md)", dir))
			continue
		}
		fm, ok := extractFrontmatter(raw)
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"%s: missing or malformed YAML frontmatter (expected a leading '---' fenced block with name + description)", rel))
			continue
		}
		var meta skillFrontmatter
		if err := yaml.Unmarshal([]byte(fm), &meta); err != nil {
			problems = append(problems, fmt.Sprintf("%s: invalid frontmatter YAML: %v", rel, err))
			continue
		}
		name := strings.TrimSpace(meta.Name)
		desc := strings.TrimSpace(meta.Description)
		if name == "" {
			problems = append(problems, fmt.Sprintf(
				"%s: frontmatter `name` is empty (the Agent Skills standard requires a name)", rel))
			continue
		}
		if name != dir {
			problems = append(problems, fmt.Sprintf(
				"%s: frontmatter name %q does not match folder %q (the canonical layout is skills/<name>/SKILL.md)", rel, name, dir))
			continue
		}
		if desc == "" {
			problems = append(problems, fmt.Sprintf(
				"%s: frontmatter `description` is empty (it is the only text shown in the prompt roster — say what the skill does and when to use it)", rel))
			continue
		}
		// Cosmetic checks: warn but still include the skill.
		if !validSkillName(name) {
			problems = append(problems, fmt.Sprintf(
				"%s: name %q should be lowercase letters, digits, and hyphens (Agent Skills naming convention)", rel, name))
		}
		if len(name) > maxSkillNameLen {
			problems = append(problems, fmt.Sprintf(
				"%s: name is %d chars; the Agent Skills standard caps it at %d", rel, len(name), maxSkillNameLen))
		}
		if len(desc) > maxSkillDescLen {
			problems = append(problems, fmt.Sprintf(
				"%s: description is %d chars; the Agent Skills standard caps it at %d", rel, len(desc), maxSkillDescLen))
		}
		skills = append(skills, Skill{Dir: dir, Name: name, Description: desc, Path: rel})
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, problems
}

// extractFrontmatter returns the YAML body between the leading '---' fence and
// the next '---' line of a SKILL.md, and whether a well-formed fence was found.
// It tolerates a UTF-8 BOM and CRLF line endings.
func extractFrontmatter(data []byte) (string, bool) {
	// Strip a leading UTF-8 BOM (EF BB BF) if an editor wrote one.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return "", false
	}
	rest := s[len("---"):] // begins with "\n…"
	// The closing fence is a line that is exactly "---": match "\n---" so a "---"
	// appearing mid-value isn't mistaken for the terminator.
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return "", false
	}
	return rest[:idx], true
}

// validSkillName reports whether name uses only the Agent Skills naming charset
// (lowercase letters, digits, hyphens).
func validSkillName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// Skills returns the bundle's well-formed Agent Skills (sorted by name). The
// generic bundle ships one example skill; a real bundle ships its own. Re-read
// from disk so an operator editing a skill in place takes effect without a
// process restart — matching the persona/protocol live-reload contract.
func (b *Bundle) Skills() []Skill {
	skills, _ := ReadSkills(b.SkillsDir)
	return skills
}

// ValidateSkills returns one human-readable problem per malformed skill under the
// bundle's skills/ dir (missing SKILL.md, bad frontmatter, name/folder mismatch,
// empty description, …); an empty slice means every skill is well-formed. Load
// logs any problems as warnings; a CI test asserts the shipped bundle returns
// none. It is the skills analogue of ValidateMCPArgPaths.
func (b *Bundle) ValidateSkills() []string {
	_, problems := ReadSkills(b.SkillsDir)
	return problems
}
