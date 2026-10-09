"use client";

// GenerativeCard — renders a show_ui card (docs/GENERATIVE-UI.md) from the
// tool call's own arguments. The spec is model-authored and therefore
// untrusted: only the catalog below renders (anything else is skipped), every
// string is drawn as text (the one markdown surface disallows raw HTML and
// images, so nothing is fetched on render), links must be https and open with
// noopener, and the only "logic" is the side-effect-free expression language
// in expr.ts. The card never calls a tool or an API: a submit composes a user
// message (model.ts buildSubmissionMessage) and hands it to onSubmit, which
// sends it through the normal chat turn — so whatever the agent does next
// passes the same governed loop and approval cards as anything typed.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { evaluateSafe, renderTemplate, toText, truthy, type Scope } from "./expr";
import {
  buildReplyMessage,
  parseListText,
  scanListText,
  buildSubmissionMessage,
  children,
  collect,
  normalizeValues,
  isInput,
  isVisible,
  MAX_CHOICE_ITEMS,
  MAX_SUBMISSION_BYTES,
  submissionBytes,
  newItem,
  optionsOf,
  scopeFor,
  tabsOf,
  walkInputs,
  type Action,
  type CardSpec,
  type Component,
  type IncludeExclude,
  type Reply,
  type Submission,
  type Tone,
  type Values,
} from "./model";

// ───────────────────────── context ─────────────────────────

// disabled: the enclosing repeater is disabled, so its fields are too.
type ItemCtx = { repeater: string; index: number; item: Values; disabled?: boolean };

type CardCtx = {
  cardId: string;
  values: Values;
  locked: boolean;
  /** Errors shown inline, by path ("id" or "rep[i].field"). */
  errors: Record<string, string>;
  setField: (id: string, v: unknown, item?: ItemCtx) => void;
  setValues: (fn: (v: Values) => Values) => void;
  focusField: (path: string) => void;
  /** Paths of inputs currently shown and enabled — the ones Fix can reach. */
  visible: Set<string>;
  /** Bumped whenever the values are replaced wholesale (an adopted answer,
   *  a restored draft, a cancelled edit): per-item UI state then starts over. */
  epoch: number;
};

const Ctx = createContext<CardCtx | null>(null);
const ItemContext = createContext<ItemCtx | null>(null);

function useCard(): CardCtx {
  const c = useContext(Ctx);
  if (!c) throw new Error("GenerativeCard context missing");
  return c;
}

function useScope(): Scope {
  const { values } = useCard();
  const item = useContext(ItemContext);
  return useMemo(() => (item ? scopeFor(values, item.item, item.index) : scopeFor(values)), [values, item]);
}

const str = (v: unknown, fallback = ""): string => (typeof v === "string" ? v : fallback);
const numOr = (v: unknown): number | undefined => (typeof v === "number" && Number.isFinite(v) ? v : undefined);

const TONE: Record<Tone, { border: string; fg: string; bg: string }> = {
  neutral: { border: "var(--color-border-strong)", fg: "var(--color-text-secondary)", bg: "var(--color-overlay-soft)" },
  info: { border: "var(--color-accent)", fg: "var(--color-text-primary)", bg: "var(--color-overlay-soft)" },
  success: { border: "var(--color-success-border)", fg: "var(--color-success)", bg: "var(--color-success-soft)" },
  warning: { border: "var(--color-warning-border)", fg: "var(--color-warning-strong)", bg: "var(--color-warning-soft)" },
  danger: { border: "var(--color-danger-border)", fg: "var(--color-danger)", bg: "var(--color-danger-soft)" },
};

function tone(v: unknown, fallback: Tone = "neutral"): Tone {
  return typeof v === "string" && v in TONE ? (v as Tone) : fallback;
}

const inputClass =
  "w-full min-w-0 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-bg)] px-2.5 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none transition focus:border-[var(--color-accent)] disabled:opacity-60";
const chipButton =
  "rounded-full border border-[var(--color-border-strong)] px-2.5 py-1 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50";

// ───────────────────────── card shell ─────────────────────────

export type GenerativeCardProps = {
  cardId: string;
  spec: CardSpec;
  /** The user's submission of this card, found in the transcript. */
  submission?: Submission | null;
  /** The message action (quick reply) the user answered this card with. */
  reply?: Reply | null;
  /** A later card named this one in `replaces`. */
  superseded?: boolean;
  /** Shared / read-only transcripts: render, never submit. */
  readOnly?: boolean;
  /**
   * Namespaces this card's browser-stored draft and pending hold, normally by
   * conversation id. A branched conversation copies the tool call — card id
   * included — so without it a draft typed in one would appear in the other.
   */
  storageScope?: string;
  /**
   * The card predates the conversation's summary (shown only when the user
   * expands compacted history). The model no longer has its definition in
   * context, so an answer would arrive as ids and values it cannot read: the
   * card renders locked and says to ask for it again.
   */
  retired?: boolean;
  /** Sends a user turn (the submission message or a quick reply). */
  // Resolves false when the message was refused (nothing was sent).
  // onUnsent: called later if a send reported as held turns out never to
  // have reached the server (see submitPrompt), so the hold is released.
  // onHeld: the conversation and queue row the held send is watched by; the
  // card stores them with its hold so a reload can resume the watch.
  onSubmit?: (
    message: string,
    onUnsent?: () => void,
    onHeld?: (convId: string, submissionId: string) => void,
  ) => void | Promise<void | boolean>;
  /**
   * Resumes the server watch on a hold restored after a page load (the
   * in-page watcher did not survive it): onUnsent releases the hold if the
   * queue row turns out to be gone without reaching the transcript.
   */
  onResumeHeld?: (convId: string, text: string, submissionId: string, onUnsent: () => void) => void;
};

const DRAFT_PREFIX = "fleet.genui.draft.";

// Drafts are bounded: an unsent card is usually abandoned (replaced by a
// newer card, its conversation deleted), and nothing else would ever reclaim
// its entry. A draft older than DRAFT_TTL_MS is dropped, and at most
// MAX_DRAFTS are kept — the least recently edited go first.
const DRAFT_TTL_MS = 7 * 24 * 60 * 60 * 1000;
const MAX_DRAFTS = 20;

// A draft records the answer it edits (`after`, the answer key — "" before
// the first answer), so "Edit and resend" changes survive a remount too: a
// draft whose answer is still the transcript's reopens the card for editing,
// and one whose answer has moved on is spent. `sent` is the answer key of
// the resend made from it: once the transcript's answer is that one, the
// draft is spent even though resending identical values leaves `after`
// matching too.
// `after` and `sent` are stored as digests (keyDigest), not the answer keys
// themselves: an answer key embeds the whole answer, so a near-limit answer
// would otherwise be stored two or three times over in one draft.
type StoredDraft = { values: Values; at: number; after: string; cleared?: string[]; sent?: string };

// keyDigest stands in for an answer key in storage: its length and a 53-bit
// hash (cyrb53). "" (no answer yet) stays "". Recent digests are memoized:
// a key is hashed once, not on every render that compares it.
const digests = new Map<string, string>();
export function keyDigest(key: string): string {
  if (key === "") return "";
  let d = digests.get(key);
  if (d === undefined) {
    let h1 = 0xdeadbeef;
    let h2 = 0x41c6ce57;
    for (let i = 0; i < key.length; i++) {
      const c = key.charCodeAt(i);
      h1 = Math.imul(h1 ^ c, 2654435761);
      h2 = Math.imul(h2 ^ c, 1597334677);
    }
    h1 = Math.imul(h1 ^ (h1 >>> 16), 2246822507) ^ Math.imul(h2 ^ (h2 >>> 13), 3266489909);
    h2 = Math.imul(h2 ^ (h2 >>> 16), 2246822507) ^ Math.imul(h1 ^ (h1 >>> 13), 3266489909);
    d = `${key.length}:${(4294967296 * (2097151 & h2) + (h1 >>> 0)).toString(36)}`;
    if (digests.size >= 64) digests.delete(digests.keys().next().value as string);
    digests.set(key, d);
  }
  return d;
}

function readDraft(key: string): StoredDraft | null {
  const raw = window.localStorage.getItem(key);
  if (!raw) return null;
  const v = JSON.parse(raw) as Partial<StoredDraft> | null;
  if (!v || typeof v.at !== "number" || !v.values || typeof v.values !== "object" || Array.isArray(v.values)) return null;
  const cleared = Array.isArray(v.cleared) ? v.cleared.filter((x): x is string => typeof x === "string") : [];
  const sent = typeof v.sent === "string" && v.sent ? v.sent : undefined;
  return { values: v.values, at: v.at, after: typeof v.after === "string" ? v.after : "", cleared, sent };
}

// The draft's values, and the server field_errors the user already answered
// by editing (so a remount does not bring those errors back).
function loadDraft(cardId: string, answerKey: string): StoredDraft | null {
  const key = DRAFT_PREFIX + cardId;
  try {
    const d = readDraft(key);
    const k = keyDigest(answerKey);
    if (d && Date.now() - d.at <= DRAFT_TTL_MS && d.after === k && draftSent(cardId, d) !== k) return d;
    window.localStorage.removeItem(key);
    return null;
  } catch {
    return null;
  }
}

/** The stored draft, without dropping a stale one (see loadDraft). */
function peekDraft(storeId: string): StoredDraft | null {
  try {
    const d = readDraft(DRAFT_PREFIX + storeId);
    return d && Date.now() - d.at <= DRAFT_TTL_MS ? d : null;
  } catch {
    return null;
  }
}

/** Every other draft, newest first; expired or unreadable ones get at -1. */
function otherDrafts(keep: string): { key: string; at: number }[] {
  const out: { key: string; at: number }[] = [];
  for (let i = 0; i < window.localStorage.length; i++) {
    const key = window.localStorage.key(i);
    if (!key || !key.startsWith(DRAFT_PREFIX) || key === keep) continue;
    let d: StoredDraft | null = null;
    try {
      d = readDraft(key);
    } catch {
      d = null;
    }
    out.push({ key, at: !d || Date.now() - d.at > DRAFT_TTL_MS ? -1 : d.at });
  }
  return out.sort((a, b) => b.at - a.at);
}

/** Drops expired or unreadable drafts, then the oldest past MAX_DRAFTS - 1. */
function pruneDrafts(keep: string) {
  for (const [i, d] of otherDrafts(keep).entries()) {
    if (d.at < 0 || i >= MAX_DRAFTS - 1) window.localStorage.removeItem(d.key);
  }
}

function saveDraft(cardId: string, values: Values | null, after = "", cleared: Iterable<string> = []) {
  const key = DRAFT_PREFIX + cardId;
  try {
    if (!values) {
      sentMemory.delete(cardId);
      window.localStorage.removeItem(key);
      return;
    }
    // Pruning scans every draft: only when this card starts one, not per edit.
    if (window.localStorage.getItem(key) === null) pruneDrafts(key);
    sentMemory.delete(cardId);
    putDraft(key, JSON.stringify({ values, at: Date.now(), after: keyDigest(after), cleared: [...cleared] }));
  } catch {
    // Private mode: a draft is a convenience, never state we rely on.
  }
}

/** Writes a draft (or a hold); true once stored. */
function putDraft(key: string, json: string): boolean {
  try {
    window.localStorage.setItem(key, json);
    return true;
  } catch {
    // A long pasted list can fill the origin's quota. Only now, make room
    // by dropping other drafts, least recently edited first,
    // until it fits.
  }
  const others = otherDrafts(key);
  for (;;) {
    const victim = others.pop();
    if (!victim) return false;
    window.localStorage.removeItem(victim.key);
    try {
      window.localStorage.setItem(key, json);
      return true;
    } catch {
      // still full: drop the next one
    }
  }
}

// A draft's `sent` marker that could not be stored (the quota is full even
// after evicting other drafts): kept for this page's life, so a remount still
// sees the resend it records.
const sentMemory = new Map<string, string>();

/** Records (or, with "", clears) the answer key a draft was resent as. */
function markDraftSent(storeId: string, sentKey: string) {
  const key = DRAFT_PREFIX + storeId;
  const sent = keyDigest(sentKey);
  if (sent) sentMemory.set(storeId, sent);
  else sentMemory.delete(storeId);
  try {
    const d = readDraft(key);
    if (!d) return;
    if (putDraft(key, JSON.stringify({ ...d, sent: sent || undefined }))) sentMemory.delete(storeId);
  } catch {
    // A draft is a convenience; the in-memory marker still holds.
  }
}

/** The digest of the answer a draft was resent as (stored, or in memory). */
function draftSent(storeId: string, d: StoredDraft): string | undefined {
  return d.sent ?? sentMemory.get(storeId);
}

