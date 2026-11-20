import { t as tr } from "@/app/lib/i18n";
// The Settings tab of a chat on Settings → Admin → Chats: its title, model and
// approval mode; handing it to another person; and archiving or deleting it
// (ADR-0115, ADR-0116). Every change takes a reason, is recorded and is noted
// in the chat.

import { useId, useState } from "react";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

import { ConnBadge, FormActions, MissingFieldsHint, SETTINGS_INPUT, btnClass } from "../../ui/atoms";
import { ConnPanel, ConnPanelHead, ConnPanelSub } from "../../ui/panels";
import { chatUrl, sendJSON, type AdminChat } from "./api";
import { Field, Status, postRequest, useRequestFollower } from "./StepIn";

export function ReasonField({ value, onChange, testId }: { value: string; onChange: (v: string) => void; testId?: string }) {
  return (
    <Field label={tr("Reason")} hint={tr("recorded and shown in the chat")}>
      <input className={SETTINGS_INPUT} value={value} onChange={(e) => onChange(e.target.value)} data-testid={testId} />
    </Field>
  );
}

export function ChatSettingsPanel({ chat, onDone }: { chat: AdminChat; onDone: () => void }) {
  const [title, setTitle] = useState(chat.title);
  const [model, setModel] = useState(chat.model);
  const [mode, setMode] = useState(chat.approval_mode);
  const [reason, setReason] = useState("");
  const [status, setStatus] = useState<{ text: string; error: boolean } | null>(null);
  const hintId = useId();

  const body: Record<string, unknown> = {};
  if (title.trim() && title.trim() !== chat.title) body.title = title.trim();
  if (model.trim() && model.trim() !== chat.model) body.model = model.trim();
  if (mode === "manual" && chat.approval_mode !== "manual") body.approval_mode = "manual";
  const dirty = Object.keys(body).length > 0;

  const save = async () => {
    try {
      await sendJSON(chatUrl(chat.id), "PATCH", { ...body, reason });
      setStatus({ text: tr("Saved and noted in the chat."), error: false });
      setReason("");
      onDone();
    } catch (err) {
      setStatus({ text: errorMessage(err, tr("The change failed")), error: true });
    }
  };
  return (
    <ConnPanel>
      <ConnPanelHead title={tr("Chat settings")} />
      <ConnPanelSub>
        {tr("You can turn a chat’s approvals down to Manual, never up: automatic approvals are the owner’s own decision. A Lockdown chat keeps its model.")}
      </ConnPanelSub>
      <div className="grid gap-2.5 sm:grid-cols-2">
        <div className="sm:col-span-2">
          <Field label={tr("Title")}>
            <input className={SETTINGS_INPUT} value={title} onChange={(e) => setTitle(e.target.value)} data-testid="admin-chat-title" />
          </Field>
        </div>
        <Field label={tr("Model")}>
          <input
            className={SETTINGS_INPUT}
            value={model}
            onChange={(e) => setModel(e.target.value)}
            disabled={chat.lockdown}
            title={chat.lockdown ? tr("A Lockdown chat keeps its model.") : undefined}
            data-testid="admin-chat-model"
          />
        </Field>
        <Field label={tr("Approvals")}>
          <select className={SETTINGS_INPUT} value={mode} onChange={(e) => setMode(e.target.value)} data-testid="admin-chat-approvals">
            {chat.approval_mode === "autonomous" ? <option value="autonomous">{tr("Autonomous (owner’s choice)")}</option> : null}
            {chat.approval_mode === "auto" ? <option value="auto">{tr("Auto-approve (owner’s choice)")}</option> : null}
            {chat.approval_mode === "notify" ? <option value="notify">{tr("Notify (owner’s choice)")}</option> : null}
            <option value="manual">{tr("Manual")}</option>
          </select>
        </Field>
        <div className="sm:col-span-2">
          <ReasonField value={reason} onChange={setReason} testId="admin-settings-reason" />
        </div>
      </div>
      <FormActions
        className="mt-3"
        hint={dirty ? <MissingFieldsHint id={hintId} missing={reason.trim() ? [] : [tr("Reason")]} /> : null}
      >
        <Button type="button" size="sm" disabled={!dirty || !reason.trim()} aria-describedby={hintId} onClick={() => void save()}>
          {tr("Save settings")}
        </Button>
      </FormActions>
      {status ? <div className="mt-2"><Status tone={status.error ? "error" : "ok"} text={status.text} /></div> : null}
    </ConnPanel>
  );
}

