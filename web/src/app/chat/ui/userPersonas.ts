// User-authored personas in the chat shell (docs/USER-PERSONAS.md). A
// conversation selects one by its ref, "user:<name>"; the bundle personas keep
// their plain basenames. These helpers keep the picker, the composer tooltip
// and anything else that names a persona agreeing on how each one reads.

export type UserPersonaOption = {
  ref: string;
  name: string;
  display_name: string;
  description: string;
  kind: "assistant" | "twin";
  autonomy: "advise" | "act";
};

export const USER_PERSONA_PREFIX = "user:";

export function isUserPersonaRef(persona: string): boolean {
  return persona.startsWith(USER_PERSONA_PREFIX);
}

/** Capitalized bundle persona basename ("assistant" → "Assistant"). */
export function formatBundlePersona(p: string): string {
  return p.charAt(0).toUpperCase() + p.slice(1);
}

/**
 * personaLabel names a persona for display: a user persona by its display
 * name (falling back to its slug when the roster has not loaded or the
 * persona was deleted), a bundle persona by its capitalized basename.
 */
export function personaLabel(persona: string, userPersonas: UserPersonaOption[]): string {
  if (isUserPersonaRef(persona)) {
    const found = userPersonas.find((p) => p.ref === persona);
    return found?.display_name || persona.slice(USER_PERSONA_PREFIX.length);
  }
  return formatBundlePersona(persona);
}
