// Pure model for generative-UI cards (show_ui): spec types, default values,
// visibility-aware validation, and the submission message. No React here so
// it unit-tests without jsdom. The renderer is GenerativeCard.tsx; the spec
// is validated server-side by internal/genui before a card ever reaches us,
// but everything here still treats the spec as untrusted input — unknown
// shapes degrade to "nothing rendered", never to a throw.

import { evaluateSafe, truthy, type Scope } from "./expr";

export const SHOW_UI_TOOL = "show_ui";
/** Must match genui.MaxListItems in internal/genui/spec.go. */
export const MAX_LIST_ITEMS = 20000;
/** Must match tools.UISubmissionPrefix in internal/tools/show_ui.go. */
export const UI_SUBMISSION_PREFIX = "[UI submission]";

export type Tone = "neutral" | "info" | "success" | "warning" | "danger";
export type Option = string | { value: string; label?: string; description?: string };

/** A component node. Props are read defensively by the renderer. */
export type Component = { type: string; id?: string; visible_if?: string } & Record<string, unknown>;

export type Action = {
  id: string;
  label: string;
  kind?: "submit" | "message";
  style?: "primary" | "secondary" | "danger";
  message?: string;
  validate?: boolean;
  confirm?: string;
  visible_if?: string;
  disabled_if?: string;
};

export type CardSpec = {
  title: string;
  description?: string;
  components: Component[];
  actions?: Action[];
  replaces?: string;
  field_errors?: { field: string; message: string }[];
};

export const INPUT_TYPES = new Set([
  "text_input",
  "number",
  "slider",
  "select",
  "choice",
  "multi_select",
  "toggle",
  "date",
  "list_input",
  "include_exclude",
  "repeater",
]);

export function isInput(c: Component): boolean {
  if (INPUT_TYPES.has(c.type)) return true;
  return c.type === "table" && (c.select === "single" || c.select === "multi") && typeof c.id === "string";
}

/** Parse a show_ui tool call's raw input. null when it is not a usable spec. */
export function parseCardSpec(input: string): CardSpec | null {
  let v: unknown;
  try {
    v = JSON.parse(input);
  } catch {
    return null;
  }
  if (!v || typeof v !== "object" || Array.isArray(v)) return null;
  const o = v as Record<string, unknown>;
  if (typeof o.title !== "string" || !Array.isArray(o.components)) return null;
  return {
    title: o.title,
    description: typeof o.description === "string" ? o.description : undefined,
    components: o.components.filter(isComponent),
    actions: Array.isArray(o.actions) ? (o.actions.filter((a) => a && typeof a === "object" && typeof (a as Action).id === "string") as Action[]) : [],
    replaces: typeof o.replaces === "string" ? o.replaces : undefined,
    field_errors: Array.isArray(o.field_errors)
      ? (o.field_errors.filter(
          (e) => e && typeof e === "object" && typeof (e as { field?: unknown }).field === "string",
        ) as { field: string; message: string }[])
      : [],
  };
}

export function isComponent(v: unknown): v is Component {
  return !!v && typeof v === "object" && !Array.isArray(v) && typeof (v as Component).type === "string";
}

export function children(c: Component, key = "children"): Component[] {
  const v = c[key];
  return Array.isArray(v) ? v.filter(isComponent) : [];
}

export function tabsOf(c: Component): { label: string; children: Component[] }[] {
  const v = c.tabs;
  if (!Array.isArray(v)) return [];
  return v
    .filter((t) => t && typeof t === "object")
    .map((t) => ({
      label: String((t as { label?: unknown }).label ?? ""),
      children: Array.isArray((t as { children?: unknown }).children)
        ? ((t as { children: unknown[] }).children.filter(isComponent) as Component[])
        : [],
    }));
}

export function optionsOf(c: Component): { value: string; label: string; description?: string }[] {
  const v = c.options;
  if (!Array.isArray(v)) return [];
  const out: { value: string; label: string; description?: string }[] = [];
  for (const o of v) {
    if (typeof o === "string") out.push({ value: o, label: o });
    else if (o && typeof o === "object" && typeof (o as { value?: unknown }).value === "string") {
      const ob = o as { value: string; label?: unknown; description?: unknown };
      out.push({
        value: ob.value,
        label: typeof ob.label === "string" && ob.label ? ob.label : ob.value,
        description: typeof ob.description === "string" ? ob.description : undefined,
      });
    }
  }
  return out;
}