export function TransferPanel({ chat, onDone }: { chat: AdminChat; onDone: () => void }) {
  const [toEmail, setToEmail] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const { status, failed, follow, setStatus, setFailed } = useRequestFollower(onDone);
  const hintId = useId();
  const missing = [toEmail.includes("@") ? "" : tr("New owner’s email"), reason.trim() ? "" : tr("Reason")].filter(Boolean);
  const submit = async () => {
    setBusy(true);
    setStatus(null);
    try {
      const req = await postRequest(chat.id, { kind: "transfer", to_email: toEmail, reason });
      if (req) follow(req);
    } catch (err) {
      setStatus(errorMessage(err, tr("The change failed")));
      setFailed(true);
    } finally {
      setBusy(false);
    }
  };
  return (
    <ConnPanel>
      <ConnPanelHead title={tr("Hand over to someone else")} />
      <ConnPanelSub>
        {tr("The chat, its files and its patrols move to the new owner, exactly as when {0} transfers it themselves.", [chat.owner_email])}
      </ConnPanelSub>
      <div className="grid gap-2.5 sm:grid-cols-2">
        <Field label={tr("New owner’s email")}>
          <input className={SETTINGS_INPUT} type="email" value={toEmail} onChange={(e) => setToEmail(e.target.value)} data-testid="admin-transfer-email" />
        </Field>
        <ReasonField value={reason} onChange={setReason} testId="admin-transfer-reason" />
      </div>
      <FormActions className="mt-3" hint={toEmail || reason ? <MissingFieldsHint id={hintId} missing={missing} /> : null}>
        <Button type="button" size="sm" variant="outline" disabled={busy || missing.length > 0} aria-describedby={hintId} onClick={() => void submit()}>
          {tr("Transfer")}
        </Button>
      </FormActions>
      <div className="mt-2">
        <Status tone={failed ? "error" : "ok"} text={status} />
      </div>
    </ConnPanel>
  );
}

// LifecyclePanel: the safe path first (archive: nothing is lost), then the
// recycle bin (restorable for 30 days), the emergency path last (a permanent
// delete, confirmed by typing the chat's title).
export function LifecyclePanel({ chat, onDone }: { chat: AdminChat; onDone: () => void }) {
  const [reason, setReason] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const { status, failed, follow, setStatus, setFailed } = useRequestFollower(onDone);
  const binned = Boolean(chat.deleted_at);

  const guarded = async (fn: () => Promise<void>) => {
    setBusy(true);
    setStatus(null);
    try {
      await fn();
    } catch (err) {
      setStatus(errorMessage(err, tr("The change failed")));
      setFailed(true);
    } finally {
      setBusy(false);
    }
  };
  const request = (kind: "delete" | "restore" | "purge") =>
    guarded(async () => {
      const req = await postRequest(chat.id, { kind, reason, confirm: kind === "purge" ? confirm : undefined });
      if (req) follow(req);
    });
  const archive = (archived: boolean) =>
    guarded(async () => {
      await sendJSON(chatUrl(chat.id), "PATCH", { archived, reason });
      setFailed(false);
      setStatus(archived ? tr("Archived and noted in the chat.") : tr("Restored from the archive and noted in the chat."));
      setReason("");
      onDone();
    });
  // The title; the id only for a chat that has none (the server checks too).
  const titleMatches = confirm.trim() !== "" && confirm.trim() === (chat.title.trim() || chat.id);
  const noReason = !reason.trim();
  return (
    <ConnPanel>
      <ConnPanelHead title={binned ? tr("In the recycle bin") : tr("Archive or delete")}>
        {binned && chat.purges_at ? (
          <ConnBadge variant="warn">{tr("deleted for good on {0}", [new Date(chat.purges_at * 1000).toLocaleDateString()])}</ConnBadge>
        ) : null}
      </ConnPanelHead>
      <ConnPanelSub>
        {binned
          ? tr("Restore brings the chat back for its owner; its patrols stay paused until someone resumes them. Delete permanently removes it and its files now.")
          : tr("Prefer archiving: nothing is lost, and its patrols pause. Delete moves the chat to the recycle bin: it disappears and stops, and is deleted for good after 30 days unless restored.")}
      </ConnPanelSub>
      <ReasonField value={reason} onChange={setReason} testId="admin-delete-reason" />
      <div className="mt-3 flex flex-wrap items-center gap-2">
        {binned ? (
          <Button type="button" size="sm" disabled={busy || noReason} onClick={() => void request("restore")}>
            {tr("Restore")}
          </Button>
        ) : (
          <>
            <Button type="button" size="sm" variant="outline" disabled={busy || noReason} onClick={() => void archive(!chat.archived)} data-testid="admin-archive">
              {chat.archived ? tr("Restore from archive") : tr("Archive")}
            </Button>
            <button type="button" className={btnClass({ sm: true, reveal: true, danger: true })} disabled={busy || noReason} onClick={() => void request("delete")}>
              {tr("Move to recycle bin")}
            </button>
          </>
        )}
      </div>
      <div className="mt-4 border-t border-[var(--color-border-subtle)] pt-3">
        <p className="mb-2 mt-0 text-[0.75rem] leading-[1.55] text-[var(--color-text-muted)]">
          {tr("Delete permanently is for emergencies, such as a full disk, and cannot be undone.")}
        </p>
        <div className="flex flex-wrap items-end gap-2">
          <input
            className={cn(SETTINGS_INPUT, "max-w-[20rem] flex-1")}
            placeholder={tr("Type the chat’s title to delete permanently")}
            aria-label={tr("Type the chat’s title to delete permanently")}
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            data-testid="admin-delete-confirm"
          />
          <button
            type="button"
            className={btnClass({ sm: true, armed: titleMatches, reveal: true })}
            disabled={busy || noReason || !titleMatches}
            onClick={() => void request("purge")}
          >
            {tr("Delete permanently")}
          </button>
        </div>
      </div>
      <div className="mt-2">
        <Status tone={failed ? "error" : "ok"} text={status} />
      </div>
    </ConnPanel>
  );
}
