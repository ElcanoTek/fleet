import { t as tr } from "@/app/lib/i18n";
import { rich } from "@/app/lib/i18n/rich";
import { useId, useRef, useState } from "react";
import { conversationApiUrl } from "@/app/lib/conversationApiUrl";
import { responseErrorText } from "@/app/lib/responseError";
import { DialogShell } from "@/app/shared/ui/DialogShell";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

// TransferChatDialog hands a chat to another person
// (docs/CHAT-TRANSFER.md): POST /api/conversations/{id}/transfer. The
// transfer is direct — there is nothing for the new owner to accept — so the
// dialog's job is to say, before the click, what goes with the chat and what
// the person giving it away loses. After it succeeds the chat is no longer
// the caller's: `onTransferred` lets the page drop it from its lists, and the
// dialog stays to say what happened to the patrols.

export type TransferChatResult = {
  ownerEmail: string;
  patrolsMoved: number;
  patrolsPaused: number;
};

export function TransferChatDialog({
  conversationId,
  title,
  onClose,
  onTransferred,
}: {
  conversationId: string;
  title: string;
  onClose: () => void;
  onTransferred: (conversationId: string, result: TransferChatResult) => void;
}) {
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<TransferChatResult | null>(null);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const inputId = useId();
  const errorId = useId();
  const target = email.trim();

  const submit = async () => {
    if (busy || !target) return;
    const url = conversationApiUrl(conversationId, "/transfer");
    if (!url) {
      setError(tr("Couldn't transfer the chat — it has an invalid id."));
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const res = await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ to_email: target }),
      });
      if (!res.ok) {
        setError(await responseErrorText(res, tr("Couldn't transfer the chat")));
        return;
      }
      const body = (await res.json().catch(() => ({}))) as {
        owner_email?: string;
        patrols_moved?: number;
        patrols_paused?: number;
      };
      const result: TransferChatResult = {
        ownerEmail: body.owner_email || target,
        patrolsMoved: body.patrols_moved ?? 0,
        patrolsPaused: body.patrols_paused ?? 0,
      };
      setDone(result);
      onTransferred(conversationId, result);
    } catch {
      setError(tr("Couldn't transfer the chat — network error."));
    } finally {
      setBusy(false);
    }
  };

  if (done) {
    return (
      <DialogShell
        label={tr("Chat transferred")}
        scrimLabel={tr("Close transfer ownership")}
        onDismiss={onClose}
        className="max-w-[28rem] p-5"
        testId="transfer-chat-done"
      >
        <div className="grid gap-2">
          <h2 className="text-[1rem] font-semibold text-[var(--color-text-primary)]">
            {tr("Chat transferred")}
          </h2>
          <p className="text-[0.875rem] leading-[1.6] text-[var(--color-text-secondary)]">
            {rich(
              "{0} now belongs to {1}.",
              <strong>&quot;{title}&quot;</strong>,
              <strong>{done.ownerEmail}</strong>,
            )}{" "}
            {done.patrolsMoved > 0 || done.patrolsPaused > 0
              ? tr("Its patrols and timers are paused until the new owner resumes them.")
              : tr("It has no patrols or timers.")}
          </p>
        </div>
        <div className="mt-4 flex justify-end">
          <Button type="button" onClick={onClose}>
            {tr("Done")}
          </Button>
        </div>
      </DialogShell>
    );
  }

  return (
    <DialogShell
      label={tr("Transfer ownership")}
      scrimLabel={tr("Close transfer ownership")}
      onDismiss={() => {
        if (!busy) onClose();
      }}
      className="max-w-[30rem] p-5"
      initialFocusRef={inputRef}
      testId="transfer-chat-dialog"
    >
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        <div className="grid gap-2">
          <h2 className="text-[1rem] font-semibold text-[var(--color-text-primary)]">
            {tr("Transfer ownership")}
          </h2>
          <p className="text-[0.875rem] leading-[1.6] text-[var(--color-text-secondary)]">
            {rich(
              "Hand {0} to someone else. It becomes their chat as soon as you confirm.",
              <strong>&quot;{title}&quot;</strong>,
            )}
          </p>
        </div>

        <div className="grid gap-1.5">
          <Label htmlFor={inputId}>{tr("New owner's email")}</Label>
          <Input
            ref={inputRef}
            id={inputId}
            type="email"
            autoComplete="off"
            spellCheck={false}
            placeholder={tr("name@company.com")}
            value={email}
            disabled={busy}
            aria-invalid={error ? true : undefined}
            aria-describedby={error ? errorId : undefined}
            data-testid="transfer-chat-email"
            onChange={(e) => {
              setEmail(e.target.value);
              setError(null);
            }}
          />
          <p className="text-[0.75rem] leading-[1.5] text-[var(--color-text-muted)]">
            {tr("They need an active account here. They can be on another team.")}
          </p>
        </div>

        <ul className="grid list-disc gap-1.5 pl-5 text-[0.8125rem] leading-[1.55] text-[var(--color-text-secondary)]">
          <li>{tr("They get everything in the chat: the messages, the files and the dashboard.")}</li>
          <li>
            {tr(
              "Its patrols and timers move with it and are paused. A patrol runs with its owner's connectors, so the new owner starts them again.",
            )}
          </li>
          <li>
            {tr(
              "It becomes private to them: team sharing, Multiplayer and any public link are turned off, and approval mode goes back to Manual.",
            )}
          </li>
          <li>{tr("You lose access to it, unless they share it with you.")}</li>
        </ul>

        {error ? (
          <Alert variant="destructive" id={errorId} data-testid="transfer-chat-error">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}

        <div className="flex items-center justify-end gap-2">
          <Button type="button" variant="outline" disabled={busy} onClick={onClose}>
            {tr("Cancel")}
          </Button>
          <Button type="submit" disabled={busy || !target} data-testid="transfer-chat-confirm">
            {busy ? tr("Transferring…") : tr("Transfer ownership")}
          </Button>
        </div>
      </form>
    </DialogShell>
  );
}
