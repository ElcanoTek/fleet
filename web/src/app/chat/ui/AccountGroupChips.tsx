import { bundleText, t as tr } from "@/app/lib/i18n";
import { useMemo } from "react";
import { Icon } from "@/app/shared/ui/Icon";
import {
  accountGroupState,
  groupChangeBlock,
  matchingGroups,
  toggleAccountGroupIn,
  unusableMembersIn,
  type AccountGroup,
  type AccountGroupState,
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

/** Why a chip is flagged or disabled, as one sentence for its tooltip. */
function chipNote(
  group: AccountGroup,
  state: AccountGroupState<Row>,
  block: ReturnType<typeof groupChangeBlock>,
  appName: string,
): string {
  const notes: string[] = [];
  const name = (server: string) => state.rowsByName.get(server)?.display_name || server;
  const missing = unusableMembersIn(state, group);
  if (missing.length > 0) {
    const list = missing
      .map((m) =>
        m.status === "account_missing" || state.rowsByName.has(m.server)
          ? tr("{0} (account {1})", [name(m.server), m.account || appName])
          : name(m.server),
      )
      .join(", ");
    notes.push(tr("Not set up here: {0}.", [list]));
  }
  if (block?.reason === "cap") {
    notes.push(
      tr("{0} already runs as {1} accounts, the most one chat can use.", [
        name(block.server),
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
  const all = accountGroups?.groups;
  // Worked out once per change of the rows or the groups, not once per chip:
  // a bundle may list hundreds of groups (accountGroups.ts).
  const state = useMemo(() => accountGroupState(rows, all ?? []), [rows, all]);
  if (!all || all.length === 0) return null;
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
          const on = state.on.has(group.id);
          const block = groupChangeBlock(state, group);
          const note = chipNote(group, state, block, appName);
          const flagged = (state.usable.get(group.id)?.length ?? 0) < group.accounts.length;
          return (
            <button
              key={group.id}
              type="button"
              aria-pressed={on}
              aria-disabled={block !== null || undefined}
              data-testid={`chat-tools-account-group-${group.id}`}
              data-flagged={flagged || undefined}
              title={note || undefined}
              // The warning icon is decorative; the reason is in the name.
              aria-label={note ? `${group.name}. ${note}` : undefined}
              onClick={(event) => {
                event.stopPropagation();
                // The changed rows are built only now, for the one chip pressed.
                const change = toggleAccountGroupIn(state, group);
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
