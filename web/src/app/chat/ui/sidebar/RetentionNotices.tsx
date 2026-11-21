import { t as tr } from "@/app/lib/i18n";
import { Icon } from "../Icon";
import { nearCap } from "../chatRetention";

// The quiet notices under the Chats heading about temporary chats
// (docs/CHAT-RETENTION.md).

// ── Temporary-chat notices ───────────────────────────────────────────────────
// Under the Chats heading, only when there is something to say: how many of
// the person's chats are about to be deleted (a line that takes them to the
// first one), and that they are close to the most temporary chats one person
// keeps. Silent otherwise — a standing notice is one people stop reading
// (docs/CHAT-RETENTION.md). One quiet line each: the rows themselves carry
// the days left.
export function RetentionNotices({
  expiringCount,
  temporaryCount,
  cap,
  onShowFirst,
}: {
  expiringCount: number;
  temporaryCount: number;
  cap?: number;
  onShowFirst: () => void;
}) {
  const capNear = nearCap(temporaryCount, cap);
  if (expiringCount === 0 && !capNear) return null;
  return (
    <div className="mb-0.5 grid gap-0.5 px-1 text-[0.75rem] leading-snug">
      {expiringCount > 0 ? (
        <button
          type="button"
          data-testid="expiring-chats-notice"
          className="flex w-full items-center gap-1.5 rounded-md px-1.5 py-1 text-left text-[var(--color-warning-text)] transition hover:bg-[var(--rail-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-9"
          title={tr("Show the first of them. Pin, file or archive a chat to keep it.")}
          onClick={onShowFirst}
        >
          <Icon name="hourglass" className="size-3 shrink-0" />
          <span className="min-w-0 truncate">
            {expiringCount === 1
              ? tr("1 chat will be deleted soon.")
              : tr("{0} chats will be deleted soon.", [expiringCount])}
          </span>
        </button>
      ) : null}
      {capNear && cap ? (
        <p
          data-testid="temporary-cap-notice"
          role="status"
          className="m-0 px-1.5 py-1 text-[var(--color-text-secondary)]"
        >
          {tr("You have {0} temporary chats. Past {1}, the least recently active are deleted first.", [temporaryCount, cap])}
        </p>
      ) : null}
    </div>
  );
}