export type Values = Record<string, unknown>;
export type IncludeExclude = { include: string[]; exclude: string[] };

/** Every input directly under a component list, through layout containers. */
export function walkInputs(list: Component[], visit: (c: Component) => void): void {
  for (const c of list) {
    if (isInput(c)) visit(c);
    if (c.type === "repeater") continue;
    walkInputs(children(c), visit);
    for (const t of tabsOf(c)) walkInputs(t.children, visit);
  }
}

const strArr = (v: unknown): string[] => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []);

/** The empty / default value an input starts with. */
export function defaultValue(c: Component): unknown {
  const v = c.value;
  switch (c.type) {
    case "text_input":
    case "date":
    case "select":
    case "choice":
      return typeof v === "string" ? v : "";
    case "number":
      return typeof v === "number" ? v : null;
    case "slider":
      return typeof v === "number" ? v : typeof c.min === "number" ? c.min : 0;
    case "toggle":
      return v === true;
    case "multi_select":
    case "list_input":
      return strArr(v);
    case "include_exclude": {
      const o = v && typeof v === "object" ? (v as Record<string, unknown>) : {};
      return { include: strArr(o.include), exclude: strArr(o.exclude) } satisfies IncludeExclude;
    }
    case "table":
      return c.select === "multi" ? strArr(v) : typeof v === "string" ? v : "";
    case "repeater": {
      const fields = children(c, "fields");
      if (Array.isArray(v)) {
        return v
          .filter((x) => x && typeof x === "object" && !Array.isArray(x))
          .map((x) => ({ ...newItem(fields), ...pickKnown(fields, x as Values) }));
      }
      const n = typeof c.min_items === "number" ? Math.max(1, c.min_items) : 1;
      return Array.from({ length: n }, () => newItem(fields));
    }
    default:
      return null;
  }
}

function pickKnown(fields: Component[], item: Values): Values {
  const out: Values = {};
  walkInputs(fields, (f) => {
    if (f.id && Object.prototype.hasOwnProperty.call(item, f.id)) {
      const probe = { ...f, value: item[f.id] };
      out[f.id] = defaultValue(probe);
    }
  });
  return out;
}

/** A fresh repeater item with every field at its default. */
export function newItem(fields: Component[]): Values {
  const item: Values = {};
  walkInputs(fields, (f) => {
    if (f.id) item[f.id] = defaultValue(f);
  });
  return item;
}

export function initialValues(spec: CardSpec): Values {
  const values: Values = {};
  walkInputs(spec.components, (c) => {
    if (c.id) values[c.id] = defaultValue(c);
  });
  return values;
}

/** The expression scope at card level, or inside one repeater item. */
export function scopeFor(values: Values, item?: Values, index?: number): Scope {
  if (!item) return values;
  return { ...values, ...item, index: (index ?? 0) + 1 };
}

export function isVisible(c: { visible_if?: unknown }, scope: Scope): boolean {
  if (typeof c.visible_if !== "string" || !c.visible_if.trim()) return true;
  return truthy(evaluateSafe(c.visible_if, scope, true));
}

function isEmpty(c: Component, v: unknown): boolean {
  if (v === null || v === undefined) return true;
  if (typeof v === "string") return v.trim() === "";
  if (Array.isArray(v)) return v.length === 0;
  if (c.type === "include_exclude") {
    const ie = v as IncludeExclude;
    return ie.include.length === 0 && ie.exclude.length === 0;
  }
  return false;
}

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

