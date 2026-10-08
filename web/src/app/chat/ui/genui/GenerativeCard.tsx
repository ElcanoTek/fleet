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
  buildSubmissionMessage,
  children,
  collect,
  normalizeValues,
  isInput,
  isVisible,
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
   * The card predates the conversation's summary (shown only when the user
   * expands compacted history). The model no longer has its definition in
   * context, so an answer would arrive as ids and values it cannot read: the
   * card renders locked and says to ask for it again.
   */
  retired?: boolean;
  /** Sends a user turn (the submission message or a quick reply). */
  // Resolves false when the message was refused (nothing was sent).
  onSubmit?: (message: string) => void | Promise<void | boolean>;
};

const DRAFT_PREFIX = "fleet.genui.draft.";

function loadDraft(cardId: string): Values | null {
  try {
    const raw = window.localStorage.getItem(DRAFT_PREFIX + cardId);
    if (!raw) return null;
    const v = JSON.parse(raw);
    return v && typeof v === "object" && !Array.isArray(v) ? (v as Values) : null;
  } catch {
    return null;
  }
}

function saveDraft(cardId: string, values: Values | null) {
  try {
    if (values) window.localStorage.setItem(DRAFT_PREFIX + cardId, JSON.stringify(values));
    else window.localStorage.removeItem(DRAFT_PREFIX + cardId);
  } catch {
    // Private mode / quota: a draft is a convenience, never state we rely on.
  }
}


const PENDING_PREFIX = "fleet.genui.pending.";
// A queued message normally echoes within a turn; past this a stale marker
// (a cancelled queue item, a closed tab) stops holding the card.
const PENDING_TTL_MS = 30 * 60 * 1000;

// A hold records the transcript answer it was armed after (`after`: that
// answer's action and values, "" for a first answer — message ids are not
// stable across a history reload, so they are left out). If the answer has
// changed by the time the card mounts, the held message echoed while the
// card was unmounted (virtualized off-screen) and the hold is spent. An
// identical resend cannot be told apart from the answer before it; that
// hold is shown only under Edit, can be dismissed, and expires.
function loadPending(cardId: string, currentKey: string): string | null {
  try {
    const raw = window.localStorage.getItem(PENDING_PREFIX + cardId);
    if (!raw) return null;
    const v = JSON.parse(raw) as { action?: unknown; at?: unknown; after?: unknown };
    if (
      typeof v.action !== "string" ||
      typeof v.at !== "number" ||
      Date.now() - v.at > PENDING_TTL_MS ||
      v.after !== currentKey
    ) {
      window.localStorage.removeItem(PENDING_PREFIX + cardId);
      return null;
    }
    return v.action;
  } catch {
    return null;
  }
}

