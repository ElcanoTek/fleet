import { t as tr } from "@/app/lib/i18n";
// How an administrator steps into someone else's chat (ADR-0115, ADR-0117).
// Three ways, each saying plainly who will see it and whether the assistant
// reads it:
//
//   - a pinned note: stays above the conversation for the chat's people until
//     it is removed; the assistant never reads it;
//   - a message to the assistant: starts a turn as the chat's owner, labelled
//     as the administrator's, always asking the owner before anything that
//     needs approval;
//   - a notice: one line in the conversation, which the assistant never reads.
//
// Nothing here types as the owner.

import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { responseErrorText } from "@/app/lib/responseError";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { Icon } from "@/app/shared/ui/Icon";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

import { FormActions, InlineConfirmButton, MissingFieldsHint, SETTINGS_INPUT, Segmented } from "../../ui/atoms";
import { ConnPanel } from "../../ui/panels";
import { formatAgo } from "../lib";
import { chatUrl, sendJSON, type AdminChat, type AdminNote, type AdminRequest } from "./api";

export const NOTE_MAX = 2000;

export function Field({ label, hint, children }: { label: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <label className="grid gap-1 text-[0.78rem] font-medium text-[var(--color-text-secondary)]">
      <span className="flex flex-wrap items-baseline justify-between gap-x-3">
        <span>{label}</span>
        {hint ? <span className="font-normal text-[var(--color-text-muted)]">{hint}</span> : null}
      </span>
      {children}
    </label>
  );
}

export function Status({ text, tone }: { text: string | null; tone: "ok" | "error" }) {
  if (!text) return null;
  return (
    <p
      role={tone === "error" ? "alert" : "status"}
      className={cn("m-0 text-[0.75rem]", tone === "error" ? "text-[var(--color-danger)]" : "text-[var(--color-text-muted)]")}
    >
      {text}
    </p>
  );
}

// useRequestFollower polls an admin request until it is settled (and, for a
// message, until its turn has ended), reporting each step.
export function useRequestFollower(onDone: () => void) {
  const [status, setStatus] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  const timer = useRef<number | null>(null);
  useEffect(() => () => {
    if (timer.current !== null) window.clearTimeout(timer.current);
  }, []);
  const follow = useCallback(
    (first: AdminRequest) => {
      const started = Date.now();
      const describe = (r: AdminRequest): [string, boolean] => {
        if (r.state === "failed") return [tr("Failed: {0}", [r.result ?? ""]), true];
        if (r.state === "pending") return [tr("Waiting for the server…"), false];
        if (r.state === "running") return [tr("The server is carrying it out…"), false];
        if (r.kind !== "message") return [tr("Done: {0}", [r.result ?? ""]), true];
        if (r.input_state === "queued") return [tr("Queued in the chat (waits for any running turn)…"), false];
        if (r.input_state === "running") return [tr("The turn is running…"), false];
        return [tr("The turn ended ({0}).", [r.input_state || tr("removed")]), true];
      };
      const step = function step(req: AdminRequest): void {
        const [text, done] = describe(req);
        setStatus(text);
        setFailed(req.state === "failed");
        // Each step reloads the chat, so the conversation above shows the
        // message arriving and the reply being written.
        onDone();
        if (done) return;
        if (Date.now() - started > 10 * 60 * 1000) {
          setStatus(tr("Still going after 10 minutes; check the chat later."));
          return;
        }
        timer.current = window.setTimeout(() => {
          fetch(`/api/admin/chat-requests/${encodeURIComponent(req.id)}`, { cache: "no-store" })
            .then(async (res) => {
              if (!res.ok) throw new Error(await responseErrorText(res, tr("Couldn’t check the request")));
              const body = (await res.json()) as { request: AdminRequest };
              step(body.request);
            })
            .catch((err: unknown) => {
              setStatus(errorMessage(err, tr("Couldn’t check the request")));
              setFailed(true);
            });
        }, 2000);
      };
      step(first);
    },
    [onDone],
  );
  return { status, failed, follow, setStatus, setFailed };
}

