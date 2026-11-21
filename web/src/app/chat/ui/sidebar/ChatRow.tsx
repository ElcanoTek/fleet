import { t as tr } from "@/app/lib/i18n";
import { useEffect, useRef } from "react";
import type { ReactNode } from "react";
import { railIconClass } from "@/app/shared/ui/railStyles";
import { Icon } from "../Icon";
import type { ConversationSummary } from "../chat-experience";
import { RowStatus, describeRowStatus, type RowStatusInput } from "@/app/shared/sidebar/RowStatus";
import { RENAME_INPUT_CLASS, ROW_CLEAR_FOR_ACTIONS, STATUS_YIELDS_TO_ACTIONS } from "./classes";

// One chat's row in the sidebar (ConversationSidebar): its title, at most one
// status, its ⋯ menu slot, and its inline rename field.

// ── A chat's row ─────────────────────────────────────────────────────────────
// The title, and one status at the right edge when it matters (RowStatus).
// The same row serves the person's own chats and teammates' chats, in Pinned
// (where it carries its kind's icon), in a project (indented under it), in
// Chats and in search results. What a status means reaches a screen reader as
// the row's description; it never changes the row's name, which is the title.
export function ChatRow({
  conversation,
  own,
  icon,
  active,
  focused,
  editing = false,
  selecting = false,
  checked = false,
  status,
  showStatus = true,
  now,
  onOpen,
  onToggleSelect,
  onCommitRename,
  onCancelRename,
  kebab,
  onDragStart,
  titleNode,
  context,
  testId,
}: {
  conversation: ConversationSummary;
  // The person's own chat: it can be renamed, selected and walked with j/k.
  own: boolean;
  // Pinned rows carry their kind's icon; rows in Chats and in a project do not.
  icon?: string;
  active: boolean;
  // focused = the keyboard j/k cursor is on this row (distinct from active =
  // the open conversation). Renders a ring so the cursor is visible.
  focused?: boolean;
  editing?: boolean;
  selecting?: boolean;
  checked?: boolean;
  // What the row has to say for itself. `showStatus` is false when the
  // person turned row statuses off (docs/SIDEBAR.md): the mark is not drawn,
  // and its sentence still reaches a screen reader.
  status: RowStatusInput;
  showStatus?: boolean;
  now: number;
  onOpen: () => void;
  onToggleSelect?: () => void;
  onCommitRename?: (title: string) => void;
  onCancelRename?: () => void;
  kebab: ReactNode;
  // When set, the row is draggable (HTML5 DnD) — the drag-a-chat-into-a-
  // project affordance. Mouse only by nature; the menu's "Move to project"
  // is the touch/keyboard path to the same action.
  onDragStart?: (e: React.DragEvent<HTMLDivElement>) => void;
  // Search results only: the title with the matched words marked, and the
  // place the chat lives (its project's name). A result is shown out of its
  // section, so the row says where it belongs.
  titleNode?: ReactNode;
  context?: string;
  testId?: string;
}) {
  // Rename starts from the menu, so the field has to take focus itself or the
  // rename is unusable. Gated on `editing` (the field only renders then).
  // onFocus still selects the text.
  const renameInputRef = useRef<HTMLInputElement | null>(null);
  useEffect(() => {
    if (editing) renameInputRef.current?.focus();
  }, [editing]);

  const description = describeRowStatus(status, now);
  const descId = `rail-row-desc-${conversation.id}`;
  const labels = conversation.labels ?? [];
  const tooltip = [
    conversation.title,
    !own && conversation.user_email ? `(${conversation.user_email})` : "",
    labels.length > 0 ? `· ${labels.join(", ")}` : "",
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <div
      data-conversation-id={conversation.id}
      data-own-row={own ? "" : undefined}
      data-focused={focused ? "true" : undefined}
      draggable={Boolean(onDragStart)}
      onDragStart={onDragStart}
      className={[
        "group relative rounded-[var(--radius-md)] transition-colors",
        selecting && checked
          ? "bg-[color-mix(in_srgb,var(--color-primary)_14%,transparent)]"
          : active
            ? "bg-[var(--rail-active)]"
            : "hover:bg-[var(--rail-hover)]",
        focused ? "ring-1 ring-inset ring-[var(--color-accent)]" : "",
      ].join(" ")}
    >
      {description ? (
        <span id={descId} hidden>
          {description}
        </span>
      ) : null}
      {editing ? (
        <input
          // Uncontrolled: remounts each time editing starts (it only renders
          // while editing), so defaultValue tracks the live title without a
          // sync effect.
          ref={renameInputRef}
          aria-label={tr("Rename {0}", [conversation.title])}
          className={RENAME_INPUT_CLASS}
          defaultValue={conversation.title}
          onFocus={(e) => e.currentTarget.select()}
          onBlur={(e) => onCommitRename?.(e.currentTarget.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              onCommitRename?.(e.currentTarget.value);
            } else if (e.key === "Escape") {
              e.preventDefault();
              onCancelRename?.();
            }
          }}
        />
      ) : (
        <button
          type="button"
          aria-pressed={selecting ? checked : undefined}
          aria-describedby={description ? descId : undefined}
          data-rail-row=""
          data-testid={testId}
          className={[
            "flex min-h-[var(--rail-row-h)] w-full min-w-0 items-center gap-2.5 rounded-[var(--radius-md)] pl-2.5 pr-2.5 text-left text-[length:var(--rail-row-text)] leading-tight transition-colors focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11",
            selecting ? "" : ROW_CLEAR_FOR_ACTIONS,
            active || (selecting && checked)
              ? "font-medium text-[var(--color-text-primary)]"
              : "text-[var(--color-text-secondary)] group-hover:text-[var(--color-text-primary)]",
          ].join(" ")}
          onClick={selecting && onToggleSelect ? onToggleSelect : onOpen}
          title={tooltip}
        >
          {/* Leading checkbox — select mode only: a 1rem square that fills
              primary with a check when on. */}
          {selecting ? (
            <span
              aria-hidden="true"
              className={[
                "inline-flex size-4 shrink-0 items-center justify-center rounded-[0.3rem] border-[1.5px] transition",
                checked
                  ? "border-[var(--color-primary-fill)] bg-[var(--color-primary-fill)]"
                  : "border-[var(--color-border-strong)]",
              ].join(" ")}
            >
              {checked ? <Icon name="check" className="size-[0.7rem] text-[var(--color-on-primary-fill)]" /> : null}
            </span>
          ) : icon ? (
            <Icon name={icon} className={railIconClass(active)} />
          ) : null}
          <span className="block min-w-0 flex-1 truncate">
            {context ? <span className="text-[var(--color-text-muted)]">{context} › </span> : null}
            {titleNode ?? conversation.title}
          </span>
          {showStatus ? (
            <span className={["ml-auto inline-flex shrink-0", selecting ? "" : STATUS_YIELDS_TO_ACTIONS].join(" ")}>
              <RowStatus status={status} now={now} />
            </span>
          ) : null}
        </button>
      )}

      {!editing && !selecting ? (
        <div className="absolute inset-y-0 right-1 flex items-center">{kebab}</div>
      ) : null}
    </div>
  );
}
