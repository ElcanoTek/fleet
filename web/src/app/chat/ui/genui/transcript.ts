// Transcript-derived generative-UI state. A card has no server row of its
// own: its spec IS the show_ui tool call's input (persisted with the turn),
// its submission IS a later user message (parseSubmissionMessage), and
// "replaced" IS a later card's `replaces`. Deriving all three from the
// messages means a reload and a second tab agree on every card's state with
// nothing extra to store.

import { createContext } from "react";
import type { Message, ToolCall } from "../history";
import {
  parseCardSpec,
  parseReplyMessage,
  parseSubmissionMessage,
  SHOW_UI_TOOL,
  walkInputs,
  type CardSpec,
  type Reply,
  type Submission,
} from "./model";

export type GenUiState = {
  cards: Map<string, CardSpec>;
  /**
   * cardId → the tool call that owns it: the latest one with that id. Card
   * ids are the provider's tool-call ids, which are only promised to pair a
   * call with its result; if a later call reuses one, the newer card owns
   * the id (it is the card_id the model was last told about), and the older
   * card renders as replaced from its own spec, never the newer card's spec,
   * answers, drafts or holds.
   */
  owners: Map<string, ToolCall>;
  submissions: Map<string, Submission>;
  superseded: Set<string>;
  /**
   * cardId → the quick reply ("message" action) sent from it, identified by
   * the "[UI reply] card=… action=…" marker the button writes — never by
   * matching text, so typing the same words yourself locks nothing.
   */
  replies: Map<string, Reply>;
};

export const EMPTY_GENUI_STATE: GenUiState = {
  cards: new Map(),
  owners: new Map(),
  submissions: new Map(),
  superseded: new Set(),
  replies: new Map(),
};

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

// Parses are cached per message / tool-call object. Derivation runs on every
// streamed delta (each one replaces the messages array), while the user's
// card answers (up to ~1 MiB each) and finished card specs never change: the
// objects carrying them are reused, so each is parsed once. The cached entry
// remembers its source text, so an object whose text did change is reparsed.
type Parsed<T> = { src: string; value: T };
const answerCache = new WeakMap<object, Parsed<{ sub: Submission | null; reply: Reply | null }>>();
const specCache = new WeakMap<object, Parsed<CardSpec | null>>();

function cached<T>(cache: WeakMap<object, Parsed<T>>, key: object, src: string, parse: (s: string) => T): T {
  const hit = cache.get(key);
  if (hit && hit.src === src) return hit.value;
  const value = parse(src);
  cache.set(key, { src, value });
  return value;
}

function parseAnswer(content: string): { sub: Submission | null; reply: Reply | null } {
  const sub = parseSubmissionMessage(content);
  return { sub, reply: sub ? null : parseReplyMessage(content) };
}

/**
 * A user message's card answer, parsed once per message object (the same
 * cache deriveGenUiState reads): the bubble rerenders on every streamed token
 * of the reply below it, and an answer can be near 1 MiB.
 */
export function answerOf(m: Message): { sub: Submission | null; reply: Reply | null } {
  return cached(answerCache, m, m.content, parseAnswer);
}

/** A card tool call's own spec (parsed once per tool-call object). */
export function cardSpecOf(tc: ToolCall): CardSpec | null {
  return cached(specCache, tc, tc.input, parseCardSpec);
}

export function deriveGenUiState(messages: Message[]): GenUiState {
  const cards = new Map<string, CardSpec>();
  const owners = new Map<string, ToolCall>();
  const submissions = new Map<string, Submission>();
  const superseded = new Set<string>();
  const replies = new Map<string, Reply>();
  for (let idx = 0; idx < messages.length; idx++) {
    const m = messages[idx];
    if (m.role === "user") {
      // A card message the server has not been shown to hold (notSent: its
      // POST was refused, never arrived, or was stopped first) is not an
      // answer — whether its slot has failed or recovery is still working
      // out what happened. The card stays editable and its sender reported
      // it unsent. Only a turn that went on to answer it — tool calls, or a
      // finished written reply — proves the server took it after all.
      const next = messages[idx + 1];
      const answered =
        !!next &&
        next.role === "assistant" &&
        ((next.toolCalls ?? []).length > 0 ||
          (next.state === "done" && !next.failed && !next.cancelled && !next.modelRequired && next.content.trim() !== ""));
      if (m.notSent && !answered) continue;
      // A card has ONE answer: the latest, whether a submission or a quick
      // reply (a submit, Edit and resend, then a quick reply ends on the
      // reply). Each kind replaces the other.
      const { sub, reply } = cached(answerCache, m, m.content, parseAnswer);
      if (sub) {
        submissions.set(sub.cardId, { ...sub, messageId: m.id });
        replies.delete(sub.cardId);
        continue;
      }
      if (reply) {
        replies.set(reply.cardId, { ...reply, messageId: m.id });
        submissions.delete(reply.cardId);
      }
      continue;
    }
    for (const tc of m.toolCalls ?? []) {
      if (!isRenderableCardCall(tc)) continue;
      const spec = cardSpecOf(tc);
      if (!spec) continue;
      // A reused id starts over: answers and replacements recorded so far
      // belonged to the earlier card with this id, not to this one.
      if (owners.has(tc.id)) {
        submissions.delete(tc.id);
        replies.delete(tc.id);
        superseded.delete(tc.id);
      }
      cards.set(tc.id, spec);
      owners.set(tc.id, tc);
      // A card never replaces itself (a refinement whose reused id equals
      // the one it names).
      if (spec.replaces && spec.replaces !== tc.id) superseded.add(spec.replaces);
    }
  }
  return { cards, owners, submissions, superseded, replies };
}

export const GenUiContext = createContext<GenUiState>(EMPTY_GENUI_STATE);

/** id → label for a card's inputs (repeater fields included). */
export function fieldLabels(spec: CardSpec | undefined): Map<string, string> {
  const out = new Map<string, string>();
  if (!spec) return out;
  const visit = (list: CardSpec["components"]) =>
    walkInputs(list, (c) => {
      if (c.id) out.set(c.id, typeof c.label === "string" && c.label.trim() ? c.label.trim() : c.id);
      if (c.type === "repeater" && Array.isArray(c.fields)) visit(c.fields as CardSpec["components"]);
    });
  visit(spec.components);
  return out;
}

// The bubble is a summary: each shown value is bounded (the full text is
// behind "Show what was sent"), so a long answer does not lay out in full.
const SUMMARY_VALUE_CHARS = 120;
const clip = (s: string) => (s.length > SUMMARY_VALUE_CHARS ? `${s.slice(0, SUMMARY_VALUE_CHARS)}…` : s);

/** One-line rendering of a submitted value for the sent bubble. */
export function summarizeValue(v: unknown): string {
  if (v === null || v === undefined || v === "") return "—";
  if (typeof v === "boolean") return v ? "Yes" : "No";
  if (typeof v === "number" || typeof v === "string") return clip(String(v));
  if (Array.isArray(v)) {
    if (v.every((x) => typeof x !== "object" || x === null)) {
      if (v.length === 0) return "—";
      const shown = v.slice(0, 8).map((x) => clip(String(x)));
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
