"use client";

// The user-side bubble for a card submission. The message itself is the
// machine-readable "[UI submission] card=… action=…" + JSON the model reads;
// people get the card's title, the button they pressed, and their answers by
// label — with the raw message one click away, so nothing the model saw is
// hidden from the person who sent it.

import { useContext, useState } from "react";
import type { Reply, Submission } from "./model";
import { fieldLabels, GenUiContext, summarizeValue } from "./transcript";

export function SubmissionBubble({ submission, raw }: { submission: Submission; raw: string }) {
  const { cards } = useContext(GenUiContext);
  const spec = cards.get(submission.cardId);
  const labels = fieldLabels(spec);
  const action = spec?.actions?.find((a) => a.id === submission.actionId)?.label ?? submission.actionId;
  const [showRaw, setShowRaw] = useState(false);
  const entries = Object.entries(submission.values);
  return (
    <div
      data-testid="genui-submission-bubble"
      data-card-id={submission.cardId}
      className="min-w-0 [overflow-wrap:anywhere] rounded-[1.1rem] bg-[var(--color-overlay-soft)] px-3 py-2.5 text-[0.8125rem] leading-[1.5] text-[var(--color-text-primary)] sm:rounded-[1.25rem] sm:px-4 sm:py-3"
    >
      <div className="flex items-center gap-1.5 font-medium">
        <span aria-hidden>◧</span>
        <span>{spec?.title ?? "Card"}</span>
        <span className="text-[var(--color-text-muted)]">· {action}</span>
      </div>
      {entries.length > 0 ? (
        <dl className="mt-1.5 grid grid-cols-[minmax(5rem,auto)_1fr] gap-x-3 gap-y-0.5 text-[0.78rem]">
          {entries.slice(0, 12).map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-[var(--color-text-muted)]">{labels.get(k) ?? k}</dt>
              <dd className="m-0 min-w-0">{summarizeValue(v)}</dd>
            </div>
          ))}
        </dl>
      ) : null}
      {entries.length > 12 ? (
        <div className="mt-1 text-[0.72rem] text-[var(--color-text-muted)]">+{entries.length - 12} more field(s)</div>
      ) : null}
      <button
        type="button"
        className="mt-1.5 text-[0.7rem] text-[var(--color-text-muted)] underline hover:text-[var(--color-text-primary)]"
        aria-expanded={showRaw}
        onClick={() => setShowRaw((s) => !s)}
      >
        {showRaw ? "Hide what was sent" : "Show what was sent"}
      </button>
      {showRaw ? (
        <pre
          className="mt-1 max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-md bg-[var(--color-overlay-strong)] p-2 text-[0.72rem]"
          style={{ fontFamily: "var(--font-code)" }}
        >
          {raw}
        </pre>
      ) : null}
    </div>
  );
}

/**
 * The user-side bubble for a quick reply: the button's text as the message,
 * with the card it answered named underneath (the marker line is for the
 * model and the transcript, not for reading).
 */
export function ReplyBubble({ reply }: { reply: Reply }) {
  const { cards } = useContext(GenUiContext);
  const spec = cards.get(reply.cardId);
  const action = spec?.actions?.find((a) => a.id === reply.actionId)?.label ?? reply.actionId;
  return (
    <div
      data-testid="genui-reply-bubble"
      data-card-id={reply.cardId}
      className="min-w-0 [overflow-wrap:anywhere] rounded-[1.1rem] bg-[var(--color-overlay-soft)] px-3 py-2.5 text-[0.875rem] leading-[1.55] text-[var(--color-text-primary)] sm:rounded-[1.25rem] sm:px-4 sm:py-3"
    >
      <div className="whitespace-pre-wrap">{reply.text}</div>
      <div className="mt-1 flex items-center gap-1.5 text-[0.7rem] text-[var(--color-text-muted)]">
        <span aria-hidden>◧</span>
        <span>
          {spec?.title ?? "Card"} · {action}
        </span>
      </div>
    </div>
  );
}