/** postRequest records an admin request (message, stop, transfer, delete…). */
export async function postRequest(chatId: string, body: Record<string, unknown>): Promise<AdminRequest | null> {
  const res = await sendJSON<{ request: AdminRequest }>(chatUrl(chatId, "/requests"), "POST", body);
  return res?.request ?? null;
}

// Reach says, for the way of stepping in that is selected, who sees it and
// whether the assistant reads it — the two facts an administrator has to get
// right before writing into someone else's chat.
function Reach({ people, assistant }: { people: string; assistant: "reads" | "never" }) {
  return (
    <ul className="m-0 flex list-none flex-wrap gap-x-4 gap-y-1 p-0 text-[0.75rem] text-[var(--color-text-secondary)]" data-testid="step-in-reach">
      <li className="flex items-center gap-1.5">
        <Icon name="users" className="size-3.5 shrink-0 text-[var(--color-text-muted)]" />
        {people}
      </li>
      <li className="flex items-center gap-1.5">
        <Icon name={assistant === "reads" ? "sparkles" : "lock"} className="size-3.5 shrink-0 text-[var(--color-text-muted)]" />
        {assistant === "reads" ? tr("The assistant reads it and replies") : tr("The assistant never reads it")}
      </li>
    </ul>
  );
}

export type StepInMode = "note" | "message" | "notice";

export function StepIn({
  chat,
  note,
  mode,
  onMode,
  onChanged,
}: {
  chat: AdminChat;
  note: AdminNote | null;
  mode: StepInMode;
  onMode: (m: StepInMode) => void;
  onChanged: () => void;
}) {
  return (
    <ConnPanel className="mb-0" >
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
        <h3 className="m-0 text-[0.95rem] font-semibold text-[var(--color-text-primary)]">{tr("Step in")}</h3>
        <Segmented<StepInMode>
          label={tr("How to step in")}
          value={mode}
          onChange={onMode}
          options={[
            { value: "note", label: tr("Pin a note") },
            { value: "message", label: tr("Message the assistant") },
            { value: "notice", label: tr("Add a notice") },
          ]}
        />
      </div>
      <div className="mt-3">
        {mode === "note" ? (
          <NoteForm key={note?.updated_at ?? 0} chat={chat} note={note} onChanged={onChanged} />
        ) : mode === "message" ? (
          <MessageForm chat={chat} onChanged={onChanged} />
        ) : (
          <NoticeForm chat={chat} onChanged={onChanged} />
        )}
      </div>
    </ConnPanel>
  );
}

