// Protocol pills — the empty-state "quick start" catalog.
//
// A pill COLLECTS the inputs a workflow would otherwise ask for (its "Step 0
// checklist") and templates them into a natural-language prompt that is
// submitted through the normal composer path. There is NO backend coupling
// here: the agent reads the relevant context on demand, runs its real tools,
// and the existing approval / render flows handle the output.
//
// The catalog is now CONFIG-DRIVEN: the live set of pills is fetched at runtime
// from `/api/client-config` (which proxies the chat-server's member-gated
// `/client-config`). Pills therefore arrive as plain JSON — they carry only
// static fields plus an optional `promptTemplate` STRING. They cannot ship
// JS functions, so prompt assembly is handled generically by `pillToPrompt`
// (see below) rather than per-pill closures.
//
// `DEFAULT_PILLS` is the neutral, client-agnostic fallback used when the config
// fetch fails or returns no cards.
//
// Two interaction shapes:
//   • Form pills (type "form")          — fill fields → templated prompt → send.
//   • Conversation pills (optionalForm) — chat-first; an OPTIONAL form can
//     point the assistant at specifics, but skipping it just starts the intake.

export type PillFieldType = "text" | "select" | "number" | "daterange" | "toggle";

export type DateRangeValue = { from: string; to: string };

export type PillFieldValue = string | number | boolean | DateRangeValue;

export interface PillField {
  key: string;
  label: string;
  type: PillFieldType;
  /** Placeholder for text/number inputs. */
  placeholder?: string;
  /** Pre-filled value. Forms are a guide, not a gate — defaults stand in for
   *  anything the user skips. */
  default?: PillFieldValue;
  /** Options for `select`. */
  options?: string[];
  /** Tuck the field under the "Advanced" disclosure. */
  advanced?: boolean;
  /** Block the Run button until this field is filled. */
  required?: boolean;
  /** Helper text rendered beneath text / select / number fields. */
  hint?: string;
  /** Minimum for `number`. */
  min?: number;
}

export type PillValues = Record<string, PillFieldValue>;

export interface ProtocolPill {
  id: string;
  /** Coarse grouping label. No longer rendered (the empty state is one flat
   *  grid) but kept as catalog metadata in case sectioning returns. */
  section: string;
  type: "form" | "conversation";
  /** Sprite glyph id (see public/icons/core-icons.svg). */
  icon: string;
  title: string;
  desc: string;
  /** Run-button label. */
  cta: string;
  /** Form fields (form pills, and the optional form on conversation pills). */
  fields?: PillField[];
  /** Plain template string the form submits. Config-sourced, so it can't be a
   *  function. `{key}` tokens are interpolated from the filled field values by
   *  `pillToPrompt`; when absent, a neutral prompt is built from the title and
   *  the filled fields. */
  promptTemplate?: string;
  /** Conversation-first pills: chat is the primary path, the form is optional. */
  optionalForm?: boolean;
  /** Sent when the user picks the chat path on a conversation pill. */
  starterPrompt?: string;
}

// ── value helpers ─────────────────────────────────────────────────────────

/** Trimmed string view of any field value (empty string when blank/unset). */
export function asText(v: PillFieldValue | undefined): string {
  if (typeof v === "string") return v.trim();
  if (typeof v === "number") return String(v);
  return "";
}

export function asNumber(v: PillFieldValue | undefined, fallback: number): number {
  if (typeof v === "number" && Number.isFinite(v)) return v;
  if (typeof v === "string" && v.trim() !== "") {
    const n = Number(v);
    if (Number.isFinite(n)) return n;
  }
  return fallback;
}

export function asRange(v: PillFieldValue | undefined): DateRangeValue {
  if (v && typeof v === "object" && "from" in v && "to" in v) return v;
  return { from: "", to: "" };
}

/** Initial form values for a pill, honoring each field's `default`. */
export function formInitialValues(pill: ProtocolPill): PillValues {
  const out: PillValues = {};
  for (const f of pill.fields ?? []) {
    if (f.default !== undefined) {
      out[f.key] = f.default;
    } else if (f.type === "daterange") {
      out[f.key] = { from: "", to: "" };
    } else if (f.type === "toggle") {
      out[f.key] = false;
    } else if (f.type === "select") {
      out[f.key] = f.options?.[0] ?? "";
    } else if (f.type === "number") {
      out[f.key] = f.min ?? 0;
    } else {
      out[f.key] = "";
    }
  }
  return out;
}

