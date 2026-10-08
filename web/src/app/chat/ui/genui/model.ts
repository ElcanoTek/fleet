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
/**
 * The largest card message (as it appears JSON-escaped in the POST body) a
 * card will send. /api/chat refuses bodies over 1 MiB (maxJSONBodyBytes in
 * internal/httpapi/routes.go); the rest is left for the other request fields.
 * A list at MAX_LIST_ITEMS of long entries can exceed this, so the card checks
 * the serialized size before sending rather than failing as "Not sent".
 */
export const MAX_SUBMISSION_BYTES = 960 * 1024;

/** The message's size as it travels in the JSON request body, in bytes. */
export function submissionBytes(message: string): number {
  return new TextEncoder().encode(JSON.stringify(message)).length;
}

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
// Parsed specs by raw input. The transcript re-derives card state on every
// streamed token, and a spec can be large; a tool call's input never changes
// once recorded, so each is parsed once.
const specCache = new Map<string, CardSpec | null>();
const SPEC_CACHE_MAX = 200;

export function parseCardSpec(input: string): CardSpec | null {
  const hit = specCache.get(input);
  if (hit !== undefined) return hit;
  const spec = parseCardSpecUncached(input);
  if (specCache.size >= SPEC_CACHE_MAX) specCache.delete(specCache.keys().next().value as string);
  specCache.set(input, spec);
  return spec;
}

