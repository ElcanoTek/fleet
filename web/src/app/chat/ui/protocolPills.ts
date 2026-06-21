// Protocol pills — the empty-state "quick start" catalog.
//
// Each pill is a curated entry point into one of Victoria's high-traffic
// workflows. A pill COLLECTS the inputs a protocol would otherwise ask for
// (its "Step 0 checklist") and templates them into the exact natural-language
// trigger the backend already understands — e.g. the DSP reporting protocol
// is invoked by "Run the DSP reporting protocol for <client> (<campaign code>)". The
// templated string is submitted through the normal composer path, so there
// is NO backend coupling here: the agent reads the relevant protocol on
// demand, runs its real tools, and the existing approval / Gamma flows render
// the output. Nothing in this file fabricates results.
//
// Two interaction shapes:
//   • Form pills (type "form")          — fill fields → templated prompt → send.
//   • Conversation pills (optionalForm) — chat-first; an OPTIONAL form can
//     point Victoria at specifics, but skipping it just starts the intake.
//
// Agents are the only editors of this catalog; keep it a plain typed module.

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
  /** Builds the prompt the form submits. */
  promptTemplate?: (v: PillValues) => string;
  /** Conversation-first pills: chat is the primary path, the form is optional. */
  optionalForm?: boolean;
  /** Sent when the user picks the chat path on a conversation pill. */
  starterPrompt?: string;
  /** Seeds the composer (instead of sending) for the "Skip the form, start in Chat" link on form pills. */
  describePreload?: (v: PillValues) => string;
}

// ── value helpers ─────────────────────────────────────────────────────────

/** Trimmed string view of any field value (empty string when blank/unset). */
export function asText(v: PillFieldValue | undefined): string {
  if (typeof v === "string") return v.trim();
  if (typeof v === "number") return String(v);
  return "";
}

function asNumber(v: PillFieldValue | undefined, fallback: number): number {
  if (typeof v === "number" && Number.isFinite(v)) return v;
  if (typeof v === "string" && v.trim() !== "") {
    const n = Number(v);
    if (Number.isFinite(n)) return n;
  }
  return fallback;
}