function savePending(cardId: string, action: string | null, after = "") {
  try {
    if (action) window.localStorage.setItem(PENDING_PREFIX + cardId, JSON.stringify({ action, at: Date.now(), after }));
    else window.localStorage.removeItem(PENDING_PREFIX + cardId);
  } catch {
    // Convenience only, like drafts.
  }
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

function CardBody({ cardId, spec, submission, reply, superseded, readOnly, retired, onSubmit }: GenerativeCardProps) {
  const [values, setValuesState] = useState<Values>(() =>
    normalizeValues(spec, submission ? submission.values : readOnly ? null : loadDraft(cardId)),
  );
  const [editing, setEditing] = useState(false);
  const [sending, setSending] = useState<string | null>(null);
  const [showErrors, setShowErrors] = useState(false);
  const [confirming, setConfirming] = useState<Action | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  // Server-sent field_errors stay until the user edits that field.
  const [clearedServerErrors, setClearedServerErrors] = useState<Set<string>>(() => new Set());
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
    ? `s\u0000${submission.messageId ?? ""}\u0000${submission.actionId}\u0000${JSON.stringify(submission.values)}`
    : reply
      ? `r\u0000${reply.messageId ?? ""}\u0000${reply.actionId}`
      : "";
  // The same answer without its message id (see loadPending).
  const answerKey = submission
    ? `s\u0000${submission.actionId}\u0000${JSON.stringify(submission.values)}`
    : reply
      ? `r\u0000${reply.actionId}`
      : "";
  const answerKeyRef = useRef(answerKey);
  useEffect(() => {
    answerKeyRef.current = answerKey;
  }, [answerKey]);
  const [seenSubmission, setSeenSubmission] = useState(submissionKey);
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
    readOnly ? null : loadPending(cardId, answerKey),
  );
  const setAwaiting = useCallback(
    (a: string | null) => {
      setAwaitingState(a);
      if (!readOnly) savePending(cardId, a, answerKeyRef.current);
    },
    [cardId, readOnly],
  );
  if (seenSubmission !== submissionKey) {
    setSeenSubmission(submissionKey);
    if (submission || reply) {
      setEditing(false);
      setAwaitingState(null);
      if (!readOnly) savePending(cardId, null);
    }
    if (submission) {
      setValuesState(normalizeValues(spec, submission.values));
    }
  }
  useEffect(() => {
    if (submissionKey && !readOnly) saveDraft(cardId, null);
  }, [cardId, submissionKey, readOnly]);
  const locked = !!readOnly || !!retired || !!superseded || (submittedAction !== null && !editing);

  const setValues = useCallback(
    (fn: (v: Values) => Values) => {
      setValuesState((prev) => {
        const next = fn(prev);
        if (!readOnly) saveDraft(cardId, next);
        return next;
      });
    },
    [cardId, readOnly],
  );

  const setField = useCallback(
    (id: string, v: unknown, item?: ItemCtx) => {
      const path = item ? `${item.repeater}[${item.index}].${id}` : id;
      setNotice(null);
      setClearedServerErrors((s) => {
        const n = new Set(s).add(path);
        // Editing an item also answers an error on the repeater as a whole
        // ("lines"), which a fixed-size repeater could otherwise never clear.
        // A separate key, so sibling items' errors stay.
        if (item) n.add(`${item.repeater}\u0000root`);
        return n;
      });
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
        if (++tries < 8) {
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
    () => ({ cardId, values, locked, errors, setField, setValues, focusField }),
    [cardId, values, locked, errors, setField, setValues, focusField],
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
      const accepted = await onSubmit(message);
      if (accepted === false) {
        setNotice("Not sent. Try again.");
      } else if (submissionKeyRef.current === keyAtClick) {
        // Hold the actions until the message reaches the transcript (a
        // queued message is accepted long before it is echoed), so a second
        // click cannot queue a duplicate — quick replies included.
        setAwaiting(action.id);
      }
    } catch {
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
    if (!passesGates(action)) {
      setConfirming(null);
      return;
    }
    void send(action);
  };

  const actions = (spec.actions ?? []).filter((a) => isVisible(a, scope));
  // A confirmation whose action has since become hidden (its visible_if no
  // longer holds) closes rather than offering a Yes for an unavailable action.
  const confirmingVisible = confirming && isVisible(confirming, scope) ? confirming : null;
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
        {actions.length > 0 || sentLabel ? (
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
                  Sent · {(spec.actions ?? []).find((a) => a.id === awaiting)?.label ?? awaiting} — it will reach the
                  assistant after the current reply
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
                  type="button"
                  className="rounded-full bg-[var(--color-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary)] hover:opacity-90"
                  onClick={() => onConfirm(confirmingVisible)}
                >
                  Yes, {confirmingVisible.label.toLowerCase()}
                </button>
                <button type="button" className={chipButton} onClick={() => setConfirming(null)}>
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
                      setValuesState(normalizeValues(spec, submission?.values));
                      saveDraft(cardId, null);
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
  const title = str(c.title) || (collapsible ? "Details" : "");
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
      <div className="text-[0.72rem] text-[var(--color-text-muted)]">{str(c.label)}</div>
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

function Table({ c, value, onChange }: Parameters<Renderer>[0]) {
  // A selectable table is an input: InputField wraps it and binds value /
  // onChange (to the card, or to the current repeater item) and draws its
  // label and errors. A display-only table renders bare.
  const { locked } = useCard();
  const item = useContext(ItemContext);
  const wrapped = onChange !== undefined;
  const cols = objs(c.columns).map((col) => ({
    key: str(col.key),
    label: str(col.label) || str(col.key),
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
  return (
    <div className="grid min-w-0 gap-1">
      {!wrapped && c.label ? <div className="text-[0.78rem] font-medium">{str(c.label)}</div> : null}
      <div className="max-h-[22rem] min-w-0 overflow-auto rounded-[var(--radius-md)] border border-[var(--color-border)]">
        <table className="w-full border-collapse text-[0.78rem]">
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
                      {cellText(r[col.key])}
                    </td>
                  ))}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {mode !== "none" ? (
        <div className="text-[0.72rem] text-[var(--color-text-muted)]">
          {mode === "multi" ? `${Array.isArray(selected) ? selected.length : 0} of ${rows.length} selected` : selected ? `Selected: ${String(selected)}` : "Select a row"}
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
  const { focusField, locked } = useCard();
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
            {field && !locked ? (
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
        <span>{str(c.label)}</span>
        <span className="tabular-nums">
          {toText(v)} / {toText(max)}
        </span>
      </div>
      <div
        className="h-1.5 overflow-hidden rounded-full bg-[var(--color-overlay-strong)]"
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={max}
        aria-valuenow={v}
        aria-label={str(c.label) || "Progress"}
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
  const series = objs(c.series).map((s) => ({
    name: str(s.name),
    values: Array.isArray(s.values) ? s.values.map((v) => (typeof v === "number" && Number.isFinite(v) ? v : null)) : [],
  }));
  const unit = str(c.unit);
  const all = series.flatMap((s) => s.values.filter((v): v is number => v !== null));
  const maxV = Math.max(0, ...all);
  const minV = Math.min(0, ...all);
  const span = maxV - minV || 1;
  const W = 600;
  const H = 200;
  const padL = 44;
  const padB = 22;
  const plotW = W - padL - 8;
  const plotH = H - padB - 8;
  const y = (v: number) => 8 + plotH - ((v - minV) / span) * plotH;
  const n = Math.max(1, labels.length);
  const band = plotW / n;
  const fmt = (v: number) => `${unit === "$" ? "$" : ""}${toText(Number(v.toPrecision(4)))}${unit && unit !== "$" ? ` ${unit}` : ""}`;
  const labelEvery = Math.ceil(n / 12);
  return (
    <figure className="m-0 grid min-w-0 gap-1">
      {c.title ? <figcaption className="text-[0.78rem] font-medium">{str(c.title)}</figcaption> : null}
      <svg viewBox={`0 0 ${W} ${H}`} className="h-auto w-full" role="img" aria-label={str(c.title) || "Chart"}>
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
  return (
    <div className="grid min-w-0 gap-1">
      {c.title ? <div className="text-[0.78rem] font-medium">{str(c.title)}</div> : null}
      <div className="overflow-auto rounded-[var(--radius-md)] border border-[var(--color-border)]">
        <table className="w-full border-collapse text-[0.78rem]">
          <tbody>
            {objs(c.rows).map((r, i) => (
              <tr key={i} className="border-b border-[var(--color-border)] last:border-b-0">
                <td className="px-2 py-1.5 text-[var(--color-text-muted)]">{str(r.label)}</td>
                <td className="px-2 py-1.5 text-[var(--color-danger)] line-through decoration-[var(--color-danger)]/60">
                  {r.before === undefined || r.before === "" ? <span className="no-underline">—</span> : str(r.before)}
                </td>
                <td className="px-2 py-1.5 text-[var(--color-success)]">{r.after === undefined || r.after === "" ? "—" : str(r.after)}</td>
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

function FieldError({ text }: { text: string }) {
  return (
    <div className="text-[0.72rem] text-[var(--color-danger)]" role="alert">
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
  const label = str(c.label);
  return (
    <div className="grid min-w-0 content-start gap-1" data-genui-field={path} data-invalid={err ? "true" : undefined}>
      {!label && c.type !== "toggle" ? (
        // No visible label: the field id still names the control for
        // assistive technology (the validator allows label-less inputs).
        <label id={`${inputId}-label`} htmlFor={inputId} className="sr-only">
          {id}
        </label>
      ) : null}
      {label && c.type !== "toggle" ? (
        <label id={`${inputId}-label`} htmlFor={inputId} className="text-[0.78rem] font-medium text-[var(--color-text-secondary)]">
          {label}
          {c.required === true ? (
            <span className="ml-0.5 text-[var(--color-danger)]" aria-hidden>
              *
            </span>
          ) : null}
        </label>
      ) : null}
      {c.type === "repeater" ? (
        // A repeater's own fields lock themselves; its expand buttons must not.
        <Render c={c} value={value} onChange={onChange} inputId={inputId} />
      ) : (
        <fieldset disabled={locked || c.disabled === true || item?.disabled === true} className="m-0 min-w-0 border-0 p-0">
          <Render c={c} value={value} onChange={onChange} inputId={inputId} />
        </fieldset>
      )}
      {c.help ? <div className="text-[0.72rem] text-[var(--color-text-muted)]">{renderTemplate(str(c.help), scope)}</div> : null}
      {err ? <FieldError text={err} /> : null}
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
    if (!(typeof value === "number" && Number(text) === value)) setText(typeof value === "number" ? String(value) : "");
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
      {opts.length > FILTER_AT ? <Filter value={q} onChange={setQ} label={`Filter ${str(c.label) || "options"}`} /> : null}
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
  const opts = optionsOf(c);
  const v = typeof value === "string" ? value : "";
  const name = useId();
  // A radiogroup is not labelable by <label for>; name it from the field's
  // visible label (InputField gives it this id), or the id as a fallback.
  const groupLabel = str(c.label)
    ? { "aria-labelledby": `${inputId}-label` }
    : { "aria-label": str(c.id) || "Choice" };
  if (c.variant === "radio") {
    return (
      <div role="radiogroup" {...groupLabel} className="grid gap-1">
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
}: {
  c: Component;
  taken: Set<string>;
  onAdd: (v: string) => void;
  inputId?: string;
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
  const max = numOr(c.max_items);
  return (
    <div className="grid min-w-0 gap-1.5">
      {v.length > 0 ? (
        <div className="flex flex-wrap gap-1">
          {v.map((x) => (
            <Chip key={x} text={labelFor(c, x)} onRemove={locked ? undefined : () => onChange?.(v.filter((y) => y !== x))} />
          ))}
        </div>
      ) : null}
      {max === undefined || v.length < max ? (
        <Adder c={c} taken={new Set(v)} onAdd={(x) => onChange?.(v.includes(x) ? v : [...v, x])} inputId={inputId} />
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
        aria-label={str(c.label) || "Toggle"}
        onClick={() => onChange?.(!on)}
        className={[
          "relative h-5 w-9 shrink-0 rounded-full transition disabled:opacity-60",
          on ? "bg-[var(--color-primary)]" : "bg-[var(--color-overlay-strong)]",
        ].join(" ")}
      >
        <span className={["absolute top-0.5 size-4 rounded-full bg-[var(--color-white)] transition-all", on ? "left-[1.125rem]" : "left-0.5"].join(" ")} />
      </button>
      <span className="text-[0.8125rem]">{str(c.label)}</span>
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
  const parsed = parseListText(text, dedupe);
  const raw = text.split(/[\r\n]+/).filter((l) => l.trim() !== "").length;
  return (
    <div className="grid min-w-0 gap-1">
      <textarea
        id={inputId}
        rows={4}
        className={inputClass + " font-mono text-[0.75rem]"}
        placeholder={str(c.placeholder) || "One per line"}
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          onChange?.(parseListText(e.target.value, dedupe));
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
  const names = { include: str(c.include_label) || "Include", exclude: str(c.exclude_label) || "Exclude" };
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
    <div className="grid min-w-0 gap-1.5">
      <div className="grid min-w-0 grid-cols-1 gap-1.5 sm:grid-cols-2">
        {lane("include")}
        {lane("exclude")}
      </div>
      {!locked ? (
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
          <Adder c={c} taken={taken} inputId={inputId} onAdd={(x) => set({ ...v, [side]: [...v[side], x] })} />
        </div>
      ) : null}
    </div>
  );
}

function Repeater({ c, value, onChange }: Parameters<Renderer>[0]) {
  const { values, locked: cardLocked } = useCard();
  // A disabled repeater (or one nested in a disabled scope) keeps its expand
  // toggles usable but offers no add / duplicate / remove, and its fields
  // disable through ItemContext.
  const locked = cardLocked || c.disabled === true;
  const fields = children(c, "fields");
  const items = Array.isArray(value) ? (value as Values[]) : [];
  const id = str(c.id);
  const min = numOr(c.min_items) ?? 0;
  const max = numOr(c.max_items) ?? 200;
  const [openIdx, setOpenIdx] = useState<Set<number>>(() => new Set([0]));
  const toggleOpen = (i: number) =>
    setOpenIdx((s) => {
      const n = new Set(s);
      if (n.has(i)) n.delete(i);
      else n.add(i);
      return n;
    });
  const update = (next: Values[], open?: number) => {
    onChange?.(next);
    if (open !== undefined) setOpenIdx((s) => new Set(s).add(open));
  };
  const labelTpl = str(c.item_label);
  const ref = useRef<HTMLDivElement | null>(null);
  useReveal(ref, (path) => {
    // Paths into this repeater read "<id>[<index>].<field>".
    if (!path.startsWith(`${id}[`)) return;
    const i = Number.parseInt(path.slice(id.length + 1), 10);
    if (Number.isInteger(i)) setOpenIdx((s) => new Set(s).add(i));
  });
  return (
    <div ref={ref} className="grid min-w-0 gap-1.5">
      {items.map((item, i) => {
        const scope = scopeFor(values, item, i);
        const label = labelTpl ? renderTemplate(labelTpl, scope) : `Item ${i + 1}`;
        const open = openIdx.has(i);
        return (
          <div key={i} className="min-w-0 rounded-[var(--radius-md)] border border-[var(--color-border)]" data-repeater-item={i}>
            <div className="flex min-w-0 items-center gap-2 px-2.5 py-1.5">
              <button type="button" aria-expanded={open} onClick={() => toggleOpen(i)} className="flex min-w-0 flex-1 items-center gap-1.5 text-left">
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
                      onClick={() => update([...items.slice(0, i + 1), { ...item }, ...items.slice(i + 1)], i + 1)}
                    >
                      Duplicate
                    </button>
                  ) : null}
                  {items.length > min ? (
                    <button
                      type="button"
                      className="text-[0.72rem] text-[var(--color-text-muted)] hover:text-[var(--color-danger)]"
                      onClick={() => {
                        update(items.filter((_, j) => j !== i));
                        setOpenIdx((s) => new Set([...s].filter((j) => j !== i).map((j) => (j > i ? j - 1 : j))));
                      }}
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
          <button type="button" className={chipButton} onClick={() => update([...items, newItem(fields)], items.length)}>
            + {str(c.add_label) || "Add"}
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
