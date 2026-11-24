import { bundleText, t as tr } from "@/app/lib/i18n";
import { Icon } from "@/app/shared/ui/Icon";
import {
  isGroupOn,
  matchingGroups,
  toggleAccountGroup,
  unusableMembers,
  type AccountGroup,
  type AccountGroups,
  type GroupRow,
} from "./accountGroups";
import { MAX_ACCOUNTS_PER_CONNECTOR } from "./mcpAccounts";

// The chip row over the Tools picker's list: one chip per account group the
// bundle supplies ("Clients": The Weather Company, Raptive, …). A chip is a
// toggle button; pressing it sets every connector in the group to the group's
// accounts (accountGroups.ts has the rules). Shared by the popover and the
// library.

type Row = GroupRow & { display_name?: string };

const CHIP =
  "hit-area-touch inline-flex h-[1.5rem] max-w-[14rem] shrink-0 items-center gap-1 rounded-[var(--radius-pill)] border px-[0.6rem] text-[0.75rem] transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:h-9 aria-disabled:cursor-not-allowed aria-disabled:opacity-50";
const CHIP_OFF =
  "border-[var(--color-pill-border)] text-[var(--color-text-secondary)] hover:bg-[var(--color-pill-hover-bg)] hover:text-[var(--color-text-primary)]";
const CHIP_ON =
  "border-[var(--color-accent)] bg-[var(--color-status-running-bg)] text-[var(--color-text-primary)]";

function connectorName(rows: readonly Row[], server: string): string {
  const row = rows.find((r) => r.name === server);
  return row?.display_name || server;
}

/** Why a chip is flagged or disabled, as one sentence for its tooltip. */
function chipNote(group: AccountGroup, groups: readonly AccountGroup[], rows: readonly Row[], appName: string): string {
  const notes: string[] = [];
  const missing = unusableMembers(group, rows);
  if (missing.length > 0) {
    const list = missing
      .map((m) =>
        m.status === "account_missing" || rows.some((r) => r.name === m.server)
          ? tr("{0} (account {1})", [connectorName(rows, m.server), m.account || appName])
          : connectorName(rows, m.server),
      )
      .join(", ");
    notes.push(tr("Not set up here: {0}.", [list]));
  }
  const change = toggleAccountGroup(rows, groups, group);
  if (!change.ok && change.reason === "cap") {
    notes.push(
      tr("{0} already runs as {1} accounts, the most one chat can use.", [
        connectorName(rows, change.server),
        MAX_ACCOUNTS_PER_CONNECTOR,
      ]),
    );
  }
  return notes.join(" ");
}

export function AccountGroupChips({
  accountGroups,
  rows,
  query,
  appName,
  onApply,
  className = "",
}: {
  accountGroups: AccountGroups | null | undefined;
  rows: readonly Row[];
  /** The picker's search text: chips are filtered by name with the rows. */
  query: string;
  /** The deployment's name, what the default account is called. */
  appName: string;
  /** The rows after a chip was pressed; the caller saves them. */
  onApply: (rows: Row[]) => void;
  className?: string;
}) {
  const all = accountGroups?.groups ?? [];
  if (all.length === 0) return null;
  const shown = matchingGroups(all, query);
  if (shown.length === 0) return null;
  // The heading is the bundle's own word for its groups, localized like any
  // other bundle text; a group's name is a proper name and is shown as given.
  const label = bundleText(accountGroups?.label?.trim() ?? "") || tr("Account groups");
  return (
    <div
      role="group"
      aria-label={label}
      data-testid="chat-tools-account-groups"
      className={`flex flex-col gap-1 ${className}`}
    >
      <div className="font-[family-name:var(--font-code)] text-[0.6875rem] uppercase tracking-[0.12em] text-[var(--color-text-subtle)]">
        {label}
      </div>
      <div className="flex max-h-[4.25rem] flex-wrap gap-1.5 overflow-y-auto [scrollbar-width:thin] any-pointer-coarse:max-h-[5.25rem]">
        {shown.map((group) => {
          const on = isGroupOn(group, rows);
          const change = toggleAccountGroup(rows, all, group);
          const note = chipNote(group, all, rows, appName);
          const flagged = unusableMembers(group, rows).length > 0;
          return (
            <button
              key={group.id}
              type="button"
              aria-pressed={on}
              aria-disabled={!change.ok || undefined}
              data-testid={`chat-tools-account-group-${group.id}`}
              data-flagged={flagged || undefined}
              title={note || undefined}
              // The warning icon is decorative; the reason is in the name.
              aria-label={note ? `${group.name}. ${note}` : undefined}
              onClick={(event) => {
                event.stopPropagation();
                if (change.ok) onApply(change.rows);
              }}
              className={`${CHIP} ${on ? CHIP_ON : CHIP_OFF}`}
            >
              {on ? <Icon name="check" className="size-3 shrink-0 text-[var(--color-accent)]" /> : null}
              <span className="min-w-0 truncate">{group.name}</span>
              {flagged ? (
                <Icon name="alert-circle" className="size-3 shrink-0 text-[var(--color-state-warn)]" />
              ) : null}
            </button>
          );
        })}
      </div>
    </div>
  );
}