function NoteForm({ chat, note, onChanged }: { chat: AdminChat; note: AdminNote | null; onChanged: () => void }) {
  const [text, setText] = useState(note?.text ?? "");
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState<{ text: string; error: boolean } | null>(null);
  const hintId = useId();
  const trimmed = text.trim();
  const unchanged = Boolean(note) && trimmed === note?.text;
  const tooLong = [...trimmed].length > NOTE_MAX;

  const run = async (fn: () => Promise<unknown>, ok: string) => {
    setBusy(true);
    try {
      await fn();
      setStatus({ text: ok, error: false });
      onChanged();
    } catch (err) {
      setStatus({ text: errorMessage(err, tr("The change failed")), error: true });
    } finally {
      setBusy(false);
    }
  };
  const pin = (e: FormEvent) => {
    e.preventDefault();
    void run(() => sendJSON(chatUrl(chat.id, "/note"), "PUT", { text: trimmed }), note ? tr("Note updated. It shows as new in the chat.") : tr("Note pinned to the chat."));
  };
  const unpin = () => {
    void run(async () => {
      await sendJSON(chatUrl(chat.id, "/note"), "DELETE");
      setText("");
    }, tr("Note removed from the chat."));
  };
  return (
    <form className="grid gap-2.5" onSubmit={pin} data-testid="step-in-note">
      <Reach
        people={chat.multiplayer ? tr("Everyone in the chat sees it, pinned above the conversation") : tr("{0} sees it, pinned above the conversation", [chat.owner_email])}
        assistant="never"
      />
      <Field label={tr("Note")} hint={tr("{0} / {1}", [[...trimmed].length, NOTE_MAX])}>
        <textarea
          className={cn(SETTINGS_INPUT, "min-h-[5.5rem]")}
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder={tr("For example: I moved your morning patrol to 07:00. Reply to the card above when you’re back and it will carry on.")}
          aria-invalid={tooLong || undefined}
          data-testid="admin-note-text"
        />
      </Field>
      <FormActions
        className="justify-between"
        hint={
          tooLong ? (
            <span id={hintId} className="min-w-0 flex-[1_1_auto] text-right text-[0.72rem] text-[var(--color-danger)]">
              {tr("A note is at most {0} characters.", [NOTE_MAX])}
            </span>
          ) : (
            <MissingFieldsHint id={hintId} missing={trimmed ? [] : [tr("Note")]} />
          )
        }
      >
        {note ? <InlineConfirmButton label={tr("Unpin")} confirmLabel={tr("Confirm unpin")} onConfirm={unpin} disabled={busy} testId="admin-note-unpin" /> : null}
        <Button type="submit" size="sm" disabled={busy || !trimmed || unchanged || tooLong} aria-describedby={hintId} data-testid="admin-note-pin">
          <Icon name="pin" className="size-3.5" />
          {note ? tr("Update note") : tr("Pin note")}
        </Button>
      </FormActions>
      <Status tone={status?.error ? "error" : "ok"} text={status?.text ?? null} />
    </form>
  );
}

function MessageForm({ chat, onChanged }: { chat: AdminChat; onChanged: () => void }) {
  const [message, setMessage] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const { status, failed, follow, setStatus, setFailed } = useRequestFollower(onChanged);
  const hintId = useId();
  const canMessage = !chat.archived && !chat.lockdown;
  const missing = [message.trim() ? "" : tr("Message"), reason.trim() ? "" : tr("Reason")].filter(Boolean);

  const send = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setStatus(null);
    try {
      const req = await postRequest(chat.id, { kind: "message", message, reason });
      if (req) follow(req);
      setMessage("");
    } catch (err) {
      setStatus(errorMessage(err, tr("The change failed")));
      setFailed(true);
    } finally {
      setBusy(false);
    }
  };
  if (!canMessage) {
    return (
      <p className="m-0 text-[0.8rem] text-[var(--color-text-secondary)]" data-testid="step-in-message-off">
        {chat.lockdown
          ? tr("A Lockdown chat takes no administrator messages. Pin a note to reach its owner instead.")
          : tr("An archived chat takes no administrator messages. Restore it first, or pin a note.")}
      </p>
    );
  }
  return (
    <form className="grid gap-2.5" onSubmit={send} data-testid="step-in-message">
      <Reach people={tr("{0} sees it, marked as yours, and the reply", [chat.owner_email])} assistant="reads" />
      <p className="m-0 text-[0.75rem] leading-[1.55] text-[var(--color-text-muted)]">
        {tr("The turn runs as {0}, with their connections. Anything that needs approval asks them first, whatever the chat’s approval setting.", [chat.owner_email])}
      </p>
      <Field label={tr("Message")}>
        <textarea
          className={cn(SETTINGS_INPUT, "min-h-[5.5rem]")}
          value={message}
          onChange={(e) => setMessage(e.target.value)}
          data-testid="admin-chat-message"
        />
      </Field>
      <Field label={tr("Reason")} hint={tr("recorded and shown in the chat")}>
        <input className={SETTINGS_INPUT} value={reason} onChange={(e) => setReason(e.target.value)} data-testid="admin-chat-reason" />
      </Field>
      <FormActions hint={<MissingFieldsHint id={hintId} missing={missing} />}>
        <Button type="submit" size="sm" disabled={busy || missing.length > 0} aria-describedby={hintId}>
          <Icon name="arrow-up" className="size-3.5" />
          {tr("Send message")}
        </Button>
      </FormActions>
      <Status tone={failed ? "error" : "ok"} text={status} />
    </form>
  );
}