// An answer's values serialize to its identity key. The transcript caches
// each parsed answer (transcript.ts), so the same values object comes back on
// every streamed render; serializing a near-1 MiB answer once per object,
// not once per render, keeps a long reply smooth.
const valuesKeys = new WeakMap<object, string>();
function valuesKey(values: Values): string {
  let k = valuesKeys.get(values);
  if (k === undefined) {
    k = JSON.stringify(values);
    valuesKeys.set(values, k);
  }
  return k;
}

/** A transcript answer without its message id (see loadPending, loadDraft). */
function answerKeyOf(submission: Submission | null | undefined, reply: Reply | null | undefined): string {
  if (submission) return `s\u0000${submission.actionId}\u0000${valuesKey(submission.values)}`;
  if (reply) return `r\u0000${reply.actionId}`;
  return "";
}

const PENDING_PREFIX = "fleet.genui.pending.";
// A queued message normally echoes within a turn; past this a stale marker
// (a cancelled queue item, a closed tab) stops holding the card. It must
// outlast the longest turn an operator can configure (the per-turn limit is
// 30 minutes by default), or another tab could re-enable a card whose answer
// is still being processed; a hold that is truly stale has Unlock.
const PENDING_TTL_MS = 6 * 60 * 60 * 1000;

// A hold records the transcript answer it was armed after (`after`: that
// answer's action and values, "" for a first answer — message ids are not
// stable across a history reload, so they are left out). If the answer has
// changed by the time the card mounts, the held message echoed while the
// card was unmounted (virtualized off-screen) and the hold is spent. An
// identical resend cannot be told apart from the answer before it; that
// hold is shown only under Edit, can be dismissed, and expires.
// A hold is also kept in memory for the page's life: it guards against a
// duplicate queued answer, so it must survive a remount even when storage is
// full or blocked (localStorage then only carries it across a reload).
// `send` names the send that set the hold, so a late verdict about an older
// send (one the user unlocked and replaced) cannot release a newer hold.
// `watch` (set once the chat reports the send held) names the conversation,
// queue row and message text, so a reloaded page can ask the server about
// that row again instead of holding the card until the TTL.
type PendingWatch = { conv: string; sid: string; text: string };
const pendingMemory = new Map<
  string,
  { action: string; at: number; after: string; send?: string; watch?: PendingWatch }
>();

/** Forgets in-memory holds (tests; the page itself never needs to). */
export function resetPendingHolds() {
  pendingMemory.clear();
  sentMemory.clear();
}

function loadPending(cardId: string, currentKey: string): string | null {
  const mem = pendingMemory.get(cardId);
  if (mem) {
    if (Date.now() - mem.at <= PENDING_TTL_MS && mem.after === keyDigest(currentKey)) return mem.action;
    pendingMemory.delete(cardId);
  }
  try {
    const raw = window.localStorage.getItem(PENDING_PREFIX + cardId);
    if (!raw) return null;
    const v = JSON.parse(raw) as { action?: unknown; at?: unknown; after?: unknown };
    if (typeof v.action !== "string" || typeof v.at !== "number" || Date.now() - v.at > PENDING_TTL_MS) {
      window.localStorage.removeItem(PENDING_PREFIX + cardId);
      return null;
    }
    // A hold typed against another answer is not this tab's: its answer has
    // moved on here. It is left in place (not removed) for a tab whose
    // transcript has not caught up yet; the TTL clears it.
    return v.after === keyDigest(currentKey) ? v.action : null;
  } catch {
    return null;
  }
}

function savePending(cardId: string, action: string | null, afterKey = "", send?: string) {
  const after = keyDigest(afterKey);
  // Re-saving the same send's hold keeps the server watch it was given.
  const prev = send ? pendingWatch(cardId) : null;
  const watch = prev && prev.send === send ? prev.watch : undefined;
  if (action) pendingMemory.set(cardId, { action, at: Date.now(), after, send, watch });
  else pendingMemory.delete(cardId);
  try {
    // A hold is small, but a full quota would still refuse it and leave
    // other tabs unaware of the send: make room by dropping drafts.
    if (action) putDraft(PENDING_PREFIX + cardId, JSON.stringify({ action, at: Date.now(), after, send, watch }));
    else window.localStorage.removeItem(PENDING_PREFIX + cardId);
  } catch {
    // Convenience only, like drafts.
  }
}

// Send ids are unique within the page; a stored hold from an earlier page
// load carries the load's timestamp prefix, so it never matches either.
const PAGE_LOAD = Date.now().toString(36);
let sendSeq = 0;
function nextSendId(): string {
  sendSeq += 1;
  return `${PAGE_LOAD}-${sendSeq}`;
}

/** Adds the server watch to the hold `send` set, if that hold is current. */
function notePendingWatch(cardId: string, send: string, watch: PendingWatch) {
  const mem = pendingMemory.get(cardId);
  if (mem && mem.send === send) pendingMemory.set(cardId, { ...mem, watch });
  try {
    const raw = window.localStorage.getItem(PENDING_PREFIX + cardId);
    if (!raw) return;
    const v = JSON.parse(raw) as { send?: unknown };
    if (v.send !== send) return;
    putDraft(PENDING_PREFIX + cardId, JSON.stringify({ ...v, watch }));
  } catch {
    // Convenience only: without it a reload holds the card until the TTL.
  }
}

/** The stored hold's send id and server watch, if it has one. */
function pendingWatch(cardId: string): { send: string; watch: PendingWatch } | null {
  const ok = (w: unknown): w is PendingWatch =>
    !!w &&
    typeof (w as PendingWatch).conv === "string" &&
    typeof (w as PendingWatch).sid === "string" &&
    typeof (w as PendingWatch).text === "string";
  const mem = pendingMemory.get(cardId);
  if (mem) return mem.send && ok(mem.watch) ? { send: mem.send, watch: mem.watch } : null;
  try {
    const raw = window.localStorage.getItem(PENDING_PREFIX + cardId);
    const v = raw ? (JSON.parse(raw) as { send?: unknown; watch?: unknown }) : null;
    return typeof v?.send === "string" && ok(v.watch) ? { send: v.send, watch: v.watch } : null;
  } catch {
    return null;
  }
}

/** When the card's current hold expires (ms since epoch), if it has one. */
function pendingExpiry(cardId: string): number | null {
  const mem = pendingMemory.get(cardId);
  if (mem) return mem.at + PENDING_TTL_MS;
  try {
    const raw = window.localStorage.getItem(PENDING_PREFIX + cardId);
    const v = raw ? (JSON.parse(raw) as { at?: unknown }) : null;
    return typeof v?.at === "number" ? v.at + PENDING_TTL_MS : null;
  } catch {
    return null;
  }
}

/** The send that set the card's current hold, if any. */
function pendingSend(cardId: string): string | undefined {
  const mem = pendingMemory.get(cardId);
  if (mem) return mem.send;
  try {
    const raw = window.localStorage.getItem(PENDING_PREFIX + cardId);
    const v = raw ? (JSON.parse(raw) as { send?: unknown }) : null;
    return typeof v?.send === "string" ? v.send : undefined;
  } catch {
    return undefined;
  }
}

// How many frames focusField keeps revealing before it gives up: every
// container layer (the validator allows nesting 12 deep, MaxDepth in
// internal/genui/spec.go) mounts only after the one around it opens, and a
// layer can take a frame to commit and another to open.
const REVEAL_FRAMES = 2 * 12 + 2;

// A held send that never reached the server (detail: the card's store id).
const UNSENT_EVENT = "genui:unsent";

/** The server proved a held send never arrived: release that send's hold. */
function releaseHeld(storeId: string, send: string) {
  // Only this send's own hold: one the user unlocked (and perhaps replaced
  // with a newer send) is not this verdict's to release.
  if (pendingSend(storeId) !== send) return;
  // The instance that sent may have unmounted (virtualized transcript):
  // clear the stored hold, and tell whichever instance is mounted now.
  savePending(storeId, null);
  markDraftSent(storeId, "");
  window.dispatchEvent(new CustomEvent(UNSENT_EVENT, { detail: storeId }));
}

export default function GenerativeCard(props: GenerativeCardProps) {
  const { spec, superseded } = props;
  const [expanded, setExpanded] = useState(false);
  if (superseded && !expanded) {
    return (
      <div
        data-testid="genui-card"
        data-card-id={props.cardId}
        data-state="superseded"
        className="flex items-center gap-2 rounded-[var(--radius-lg)] border border-[var(--color-border)] px-3 py-2 text-[0.75rem] text-[var(--color-text-muted)]"
      >
        <span aria-hidden>◧</span>
        <span className="min-w-0 flex-1 truncate">
          {spec.title} · replaced by an updated card below
        </span>
        <button type="button" className="underline hover:text-[var(--color-text-primary)]" onClick={() => setExpanded(true)}>
          Show
        </button>
      </div>
    );
  }
  return <CardBody {...props} />;
}