/** One input's own problem, or "" (required-ness included). */
export function checkField(c: Component, v: unknown): string {
  if (c.required === true && c.type !== "toggle" && isEmpty(c, v)) return "Required";
  if (isEmpty(c, v)) return "";
  switch (c.type) {
    case "text_input": {
      const s = String(v);
      if (typeof c.min_length === "number" && s.length < c.min_length) return `At least ${c.min_length} characters`;
      if (typeof c.max_length === "number" && s.length > c.max_length) return `At most ${c.max_length} characters`;
      if (c.format === "email" && !EMAIL_RE.test(s.trim())) return "Enter an email address";
      if (c.format === "url" && !/^https?:\/\/\S+$/i.test(s.trim())) return "Enter a URL (https://…)";
      return "";
    }
    case "number":
    case "slider": {
      const n = typeof v === "number" ? v : Number(v);
      if (!Number.isFinite(n)) return "Enter a number";
      if (typeof c.min === "number" && n < c.min) return `Must be at least ${c.min}`;
      if (typeof c.max === "number" && n > c.max) return `Must be at most ${c.max}`;
      return "";
    }
    case "multi_select":
      if (typeof c.max_items === "number" && Array.isArray(v) && v.length > c.max_items) return `At most ${c.max_items} items`;
      return "";
    case "list_input": {
      // The protocol cap applies even when the card sets no max_items: a
      // paste must not become an unbounded user turn.
      const cap = Math.min(typeof c.max_items === "number" ? c.max_items : MAX_LIST_ITEMS, MAX_LIST_ITEMS);
      if (Array.isArray(v) && v.length > cap) return `At most ${cap.toLocaleString()} items`;
      return "";
    }
    case "date":
      if (typeof c.min === "string" && String(v) < c.min) return `On or after ${c.min}`;
      if (typeof c.max === "string" && String(v) > c.max) return `On or before ${c.max}`;
      return "";
    case "repeater": {
      const n = Array.isArray(v) ? v.length : 0;
      if (typeof c.min_items === "number" && n < c.min_items) return `Add at least ${c.min_items}`;
      if (typeof c.max_items === "number" && n > c.max_items) return `At most ${c.max_items}`;
      return "";
    }
    default:
      return "";
  }
}

/**
 * The submitted payload: only VISIBLE inputs, keyed by id (a hidden field's
 * stale value would read as an answer the user never saw), plus every
 * visible field's validation error keyed by path ("id" or "rep[i].field").
 */
export function collect(spec: CardSpec, values: Values): { values: Values; errors: Record<string, string> } {
  const out: Values = {};
  const errors: Record<string, string> = {};
  // read: where this level's values live (the card, or one repeater item);
  // write: where the submitted copy goes; prefix: the error-path prefix.
  const visit = (list: Component[], scope: Scope, read: Values, write: Values, prefix: string) => {
    for (const c of list) {
      if (!isVisible(c, scope)) continue;
      if (isInput(c) && c.id) {
        const v = read[c.id];
        if (c.type === "repeater") {
          const fields = children(c, "fields");
          const items = Array.isArray(v) ? (v as Values[]) : [];
          write[c.id] = items.map((item, i) => {
            const w: Values = {};
            visit(fields, scopeFor(values, item, i), item, w, `${c.id}[${i}].`);
            return w;
          });
          const err = checkField(c, items);
          if (err) errors[prefix + c.id] = err;
          continue;
        }
        const err = checkField(c, v);
        if (err) errors[prefix + c.id] = err;
        write[c.id] = v;
      }
      visit(children(c), scope, read, write, prefix);
      for (const t of tabsOf(c)) visit(t.children, scope, read, write, prefix);
    }
  };
  visit(spec.components, scopeFor(values), values, out, "");
  return { values: out, errors };
}

export function buildSubmissionMessage(cardId: string, actionId: string, values: Values): string {
  return `${UI_SUBMISSION_PREFIX} card=${cardId} action=${actionId}\n\`\`\`json\n${JSON.stringify(values)}\n\`\`\``;
}

export type Submission = { cardId: string; actionId: string; values: Values };

const SUBMISSION_RE = /^\[UI submission\] card=(\S+) action=(\S+)\n```json\n([\s\S]*)\n```\s*$/;

export function parseSubmissionMessage(text: string): Submission | null {
  if (!text.startsWith(UI_SUBMISSION_PREFIX)) return null;
  const m = SUBMISSION_RE.exec(text);
  if (!m) return null;
  try {
    const v = JSON.parse(m[3]);
    if (!v || typeof v !== "object" || Array.isArray(v)) return null;
    return { cardId: m[1], actionId: m[2], values: v as Values };
  } catch {
    return null;
  }
}
