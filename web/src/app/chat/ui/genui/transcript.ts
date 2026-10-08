// Transcript-derived generative-UI state. A card has no server row of its
// own: its spec IS the show_ui tool call's input (persisted with the turn),
// its submission IS a later user message (parseSubmissionMessage), and
// "replaced" IS a later card's `replaces`. Deriving all three from the
// messages means a reload and a second tab agree on every card's state with
// nothing extra to store.

import { createContext } from "react";
import type { Message, ToolCall } from "../history";
import { parseCardSpec, parseSubmissionMessage, SHOW_UI_TOOL, walkInputs, type CardSpec, type Submission } from "./model";

export type GenUiState = {
  cards: Map<string, CardSpec>;
  submissions: Map<string, Submission>;
  superseded: Set<string>;
};

export const EMPTY_GENUI_STATE: GenUiState = { cards: new Map(), submissions: new Map(), superseded: new Set() };

/**
 * Whether a tool call should draw a card: a show_ui call the server accepted.
 * A refused spec (UI_INVALID, an error result) draws nothing — the model is
 * told to fix and re-call, and the retry is the card the user should see.
 * Only an explicit UI_DISPLAYED result counts: history reconstruction marks a
 * call with no stored result as done (a cancelled turn, a server stopped
 * between call and result), and such a spec never passed validation.
 */
export function isRenderableCardCall(tc: ToolCall): boolean {
  if (tc.name !== SHOW_UI_TOOL || tc.state !== "done") return false;
  return typeof tc.resultText === "string" && tc.resultText.startsWith("UI_DISPLAYED");
}

export function deriveGenUiState(messages: Message[]): GenUiState {
  const cards = new Map<string, CardSpec>();
  const submissions = new Map<string, Submission>();
  const superseded = new Set<string>();
  for (const m of messages) {
    if (m.role === "user") {
      const sub = parseSubmissionMessage(m.content);
      if (sub) submissions.set(sub.cardId, sub);
      continue;
    }
    for (const tc of m.toolCalls ?? []) {
      if (!isRenderableCardCall(tc)) continue;
      const spec = parseCardSpec(tc.input);
      if (!spec) continue;
      cards.set(tc.id, spec);
      if (spec.replaces) superseded.add(spec.replaces);
    }
  }
  return { cards, submissions, superseded };
}

export const GenUiContext = createContext<GenUiState>(EMPTY_GENUI_STATE);

/** id → label for a card's inputs (repeater fields included). */
export function fieldLabels(spec: CardSpec | undefined): Map<string, string> {
  const out = new Map<string, string>();
  if (!spec) return out;
  const visit = (list: CardSpec["components"]) =>
    walkInputs(list, (c) => {
      if (c.id) out.set(c.id, typeof c.label === "string" && c.label ? c.label : c.id);
      if (c.type === "repeater" && Array.isArray(c.fields)) visit(c.fields as CardSpec["components"]);
    });
  visit(spec.components);
  return out;
}

/** One-line rendering of a submitted value for the sent bubble. */
export function summarizeValue(v: unknown): string {
  if (v === null || v === undefined || v === "") return "—";
  if (typeof v === "boolean") return v ? "Yes" : "No";
  if (typeof v === "number" || typeof v === "string") return String(v);
  if (Array.isArray(v)) {
    if (v.every((x) => typeof x !== "object" || x === null)) {
      if (v.length === 0) return "—";
      const shown = v.slice(0, 8).map((x) => String(x));
      return v.length > 8 ? `${shown.join(", ")} +${v.length - 8} more` : shown.join(", ");
    }
    return `${v.length} item${v.length === 1 ? "" : "s"}`;
  }
  if (typeof v === "object") {
    const o = v as Record<string, unknown>;
    if (Array.isArray(o.include) || Array.isArray(o.exclude)) {
      const inc = Array.isArray(o.include) ? o.include.length : 0;
      const exc = Array.isArray(o.exclude) ? o.exclude.length : 0;
      return `${inc} included · ${exc} excluded`;
    }
  }
  return "…";
}
