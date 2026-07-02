// Skill "/" autocomplete (#513 phase 1) — the pure logic behind the composer's
// slash-command popover, kept out of Composer.tsx so it can be unit-tested
// like protocolPills.
//
// The popover mirrors the SERVER's explicit-invocation rule (see
// internal/httpapi/skills.go): a message that STARTS with "/<skill-name>"
// (exact, case-sensitive match against the bundle roster) invokes that skill.
// The client only helps the user type a valid invocation — the server is the
// authority on what actually matches, so an unrecognized "/token" sends as
// plain text with no error.

export type SkillInfo = {
  name: string;
  description: string;
};

/**
 * The partial skill token being typed, or null when the popover should be
 * closed. Active only while the draft is a bare "/<token>": the "/" must be
 * the FIRST character (a slash mid-text — or a path pasted later in the
 * message — never triggers), and once any whitespace follows the token the
 * user has moved on to arguments, so the popover closes. "/" alone returns ""
 * (show the full roster).
 */
export function skillSlashQuery(draft: string): string | null {
  if (!draft.startsWith("/")) return null;
  const token = draft.slice(1);
  if (/\s/.test(token)) return null;
  return token;
}

/**
 * Roster entries whose name starts with the typed prefix. Case-sensitive to
 * match the server's exact-match invocation rule (skill names are lowercase
 * by the Agent Skills convention). An empty query returns the whole roster.
 */
export function filterSkills(skills: SkillInfo[], query: string): SkillInfo[] {
  return skills.filter((s) => s.name.startsWith(query));
}

/** The composer draft after accepting a suggestion: "/<name> ", ready for args. */
export function completeSkill(name: string): string {
  return `/${name} `;
}
