// Client-side helpers for the "new task from a template" flow (#262).
//
// The orchestrator's GET /task-templates already extracts each template's
// {variable} placeholder names server-side, but the UI still needs to: (1) decide
// which of those need a value from the user vs. can be filled automatically from
// built-ins, and (2) substitute the chosen values into the prompt before
// pre-filling the create form. Those two concerns live here so they can be unit
// tested in isolation from React.

// TEMPLATE_VAR_PATTERN matches a single {token} placeholder. A token is one or
// more word characters (letters, digits, underscore). Mirrors the backend's
// taskTemplateVarPattern so the client and server agree on what a variable is.
const TEMPLATE_VAR_PATTERN = /\{(\w+)\}/g;

// BuiltinVarResolver supplies values for the auto-filled built-in variables. The
// caller wires in live context (the signed-in user's display name); date is
// computed at call time so it always reflects "today".
export type BuiltinVarResolver = {
  // userName is the authenticated user's display name, or undefined if unknown.
  userName?: string;
  // today overrides the date used for {date} (ISO 8601). Defaults to now.
  // Primarily a test seam.
  today?: Date;
};

// extractTemplateVars returns the de-duplicated, sorted set of {token} names in a
// prompt. Kept as a fallback for callers that have the prompt but not the
// server-provided `variables` list; the server list is authoritative when present.
export function extractTemplateVars(prompt: string): string[] {
  const seen = new Set<string>();
  for (const match of prompt.matchAll(TEMPLATE_VAR_PATTERN)) {
    seen.add(match[1]);
  }
  return Array.from(seen).sort();
}

// resolveBuiltinVar returns the auto-fill value for a built-in variable, or
// undefined if the variable is not a built-in (or has no available value, e.g.
// user_name when the display name is unknown — then it must be prompted).
export function resolveBuiltinVar(name: string, ctx: BuiltinVarResolver): string | undefined {
  switch (name) {
    case "date":
      return (ctx.today ?? new Date()).toISOString().slice(0, 10); // YYYY-MM-DD
    case "user_name":
      return ctx.userName && ctx.userName.trim() ? ctx.userName.trim() : undefined;
    default:
      return undefined;
  }
}

// promptableVars returns the subset of a template's variables that still need a
// value from the user — i.e. everything that is not an auto-filled built-in. When
// this is empty the form can be pre-filled immediately with no dialog. `vars`
// should be the server-provided list (authoritative); pass extractTemplateVars
// output if you only have the prompt.
export function promptableVars(vars: string[], ctx: BuiltinVarResolver): string[] {
  return vars.filter((name) => resolveBuiltinVar(name, ctx) === undefined);
}

// humanizeVarName turns a snake_case variable token into a friendly field label,
// e.g. "repo_path" -> "Repo path". Used to label the variable-fill dialog inputs.
export function humanizeVarName(name: string): string {
  const spaced = name.replace(/_/g, " ").trim();
  if (!spaced) return name;
  return spaced.charAt(0).toUpperCase() + spaced.slice(1);
}

// applyTemplateVars substitutes every {token} in the prompt with its value.
// Resolution order per token: an explicit user-supplied value wins, else the
// built-in value, else the placeholder is left intact (so a missing value is
// visible in the form rather than silently dropped). Returns the substituted
// prompt.
export function applyTemplateVars(
  prompt: string,
  userValues: Record<string, string>,
  ctx: BuiltinVarResolver,
): string {
  return prompt.replace(TEMPLATE_VAR_PATTERN, (whole, name: string) => {
    const supplied = userValues[name];
    if (supplied !== undefined && supplied !== "") return supplied;
    const builtin = resolveBuiltinVar(name, ctx);
    if (builtin !== undefined) return builtin;
    return whole; // leave {token} intact when nothing fills it
  });
}