function parseCardSpecUncached(input: string): CardSpec | null {
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
/** A pasted list: one item per line, trimmed, blanks dropped, deduped by default. */
export function parseListText(text: string, dedupe: boolean): string[] {
  return normalizeList(text.split(/[\r\n]+/), dedupe);
}

export function normalizeList(items: string[], dedupe: boolean): string[] {
  const lines = items.map((l) => l.trim()).filter((l) => l !== "");
  return dedupe ? Array.from(new Set(lines)) : lines;
}

/** An absolute http(s) URL with a host — what format: "url" promises. */
export function isWebUrl(s: string): boolean {
  try {
    const u = new URL(s);
    return (u.protocol === "http:" || u.protocol === "https:") && u.hostname !== "";
  } catch {
    return false;
  }
}

const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;

/** A real calendar date in YYYY-MM-DD (rejects 2026-02-31), like Go's time.Parse. */
export function isCalendarDate(s: string): boolean {
  if (!DATE_RE.test(s)) return false;
  const [y, m, d] = s.split("-").map(Number);
  // setUTCFullYear, not Date.UTC: Date.UTC maps years 0–99 to 1900–1999.
  const dt = new Date(0);
  dt.setUTCFullYear(y, m - 1, d);
  return dt.getUTCFullYear() === y && dt.getUTCMonth() === m - 1 && dt.getUTCDate() === d;
}

/**
 * The value an input starts with: its `value` when that is something the
 * control could have produced, else the empty value. "Could have produced"
 * covers meaning, not just shape — an option the input does not offer, a
 * malformed date or an unknown table row is dropped — because the same
 * function normalizes drafts and transcript submissions, which the server
 * never validated.
 */
export function defaultValue(c: Component): unknown {
  const v = c.value;
  const optionSet = () => new Set(optionsOf(c).map((o) => o.value));
  const custom = c.allow_custom === true;
  switch (c.type) {
    case "text_input":
      return typeof v === "string" ? v : "";
    case "date":
      return typeof v === "string" && isCalendarDate(v) ? v : "";
    case "select":
    case "choice":
      return typeof v === "string" && optionSet().has(v) ? v : "";
    case "number":
      return typeof v === "number" ? v : null;
    case "slider":
      return typeof v === "number" ? v : typeof c.min === "number" ? c.min : 0;
    case "toggle":
      return v === true;
    case "multi_select": {
      const opts = optionSet();
      return Array.from(new Set(strArr(v))).filter((x) => custom || opts.has(x));
    }
    case "list_input":
      // Same trim / drop-blank / dedupe the textarea applies to typed text.
      return normalizeList(strArr(v), c.dedupe !== false);
    case "include_exclude": {
      const o = v && typeof v === "object" ? (v as Record<string, unknown>) : {};
      const opts = optionSet();
      const ok = (x: string) => custom || opts.has(x);
      const include = Array.from(new Set(strArr(o.include))).filter(ok);
      // An entry cannot sit in both lanes; include wins.
      const exclude = Array.from(new Set(strArr(o.exclude))).filter((x) => ok(x) && !include.includes(x));
      return { include, exclude } satisfies IncludeExclude;
    }
    case "table": {
      const key = typeof c.row_key === "string" ? c.row_key : "";
      const rowKeys = new Set(
        (Array.isArray(c.rows) ? c.rows : [])
          .map((r) => (r && typeof r === "object" ? (r as Record<string, unknown>)[key] : undefined))
          .filter((k): k is string => typeof k === "string"),
      );
      if (c.select === "multi") return Array.from(new Set(strArr(v))).filter((k) => rowKeys.has(k));
      return typeof v === "string" && rowKeys.has(v) ? v : "";
    }
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

/**
 * The card's values with `saved` (a submission from the transcript, or a
 * local draft) laid over the defaults — each saved value normalized through
 * its own input exactly like a default (unknown ids dropped, wrong shapes
 * reset, repeater items filtered to objects). A transcript message only
 * LOOKS like buildSubmissionMessage output; it may come from the composer,
 * another client or an older build, so it is never adopted raw.
 */
export function normalizeValues(spec: CardSpec, saved: Values | null | undefined): Values {
  const out = initialValues(spec);
  if (!saved || typeof saved !== "object") return out;
  walkInputs(spec.components, (c) => {
    if (c.id && Object.prototype.hasOwnProperty.call(saved, c.id)) {
      out[c.id] = defaultValue({ ...c, value: saved[c.id] });
    }
  });
  return out;
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

// The HTML spec's "valid email address" (what <input type="email"> checks),
// plus a dot in the domain: actions skip native form validation, so this is
// the check that counts. Rejects "a@.com" and "a@b..com".
const EMAIL_LABEL = "[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?";
const EMAIL_RE = new RegExp(`^[a-zA-Z0-9.!#$%&'*+/=?^_\`{|}~-]+@${EMAIL_LABEL}(?:\\.${EMAIL_LABEL})+$`);

/** One input's own problem, or "" (required-ness included). */
export function checkField(c: Component, v: unknown): string {
  // A required toggle is an acknowledgement: it must be switched on.
  if (c.required === true && c.type === "toggle") return v === true ? "" : "Required";
  if (c.required === true && isEmpty(c, v)) return "Required";
  if (isEmpty(c, v)) return "";
  switch (c.type) {
    case "text_input": {
      const s = String(v);
      if (typeof c.min_length === "number" && s.length < c.min_length) return `At least ${c.min_length} characters`;
      if (typeof c.max_length === "number" && s.length > c.max_length) return `At most ${c.max_length} characters`;
      if (c.format === "email" && !EMAIL_RE.test(s.trim())) return "Enter an email address";
      if (c.format === "url" && !isWebUrl(s.trim())) return "Enter a URL (https://…)";
      return "";
    }
    case "number":
    case "slider": {
      const n = typeof v === "number" ? v : Number(v);
      if (!Number.isFinite(n)) return "Enter a number";
      if (typeof c.min === "number" && n < c.min) return `Must be at least ${c.min}`;
      if (typeof c.max === "number" && n > c.max) return `Must be at most ${c.max}`;
      // The card does not submit through HTML form validation, so the
      // input's step rule (based at min, like the native control) is
      // enforced here.
      if (typeof c.step === "number" && c.step > 0) {
        const base = typeof c.min === "number" ? c.min : 0;
        const k = (n - base) / c.step;
        if (Math.abs(k - Math.round(k)) > 1e-9) return `Must be in steps of ${c.step}${base ? ` from ${base}` : ""}`;
      }
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
export function collect(
  spec: CardSpec,
  values: Values,
): { values: Values; errors: Record<string, string>; visible: Set<string> } {
  const out: Values = {};
  const errors: Record<string, string> = {};
  // Every currently visible, enabled input's path — what a server
  // field_error may still block on (a hidden, removed or disabled field
  // cannot be fixed by the user).
  const visible = new Set<string>();
  // read: where this level's values live (the card, or one repeater item);
  // write: where the submitted copy goes; prefix: the error-path prefix.
  const visit = (list: Component[], scope: Scope, read: Values, write: Values, prefix: string, disabled = false) => {
    for (const c of list) {
      if (!isVisible(c, scope)) continue;
      if (isInput(c) && c.id) {
        // Only an input the user can change can clear an error on it.
        const off = disabled || c.disabled === true;
        if (!off) visible.add(prefix + c.id);
        const v = read[c.id];
        if (c.type === "repeater") {
          const fields = children(c, "fields");
          const items = Array.isArray(v)
            ? (v as unknown[]).map((x) => (x && typeof x === "object" && !Array.isArray(x) ? (x as Values) : {}))
            : [];
          write[c.id] = items.map((item, i) => {
            const w: Values = {};
            visit(fields, scopeFor(values, item, i), item, w, `${c.id}[${i}].`, off);
            return w;
          });
          const err = off ? "" : checkField(c, items);
          if (err) errors[prefix + c.id] = err;
          continue;
        }
        // A disabled field cannot be fixed by the user, so it never blocks.
        const err = off ? "" : checkField(c, v);
        if (err) errors[prefix + c.id] = err;
        write[c.id] = v;
      }
      visit(children(c), scope, read, write, prefix, disabled);
      for (const t of tabsOf(c)) visit(t.children, scope, read, write, prefix, disabled);
    }
  };
  visit(spec.components, scopeFor(values), values, out, "");
  return { values: out, errors, visible };
}

/** Must match tools.UIReplyPrefix in internal/tools/show_ui.go. */
export const UI_REPLY_PREFIX = "[UI reply]";

export type Reply = { cardId: string; actionId: string; text: string; messageId?: number };

/** A quick-reply button's message: a marker line naming the card, then its text. */
export function buildReplyMessage(cardId: string, actionId: string, text: string): string {
  return `${UI_REPLY_PREFIX} card=${cardId} action=${actionId}\n${text}`;
}

const REPLY_RE = /^\[UI reply\] card=(\S+) action=(\S+)\n([\s\S]*)$/;

/**
 * Whether a message was written by a card (a submission or a quick reply),
 * not typed in the composer. Such a send must leave the composer alone: its
 * text, its attachments, and — when the send is refused — no marker text
 * restored into it.
 */
export function isCardMessage(text: string): boolean {
  return text.startsWith(UI_SUBMISSION_PREFIX) || text.startsWith(UI_REPLY_PREFIX);
}

export function parseReplyMessage(text: string): Reply | null {
  if (!text.startsWith(UI_REPLY_PREFIX)) return null;
  const m = REPLY_RE.exec(text);
  return m ? { cardId: m[1], actionId: m[2], text: m[3] } : null;
}

export function buildSubmissionMessage(cardId: string, actionId: string, values: Values): string {
  return `${UI_SUBMISSION_PREFIX} card=${cardId} action=${actionId}\n\`\`\`json\n${JSON.stringify(values)}\n\`\`\``;
}

export type Submission = {
  cardId: string;
  actionId: string;
  values: Values;
  /** The transcript message it came from — two identical resends differ here. */
  messageId?: number;
};

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
