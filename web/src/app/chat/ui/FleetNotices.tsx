import { t as tr } from "@/app/lib/i18n";
import { labelForModel } from "@/app/lib/modelAliases";
// Rows fleet writes itself, rendered as notices — never as the user's words.
//
// AutoContinueNotice is the input of a turn fleet started on its own after an
// approval card was settled (agent_policy.critical_tool_resume,
// docs/RESUME-AFTER-APPROVAL.md). It is stored as the turn's user-role entry
// (the model reads it as the turn's input), so without this it would render
// as a bubble the user never typed. The label says what happened; the exact
// text the model received sits behind Show, verbatim, like the injected
// context note.
//
// FleetNotices renders UI-only notice entries that are not a turn — e.g. an
// operator model upgrade rollout, an automatic continue that was skipped (the
// hourly cap), or one dropped by a restart.

import { useId, useState } from "react";
import { MODEL_UPGRADED_NOTICE_KIND, type NoticeItem } from "./history";
import { Icon } from "./Icon";

export function AutoContinueNotice({ text }: { text: string }) {
  const [expanded, setExpanded] = useState(false);
  const panelId = useId();
  return (
    <div
      data-testid="auto-continue-notice"
      className="w-full rounded-[var(--radius-lg)] border border-dashed border-[var(--color-border)] bg-[color-mix(in_srgb,var(--color-overlay-soft)_68%,transparent)] px-3 py-2 text-left text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)] sm:text-[0.82rem]"
    >
      <button
        type="button"
        className="flex w-full items-center justify-between gap-3 text-left"
        onClick={() => setExpanded((v) => !v)}
        aria-expanded={expanded}
        aria-controls={panelId}
      >
        <span className="flex min-w-0 items-center gap-1.5">
          <Icon name="info" className="size-3 shrink-0 text-[var(--color-text-muted)]" />
          <span className="min-w-0 truncate text-[0.68rem] font-medium uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
            {tr("Continued automatically after an approval — not typed by you")}
          </span>
        </span>
        <span className="shrink-0 text-[0.68rem] text-[var(--color-text-muted)]">
          {expanded ? tr("Hide") : tr("Show")}
        </span>
      </button>
      <div id={panelId} hidden={!expanded}>
        <pre className="mt-2 max-h-72 overflow-auto whitespace-pre-wrap break-words border-t border-[var(--color-border)] pt-2 font-mono text-[0.7rem] leading-[1.5] text-[var(--color-text-secondary)]">
          {text.trim()}
        </pre>
      </div>
    </div>
  );
}

export function PatrolTurnNotice({ text }: { text: string }) {
  const [expanded, setExpanded] = useState(false);
  const panelId = useId();
  // Extract a clean title from `[Scheduled Patrol — Run #N: Title]` if present
  const headerMatch = text.match(/^\[Scheduled (?:Patrol|Timer)[^\]]*\]/i);
  const badgeText = headerMatch
    ? headerMatch[0].slice(1, -1)
    : tr("Scheduled patrol run — triggered automatically in this chat");
  return (
    <div
      data-testid="patrol-turn-notice"
      className="w-full rounded-[var(--radius-lg)] border border-[color-mix(in_srgb,var(--color-accent)_32%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-accent)_6%,var(--color-overlay-soft))] px-3 py-2 text-left text-[0.78rem] leading-[1.55] text-[var(--color-text-secondary)] sm:text-[0.82rem]"
    >
      <button
        type="button"
        className="flex w-full items-center justify-between gap-3 text-left"
        onClick={() => setExpanded((v) => !v)}
        aria-expanded={expanded}
        aria-controls={panelId}
      >
        <span className="flex min-w-0 items-center gap-2">
          <span className="inline-block size-2 shrink-0 rounded-full bg-[var(--color-accent)]" />
          <Icon name="clock" className="size-3.5 shrink-0 text-[var(--color-accent)]" />
          <span className="min-w-0 truncate text-[0.7rem] font-semibold uppercase tracking-[0.07em] text-[var(--color-text-primary)]">
            {badgeText}
          </span>
        </span>
        <span className="shrink-0 text-[0.68rem] text-[var(--color-text-muted)]">
          {expanded ? tr("Hide prompt") : tr("Show prompt")}
        </span>
      </button>
      <div id={panelId} hidden={!expanded}>
        <pre className="mt-2 max-h-72 overflow-auto whitespace-pre-wrap break-words border-t border-[var(--color-border)] pt-2 font-mono text-[0.7rem] leading-[1.5] text-[var(--color-text-secondary)]">
          {text.trim()}
        </pre>
      </div>
    </div>
  );
}

export function FleetNotices({
  notices,
  items,
}: {
  notices: string[] | undefined;
  items?: NoticeItem[];
}) {
  const resolvedItems: NoticeItem[] =
    items && items.length > 0
      ? items
      : (notices ?? []).map((text) => ({ text }));
  if (resolvedItems.length === 0) return null;
  return (
    <div className="grid gap-1.5" data-testid="fleet-notices">
      {resolvedItems.map((item, i) => {
        if (item.kind === MODEL_UPGRADED_NOTICE_KIND) {
          const toLabel = item.toModel ? labelForModel(item.toModel) : "";
          const fromLabel =
            item.fromModel && item.fromModel !== item.toModel
              ? labelForModel(item.fromModel)
              : "";
          return (
            <div
              key={i}
              data-testid="model-upgrade-notice"
              className="flex flex-wrap items-center gap-x-2 gap-y-0.5 rounded-[var(--radius-md)] border border-[color-mix(in_srgb,var(--color-accent)_26%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-accent)_6%,var(--color-overlay-soft))] px-2.5 py-1.5 text-[0.76rem] leading-[1.45] text-[var(--color-text-secondary)]"
            >
              <span className="flex min-w-0 items-center gap-1.5 font-medium text-[var(--color-text-primary)]">
                <Icon name="sparkles" className="size-3.5 shrink-0 text-[var(--color-accent)]" />
                <span className="min-w-0">
                  {toLabel
                    ? tr("Model upgraded to {0}", [toLabel])
                    : item.text}
                </span>
              </span>
              {toLabel && fromLabel ? (
                <span className="text-[0.72rem] text-[var(--color-text-muted)]">
                  {tr("· Previously {0} · New replies use {1}", [fromLabel, toLabel])}
                </span>
              ) : toLabel ? (
                <span className="text-[0.72rem] text-[var(--color-text-muted)]">
                  {tr("· New replies use {0}", [toLabel])}
                </span>
              ) : null}
            </div>
          );
        }
        return (
          <p
            key={i}
            className="flex items-start gap-1.5 text-[0.78rem] leading-[1.5] text-[var(--color-text-muted)]"
          >
            <Icon name="info" className="mt-0.5 size-3 shrink-0" />
            <span className="min-w-0">{item.text}</span>
          </p>
        );
      })}
    </div>
  );
}