function asRange(v: PillFieldValue | undefined): DateRangeValue {
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

export function getPill(id: string): ProtocolPill | undefined {
  return PROTOCOL_PILLS.find((p) => p.id === id);
}

// Joins "Label: value" clauses for fields the user actually filled, so empty
// optional inputs don't inject hollow directives the agent has to ignore.
function detailLine(parts: string[]): string {
  return parts.length ? ` ${parts.join(". ")}.` : "";
}

// ── catalog ─────────────────────────────────────────────────────────────

export const PROTOCOL_PILLS: ProtocolPill[] = [
  {
    id: "weekly",
    section: "Reporting",
    type: "form",
    icon: "bar-chart",
    title: "Weekly Performance Report",
    desc: "Templated DSP report — fill a few fields, preview, and send.",
    cta: "Run report",
    fields: [
      { key: "client", label: "Client name", type: "text", required: true, placeholder: "Meridian Auto" },
      { key: "elc", label: "Campaign code", type: "text", required: true, placeholder: "e.g. AUTO-2024" },
      { key: "mailbox", label: "Mailbox alias", type: "text", placeholder: "name@victoria.elcanotek.com" },
      { key: "kpi", label: "Primary KPI (name + formula)", type: "text", placeholder: "CTR = clicks / impressions" },
      { key: "recipient", label: "Recipient(s)", type: "text", placeholder: "name@client.com" },
      { key: "window", label: "Reporting window", type: "text", advanced: true, default: "Last full Mon–Sun" },
      {
        key: "breakout",
        label: "Breakout",
        type: "select",
        advanced: true,
        default: "Channel",
        options: ["Channel", "Campaign", "Creative", "Daypart"],
      },
      { key: "pacing", label: "Pacing target", type: "text", advanced: true, placeholder: "none" },
    ],
    promptTemplate: (v) => {
      const parts: string[] = [];
      if (asText(v.mailbox)) parts.push(`Mailbox: ${asText(v.mailbox)}`);
      if (asText(v.recipient)) parts.push(`Recipient(s): ${asText(v.recipient)}`);
      if (asText(v.kpi)) parts.push(`Primary KPI: ${asText(v.kpi)}`);
      if (asText(v.window)) parts.push(`Reporting window: ${asText(v.window)}`);
      if (asText(v.breakout)) parts.push(`Breakout: ${asText(v.breakout)}`);
      if (asText(v.pacing)) parts.push(`Pacing target: ${asText(v.pacing)}`);
      return (
        `Run the DSP reporting protocol for ${asText(v.client) || "{client}"} ` +
        `(${asText(v.elc) || "{campaign code}"}).` +
        detailLine(parts)
      );
    },
    describePreload: (v) =>
      `Weekly performance report for ${asText(v.client) || "a client"} ` +
      `(${asText(v.elc) || "campaign code"}) — `,
  },
  {
    id: "diagnostic",
    section: "Reporting",
    type: "conversation",
    optionalForm: true,
    icon: "activity",
    title: "Performance Diagnostic",
    desc: "Talk it through — I’ll ask a few questions, then dig into the numbers.",
    cta: "Run diagnostic",
    starterPrompt:
      "I’d like to run a performance diagnostic on a campaign. Ask me what you need — " +
      "which campaign (client + campaign code), the KPI that matters most, whether to include " +
      "unallocated conversions, and the date range — then dig into the numbers and tell me " +
      "what stands out.",
    fields: [
      { key: "client", label: "Client name", type: "text", placeholder: "Meridian Auto" },
      { key: "elc", label: "Campaign code", type: "text", placeholder: "e.g. AUTO-2024" },
      { key: "kpi", label: "Primary KPI", type: "text", placeholder: "CPA" },
      { key: "range", label: "Date range", type: "daterange" },
      { key: "unallocated", label: "Include unallocated conversions", type: "toggle", default: false },
    ],
    promptTemplate: (v) => {
      const parts: string[] = [];
      const client = asText(v.client);
      const elc = asText(v.elc);
      const campaign = client && elc ? `${client} (${elc})` : client || elc;
      if (campaign) parts.push(`Campaign: ${campaign}`);
      if (asText(v.kpi)) parts.push(`Primary KPI: ${asText(v.kpi)}`);
      const r = asRange(v.range);
      if (r.from || r.to) parts.push(`Date range: ${r.from || "?"} → ${r.to || "?"}`);
      parts.push(`Include unallocated conversions: ${v.unallocated ? "yes" : "no"}`);
      return (
        "Run a performance diagnostic." +
        detailLine(parts) +
        " Dig into the numbers and walk me through what stands out — wins, risks, and what to change."
      );
    },
  },
  {
    id: "wrap",
    section: "Reporting",
    type: "form",
    icon: "layers",
    title: "End-of-Campaign Wrap",
    desc: "Full-flight summary, optimizations, and the case for renewal.",
    cta: "Build wrap",
    fields: [
      { key: "client", label: "Client name", type: "text", required: true, placeholder: "Meridian Auto" },
      { key: "elc", label: "Campaign code", type: "text", required: true, placeholder: "e.g. AUTO-2024" },
      { key: "flight", label: "Full-flight dates", type: "daterange", required: true },
      { key: "audience", label: "Audience", type: "select", default: "Client", options: ["Client", "Prospect", "Internal"] },
      {
        key: "gammaDeck",
        label: "Also build a Gamma slide deck",
        type: "toggle",
        default: false,
      },
      { key: "year", label: "Campaign year", type: "number", advanced: true, default: new Date().getFullYear(), min: 2000 },
      { key: "format", label: "Deck export format", type: "select", advanced: true, default: "PPTX", options: ["PPTX", "PDF"] },
      { key: "logo", label: "Client logo URL", type: "text", advanced: true, placeholder: "https://… (optional)" },
    ],
    promptTemplate: (v) => {
      const flight = asRange(v.flight);
      const flightStr =
        flight.from || flight.to ? `${flight.from || "{start}"}–${flight.to || "{end}"}` : "the full flight";
      const audience = (asText(v.audience) || "Client").toLowerCase();
      let out =
        `Build an end-of-campaign wrap for ${asText(v.client) || "{client}"} ` +
        `(${asText(v.elc) || "{campaign code}"}), full flight ${flightStr}, for the ${audience} audience. ` +
        "Pull the campaign’s performance from the reports we have and summarize delivery vs. goal, " +
        "the trend across the flight, the optimizations we made and their impact, and the case for a " +
        "renewed or expanded flight.";
      if (v.gammaDeck) {
        const fmt = (asText(v.format) || "PPTX").toUpperCase();
        const year = asNumber(v.year, new Date().getFullYear());
        const logo = asText(v.logo);
        out +=
          ` Then assemble that into the Campaign Wrap-Up presentation deck via Gamma ` +
          `(export as ${fmt}, campaign year ${year}${logo ? `, client logo ${logo}` : ""}).`;
      } else {
        out += " Deliver it as a written end-of-campaign summary.";
      }
      return out;
    },
    describePreload: (v) =>
      `End-of-campaign wrap for ${asText(v.client) || "a client"} ` +
      `(${asText(v.elc) || "campaign code"}) — `,
  },
  {
    id: "optimization",
    section: "Optimization",
    type: "form",
    icon: "target",
    title: "Optimization Report",
    desc: "Ranked recommendations from the optimizations you’ve logged.",
    cta: "Generate report",
    fields: [
      { key: "alias", label: "Mailbox alias", type: "text", required: true, placeholder: "name@victoria.elcanotek.com" },
      { key: "recipient", label: "Recipient(s)", type: "text", required: true, placeholder: "name@client.com" },
      { key: "elc", label: "Campaign code", type: "text", placeholder: "e.g. AUTO-2024 (optional)" },
      { key: "lookback", label: "Lookback days", type: "number", advanced: true, default: 14, min: 1 },
    ],
    promptTemplate: (v) =>
      `Run the optimization protocol for emails sent to '${asText(v.alias) || "{mailbox}"}' ` +
      `and send recommendations to '${asText(v.recipient) || "{recipient}"}'.` +
      (asText(v.elc) ? ` Campaign code: ${asText(v.elc)}.` : "") +
      ` Lookback: ${asNumber(v.lookback, 14)}d.`,
    describePreload: (v) =>
      `Optimization recommendations from ${asText(v.alias) || "my mailbox"} — `,
  },
];