/** True when every required field on the pill has a usable value. */
export function isPillReady(pill: ProtocolPill, values: PillValues): boolean {
  for (const f of pill.fields ?? []) {
    if (!f.required) continue;
    const v = values[f.key];
    if (f.type === "daterange") {
      const r = asRange(v);
      if (!r.from || !r.to) return false;
    } else if (asText(v) === "") {
      return false;
    }
  }
  return true;
}

/** Look a pill up by id within a (dynamic, config-sourced) pill list. */
export function getPill(id: string, pills: ProtocolPill[]): ProtocolPill | undefined {
  return pills.find((p) => p.id === id);
}

// Joins "Label: value" clauses for fields the user actually filled, so empty
// optional inputs don't inject hollow directives the agent has to ignore.
export function detailLine(parts: string[]): string {
  return parts.length ? ` ${parts.join(". ")}.` : "";
}

// Renders a single field's value for a "Label: value" detail line. Daterange
// and toggle get human-readable forms; everything else uses the trimmed text.
function fieldValueText(field: PillField, value: PillFieldValue | undefined): string {
  if (field.type === "toggle") return value ? "yes" : "no";
  if (field.type === "daterange") {
    const r = asRange(value);
    return r.from || r.to ? `${r.from || "?"} → ${r.to || "?"}` : "";
  }
  return asText(value);
}

// ── prompt assembly ─────────────────────────────────────────────────────────

/**
 * Build the prompt a pill submits, from its (config-sourced, static) shape and
 * the form values the user filled.
 *
 * - When the pill carries a `promptTemplate` STRING, that string is used.
 *   Any `{key}` tokens are interpolated from `values` (a token whose field is
 *   blank is dropped). The template is otherwise returned verbatim — if it has
 *   no tokens, it's sent as-is.
 * - When there is NO template, a neutral fallback is built from the pill title
 *   plus "Label: value" lines for every field the user actually filled.
 *
 * This replaces the per-pill function closures the catalog used to ship, so the
 * same renderer works for pills that arrive over JSON.
 */
export function pillToPrompt(pill: ProtocolPill, values: PillValues): string {
  const template = pill.promptTemplate;
  if (typeof template === "string" && template.trim() !== "") {
    if (!template.includes("{")) return template;
    return template.replace(/\{(\w+)\}/g, (whole, key: string) => {
      const field = pill.fields?.find((f) => f.key === key);
      const text = field
        ? fieldValueText(field, values[key])
        : asText(values[key]);
      // Leave the token in place when there's no field value to fill it with,
      // so the agent can still see what was intended.
      return text || whole;
    });
  }

  const parts: string[] = [];
  for (const f of pill.fields ?? []) {
    const text = fieldValueText(f, values[f.key]);
    if (text) parts.push(`${f.label}: ${text}`);
  }
  return `${pill.title}.${detailLine(parts)}`;
}

// ── neutral fallback catalog ────────────────────────────────────────────────
//
// Client-agnostic quick-start cards used when the config fetch fails or returns
// no cards. Mirrors config/default/manifest.yaml's empty_state.cards so the bare
// fleet experience matches the generic bundle.

export const DEFAULT_PILLS: ProtocolPill[] = [
  {
    id: "summarize",
    section: "Get started",
    type: "form",
    icon: "file-text",
    title: "Summarize a document",
    desc: "Paste or attach a document and get a concise summary.",
    cta: "Summarize",
    fields: [
      {
        key: "focus",
        label: "What should the summary focus on?",
        type: "text",
        placeholder: "key decisions, action items, risks…",
      },
    ],
    promptTemplate: "Summarize the attached/pasted document.",
  },
  {
    id: "analyze-data",
    section: "Get started",
    type: "conversation",
    optionalForm: true,
    icon: "bar-chart",
    title: "Analyze a dataset",
    desc: "Attach a CSV and ask questions — I'll run Python to dig in.",
    cta: "Start analysis",
    starterPrompt:
      "I'd like to analyze a dataset. Ask me what you need to know, then load it with " +
      "Python and walk me through what stands out.",
  },
  {
    id: "draft",
    section: "Get started",
    type: "form",
    icon: "edit",
    title: "Draft something",
    desc: "An email, a plan, a snippet of code — describe it and I'll draft it.",
    cta: "Draft it",
    fields: [
      {
        key: "what",
        label: "What should I draft?",
        type: "text",
        required: true,
        placeholder: "a follow-up email to a client about…",
      },
    ],
    promptTemplate: "Draft the following for me, and ask me anything you need first.",
  },
];
