import { t as tr } from "@/app/lib/i18n";
// The read-only conversation on Settings → Admin → Chats: the chat laid out
// the way its owner sees it — their messages as bubbles, the assistant's
// replies as formatted text, patrol runs, administrator messages and notices
// each in the form the chat gives them — so an administrator helping someone
// reads what that person is looking at, not a log.
//
// It is a view, not a seat. There is no composer and nothing here acts as the
// owner; an administrator steps in through the panel below it, and every one
// of those actions is labelled as theirs. Files stay in the owner's workspace
// (the workspace route is the owner's), so a file or image the conversation
// points at is named, never fetched.

import { lazy, Suspense, useMemo, type ReactNode } from "react";
import { AdminMessageNotice, AutoContinueNotice, FleetNotices, PatrolTurnNotice } from "@/app/chat/ui/FleetNotices";
import { ReplyBubble, SubmissionBubble } from "@/app/chat/ui/genui/SubmissionBubble";
import { parseReplyMessage, parseSubmissionMessage } from "@/app/chat/ui/genui/model";
import { ADMIN_MESSAGE_KIND, APPROVAL_RESUME_KIND, CHAT_PATROL_KIND } from "@/app/chat/ui/history";
import { ReadOnlyFilesContext } from "@/app/chat/ui/LockedFiles";
import { redactUnsharedFiles, type ReadOnlyFilePolicy } from "@/app/chat/ui/workspaceHref";
import { Icon } from "@/app/shared/ui/Icon";

import { formatAgo } from "../lib";
import type { TranscriptEntry } from "./api";

// The markdown pipeline is lazy-loaded for the same reason the chat does it:
// react-markdown + micromark are worth keeping off this page's first paint.
const AssistantMarkdown = lazy(() => import("@/app/chat/ui/AssistantContent"));

type Row =
  | { kind: "user"; id: number; at: number; text: string; sender?: string }
  | { kind: "assistant"; id: number; at: number; text: string; truncated: boolean }
  | { kind: "admin"; id: number; at: number; text: string; sender?: string }
  | { kind: "patrol" | "resume" | "notice" | "fleet"; id: number; at: number; text: string }
  | { kind: "tool"; id: number; at: number; tool: string; text: string; result: boolean; error: boolean };

// toRows shapes stored entries into what the chat draws. Consecutive
// assistant text entries are one reply (a reply can be stored in several
// pieces within a turn).
export function toRows(entries: TranscriptEntry[]): Row[] {
  const rows: Row[] = [];
  for (const e of entries) {
    if (e.type === "tool_call" || e.type === "tool_result") {
      rows.push({ kind: "tool", id: e.id, at: e.created_at, tool: e.tool ?? "", text: e.text, result: e.type === "tool_result", error: Boolean(e.is_error) });
      continue;
    }
    if (e.type === "notice") {
      rows.push({ kind: "notice", id: e.id, at: e.created_at, text: e.text });
      continue;
    }
    if (e.type !== "text" || !e.text) continue;
    if (e.role === "user") {
      if (e.kind === ADMIN_MESSAGE_KIND) rows.push({ kind: "admin", id: e.id, at: e.created_at, text: e.text, sender: e.sender });
      else if (e.kind === CHAT_PATROL_KIND) rows.push({ kind: "patrol", id: e.id, at: e.created_at, text: e.text });
      else if (e.kind === APPROVAL_RESUME_KIND) rows.push({ kind: "resume", id: e.id, at: e.created_at, text: e.text });
      else if (e.kind && e.kind !== "text") rows.push({ kind: "fleet", id: e.id, at: e.created_at, text: e.text });
      else rows.push({ kind: "user", id: e.id, at: e.created_at, text: e.text, sender: e.sender });
      continue;
    }
    const last = rows[rows.length - 1];
    if (last && last.kind === "assistant") {
      last.text += e.text;
      last.truncated = last.truncated || Boolean(e.truncated);
    } else {
      rows.push({ kind: "assistant", id: e.id, at: e.created_at, text: e.text, truncated: Boolean(e.truncated) });
    }
  }
  return rows;
}

function Stamp({ at, children }: { at: number; children?: ReactNode }) {
  return (
    <div className="mt-1 flex items-center gap-1.5 text-[0.68rem] text-[var(--color-text-muted)]">
      {children}
      <span>{formatAgo(at)}</span>
    </div>
  );
}

