import { rich } from "@/app/lib/i18n/rich";
import { t as tr } from "@/app/lib/i18n";
import { useRef, useState } from "react";
import { Menu } from "@/app/shared/ui/Menu";
import { Icon } from "../Icon";
import { LabelsPanel } from "./panels";

// ── Select mode: the bar above the account menu ───────────────────────────
// Chat's select mode, entered from the Chats menu's "Select chats…" (on any
// other page that item hands over to Chat, so only Chat draws this bar).
// Count + icon actions with data-tip-top tooltips. Add-label reuses the label
// menu's panel (including its inline create input) in a menu that opens
// above the bar. Actions disable at zero selected.

// A bulk action button (the design's .bulk-btn).
const BULK_BTN_CLASS =
  "relative inline-flex size-[1.9rem] items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-secondary)] transition enabled:hover:bg-[var(--rail-hover)] enabled:hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:cursor-not-allowed disabled:opacity-40";

export function BulkBar({
  selectedIds,
  railCollapsed,
  allLabelNames,
  onBulkPin,
  onBulkDelete,
  onExitSelectMode,
  onBulkAddLabel,
}: {
  selectedIds: ReadonlySet<string>;
  // ≥sm the bar hides with the rest of the wide-only content when the rail
  // is collapsed.
  railCollapsed: boolean;
  allLabelNames: string[];
  onBulkPin: () => void;
  onBulkDelete: () => void;
  onExitSelectMode: () => void;
  onBulkAddLabel: (label: string) => void;
}) {
  const [bulkPanel, setBulkPanel] = useState<"none" | "labels">("none");
  const bulkLabelsRef = useRef<HTMLButtonElement | null>(null);
  const largeSelection = selectedIds.size > 50;
  return (
    <div className={railCollapsed ? "sm:hidden" : ""}>
      <div className="mb-1.5 flex items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] py-[0.4rem] pl-[0.65rem] pr-[0.3rem] motion-safe:animate-pop-up">
        <span className="min-w-0 flex-1 text-[0.8rem] font-medium tabular-nums text-[var(--color-text-primary)]">
          {rich("{0} selected", selectedIds.size)}
        </span>
        <div className="flex items-center gap-[0.15rem]">
          <button
            type="button"
            aria-label={tr("Pin selected")}
            data-tip-top="Pin"
            disabled={selectedIds.size === 0}
            className={BULK_BTN_CLASS}
            onClick={onBulkPin}
          >
            <Icon name="pin" className="size-[0.95rem]" />
          </button>
          <button
            ref={bulkLabelsRef}
            type="button"
            aria-label={tr("Add label")}
            aria-haspopup="menu"
            aria-expanded={bulkPanel === "labels"}
            data-tip-top="Add label"
            disabled={selectedIds.size === 0}
            className={BULK_BTN_CLASS}
            onClick={() => setBulkPanel((p) => (p === "labels" ? "none" : "labels"))}
          >
            <Icon name="tag" className="size-[0.95rem]" />
          </button>
          <button
            type="button"
            aria-label={tr("Delete selected")}
            data-tip-top="Delete"
            disabled={selectedIds.size === 0}
            className={[
              BULK_BTN_CLASS,
              "enabled:hover:bg-[var(--color-status-error-bg)] enabled:hover:text-[var(--color-status-error-fg)]",
            ].join(" ")}
            onClick={onBulkDelete}
          >
            <Icon name="trash" className="size-[0.95rem]" />
          </button>
          <button
            type="button"
            aria-label={tr("Exit selection")}
            data-tip-top="Cancel"
            className={BULK_BTN_CLASS}
            onClick={onExitSelectMode}
          >
            <Icon name="close" className="size-[0.95rem]" />
          </button>
        </div>
        <Menu
          open={bulkPanel === "labels"}
          onClose={() => setBulkPanel("none")}
          anchorRef={bulkLabelsRef}
          placement="top-end"
          label={tr("Add label to selected")}
        >
          <LabelsPanel
            current={[]}
            suggestions={allLabelNames}
            onAdd={(label) => {
              onBulkAddLabel(label);
              setBulkPanel("none");
            }}
          />
        </Menu>
      </div>
      {largeSelection ? (
        <p className="mb-1.5 text-[0.6875rem] text-[var(--color-danger)]">
          {rich("Selecting {0} conversations — large bulk deletes are permanent.", selectedIds.size)}
        </p>
      ) : null}
    </div>
  );
}