function NoticeForm({ chat, onChanged }: { chat: AdminChat; onChanged: () => void }) {
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState<{ text: string; error: boolean } | null>(null);
  const hintId = useId();
  const post = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await sendJSON(chatUrl(chat.id, "/notice"), "POST", { text: notice });
      setNotice("");
      setStatus({ text: tr("Notice posted."), error: false });
      onChanged();
    } catch (err) {
      setStatus({ text: errorMessage(err, tr("The change failed")), error: true });
    } finally {
      setBusy(false);
    }
  };
  return (
    <form className="grid gap-2.5" onSubmit={post} data-testid="step-in-notice">
      <Reach people={tr("A line in the conversation, where the chat’s people will scroll past it")} assistant="never" />
      <Field label={tr("Notice")}>
        <input className={SETTINGS_INPUT} value={notice} onChange={(e) => setNotice(e.target.value)} maxLength={1000} data-testid="admin-notice-text" />
      </Field>
      <FormActions hint={<MissingFieldsHint id={hintId} missing={notice.trim() ? [] : [tr("Notice")]} />}>
        <Button type="submit" size="sm" variant="outline" disabled={busy || !notice.trim()} aria-describedby={hintId}>
          {tr("Post notice")}
        </Button>
      </FormActions>
      <Status tone={status?.error ? "error" : "ok"} text={status?.text ?? null} />
    </form>
  );
}

// PinnedNoteCard is the note as the administrator sees it: what it says, who
// pinned it, and whether anyone in the chat has acknowledged it.
export function PinnedNoteCard({ note, onEdit }: { note: AdminNote; onEdit: () => void }) {
  return (
    <div
      className="rounded-[var(--radius-lg)] border border-[color-mix(in_srgb,var(--color-accent)_34%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-accent)_7%,transparent)] px-3.5 py-3"
      data-testid="admin-pinned-note"
    >
      <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
        <span className="flex min-w-0 items-center gap-1.5 text-[0.7rem] font-semibold uppercase tracking-[0.07em] text-[var(--color-text-secondary)]">
          <Icon name="pin" className="size-3.5 shrink-0 text-[var(--color-accent)]" />
          {tr("Pinned note")}
        </span>
        <Button type="button" variant="ghost" size="xs" onClick={onEdit}>
          <Icon name="edit" className="size-3" />
          {tr("Edit")}
        </Button>
      </div>
      <p className="mb-0 mt-1.5 whitespace-pre-wrap text-[0.86rem] leading-[1.55] text-[var(--color-text-primary)] [overflow-wrap:anywhere]">{note.text}</p>
      <p className="mb-0 mt-2 flex flex-wrap items-center gap-x-1.5 gap-y-0.5 text-[0.72rem] text-[var(--color-text-muted)]">
        <span className="min-w-0 [overflow-wrap:anywhere]">{tr("{0} · {1}", [note.author, formatAgo(note.updated_at)])}</span>
        <span aria-hidden="true">·</span>
        {note.seen_at ? (
          <span className="flex items-center gap-1 text-[var(--color-success)]" data-testid="admin-note-seen">
            <Icon name="check" className="size-3 shrink-0" />
            <span className="min-w-0 [overflow-wrap:anywhere]">{tr("Seen by {0} {1}", [note.seen_by ?? "", formatAgo(note.seen_at)])}</span>
          </span>
        ) : (
          <span data-testid="admin-note-unseen">{tr("Not acknowledged yet")}</span>
        )}
      </p>
    </div>
  );
}