function CardBody({
  cardId,
  spec,
  submission,
  reply,
  superseded,
  readOnly,
  retired,
  storageScope,
  onSubmit,
  onResumeHeld,
}: GenerativeCardProps) {
  // The key this card's draft and pending hold are stored under.
  const storeId = storageScope ? `${storageScope}:${cardId}` : cardId;
  // An unsent draft wins over the submitted values: it is an edit of that
  // submission in progress, so the card reopens for editing.
  const [initialDraft] = useState(() => (readOnly ? null : loadDraft(storeId, answerKeyOf(submission, reply))));
  const [values, setValuesState] = useState<Values>(() =>
    normalizeValues(spec, initialDraft?.values ?? (submission ? submission.values : null)),
  );
  const [epoch, setEpoch] = useState(0);
  const replaceValues = (v: Values) => {
    setValuesState(v);
    setEpoch((e) => e + 1);
  };
  const [editing, setEditing] = useState(() => !!submission && initialDraft !== null);
  const [sending, setSending] = useState<string | null>(null);
  const [showErrors, setShowErrors] = useState(false);
  const [confirming, setConfirming] = useState<Action | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  // Server-sent field_errors stay until the user edits that field.
  const [clearedServerErrors, setClearedServerErrors] = useState<Set<string>>(() => new Set(initialDraft?.cleared ?? []));
  // The same set, readable when a draft is saved (setField updates both).
  const clearedRef = useRef(clearedServerErrors);
  useEffect(() => {
    clearedRef.current = clearedServerErrors;
  }, [clearedServerErrors]);
  const rootRef = useRef<HTMLDivElement | null>(null);

  // The card locks on the TRANSCRIPT's submission, never optimistically: a
  // send that was refused (a rejected queue request, an upload failure) must
  // leave the card editable and its draft intact. When a new submission
  // lands (first send, or an edit resent), adopt its values and drop the
  // draft.
  // A quick reply answers the card as surely as a submit does: both lock it.
  const submittedAction = submission?.actionId ?? reply?.actionId ?? null;
  // Keyed by the transcript message, not the payload: resending identical
  // values is a new submission and must lock the card again.
  const submissionKey = submission
    ? `s\u0000${submission.messageId ?? ""}\u0000${submission.actionId}\u0000${valuesKey(submission.values)}`
    : reply
      ? `r\u0000${reply.messageId ?? ""}\u0000${reply.actionId}`
      : "";
  const answerKey = answerKeyOf(submission, reply);
  const answerKeyRef = useRef(answerKey);
  useEffect(() => {
    answerKeyRef.current = answerKey;
  }, [answerKey]);
  const [seenSubmission, setSeenSubmission] = useState(submissionKey);
  // The message id of the answer last seen, to tell a rollback (a withdrawn
  // resend uncovers an OLDER message) from a new answer with the same values.
  const [seenMessageId, setSeenMessageId] = useState<number | null>(submission?.messageId ?? reply?.messageId ?? null);
  const submissionKeyRef = useRef(submissionKey);
  useEffect(() => {
    submissionKeyRef.current = submissionKey;
  }, [submissionKey]);
  // An accepted submit that has not reached the transcript yet — a message
  // queued behind a running turn is accepted but not echoed until that turn
  // ends. Actions stay disabled so a second click cannot queue a duplicate.
  // Kept in localStorage too: the transcript is virtualized (an off-screen
  // card unmounts), and a remount must not re-enable a button whose message
  // is still queued.
  const [awaiting, setAwaitingState] = useState<string | null>(() =>
    readOnly ? null : loadPending(storeId, answerKey),
  );
  const setAwaiting = useCallback(
    (a: string | null, send?: string) => {
      setAwaitingState(a);
      if (!readOnly) savePending(storeId, a, answerKeyRef.current, send);
    },
    [storeId, readOnly],
  );
  if (seenSubmission !== submissionKey) {
    setSeenSubmission(submissionKey);
    if (submission || reply) {
      setEditing(false);
      setAwaitingState(null);
      // A confirmation opened before this answer arrived (from another tab,
      // say) belongs to an edit session that is over; so does a "Not sent"
      // notice from an earlier attempt — the answer is in now.
      setConfirming(null);
      setNotice(null);
      // The draft is NOT deleted here: an optimistic answer can still be
      // refused and vanish, and the user's values must survive that. A draft
      // records the answer it was typed against, so once this answer holds,
      // the next load finds it stale and drops it.
      // Only this tab's copy of the hold goes: the stored one stays for a tab
      // whose transcript has not caught up, and reads as stale here.
      pendingMemory.delete(storeId);
    }
    // The transcript can move BACK to an earlier answer: a resend from
    // "Edit and resend" was refused and its optimistic message withdrawn. If
    // the stored draft was typed against the answer now current, it is that
    // unsent edit — restore it and reopen editing, so the changes are not
    // replaced by the older values.
    // A card's first answer withdrawn (no answer left at all) is a rollback
    // too: the edits typed before it was sent must come back.
    const withdrawn = !submission && !reply && seenSubmission !== "";
    const rolledBack =
      withdrawn ||
      (!!submission && submission.messageId !== undefined && seenMessageId !== null && submission.messageId < seenMessageId);
    // Either kind of answer counts: a refused quick reply withdrawn over an
    // older submission is a rollback too.
    setSeenMessageId(submission?.messageId ?? reply?.messageId ?? null);
    const edit = rolledBack && !readOnly ? peekDraft(storeId) : null;
    if (edit && edit.after === keyDigest(answerKey) && draftSent(storeId, edit) !== keyDigest(answerKey)) {
      replaceValues(normalizeValues(spec, edit.values));
      // Editing is a mode of an answered card; with none left it is just open.
      setEditing(!!submission || !!reply);
      setClearedServerErrors(new Set(edit.cleared ?? []));
    } else if (submission) {
      replaceValues(normalizeValues(spec, submission.values));
    } else if (reply) {
      // A quick reply sends only its fixed text: show what a reload would
      // (the defaults), not edits that were never sent.
      replaceValues(normalizeValues(spec, null));
    }
  }
  const locked = !!readOnly || !!retired || !!superseded || (submittedAction !== null && !editing);
  const fieldsLocked = locked || awaiting !== null || sending !== null;

  // Another tab holding (or releasing) this card: storage events reach every
  // other tab, so the same card open twice cannot queue the same answer twice.
  useEffect(() => {
    if (readOnly) return;
    const key = PENDING_PREFIX + storeId;
    const onStorage = (e: StorageEvent) => {
      if (e.key !== key && e.key !== null) return;
      // This tab's in-memory copy is stale once another tab wrote the key.
      pendingMemory.delete(storeId);
      setAwaitingState(loadPending(storeId, answerKeyRef.current));
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, [storeId, readOnly]);

  // A hold restored from an earlier page load lost the in-page watcher that
  // would release it if its queue row were removed: resume that watch.
  useEffect(() => {
    if (!awaiting || readOnly || !onResumeHeld) return;
    const held = pendingWatch(storeId);
    if (!held || held.send.startsWith(`${PAGE_LOAD}-`)) return;
    const { send, watch } = held;
    onResumeHeld(watch.conv, watch.text, watch.sid, () => releaseHeld(storeId, send));
    // The resume is keyed by the held send; onResumeHeld is re-created
    // every render and dedupes by queue row itself.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [awaiting, storeId, readOnly]);

  // A hold lasts PENDING_TTL_MS. Checked only on load, one that expired while
  // the card stayed mounted (its sender gone after a reload, the answer lost)
  // would lock the card for good: re-check it when it is due.
  useEffect(() => {
    if (!awaiting || readOnly) return;
    const due = pendingExpiry(storeId);
    if (due === null) return;
    const timer = window.setTimeout(
      () => setAwaitingState(loadPending(storeId, answerKeyRef.current)),
      Math.max(0, due - Date.now()) + 1000,
    );
    return () => window.clearTimeout(timer);
  }, [awaiting, storeId, readOnly]);

  useEffect(() => {
    if (!awaiting) return;
    const onUnsent = (e: Event) => {
      if ((e as CustomEvent<string>).detail !== storeId) return;
      setAwaitingState(null);
      setNotice("Not sent. Try again.");
    };
    window.addEventListener(UNSENT_EVENT, onUnsent);
    return () => window.removeEventListener(UNSENT_EVENT, onUnsent);
  }, [awaiting, storeId]);

  const setValues = useCallback(
    (fn: (v: Values) => Values) => {
      setValuesState((prev) => {
        const next = fn(prev);
        if (!readOnly) saveDraft(storeId, next, answerKeyRef.current, clearedRef.current);
        return next;
      });
    },
    [storeId, readOnly],
  );

  const setField = useCallback(
    (id: string, v: unknown, item?: ItemCtx) => {
      const path = item ? `${item.repeater}[${item.index}].${id}` : id;
      setNotice(null);
      const n = new Set(clearedRef.current).add(path);
      // Editing an item also answers an error on the repeater as a whole
      // ("lines"), which a fixed-size repeater could otherwise never clear.
      // A separate key, so sibling items' errors stay.
      if (item) n.add(`${item.repeater}\u0000root`);
      clearedRef.current = n;
      setClearedServerErrors(n);
      setValues((prev) => {
        if (!item) return { ...prev, [id]: v };
        const arr = Array.isArray(prev[item.repeater]) ? [...(prev[item.repeater] as Values[])] : [];
        arr[item.index] = { ...(arr[item.index] ?? {}), [id]: v };
        return { ...prev, [item.repeater]: arr };
      });
    },
    [setValues],
  );

  const { values: submitValues, errors: liveErrors, visible } = useMemo(() => collect(spec, values), [spec, values]);

  // A server error clears when its field is edited, or — for a repeater
  // field — when the repeater itself changes shape (add / duplicate /
  // remove re-index the items, so "lines[2].cpm" no longer names the row
  // the agent meant).
  const serverErrorCleared = useCallback(
    (field: string) =>
      clearedServerErrors.has(field) ||
      (field.includes("[") ? clearedServerErrors.has(field.split("[")[0]) : clearedServerErrors.has(`${field}\u0000root`)),
    [clearedServerErrors],
  );

  const errors = useMemo(() => {
    const out: Record<string, string> = {};
    if (!locked) {
      for (const fe of spec.field_errors ?? []) {
        if (!serverErrorCleared(fe.field) && typeof fe.message === "string") out[fe.field] = fe.message;
      }
    }
    if (showErrors && !locked) Object.assign(out, liveErrors);
    return out;
  }, [spec.field_errors, serverErrorCleared, showErrors, liveErrors, locked]);

  const focusField = useCallback((path: string) => {
    const root = rootRef.current;
    if (!root) return;
    const sel = `[data-genui-field="${CSS.escape(path)}"]`;
    // A field inside an inactive tab is not mounted; ask the tabs to reveal it.
    // Tabs, collapsed sections and closed repeater items listen and open.
    // Containers can nest (a collapsed section inside an inactive tab): the
    // inner one only mounts after the outer opens, so the event is sent
    // again each frame until the field is in the DOM.
    const reveal = () => root.dispatchEvent(new CustomEvent("genui:reveal", { detail: path }));
    reveal();
    let tries = 0;
    const find = () => {
      const el = root.querySelector<HTMLElement>(sel);
      if (!el) {
        if (++tries < REVEAL_FRAMES) {
          reveal();
          window.requestAnimationFrame(find);
        }
        return;
      }
      el.scrollIntoView?.({ block: "center", behavior: "smooth" });
      el.querySelector<HTMLElement>("input,select,textarea,button")?.focus({ preventScroll: true });
    };
    window.requestAnimationFrame(find);
  }, []);

  const ctx = useMemo<CardCtx>(
    // Fields freeze while an answer is on its way too (sending, or held
    // until it reaches the transcript): an edit made then would be typed
    // against the answer being replaced and lost when it lands.
    () => ({ cardId, values, locked: fieldsLocked, errors, setField, setValues, focusField, visible, epoch }),
    [cardId, values, fieldsLocked, errors, setField, setValues, focusField, visible, epoch],
  );

  const scope = scopeFor(values);

  const send = async (action: Action) => {
    if (!onSubmit) return;
    // A direct send resolves only after the whole turn streams, by which
    // time its message has usually echoed (and cleared any hold). Hold only
    // if the transcript has not moved on since the click.
    const keyAtClick = submissionKeyRef.current;
    setConfirming(null);
    setNotice(null);
    setSending(action.id);
    // Takes back the hold this send published, if it is still this send's.
    let release = () => {};
    try {
      const message =
        action.kind === "message"
          ? buildReplyMessage(cardId, action.id, str(action.message))
          : buildSubmissionMessage(cardId, action.id, submitValues);
      const bytes = submissionBytes(message);
      if (bytes > MAX_SUBMISSION_BYTES) {
        setNotice(
          `Too large to send (${Math.ceil(bytes / 1024).toLocaleString()} KB; the chat takes up to ${(MAX_SUBMISSION_BYTES / 1024).toLocaleString()} KB). Shorten the longest list.`,
        );
        return;
      }
      const sentKey =
        action.kind === "message"
          ? answerKeyOf(null, { cardId, actionId: action.id, text: "" })
          : answerKeyOf({ cardId, actionId: action.id, values: submitValues }, null);
      if (!readOnly) markDraftSent(storeId, sentKey);
      const sendId = nextSendId();
      // Publish the hold before the send, not after: a direct send resolves
      // only once its whole turn has run, and the same card open in another
      // tab must lock now (storage events reach it), or a click there could
      // queue a duplicate. Refused, it is taken back below.
      if (!readOnly) savePending(storeId, action.id, answerKeyRef.current, sendId);
      release = () => {
        if (!readOnly && pendingSend(storeId) === sendId) savePending(storeId, null);
      };
      const accepted = await onSubmit(
        message,
        () => {
          if (!readOnly) releaseHeld(storeId, sendId);
        },
        (conv, sid) => {
          if (!readOnly) notePendingWatch(storeId, sendId, { conv, sid, text: message });
        },
      );
      if (accepted === false) {
        release();
        if (!readOnly) markDraftSent(storeId, "");
        setNotice("Not sent. Try again.");
      } else if (submissionKeyRef.current === keyAtClick) {
        // Hold the actions until the message reaches the transcript (a
        // queued message is accepted long before it is echoed), so a second
        // click cannot queue a duplicate — quick replies included.
        setAwaiting(action.id, sendId);
      }
    } catch {
      release();
      if (!readOnly) markDraftSent(storeId, "");
      setNotice("Could not send. Try again.");
    } finally {
      setSending(null);
    }
  };

  // Every path that blocks a validated submit: the live checks, plus server
  // field_errors the user has not edited yet (a value the agent just reported
  // as failing a live-system check must be changed before it is resent).
  const blockingPaths = () => {
    const paths = Object.keys(liveErrors);
    for (const fe of spec.field_errors ?? []) {
      // Only a field the user can still see and fix may block.
      if (visible.has(fe.field) && !serverErrorCleared(fe.field) && !paths.includes(fe.field)) paths.push(fe.field);
    }
    return paths;
  };

  /** Runs the action's gates; true when it may proceed. */
  const passesGates = (action: Action): boolean => {
    if (!isVisible(action, scope)) return false;
    if (typeof action.disabled_if === "string" && truthy(evaluateSafe(action.disabled_if, scope, false))) return false;
    if (action.kind !== "message" && action.validate !== false) {
      const paths = blockingPaths();
      if (paths.length > 0) {
        setShowErrors(true);
        setNotice(paths.length === 1 ? "Fix 1 field to continue." : `Fix ${paths.length} fields to continue.`);
        focusField(paths[0]);
        return false;
      }
    }
    return true;
  };

  const onAction = (action: Action) => {
    if (!passesGates(action)) return;
    if (typeof action.confirm === "string" && action.confirm.trim()) {
      setConfirming(action);
      return;
    }
    void send(action);
  };

  // The card stays editable while the confirmation is open, so the gates run
  // again on the values actually being sent.
  const onConfirm = (action: Action) => {
    // Another tab may have sent from this card while the confirmation was
    // open: its hold turns the buttons off here too, Yes included.
    const held = awaiting ?? (readOnly ? null : loadPending(storeId, answerKeyRef.current));
    if (held) {
      setAwaitingState(held);
      setConfirming(null);
      return;
    }
    if (!passesGates(action)) {
      setConfirming(null);
      return;
    }
    void send(action);
  };

  const actions = (spec.actions ?? []).filter((a) => isVisible(a, scope));
  // A confirmation whose action has since become hidden (its visible_if no
  // longer holds) closes rather than offering a Yes for an unavailable action.
  // So does one opened before another tab's send put this card on hold.
  const confirmingVisible = confirming && !awaiting && isVisible(confirming, scope) ? confirming : null;
  // Discard it outright, so it does not silently return if the action shows again.
  if (confirming && !confirmingVisible) setConfirming(null);
  // Keyboard focus follows the confirmation: the action row it replaces
  // unmounts the focused button, so focus moves to Yes when it opens and
  // back to the originating action when the user backs out.
  const confirmId = confirmingVisible?.id ?? null;
  const confirmYesRef = useRef<HTMLButtonElement | null>(null);
  const refocusRef = useRef<string | null>(null);
  useEffect(() => {
    if (confirmId) {
      confirmYesRef.current?.focus();
      return;
    }
    const back = refocusRef.current;
    refocusRef.current = null;
    if (back) rootRef.current?.querySelector<HTMLElement>(`[data-action-id="${CSS.escape(back)}"]`)?.focus();
  }, [confirmId]);
  const backOut = () => {
    refocusRef.current = confirming?.id ?? null;
    setConfirming(null);
  };
  // Escape backs out from anywhere in the card while the confirmation is
  // open: the fields stay editable, so focus may be in one of them.
  const backOutRef = useRef(backOut);
  useEffect(() => {
    backOutRef.current = backOut;
  });
  useEffect(() => {
    const root = rootRef.current;
    if (!confirmId || !root) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !e.defaultPrevented) {
        e.preventDefault();
        backOutRef.current();
      }
    };
    root.addEventListener("keydown", onKey);
    return () => root.removeEventListener("keydown", onKey);
  }, [confirmId]);

  const sentLabel = submittedAction
    ? (spec.actions ?? []).find((a) => a.id === submittedAction)?.label ?? submittedAction
    : null;

  return (
    <Ctx.Provider value={ctx}>
      <div
        ref={rootRef}
        data-testid="genui-card"
        data-card-id={cardId}
        data-state={superseded ? "superseded" : submittedAction && !editing ? "submitted" : "open"}
        className="min-w-0 rounded-[var(--radius-lg)] border border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--color-overlay-soft)_55%,transparent)] text-[0.8125rem] leading-[1.5] text-[var(--color-text-primary)]"
      >
        <div className="border-b border-[var(--color-border)] px-3.5 py-2.5">
          <div className="font-medium">{spec.title}</div>
          {spec.description ? (
            <div className="text-[0.75rem] text-[var(--color-text-muted)]">{renderTemplate(spec.description, scope)}</div>
          ) : null}
        </div>
        {/* Inputs disable themselves when locked (InputField); layout controls
            — tabs, collapsible sections, repeater items — stay usable so a
            submitted card can still be read in full. */}
        <div className="grid min-w-0 gap-3 px-3.5 py-3">
          <Nodes list={spec.components} />
        </div>
        {actions.length > 0 || sentLabel || (retired && !readOnly && !superseded) ? (
          <div className="flex flex-col gap-2 border-t border-[var(--color-border)] px-3.5 py-2.5">
            {retired && !readOnly && !superseded && !sentLabel ? (
              <div data-testid="genui-retired" className="text-[0.75rem] text-[var(--color-text-muted)]">
                From before this conversation was summarized — ask the assistant to show it again to answer it.
              </div>
            ) : null}
            {locked && sentLabel && !superseded ? (
              <div className="flex flex-wrap items-center gap-2 text-[0.75rem] text-[var(--color-text-muted)]">
                <span data-testid="genui-submitted">Sent · {sentLabel}</span>
                {!readOnly && !retired && onSubmit && submission && (spec.actions ?? []).some((a) => a.kind !== "message") ? (
                  <button type="button" className="underline hover:text-[var(--color-text-primary)]" onClick={() => setEditing(true)}>
                    Edit and resend
                  </button>
                ) : null}
              </div>
            ) : null}
            {!locked && awaiting ? (
              <div className="flex flex-wrap items-center gap-2 text-[0.75rem] text-[var(--color-text-muted)]" role="status">
                <span data-testid="genui-awaiting">
                  {/* Queued behind a running reply, or sent while the
                      connection dropped: either way it has not reached the
                      transcript yet. */}
                  Sent · {(spec.actions ?? []).find((a) => a.id === awaiting)?.label ?? awaiting} — waiting to reach the
                  assistant
                </span>
                <button type="button" className="underline hover:text-[var(--color-text-primary)]" onClick={() => setAwaiting(null)}>
                  Unlock
                </button>
              </div>
            ) : null}
            {!locked && confirmingVisible ? (
              <div className="flex flex-wrap items-center gap-2" role="alertdialog" aria-label="Confirm">
                <span className="text-[0.78rem]">{confirmingVisible.confirm}</span>
                <button
                  ref={confirmYesRef}
                  type="button"
                  className="rounded-full bg-[var(--color-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary)] hover:opacity-90"
                  onClick={() => onConfirm(confirmingVisible)}
                >
                  Yes, {confirmingVisible.label.toLowerCase()}
                </button>
                <button type="button" className={chipButton} onClick={backOut}>
                  Back
                </button>
              </div>
            ) : null}
            {!locked && !confirmingVisible && !awaiting && actions.length > 0 ? (
              <div className="flex flex-wrap items-center gap-2">
                {actions.map((a) => {
                  const disabled =
                    sending !== null ||
                    !onSubmit ||
                    (typeof a.disabled_if === "string" && truthy(evaluateSafe(a.disabled_if, scope, false)));
                  const style = a.style ?? (a.kind === "message" ? "secondary" : "primary");
                  return (
                    <button
                      key={a.id}
                      type="button"
                      data-action-id={a.id}
                      disabled={disabled}
                      onClick={() => onAction(a)}
                      className={
                        style === "primary"
                          ? "rounded-full bg-[var(--color-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary)] transition hover:opacity-90 disabled:opacity-50"
                          : style === "danger"
                            ? "rounded-full border border-[var(--color-danger-border)] px-3 py-1.5 text-[0.75rem] text-[var(--color-danger)] transition hover:opacity-90 disabled:opacity-50"
                            : chipButton + " py-1.5"
                      }
                    >
                      {sending === a.id ? "Sending…" : a.label}
                    </button>
                  );
                })}
                {editing ? (
                  <button type="button" className="text-[0.75rem] text-[var(--color-text-muted)] underline" onClick={() => {
                      // Back to exactly what was sent: unsent edits must not
                      // sit under the "Sent" label.
                      setEditing(false);
                      setShowErrors(false);
                      replaceValues(normalizeValues(spec, submission?.values));
                      saveDraft(storeId, null);
                      // The sent values are back, so are their server errors.
                      clearedRef.current = new Set();
                      setClearedServerErrors(clearedRef.current);
                    }}>
                    Cancel edit
                  </button>
                ) : null}
              </div>
            ) : null}
            {notice ? (
              <div role="status" className="text-[0.75rem] text-[var(--color-danger)]">
                {notice}
              </div>
            ) : null}
          </div>
        ) : null}
      </div>
    </Ctx.Provider>
  );
}

// ───────────────────────── node dispatch ─────────────────────────

function Nodes({ list }: { list: Component[] }) {
  return (
    <>
      {list.map((c, i) => (
        <Node key={`${c.type}-${c.id ?? i}`} c={c} />
      ))}
    </>
  );
}

function Node({ c }: { c: Component }) {
  const scope = useScope();
  if (!isVisible(c, scope)) return null;
  const R = RENDERERS[c.type];
  if (!R) return null; // unknown component: the server refuses these; draw nothing
  if (isInput(c)) return <InputField c={c} Render={R} />;
  return <R c={c} />;
}

type Renderer = (p: { c: Component; value?: unknown; onChange?: (v: unknown) => void; inputId?: string }) => ReactNode;

// ───────────────────────── layout ─────────────────────────

/** The input id a field path starts with ("lines[2].cpm" → "lines"). */
const rootOfPath = (path: string) => path.split(/[[.]/)[0];

/**
 * The input id a container should look a field path up by. At card level
 * that is the path's root ("lines[2].cpm" → "lines", owned by whatever holds
 * the repeater). Inside a repeater item it is the field's own id, and only
 * for this item's paths ("lines[2].cpm" → "cpm" in item 2, nothing in item
 * 0), since a section or tabs in the repeater's fields owns item fields.
 */
function containerKey(path: string, item: ItemCtx | null): string | null {
  if (!item) return rootOfPath(path);
  const prefix = `${item.repeater}[${item.index}].`;
  return path.startsWith(prefix) ? rootOfPath(path.slice(prefix.length)) : null;
}

/**
 * Subscribe a container to the card's "genui:reveal" event (dispatched by
 * focusField with the target field path) so collapsed content opens before
 * the field is looked up.
 */
function useReveal(ref: { current: HTMLElement | null }, onReveal: (path: string) => void) {
  const handler = useRef(onReveal);
  useEffect(() => {
    handler.current = onReveal;
  });
  useEffect(() => {
    const card = ref.current?.closest("[data-testid='genui-card']");
    if (!card) return;
    const listener = (e: Event) => handler.current((e as CustomEvent<string>).detail);
    card.addEventListener("genui:reveal", listener);
    return () => card.removeEventListener("genui:reveal", listener);
  }, [ref]);
}

/** Every input id under a component list, repeater fields included. */
function idsUnder(list: Component[]): Set<string> {
  const ids = new Set<string>();
  walkInputs(list, (f) => {
    if (f.id) ids.add(f.id);
    if (f.type === "repeater") walkInputs(children(f, "fields"), (g) => g.id && ids.add(g.id));
  });
  return ids;
}

function Section({ c }: { c: Component }) {
  const scope = useScope();
  const collapsible = c.collapsible === true;
  const [open, setOpen] = useState(!(collapsible && c.collapsed === true));
  // A collapsible section always has a toggle; the validator requires a
  // title, and the renderer still never strands content behind no header.
  const title = str(c.title).trim() || (collapsible ? "Details" : "");
  const ref = useRef<HTMLElement | null>(null);
  const owned = useMemo(() => idsUnder(children(c)), [c]);
  const item = useContext(ItemContext);
  useReveal(ref, (path) => {
    const key = containerKey(path, item);
    if (key !== null && owned.has(key)) setOpen(true);
  });
  return (
    <section ref={ref} className="grid min-w-0 gap-2.5 rounded-[var(--radius-md)] border border-[var(--color-border)] p-3">
      {title || c.description ? (
        <div>
          {collapsible ? (
            <button
              type="button"
              aria-expanded={open}
              onClick={() => setOpen((o) => !o)}
              className="flex w-full items-center gap-1.5 text-left font-medium"
            >
              <span aria-hidden className="text-[0.7rem] text-[var(--color-text-muted)]">
                {open ? "▾" : "▸"}
              </span>
              {title}
            </button>
          ) : title ? (
            <div className="font-medium">{title}</div>
          ) : null}
          {c.description ? (
            <div className="text-[0.75rem] text-[var(--color-text-muted)]">{renderTemplate(str(c.description), scope)}</div>
          ) : null}
        </div>
      ) : null}
      {open ? <Nodes list={children(c)} /> : null}
    </section>
  );
}

function Columns({ c }: { c: Component }) {
  const kids = children(c);
  const cols = Math.min(4, Math.max(1, kids.length));
  const grid = ["", "sm:grid-cols-1", "sm:grid-cols-2", "sm:grid-cols-3", "sm:grid-cols-4"][cols];
  return (
    <div className={`grid min-w-0 grid-cols-1 gap-3 ${grid}`}>
      {kids.map((k, i) => (
        <div key={i} className="grid min-w-0 content-start gap-3">
          <Node c={k} />
        </div>
      ))}
    </div>
  );
}

function Tabs({ c }: { c: Component }) {
  const tabs = tabsOf(c);
  const steps = c.variant === "steps";
  const [active, setActive] = useState(0);
  const { errors } = useCard();
  const baseId = useId();

  // Which tab owns each input id, for error dots and reveal-on-jump.
  const owner = useMemo(() => {
    const m = new Map<string, number>();
    tabs.forEach((t, i) =>
      walkInputs(t.children, (f) => {
        if (f.id) m.set(f.id, i);
        if (f.type === "repeater") walkInputs(children(f, "fields"), (g) => g.id && m.set(g.id, i));
      }),
    );
    return m;
  }, [tabs]);
  const item = useContext(ItemContext);
  const ownerOf = (path: string) => {
    const key = containerKey(path, item);
    return key === null ? undefined : owner.get(key);
  };
  const errCount = (i: number) => Object.keys(errors).filter((p) => ownerOf(p) === i).length;

  const ref = useRef<HTMLDivElement | null>(null);
  useReveal(ref, (path) => {
    const i = ownerOf(path);
    if (i !== undefined) setActive(i);
  });

  const current = tabs[Math.min(active, tabs.length - 1)];
  if (!current) return null;
  return (
    <div ref={ref} className="grid min-w-0 gap-3">
      <div role="tablist" className="flex min-w-0 flex-wrap gap-1 border-b border-[var(--color-border)]">
        {tabs.map((t, i) => {
          const n = errCount(i);
          const selected = i === active;
          return (
            <button
              key={i}
              type="button"
              role="tab"
              id={`${baseId}-tab-${i}`}
              aria-selected={selected}
              aria-controls={`${baseId}-panel`}
              onClick={() => setActive(i)}
              className={[
                "-mb-px flex items-center gap-1.5 border-b-2 px-2.5 py-1.5 text-[0.78rem] transition",
                selected
                  ? "border-[var(--color-accent)] text-[var(--color-text-primary)]"
                  : "border-transparent text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]",
              ].join(" ")}
            >
              {steps ? (
                <span className="inline-flex size-4 items-center justify-center rounded-full border border-current text-[0.65rem]">
                  {i + 1}
                </span>
              ) : null}
              {t.label}
              {n > 0 ? (
                <span className="rounded-full bg-[var(--color-danger-soft)] px-1.5 text-[0.65rem] text-[var(--color-danger)]" aria-label={`${n} to fix`}>
                  {n}
                </span>
              ) : null}
            </button>
          );
        })}
      </div>
      <div role="tabpanel" id={`${baseId}-panel`} aria-labelledby={`${baseId}-tab-${active}`} className="grid min-w-0 gap-3">
        <Nodes list={current.children} />
      </div>
      {steps && tabs.length > 1 ? (
        <div className="flex items-center gap-2">
          {active > 0 ? (
            <button type="button" className={chipButton} onClick={() => setActive(active - 1)}>
              ← {tabs[active - 1].label}
            </button>
          ) : null}
          {active < tabs.length - 1 ? (
            <button type="button" className={chipButton} onClick={() => setActive(active + 1)}>
              {tabs[active + 1].label} →
            </button>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

// ───────────────────────── display ─────────────────────────

function Heading({ c }: { c: Component }) {
  const scope = useScope();
  return <div className="text-[0.875rem] font-semibold">{renderTemplate(str(c.text), scope)}</div>;
}

function Markdown({ text }: { text: string }) {
  return (
    <div className="genui-md min-w-0 break-words [&_a]:underline [&_code]:font-mono [&_ol]:list-decimal [&_ol]:pl-5 [&_p]:my-1 [&_table]:text-[0.78rem] [&_td]:border [&_td]:border-[var(--color-border)] [&_td]:px-1.5 [&_th]:border [&_th]:border-[var(--color-border)] [&_th]:px-1.5 [&_ul]:list-disc [&_ul]:pl-5">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        // No raw HTML (react-markdown's default), and no images: a card must
        // never make the browser fetch a model-chosen URL on render.
        disallowedElements={["img"]}
        urlTransform={(url) => (/^https:\/\//i.test(url) ? url : "")}
        components={{
          a: ({ href, children: kids }) =>
            href ? (
              <a href={href} target="_blank" rel="noopener noreferrer nofollow">
                {kids}
              </a>
            ) : (
              <span>{kids}</span>
            ),
        }}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
}

function Text({ c }: { c: Component }) {
  const scope = useScope();
  const text = renderTemplate(str(c.text), scope);
  const muted = c.tone === "muted";
  return (
    <div className={muted ? "text-[0.78rem] text-[var(--color-text-muted)]" : "text-[var(--color-text-secondary)]"}>
      {c.markdown === true ? <Markdown text={text} /> : <div className="whitespace-pre-wrap break-words">{text}</div>}
    </div>
  );
}

function Callout({ c }: { c: Component }) {
  const scope = useScope();
  const t = TONE[tone(c.tone, "info")];
  return (
    <div className="rounded-[var(--radius-md)] border px-3 py-2" style={{ borderColor: t.border, background: t.bg }} role="note">
      {c.title ? (
        <div className="font-medium" style={{ color: t.fg }}>
          {str(c.title)}
        </div>
      ) : null}
      <div className="whitespace-pre-wrap break-words text-[var(--color-text-secondary)]">{renderTemplate(str(c.text), scope)}</div>
    </div>
  );
}

function objs(v: unknown): Record<string, unknown>[] {
  return Array.isArray(v) ? (v.filter((x) => x && typeof x === "object" && !Array.isArray(x)) as Record<string, unknown>[]) : [];
}

function Badges({ c }: { c: Component }) {
  const scope = useScope();
  return (
    <div className="flex flex-wrap gap-1.5">
      {objs(c.items).map((b, i) => {
        const t = TONE[tone(b.tone)];
        return (
          <span key={i} className="rounded-full border px-2 py-0.5 text-[0.72rem]" style={{ borderColor: t.border, color: t.fg, background: t.bg }}>
            {renderTemplate(str(b.text), scope)}
          </span>
        );
      })}
    </div>
  );
}

function Stat({ c }: { c: Component }) {
  const scope = useScope();
  const t = TONE[tone(c.tone)];
  return (
    <div className="min-w-0 rounded-[var(--radius-md)] border border-[var(--color-border)] px-3 py-2">
      <div className="text-[0.72rem] text-[var(--color-text-muted)]">{str(c.label).trim()}</div>
      <div
        className="truncate text-[1.25rem] font-semibold tabular-nums"
        style={tone(c.tone) === "neutral" ? undefined : { color: t.fg }}
        data-testid="genui-stat-value"
      >
        {renderTemplate(str(c.value), scope) || "—"}
      </div>
      {c.caption ? <div className="text-[0.72rem] text-[var(--color-text-muted)]">{renderTemplate(str(c.caption), scope)}</div> : null}
    </div>
  );
}

function Facts({ c }: { c: Component }) {
  const scope = useScope();
  return (
    <dl className="grid min-w-0 grid-cols-[minmax(6rem,auto)_1fr] gap-x-3 gap-y-1">
      {objs(c.items).map((f, i) => (
        <div key={i} className="contents">
          <dt className="text-[var(--color-text-muted)]">{str(f.label)}</dt>
          <dd className="m-0 min-w-0 break-words">{renderTemplate(str(f.value), scope) || "—"}</dd>
        </div>
      ))}
    </dl>
  );
}

function cellText(v: unknown): string {
  if (v === null || v === undefined) return "—";
  if (typeof v === "number") return Number.isInteger(v) ? v.toLocaleString() : toText(v);
  if (typeof v === "boolean") return v ? "Yes" : "No";
  return String(v);
}

function Table({ c, value, onChange, inputId }: Parameters<Renderer>[0]) {
  // A selectable table is an input: InputField wraps it and binds value /
  // onChange (to the card, or to the current repeater item) and draws its
  // label and errors. A display-only table renders bare.
  const { locked } = useCard();
  // One radio group per single-select table, so arrow keys move between rows
  // and assistive technology announces the rows as one exclusive choice.
  const groupName = useId();
  const item = useContext(ItemContext);
  const wrapped = onChange !== undefined;
  const cols = objs(c.columns).map((col) => ({
    key: str(col.key),
    label: str(col.label).trim() || str(col.key),
    align: col.align === "right" || col.align === "center" ? (col.align as string) : "left",
  }));
  const rows = objs(c.rows);
  const mode = c.select === "single" || c.select === "multi" ? (c.select as string) : "none";
  const rowKey = str(c.row_key);
  const selected = wrapped ? value : undefined;
  const isSel = (k: string) => (mode === "multi" ? Array.isArray(selected) && selected.includes(k) : selected === k);
  const toggle = (k: string) => {
    // The row is clickable markup, not a form control, so the disabled
    // fieldset around it does not stop it: check every disabling source.
    if (!wrapped || locked || c.disabled === true || item?.disabled === true) return;
    if (mode === "single") onChange?.(selected === k ? "" : k);
    else {
      const cur = Array.isArray(selected) ? (selected as string[]) : [];
      onChange?.(cur.includes(k) ? cur.filter((x) => x !== k) : [...cur, k]);
    }
  };
  // A selectable table is one question: name the group by the field's label
  // (InputField draws it), so its row checkboxes are told apart from another
  // table's rows with the same keys.
  const group = wrapped && inputId ? { role: "group", id: inputId, tabIndex: -1, "aria-labelledby": `${inputId}-label` } : {};
  return (
    <div className="grid min-w-0 gap-1" {...group}>
      {!wrapped && c.label ? <div className="text-[0.78rem] font-medium">{str(c.label).trim()}</div> : null}
      <div className="max-h-[22rem] min-w-0 overflow-auto rounded-[var(--radius-md)] border border-[var(--color-border)]">
        <table
          className="w-full border-collapse text-[0.78rem]"
          // Named by the field's label when it is an input, else by its own.
          {...(wrapped && inputId ? { "aria-labelledby": `${inputId}-label` } : { "aria-label": str(c.label).trim() || "Table" })}
        >
          <thead className="sticky top-0 bg-[var(--color-bg)]">
            <tr>
              {mode !== "none" ? <th className="w-8 border-b border-[var(--color-border)]" aria-label="Select" /> : null}
              {cols.map((col) => (
                <th
                  key={col.key}
                  className="border-b border-[var(--color-border)] px-2 py-1.5 font-medium text-[var(--color-text-muted)]"
                  style={{ textAlign: col.align as "left" | "right" | "center" }}
                >
                  {col.label}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((r, i) => {
              // The validator requires a unique string row_key per row on a
              // selectable table; submit that exact value, never display text.
              const k = rowKey && typeof r[rowKey] === "string" ? (r[rowKey] as string) : String(i);
              const on = mode !== "none" && isSel(k);
              return (
                <tr
                  key={i}
                  className={on ? "bg-[var(--color-overlay-strong)]" : mode !== "none" ? "cursor-pointer hover:bg-[var(--color-overlay-soft)]" : ""}
                  onClick={mode !== "none" ? () => toggle(k) : undefined}
                >
                  {mode !== "none" ? (
                    <td className="border-b border-[var(--color-border)] px-2 text-center">
                      <input
                        type={mode === "single" ? "radio" : "checkbox"}
                        name={mode === "single" ? groupName : undefined}
                        checked={on}
                        aria-label={`Select ${k}`}
                        onChange={() => toggle(k)}
                        onClick={(e) => e.stopPropagation()}
                        className="accent-[var(--color-primary)]"
                      />
                    </td>
                  ) : null}
                  {cols.map((col) => (
                    <td
                      key={col.key}
                      className="border-b border-[var(--color-border)] px-2 py-1.5 tabular-nums"
                      style={{ textAlign: col.align as "left" | "right" | "center" }}
                    >
                      {cellText(Object.prototype.hasOwnProperty.call(r, col.key) ? r[col.key] : undefined)}
                    </td>
                  ))}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {mode !== "none" ? (
        <div className="flex flex-wrap items-center gap-2 text-[0.72rem] text-[var(--color-text-muted)]">
          <span>
            {mode === "multi" ? `${Array.isArray(selected) ? selected.length : 0} of ${rows.length} selected` : selected ? `Selected: ${String(selected)}` : "Select a row"}
          </span>
          {/* A checked native radio cannot be unchecked from the keyboard, and
              the row itself is not focusable: an optional single choice
              needs an explicit way back to "none". */}
          {mode === "single" && selected && c.required !== true && !locked && c.disabled !== true && item?.disabled !== true ? (
            <button type="button" className="underline hover:text-[var(--color-text-primary)]" onClick={() => onChange?.("")}>
              Clear selection
            </button>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

const STATUS_GLYPH: Record<string, { glyph: string; color: string; label: string }> = {
  pass: { glyph: "✓", color: "var(--color-success)", label: "Pass" },
  fail: { glyph: "✗", color: "var(--color-danger)", label: "Fail" },
  warn: { glyph: "!", color: "var(--color-warning-strong)", label: "Warning" },
  info: { glyph: "i", color: "var(--color-accent)", label: "Info" },
  pending: { glyph: "…", color: "var(--color-text-muted)", label: "Pending" },
};

function StatusList({ c }: { c: Component }) {
  const scope = useScope();
  const { focusField, locked, visible } = useCard();
  return (
    <ul className="m-0 grid list-none gap-1 p-0">
      {objs(c.items).map((it, i) => {
        const s = STATUS_GLYPH[str(it.status)] ?? STATUS_GLYPH.info;
        const field = str(it.field);
        return (
          <li key={i} className="flex min-w-0 items-start gap-2" data-status={str(it.status)}>
            <span
              aria-label={s.label}
              className="mt-0.5 inline-flex size-4 shrink-0 items-center justify-center rounded-full border text-[0.65rem] font-semibold"
              style={{ color: s.color, borderColor: s.color }}
            >
              {s.glyph}
            </span>
            <div className="min-w-0 flex-1">
              <div className="break-words">{renderTemplate(str(it.label), scope)}</div>
              {it.detail ? <div className="text-[0.75rem] text-[var(--color-text-muted)]">{renderTemplate(str(it.detail), scope)}</div> : null}
            </div>
            {/* Only where Fix can land: a field hidden by visible_if,
                disabled, or in a removed repeater item cannot be focused. */}
            {field && !locked && visible.has(field) ? (
              <button type="button" className="shrink-0 text-[0.72rem] text-[var(--color-accent)] underline" onClick={() => focusField(field)}>
                Fix
              </button>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}

function Progress({ c }: { c: Component }) {
  const scope = useScope();
  const max = numOr(c.max) ?? 100;
  const raw = Number(evaluateSafe(str(c.value), scope, 0));
  const v = Number.isFinite(raw) ? raw : 0;
  const pct = max > 0 ? Math.max(0, Math.min(100, (v / max) * 100)) : 0;
  return (
    <div className="grid gap-1">
      <div className="flex items-center justify-between text-[0.75rem] text-[var(--color-text-muted)]">
        <span>{str(c.label).trim()}</span>
        <span className="tabular-nums">
          {toText(v)} / {toText(max)}
        </span>
      </div>
      <div
        className="h-1.5 overflow-hidden rounded-full bg-[var(--color-overlay-strong)]"
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={max}
        // The bar and its ARIA value stay in range; the label shows the raw value.
        aria-valuenow={max > 0 ? Math.max(0, Math.min(max, v)) : 0}
        aria-label={str(c.label).trim() || "Progress"}
      >
        <div className="h-full rounded-full bg-[var(--color-accent)]" style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}

const SERIES_COLORS = [
  "var(--color-accent)",
  "var(--color-success)",
  "var(--color-warning)",
  "var(--color-danger)",
  "var(--color-secondary)",
  "var(--color-text-secondary)",
  "var(--color-syntax-keyword)",
  "var(--color-syntax-string)",
];

function Chart({ c }: { c: Component }) {
  const labels = Array.isArray(c.labels) ? c.labels.map((l) => String(l)) : [];
  const series = objs(c.series).map((s, si) => ({
    name: str(s.name).trim() || `Series ${si + 1}`,
    values: Array.isArray(s.values) ? s.values.map((v) => (typeof v === "number" && Number.isFinite(v) ? v : null)) : [],
  }));
  const unit = str(c.unit);
  const all = series.flatMap((s) => s.values.filter((v): v is number => v !== null));
  const maxV = Math.max(0, ...all);
  const minV = Math.min(0, ...all);
  // Halved before subtracting: maxV - minV overflows to Infinity for values
  // near ±1.7e308, which would turn every coordinate into NaN.
  const span = maxV / 2 - minV / 2 || 1;
  const W = 600;
  const H = 200;
  const padL = 44;
  const padB = 22;
  const plotW = W - padL - 8;
  const plotH = H - padB - 8;
  const y = (v: number) => 8 + plotH - ((v / 2 - minV / 2) / span) * plotH;
  const n = Math.max(1, labels.length);
  const band = plotW / n;
  const fmt = (v: number) => `${unit === "$" ? "$" : ""}${toText(Number(v.toPrecision(4)))}${unit && unit !== "$" ? ` ${unit}` : ""}`;
  const labelEvery = Math.ceil(n / 12);
  return (
    <figure className="m-0 grid min-w-0 gap-1">
      {c.title ? <figcaption className="text-[0.78rem] font-medium">{str(c.title)}</figcaption> : null}
      {/* The drawing is visual only; the table below carries the same data
          for assistive technology (an SVG image is announced by its name
          alone, and its <title> tooltips are not reachable). */}
      <svg viewBox={`0 0 ${W} ${H}`} className="h-auto w-full" aria-hidden="true">
        <line x1={padL} x2={W - 8} y1={y(0)} y2={y(0)} stroke="var(--color-border-strong)" />
        <text x={padL - 4} y={y(maxV) + 4} textAnchor="end" fontSize="11" fill="var(--color-text-muted)">
          {fmt(maxV)}
        </text>
        {minV < 0 ? (
          <text x={padL - 4} y={y(minV) + 4} textAnchor="end" fontSize="11" fill="var(--color-text-muted)">
            {fmt(minV)}
          </text>
        ) : (
          <text x={padL - 4} y={y(0) + 4} textAnchor="end" fontSize="11" fill="var(--color-text-muted)">
            0
          </text>
        )}
        {labels.map((l, i) =>
          i % labelEvery === 0 ? (
            <text key={i} x={padL + band * (i + 0.5)} y={H - 6} textAnchor="middle" fontSize="11" fill="var(--color-text-muted)">
              {l.length > 10 ? l.slice(0, 9) + "…" : l}
            </text>
          ) : null,
        )}
        {c.kind === "line"
          ? series.map((s, si) => {
              const pts = s.values
                .map((v, i) => (v === null ? null : `${padL + band * (i + 0.5)},${y(v)}`))
                .reduce<string[][]>((segs, p) => {
                  if (p === null) segs.push([]);
                  else segs[segs.length - 1].push(p);
                  return segs;
                }, [[]])
                .filter((seg) => seg.length > 0);
              const color = SERIES_COLORS[si % SERIES_COLORS.length];
              return (
                <g key={si}>
                  {pts.map((seg, k) => (
                    <polyline key={k} points={seg.join(" ")} fill="none" stroke={color} strokeWidth="2" />
                  ))}
                  {s.values.map((v, i) =>
                    v === null ? null : (
                      <circle key={i} cx={padL + band * (i + 0.5)} cy={y(v)} r="3" fill={color}>
                        <title>{`${s.name} · ${labels[i] ?? ""}: ${fmt(v)}`}</title>
                      </circle>
                    ),
                  )}
                </g>
              );
            })
          : series.map((s, si) => {
              const bw = (band * 0.8) / Math.max(1, series.length);
              const color = SERIES_COLORS[si % SERIES_COLORS.length];
              return s.values.map((v, i) => {
                if (v === null) return null;
                const x = padL + band * i + band * 0.1 + bw * si;
                const top = Math.min(y(v), y(0));
                return (
                  <rect key={`${si}-${i}`} x={x} y={top} width={Math.max(1, bw - 1)} height={Math.abs(y(v) - y(0))} fill={color} rx="2">
                    <title>{`${s.name} · ${labels[i] ?? ""}: ${fmt(v)}`}</title>
                  </rect>
                );
              });
            })}
      </svg>
      <table className="sr-only">
        <caption>{str(c.title).trim() || "Chart"}</caption>
        <thead>
          <tr>
            <th scope="col">Label</th>
            {series.map((s, si) => (
              <th key={si} scope="col">
                {s.name}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {labels.map((l, i) => (
            <tr key={i}>
              <th scope="row">{l}</th>
              {series.map((s, si) => (
                <td key={si}>{s.values[i] == null ? "No value" : fmt(s.values[i] as number)}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      {series.length > 1 ? (
        <div className="flex flex-wrap gap-3 text-[0.72rem] text-[var(--color-text-muted)]">
          {series.map((s, si) => (
            <span key={si} className="flex items-center gap-1">
              <span className="inline-block size-2 rounded-sm" style={{ background: SERIES_COLORS[si % SERIES_COLORS.length] }} />
              {s.name}
            </span>
          ))}
        </div>
      ) : null}
    </figure>
  );
}

function Code({ c }: { c: Component }) {
  return (
    <pre
      className="m-0 max-h-72 min-w-0 overflow-auto whitespace-pre-wrap break-all rounded-[var(--radius-md)] bg-[var(--color-overlay-strong)] p-2 text-[0.75rem]"
      style={{ fontFamily: "var(--font-code)" }}
      data-language={str(c.language) || undefined}
    >
      {str(c.text)}
    </pre>
  );
}

function LinkNode({ c }: { c: Component }) {
  const url = str(c.url);
  // Re-checked here: the validator refuses non-https URLs, but the renderer
  // never trusts that a spec went through it.
  if (!/^https:\/\//i.test(url)) return <span>{str(c.text)}</span>;
  return (
    <a href={url} target="_blank" rel="noopener noreferrer nofollow" className="text-[var(--color-accent)] underline">
      {str(c.text)}
    </a>
  );
}

function Diff({ c }: { c: Component }) {
  // Before / after are told apart by color and strikethrough on screen; the
  // visually hidden header row and "none" text say it to assistive tech.
  const empty = (
    <>
      <span aria-hidden>—</span>
      <span className="sr-only">none</span>
    </>
  );
  const title = str(c.title).trim();
  return (
    <div className="grid min-w-0 gap-1">
      {title ? <div className="text-[0.78rem] font-medium">{title}</div> : null}
      <div className="overflow-auto rounded-[var(--radius-md)] border border-[var(--color-border)]">
        <table className="w-full border-collapse text-[0.78rem]" aria-label={title || "Changes"}>
          <thead className="sr-only">
            <tr>
              <th scope="col">Field</th>
              <th scope="col">Before</th>
              <th scope="col">After</th>
            </tr>
          </thead>
          <tbody>
            {objs(c.rows).map((r, i) => (
              <tr key={i} className="border-b border-[var(--color-border)] last:border-b-0">
                <th scope="row" className="px-2 py-1.5 text-left font-normal text-[var(--color-text-muted)]">
                  {str(r.label)}
                </th>
                <td className="px-2 py-1.5 text-[var(--color-danger)] line-through decoration-[var(--color-danger)]/60">
                  {r.before === undefined || r.before === "" ? <span className="no-underline">{empty}</span> : str(r.before)}
                </td>
                <td className="px-2 py-1.5 text-[var(--color-success)]">{r.after === undefined || r.after === "" ? empty : str(r.after)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function Divider() {
  return <hr className="m-0 border-0 border-t border-[var(--color-border)]" />;
}

// ───────────────────────── inputs ─────────────────────────

function FieldError({ text, id }: { text: string; id?: string }) {
  return (
    <div id={id} className="text-[0.72rem] text-[var(--color-danger)]" role="alert">
      {text}
    </div>
  );
}

/** Label + help + error chrome around one input, bound to the value store. */
function InputField({ c, Render }: { c: Component; Render: Renderer }) {
  const { values, setField, errors, locked } = useCard();
  const item = useContext(ItemContext);
  const scope = useScope();
  const inputId = useId();
  const id = str(c.id);
  const path = item ? `${item.repeater}[${item.index}].${id}` : id;
  const value = item ? item.item[id] : values[id];
  const onChange = (v: unknown) => setField(id, v, item ?? undefined);
  const err = errors[path];
  const label = str(c.label).trim();
  const help = c.help ? renderTemplate(str(c.help), scope) : "";
  // Tie the help text and the error to the control (or its group) itself, so
  // a screen reader hears them on the field, not only as stray text below.
  // Every renderer puts inputId on its control; set the attributes on it
  // directly rather than threading them through each renderer.
  useEffect(() => {
    const el = document.getElementById(inputId);
    if (!el) return;
    const described = [help ? `${inputId}-help` : "", err ? `${inputId}-error` : ""].filter(Boolean).join(" ");
    if (described) el.setAttribute("aria-describedby", described);
    else el.removeAttribute("aria-describedby");
    if (err) el.setAttribute("aria-invalid", "true");
    else el.removeAttribute("aria-invalid");
  });
  return (
    <div className="grid min-w-0 content-start gap-1" data-genui-field={path} data-invalid={err ? "true" : undefined}>
      {!label && c.type !== "toggle" ? (
        // No visible label: the field id still names the control for
        // assistive technology (the validator allows label-less inputs).
        <label id={`${inputId}-label`} htmlFor={inputId} className="sr-only">
          {id}
          {c.required === true ? " (required)" : null}
        </label>
      ) : null}
      {label && c.type !== "toggle" ? (
        <label id={`${inputId}-label`} htmlFor={inputId} className="text-[0.78rem] font-medium text-[var(--color-text-secondary)]">
          {label}
          {c.required === true ? (
            <>
              <span className="ml-0.5 text-[var(--color-danger)]" aria-hidden>
                *
              </span>
              {/* The asterisk is visual only. Every control and group is named
                  by this label, so this is how a screen reader hears that the
                  field is required, whatever the control type. */}
              <span className="sr-only"> (required)</span>
            </>
          ) : null}
        </label>
      ) : null}
      {c.type === "repeater" ? (
        // A repeater's own fields lock themselves; its expand buttons must not.
        <Render c={c} value={value} onChange={onChange} inputId={inputId} />
      ) : (
        // The fieldset only carries `disabled` to the control inside; it has
        // no legend, so it is not announced as an unnamed group.
        <fieldset role="none" disabled={locked || c.disabled === true || item?.disabled === true} className="m-0 min-w-0 border-0 p-0">
          <Render c={c} value={value} onChange={onChange} inputId={inputId} />
        </fieldset>
      )}
      {help ? (
        <div id={`${inputId}-help`} className="text-[0.72rem] text-[var(--color-text-muted)]">
          {help}
        </div>
      ) : null}
      {err ? <FieldError text={err} id={`${inputId}-error`} /> : null}
    </div>
  );
}

function TextInput({ c, value, onChange, inputId }: Parameters<Renderer>[0]) {
  const v = typeof value === "string" ? value : "";
  const common = {
    id: inputId,
    value: v,
    placeholder: str(c.placeholder) || undefined,
    maxLength: numOr(c.max_length),
    required: c.required === true,
    className: inputClass,
  };
  if (c.multiline === true) {
    return <textarea {...common} rows={3} onChange={(e) => onChange?.(e.target.value)} />;
  }
  const type = c.format === "email" ? "email" : c.format === "url" ? "url" : "text";
  return <input {...common} type={type} onChange={(e) => onChange?.(e.target.value)} />;
}

function Adorned({ prefix, suffix, children: kids }: { prefix?: string; suffix?: string; children: ReactNode }) {
  if (!prefix && !suffix) return <>{kids}</>;
  return (
    <div className="flex min-w-0 items-center gap-1.5">
      {prefix ? <span className="text-[var(--color-text-muted)]">{prefix}</span> : null}
      <div className="min-w-0 flex-1">{kids}</div>
      {suffix ? <span className="text-[var(--color-text-muted)]">{suffix}</span> : null}
    </div>
  );
}

function NumberInput({ c, value, onChange, inputId }: Parameters<Renderer>[0]) {
  const [text, setText] = useState(typeof value === "number" ? String(value) : "");
  // Re-sync when the value changes from outside (Undo, a replaced card).
  const [seen, setSeen] = useState(value);
  if (seen !== value) {
    setSeen(value);
    // Keep the user's own spelling ("1.50") of the same number, but never a
    // blank box over a real value: Number("") is 0.
    if (!(typeof value === "number" && text.trim() !== "" && Number(text) === value)) {
      setText(typeof value === "number" ? String(value) : "");
    }
  }
  return (
    <Adorned prefix={str(c.prefix) || undefined} suffix={str(c.suffix) || undefined}>
      <input
        id={inputId}
        type="number"
        inputMode="decimal"
        className={inputClass + " tabular-nums"}
        value={text}
        min={numOr(c.min)}
        max={numOr(c.max)}
        step={numOr(c.step) ?? "any"}
        placeholder={str(c.placeholder) || undefined}
        onChange={(e) => {
          setText(e.target.value);
          const t = e.target.value.trim();
          onChange?.(t === "" ? null : Number.isFinite(Number(t)) ? Number(t) : t);
        }}
      />
    </Adorned>
  );
}

function Slider({ c, value, onChange, inputId }: Parameters<Renderer>[0]) {
  const min = numOr(c.min) ?? 0;
  const max = numOr(c.max) ?? 100;
  const v = typeof value === "number" ? value : min;
  return (
    <div className="flex min-w-0 items-center gap-3">
      <input
        id={inputId}
        type="range"
        className="min-w-0 flex-1 accent-[var(--color-primary)]"
        min={min}
        max={max}
        step={numOr(c.step) ?? 1}
        value={v}
        onChange={(e) => onChange?.(Number(e.target.value))}
      />
      <span className="shrink-0 tabular-nums text-[var(--color-text-secondary)]">
        {str(c.prefix)}
        {v.toLocaleString()}
        {str(c.suffix) ? ` ${str(c.suffix)}` : ""}
      </span>
    </div>
  );
}

function Filter({ value, onChange, label }: { value: string; onChange: (s: string) => void; label: string }) {
  return (
    <input
      type="search"
      aria-label={label}
      placeholder="Filter…"
      className={inputClass + " mb-1"}
      value={value}
      onChange={(e) => onChange(e.target.value)}
    />
  );
}

const FILTER_AT = 12;

function matches(o: { value: string; label: string; description?: string }, q: string) {
  if (!q) return true;
  const s = q.toLowerCase();
  return o.label.toLowerCase().includes(s) || o.value.toLowerCase().includes(s) || (o.description ?? "").toLowerCase().includes(s);
}

function Select({ c, value, onChange, inputId }: Parameters<Renderer>[0]) {
  const opts = optionsOf(c);
  const [q, setQ] = useState("");
  const v = typeof value === "string" ? value : "";
  const shown = opts.filter((o) => matches(o, q) || o.value === v);
  const desc = opts.find((o) => o.value === v)?.description;
  return (
    <div className="grid min-w-0 gap-0.5">
      {opts.length > FILTER_AT ? <Filter value={q} onChange={setQ} label={`Filter ${str(c.label).trim() || "options"}`} /> : null}
      <select id={inputId} className={inputClass} value={v} onChange={(e) => onChange?.(e.target.value)}>
        <option value="">{str(c.placeholder) || "Choose…"}</option>
        {shown.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
      {desc ? <div className="text-[0.72rem] text-[var(--color-text-muted)]">{desc}</div> : null}
    </div>
  );
}

function Choice({ c, value, onChange, inputId }: Parameters<Renderer>[0]) {
  const { locked: cardLocked } = useCard();
  const item = useContext(ItemContext);
  const locked = cardLocked || c.disabled === true || item?.disabled === true;
  const opts = optionsOf(c);
  const v = typeof value === "string" ? value : "";
  const name = useId();
  // A radiogroup is not labelable by <label for>; name it from the field's
  // visible label (InputField gives it this id), or the id as a fallback.
  const groupLabel = str(c.label).trim()
    ? { "aria-labelledby": `${inputId}-label` }
    : { "aria-label": str(c.id) || "Choice" };
  if (c.variant === "radio") {
    return (
      <div className="grid gap-1">
      <div role="radiogroup" id={inputId} {...groupLabel} className="grid gap-1">
        {opts.map((o) => (
          <label key={o.value} className="flex cursor-pointer items-start gap-2">
            <input
              type="radio"
              name={name}
              className="mt-1 accent-[var(--color-primary)]"
              checked={v === o.value}
              onChange={() => onChange?.(o.value)}
            />
            <span>
              {o.label}
              {o.description ? <span className="block text-[0.72rem] text-[var(--color-text-muted)]">{o.description}</span> : null}
            </span>
          </label>
        ))}
      </div>
      {/* Native radios can only select; an optional choice needs a way back
          to "no answer" (the segmented variant toggles off instead). */}
      {c.required !== true && v !== "" && !locked ? (
        <button
          type="button"
          className="justify-self-start text-[0.72rem] text-[var(--color-text-muted)] underline hover:text-[var(--color-text-primary)]"
          onClick={() => onChange?.("")}
        >
          Clear choice
        </button>
      ) : null}
      </div>
    );
  }
  return (
    <div role="radiogroup" id={inputId} {...groupLabel} className="flex min-w-0 flex-wrap gap-1">
      {opts.map((o) => {
        const on = v === o.value;
        return (
          <button
            key={o.value}
            type="button"
            role="radio"
            aria-checked={on}
            title={o.description}
            onClick={() => onChange?.(on && c.required !== true ? "" : o.value)}
            className={[
              "rounded-[var(--radius-md)] border px-2.5 py-1 text-[0.78rem] transition disabled:opacity-60",
              on
                ? "border-[var(--color-accent)] bg-[var(--color-overlay-strong)] text-[var(--color-text-primary)]"
                : "border-[var(--color-border-strong)] text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]",
            ].join(" ")}
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}

function Chip({ text, onRemove, extra }: { text: string; onRemove?: () => void; extra?: ReactNode }) {
  return (
    <span className="inline-flex max-w-full items-center gap-1 rounded-full border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] py-0.5 pl-2 pr-1 text-[0.75rem]">
      <span className="truncate">{text}</span>
      {extra}
      {onRemove ? (
        <button type="button" aria-label={`Remove ${text}`} onClick={onRemove} className="rounded-full px-1 text-[var(--color-text-muted)] hover:text-[var(--color-danger)]">
          ×
        </button>
      ) : null}
    </span>
  );
}

/** An add-a-value control: a filtered option list and/or free entry. */
function Adder({
  c,
  taken,
  onAdd,
  inputId,
  labelledBy,
}: {
  c: Component;
  taken: Set<string>;
  onAdd: (v: string) => void;
  inputId?: string;
  labelledBy?: string;
}) {
  const opts = optionsOf(c).filter((o) => !taken.has(o.value));
  const custom = c.allow_custom === true;
  const [q, setQ] = useState("");
  const { locked } = useCard();
  if (locked) return null;
  const shown = opts.filter((o) => matches(o, q)).slice(0, 50);
  const commit = () => {
    const t = q.trim();
    if (!t) return;
    const exact = opts.find((o) => o.label.toLowerCase() === t.toLowerCase() || o.value === t);
    if (exact) onAdd(exact.value);
    else if (custom) onAdd(t);
    else return;
    setQ("");
  };
  return (
    <div className="grid min-w-0 gap-1">
      <input
        id={inputId}
        aria-labelledby={labelledBy}
        type="text"
        className={inputClass}
        placeholder={str(c.placeholder) || (custom ? "Type and press Enter…" : "Search options…")}
        value={q}
        onChange={(e) => setQ(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") {
            e.preventDefault();
            commit();
          }
        }}
      />
      {opts.length > 0 && (q || opts.length <= FILTER_AT) ? (
        <div className="flex max-h-32 flex-wrap gap-1 overflow-auto">
          {shown.map((o) => (
            <button key={o.value} type="button" title={o.description} className={chipButton} onClick={() => onAdd(o.value)}>
              + {o.label}
            </button>
          ))}
          {shown.length === 0 && !custom ? <span className="text-[0.72rem] text-[var(--color-text-muted)]">No match</span> : null}
        </div>
      ) : null}
    </div>
  );
}

function labelFor(c: Component, v: string): string {
  return optionsOf(c).find((o) => o.value === v)?.label ?? v;
}

function MultiSelect({ c, value, onChange, inputId }: Parameters<Renderer>[0]) {
  const v = Array.isArray(value) ? (value as string[]) : [];
  const { locked } = useCard();
  // The protocol cap applies even when the card sets no max_items.
  const max = Math.min(numOr(c.max_items) ?? MAX_CHOICE_ITEMS, MAX_CHOICE_ITEMS);
  // The adder disappears at max_items, so the field's id, label, help and
  // error state live on this always-rendered group instead.
  const labelId = inputId ? `${inputId}-label` : undefined;
  return (
    <div className="grid min-w-0 gap-1.5" role="group" id={inputId} aria-labelledby={labelId}>
      {v.length > 0 ? (
        <div className="flex flex-wrap gap-1">
          {v.map((x) => (
            <Chip key={x} text={labelFor(c, x)} onRemove={locked ? undefined : () => onChange?.(v.filter((y) => y !== x))} />
          ))}
        </div>
      ) : null}
      {v.length < max ? (
        <Adder
          c={c}
          taken={new Set(v)}
          onAdd={(x) => onChange?.(v.includes(x) ? v : [...v, x])}
          inputId={inputId ? `${inputId}-add` : undefined}
          labelledBy={labelId}
        />
      ) : null}
    </div>
  );
}

function Toggle({ c, value, onChange, inputId }: Parameters<Renderer>[0]) {
  const on = value === true;
  return (
    <label htmlFor={inputId} className="flex cursor-pointer items-center gap-2">
      <button
        id={inputId}
        type="button"
        role="switch"
        aria-checked={on}
        aria-label={`${str(c.label).trim() || str(c.id) || "Toggle"}${c.required === true ? " (required)" : ""}`}
        onClick={() => onChange?.(!on)}
        className={[
          "relative h-5 w-9 shrink-0 rounded-full transition disabled:opacity-60",
          on ? "bg-[var(--color-primary)]" : "bg-[var(--color-overlay-strong)]",
        ].join(" ")}
      >
        <span className={["absolute top-0.5 size-4 rounded-full bg-[var(--color-white)] transition-all", on ? "left-[1.125rem]" : "left-0.5"].join(" ")} />
      </button>
      <span className="text-[0.8125rem]">
        {str(c.label).trim()}
        {/* The same visual marker as every other required field; the
            switch's accessible name already says "(required)". */}
        {c.required === true ? (
          <span className="ml-0.5 text-[var(--color-danger)]" aria-hidden data-testid="genui-required-mark">
            *
          </span>
        ) : null}
      </span>
    </label>
  );
}

function DateInput({ c, value, onChange, inputId }: Parameters<Renderer>[0]) {
  return (
    <input
      id={inputId}
      type="date"
      className={inputClass}
      value={typeof value === "string" ? value : ""}
      min={str(c.min) || undefined}
      max={str(c.max) || undefined}
      onChange={(e) => onChange?.(e.target.value)}
    />
  );
}

export { parseListText };

function ListInput({ c, value, onChange, inputId }: Parameters<Renderer>[0]) {
  const v = Array.isArray(value) ? (value as string[]) : [];
  const [text, setText] = useState(v.join("\n"));
  const dedupe = c.dedupe !== false;
  const max = numOr(c.max_items);
  const [seen, setSeen] = useState(value);
  if (seen !== value) {
    setSeen(value);
    if (parseListText(text, dedupe).join("\n") !== v.join("\n")) setText(v.join("\n"));
  }
  // Parsed once per edit, not on every render of the card.
  const parsed = useMemo(() => parseListText(text, dedupe), [text, dedupe]);
  const raw = useMemo(() => (dedupe ? parseListText(text, false).length : parsed.length), [text, dedupe, parsed]);
  return (
    <div className="grid min-w-0 gap-1">
      <textarea
        id={inputId}
        rows={4}
        className={inputClass + " font-mono text-[0.75rem]"}
        placeholder={str(c.placeholder) || "One per line"}
        value={text}
        onChange={(e) => {
          // Keep only the text the entries came from: a paste far past
          // the item cap is cut where scanning stopped, not held whole.
          const { items, end } = scanListText(e.target.value, dedupe);
          setText(end < e.target.value.length ? e.target.value.slice(0, end) : e.target.value);
          onChange?.(items);
        }}
      />
      <div className="text-[0.72rem] text-[var(--color-text-muted)]">
        {parsed.length.toLocaleString()} item{parsed.length === 1 ? "" : "s"}
        {dedupe && raw > parsed.length ? ` · ${raw - parsed.length} duplicate${raw - parsed.length === 1 ? "" : "s"} removed` : ""}
        {max !== undefined ? ` · max ${max.toLocaleString()}` : ""}
      </div>
    </div>
  );
}

function IncludeExcludeInput({ c, value, onChange, inputId }: Parameters<Renderer>[0]) {
  const v: IncludeExclude =
    value && typeof value === "object" && !Array.isArray(value)
      ? {
          include: Array.isArray((value as IncludeExclude).include) ? (value as IncludeExclude).include : [],
          exclude: Array.isArray((value as IncludeExclude).exclude) ? (value as IncludeExclude).exclude : [],
        }
      : { include: [], exclude: [] };
  const [side, setSide] = useState<"include" | "exclude">("include");
  const { locked } = useCard();
  const names = { include: str(c.include_label).trim() || "Include", exclude: str(c.exclude_label).trim() || "Exclude" };
  // The adder is gone once the card locks, so the field's id, label, help
  // and error state live on this always-rendered group (as for multi_select).
  const labelId = inputId ? `${inputId}-label` : undefined;
  const taken = new Set([...v.include, ...v.exclude]);
  const set = (next: IncludeExclude) => onChange?.(next);
  const lane = (k: "include" | "exclude") => {
    const other = k === "include" ? "exclude" : "include";
    return (
      <div className="grid min-w-0 content-start gap-1 rounded-[var(--radius-md)] border border-[var(--color-border)] p-2" data-lane={k}>
        <div className="text-[0.72rem] font-medium" style={{ color: k === "include" ? "var(--color-success)" : "var(--color-danger)" }}>
          {names[k]} ({v[k].length})
        </div>
        <div className="flex flex-wrap gap-1">
          {v[k].length === 0 ? <span className="text-[0.72rem] text-[var(--color-text-muted)]">None</span> : null}
          {v[k].map((x) => (
            <Chip
              key={x}
              text={labelFor(c, x)}
              onRemove={locked ? undefined : () => set({ ...v, [k]: v[k].filter((y) => y !== x) })}
              extra={
                locked ? undefined : (
                  <button
                    type="button"
                    title={`Move to ${names[other]}`}
                    aria-label={`Move ${x} to ${names[other]}`}
                    className="rounded-full px-1 text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
                    onClick={() => set({ ...v, [k]: v[k].filter((y) => y !== x), [other]: [...v[other], x] })}
                  >
                    ⇄
                  </button>
                )
              }
            />
          ))}
        </div>
      </div>
    );
  };
  return (
    <div className="grid min-w-0 gap-1.5" role="group" id={inputId} aria-labelledby={labelId}>
      <div className="grid min-w-0 grid-cols-1 gap-1.5 sm:grid-cols-2">
        {lane("include")}
        {lane("exclude")}
      </div>
      {!locked && taken.size < MAX_CHOICE_ITEMS ? (
        <div className="grid min-w-0 gap-1">
          <div className="flex items-center gap-1 text-[0.72rem] text-[var(--color-text-muted)]">
            Add to
            {(["include", "exclude"] as const).map((k) => (
              <button
                key={k}
                type="button"
                aria-pressed={side === k}
                onClick={() => setSide(k)}
                className={[
                  "rounded-full border px-2 py-0.5",
                  side === k ? "border-[var(--color-accent)] text-[var(--color-text-primary)]" : "border-[var(--color-border-strong)]",
                ].join(" ")}
              >
                {names[k]}
              </button>
            ))}
          </div>
          <Adder
            c={c}
            taken={taken}
            inputId={inputId ? `${inputId}-add` : undefined}
            labelledBy={labelId}
            onAdd={(x) => set({ ...v, [side]: [...v[side], x] })}
          />
        </div>
      ) : null}
    </div>
  );
}

// Repeater items get client-side identities for React keys and open state:
// keyed by position, an item's stateful controls (an adder's half-typed
// query, an include/exclude lane choice) would move to the next item after a
// remove or duplicate.
let itemKeySeq = 0;
function freshKeys(n: number): number[] {
  return Array.from({ length: n }, () => ++itemKeySeq);
}

function Repeater({ c, value, onChange, inputId }: Parameters<Renderer>[0]) {
  const { values, locked: cardLocked, epoch } = useCard();
  // A disabled repeater (or one nested in a disabled scope) keeps its expand
  // toggles usable but offers no add / duplicate / remove, and its fields
  // disable through ItemContext.
  const locked = cardLocked || c.disabled === true;
  const fields = children(c, "fields");
  const items = Array.isArray(value) ? (value as Values[]) : [];
  const id = str(c.id);
  const min = numOr(c.min_items) ?? 0;
  const max = numOr(c.max_items) ?? 200;
  const [keys, setKeys] = useState<number[]>(() => freshKeys(items.length));
  // The first item starts open.
  const [openKeys, setOpenKeys] = useState<Set<number>>(() => new Set(keys.slice(0, 1)));
  // The value was replaced from outside (a restored draft, an adopted answer
  // — the card bumps its epoch — or a length this repeater did not make):
  // identities can no longer be matched, so the items start afresh.
  const [seenEpoch, setSeenEpoch] = useState(epoch);
  let itemKeys = keys;
  if (keys.length !== items.length || seenEpoch !== epoch) {
    itemKeys = freshKeys(items.length);
    setKeys(itemKeys);
    setSeenEpoch(epoch);
    setOpenKeys(new Set(itemKeys.slice(0, 1)));
  }
  const toggleOpen = (k: number) =>
    setOpenKeys((s) => {
      const n = new Set(s);
      if (n.has(k)) n.delete(k);
      else n.add(k);
      return n;
    });
  const update = (next: Values[], nextKeys: number[], open?: number) => {
    setKeys(nextKeys);
    if (open !== undefined) setOpenKeys((s) => new Set(s).add(open));
    onChange?.(next);
  };
  const labelTpl = str(c.item_label);
  const ref = useRef<HTMLDivElement | null>(null);
  const keysRef = useRef(itemKeys);
  useEffect(() => {
    keysRef.current = itemKeys;
  });
  useReveal(ref, (path) => {
    // Paths into this repeater read "<id>[<index>].<field>".
    if (!path.startsWith(`${id}[`)) return;
    const i = Number.parseInt(path.slice(id.length + 1), 10);
    const k = Number.isInteger(i) ? keysRef.current[i] : undefined;
    if (k !== undefined) setOpenKeys((s) => new Set(s).add(k));
  });
  return (
    // One labelled group: the field label names it, and InputField ties the
    // help, error and required state to it.
    <div ref={ref} className="grid min-w-0 gap-1.5" role="group" id={inputId} aria-labelledby={inputId ? `${inputId}-label` : undefined}>
      {items.map((item, i) => {
        const k = itemKeys[i];
        const scope = scopeFor(values, item, i);
        // A template can render blank (an empty field it reads): fall back
        // so the expand button keeps a name.
        const label = (labelTpl ? renderTemplate(labelTpl, scope).trim() : "") || `Item ${i + 1}`;
        const open = openKeys.has(k);
        return (
          <div key={k} className="min-w-0 rounded-[var(--radius-md)] border border-[var(--color-border)]" data-repeater-item={i}>
            <div className="flex min-w-0 items-center gap-2 px-2.5 py-1.5">
              <button type="button" aria-expanded={open} onClick={() => toggleOpen(k)} className="flex min-w-0 flex-1 items-center gap-1.5 text-left">
                <span aria-hidden className="text-[0.7rem] text-[var(--color-text-muted)]">
                  {open ? "▾" : "▸"}
                </span>
                <span className="truncate font-medium">{label}</span>
              </button>
              {!locked ? (
                <>
                  {items.length < max ? (
                    <button
                      type="button"
                      className="text-[0.72rem] text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
                      onClick={() => {
                        const [dup] = freshKeys(1);
                        update(
                          [...items.slice(0, i + 1), { ...item }, ...items.slice(i + 1)],
                          [...itemKeys.slice(0, i + 1), dup, ...itemKeys.slice(i + 1)],
                          dup,
                        );
                      }}
                    >
                      Duplicate
                    </button>
                  ) : null}
                  {items.length > min ? (
                    <button
                      type="button"
                      className="text-[0.72rem] text-[var(--color-text-muted)] hover:text-[var(--color-danger)]"
                      onClick={() =>
                        update(
                          items.filter((_, j) => j !== i),
                          itemKeys.filter((_, j) => j !== i),
                        )
                      }
                    >
                      Remove
                    </button>
                  ) : null}
                </>
              ) : null}
            </div>
            {open ? (
              <div className="grid min-w-0 gap-3 border-t border-[var(--color-border)] p-2.5">
                <ItemContext.Provider value={{ repeater: id, index: i, item, disabled: c.disabled === true }}>
                  <Nodes list={fields} />
                </ItemContext.Provider>
              </div>
            ) : null}
          </div>
        );
      })}
      {!locked && items.length < max ? (
        <div>
          <button
            type="button"
            className={chipButton}
            onClick={() => {
              const [added] = freshKeys(1);
              update([...items, newItem(fields)], [...itemKeys, added], added);
            }}
          >
            + {str(c.add_label).trim() || "Add"}
          </button>
        </div>
      ) : null}
    </div>
  );
}

// ───────────────────────── registry ─────────────────────────

// Keep in step with internal/genui/spec.go `components` and
// internal/genui/testdata/catalog.json (GenerativeCard.test.tsx pins it).
export const RENDERERS: Record<string, Renderer> = {
  section: Section,
  columns: Columns,
  tabs: Tabs,
  divider: Divider,
  heading: Heading,
  text: Text,
  callout: Callout,
  badges: Badges,
  stat: Stat,
  facts: Facts,
  table: Table,
  status_list: StatusList,
  progress: Progress,
  chart: Chart,
  code: Code,
  link: LinkNode,
  diff: Diff,
  text_input: TextInput,
  number: NumberInput,
  slider: Slider,
  select: Select,
  choice: Choice,
  multi_select: MultiSelect,
  toggle: Toggle,
  date: DateInput,
  list_input: ListInput,
  include_exclude: IncludeExcludeInput,
  repeater: Repeater,
};
