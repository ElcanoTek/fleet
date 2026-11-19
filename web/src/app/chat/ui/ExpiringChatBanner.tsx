import { t as tr } from "@/app/lib/i18n";
// The notice inside a temporary chat that is about to be deleted
// (docs/CHAT-RETENTION.md): how long it has left, and the two ways to keep it
// from where the person already is. Sending a message restarts the chat's
// inactivity window; Pin keeps it for good. Shown only inside the warning
// window, so a chat with months left carries nothing.

import { Icon } from "./Icon";

export function ExpiringChatBanner({
  daysLeft,
  ttlDays,
  onPin,
}: {
  // Whole days until the sweep deletes the chat (0 or 1 = within a day).
  daysLeft: number;
  // The deployment's inactivity window, for "another N days".
  ttlDays: number;
  onPin: () => void;
}) {
  return (
    <div
      role="status"
      data-testid="expiring-chat-banner"
      className="flex flex-wrap items-center gap-x-3 gap-y-1.5 rounded-[0.625rem] border border-[var(--color-warning-border)] px-3 py-2 text-[0.78rem] text-[var(--color-text-secondary)]"
    >
      <Icon name="hourglass" className="size-3.5 shrink-0 text-[var(--color-warning-text)]" />
      <span className="min-w-0 flex-1 basis-56">
        {daysLeft <= 1
          ? tr("This temporary chat will be deleted within a day.")
          : tr("This temporary chat will be deleted in {0} days.", [daysLeft])}{" "}
        {tr("Sending a message keeps it for another {0} days; pinning keeps it for good.", [ttlDays])}
      </span>
      <button
        type="button"
        className="inline-flex items-center gap-1.5 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-2.5 py-1 font-medium whitespace-nowrap text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11"
        onClick={onPin}
      >
        <Icon name="pin" className="size-3.5 shrink-0" />
        {tr("Pin to keep")}
      </button>
    </div>
  );
}