function UserRow({ row, owner }: { row: Extract<Row, { kind: "user" }>; owner: string }) {
  const submission = parseSubmissionMessage(row.text);
  const reply = submission ? null : parseReplyMessage(row.text);
  // A Multiplayer participant's message names its sender; the owner's own
  // needs no label in their chat, and none here beyond the header.
  const from = row.sender && row.sender.toLowerCase() !== owner.toLowerCase() ? row.sender : "";
  return (
    <div className="flex flex-col items-end" data-testid="admin-transcript-user">
      <div className="max-w-[85%]">
        {submission ? (
          <SubmissionBubble submission={submission} raw={row.text} />
        ) : reply ? (
          <ReplyBubble reply={reply} />
        ) : (
          <div className="whitespace-pre-wrap rounded-[1rem] bg-[var(--color-overlay-soft)] px-4 py-2.5 text-[0.9rem] leading-[1.55] text-[var(--color-text-primary)] [overflow-wrap:anywhere]">
            {row.text}
          </div>
        )}
      </div>
      <Stamp at={row.at}>{from ? <span className="min-w-0 truncate">{from} ·</span> : null}</Stamp>
    </div>
  );
}

function ToolRow({ row }: { row: Extract<Row, { kind: "tool" }> }) {
  return (
    <details className="group rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] text-[0.75rem]" data-testid="admin-transcript-tool">
      <summary className="flex cursor-pointer list-none items-center gap-2 px-2.5 py-1.5 text-[var(--color-text-secondary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] [&::-webkit-details-marker]:hidden">
        <Icon name="tools" className="size-3.5 shrink-0 text-[var(--color-text-muted)]" />
        <span className="min-w-0 flex-1 truncate">
          {row.result
            ? row.error
              ? tr("Tool error · {0}", [row.tool])
              : tr("Tool result · {0}", [row.tool])
            : tr("Tool call · {0}", [row.tool])}
        </span>
        <Icon name="chevron-right" className="size-3 shrink-0 text-[var(--color-text-muted)] transition-transform group-open:rotate-90" />
      </summary>
      <pre className="m-0 max-h-64 overflow-auto whitespace-pre-wrap border-t border-[var(--color-border-subtle)] px-2.5 py-2 font-mono text-[0.7rem] leading-[1.5] text-[var(--color-text-secondary)] [overflow-wrap:anywhere]">
        {row.text.trim() || tr("(empty)")}
      </pre>
    </details>
  );
}

export function AdminTranscript({ entries, owner }: { entries: TranscriptEntry[]; owner: string }) {
  const rows = useMemo(() => toRows(entries), [entries]);
  const imagePlaceholder = tr("Image kept in the owner’s workspace.");
  const policy = useMemo<ReadOnlyFilePolicy>(() => ({ mode: "withhold", imagePlaceholder }), [imagePlaceholder]);
  return (
    <ReadOnlyFilesContext.Provider value={policy}>
      <ol className="m-0 flex list-none flex-col gap-4 p-0" data-testid="admin-chat-transcript">
        {rows.map((row) => (
          <li key={`${row.kind}-${row.id}`} className="min-w-0">
            {row.kind === "user" ? (
              <UserRow row={row} owner={owner} />
            ) : row.kind === "assistant" ? (
              <div data-testid="admin-transcript-assistant">
                <div className="assistant-markdown max-w-full text-[0.9rem] leading-[1.6] text-[var(--color-text-primary)]">
                  <Suspense fallback={<div className="whitespace-pre-wrap">{row.text}</div>}>
                    <AssistantMarkdown content={redactUnsharedFiles(row.text, imagePlaceholder)} />
                  </Suspense>
                </div>
                <Stamp at={row.at}>
                  {row.truncated ? <span>{tr("Long reply, cut here ·")}</span> : null}
                </Stamp>
              </div>
            ) : row.kind === "admin" ? (
              <AdminMessageNotice sender={row.sender} text={row.text} />
            ) : row.kind === "patrol" ? (
              <PatrolTurnNotice text={row.text} />
            ) : row.kind === "resume" ? (
              <AutoContinueNotice text={row.text} />
            ) : row.kind === "tool" ? (
              <ToolRow row={row} />
            ) : (
              <FleetNotices notices={[row.text]} />
            )}
          </li>
        ))}
      </ol>
    </ReadOnlyFilesContext.Provider>
  );
}
