import { t as tr } from "@/app/lib/i18n";
import { useState } from "react";
import { MenuItem, MenuSeparator } from "@/app/shared/ui/Menu";
import { labelChipStyle } from "@/app/shared/lib/labelColors";
import { Icon } from "../Icon";
import { MAX_LABELS, MAX_LABEL_LEN, canAddLabel } from "../conversationOrganization";
import type { Project } from "../ProjectsModal";

// The pieces the row menus open: a label chip, the Move to project panel and
// the labels editor (also used by the bulk bar's Add label).

// ── Label chips ────────────────────────────────────────────────────────────
export function LabelChip({
  name,
  removable,
  onRemove,
}: {
  name: string;
  removable?: boolean;
  onRemove?: () => void;
}) {
  return (
    <span className="conv-label-chip" style={labelChipStyle(name)}>
      {name}
      {removable ? (
        <button
          type="button"
          aria-label={tr("Remove {0}", [name])}
          className="hit-area ml-0.5 inline-flex size-3.5 items-center justify-center rounded-full text-current opacity-70 transition hover:bg-white/20 hover:opacity-100 focus-visible:opacity-100 focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          onClick={(e) => {
            e.stopPropagation();
            onRemove?.();
          }}
        >
          <Icon name="close" className="size-2.5" />
        </button>
      ) : null}
    </span>
  );
}

// CONV_DRAG_MIME is the custom drag payload type for a conversation row —
// namespaced so a project row only accepts our rows, never a stray text/file
// drag from outside the app.

// ── Project picker panel (per-row kebab) ─────────────────────────────────────
// The touch/keyboard counterpart of dragging a chat onto a rail project
// (#509 follow-up): pick a project to re-file into, or unfile. Projects are
// created in the Projects modal (they carry instructions/membership), so
// there is no inline "New project…" here.
export function ProjectPanel({
  projects,
  currentProjectId,
  onPick,
  onRemove,
}: {
  projects: Project[];
  currentProjectId?: string;
  onPick: (projectID: string) => void;
  onRemove: () => void;
}) {
  return (
    <>
      {projects.map((p) => (
        <MenuItem
          key={p.id}
          icon={
            <Icon
              name="check"
              className={[
                "size-4",
                currentProjectId === p.id
                  ? "opacity-100 text-[var(--color-accent)]"
                  : "opacity-0",
              ].join(" ")}
            />
          }
          onClick={() => onPick(p.id)}
        >
          {p.name}
        </MenuItem>
      ))}
      {currentProjectId ? (
        <>
          {projects.length > 0 ? <MenuSeparator /> : null}
          <MenuItem
            icon={<Icon name="close" className="size-4" />}
            onClick={onRemove}
          >
            {tr("Remove from project")}
          </MenuItem>
        </>
      ) : null}
    </>
  );
}

// ── Labels editor panel (shared by per-row kebab + bulk bar) ─────────────────
export function LabelsPanel({
  current,
  suggestions,
  onAdd,
  onRemove,
}: {
  current: string[];
  suggestions: string[];
  onAdd: (label: string) => void;
  onRemove?: (label: string) => void;
}) {
  const [input, setInput] = useState("");
  const atMax = current.length >= MAX_LABELS;
  const add = (raw: string) => {
    if (!canAddLabel(current, raw)) return;
    onAdd(raw.trim().slice(0, MAX_LABEL_LEN));
    setInput("");
  };
  const fresh = suggestions.filter((s) => !current.includes(s));
  return (
    <div className="flex flex-col gap-2 p-1">
      {current.length > 0 ? (
        <div className="flex flex-wrap gap-1.5">
          {current.map((l) => (
            <LabelChip
              key={l}
              name={l}
              removable
              onRemove={() => onRemove?.(l)}
            />
          ))}
        </div>
      ) : null}
      {/* No autoFocus, and no replacement effect: this panel only ever renders
          inside a <Menu> surface, and Menu already moves focus to the first
          focusable item of a menu/flyout it opens. The attribute was fighting
          that — it ran at commit and Menu's effect ran after it, so with any
          existing chips the browser landed on the first chip's Remove button
          anyway. Removing it changes nothing a user sees; the covering test
          asserts the field is still focused when the panel opens. */}
      <input
        className="rounded-[0.4rem] border border-[var(--color-accent)] bg-[var(--color-surface-1)] px-2 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none disabled:cursor-not-allowed disabled:opacity-60"
        placeholder={atMax ? tr("Max {0} labels", [MAX_LABELS]) : tr("Add a label…")}
        maxLength={MAX_LABEL_LEN}
        disabled={atMax}
        value={input}
        onChange={(e) => setInput(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") {
            e.preventDefault();
            add(input);
          }
        }}
      />
      {fresh.length > 0 && !atMax ? (
        <div className="flex flex-col gap-1">
          <span className="text-[0.66rem] uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
            {tr("Suggestions")}
          </span>
          <div className="flex flex-wrap gap-1.5">
            {fresh.map((s) => (
              <button
                key={s}
                type="button"
                className="conv-label-chip"
                style={labelChipStyle(s)}
                onClick={() => add(s)}
              >
                {s}
              </button>
            ))}
          </div>
        </div>
      ) : null}
    </div>
  );
}
