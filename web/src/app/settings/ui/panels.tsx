import { t as tr } from "@/app/lib/i18n";
// Settings-area panels + composite pieces (fleet-unified settings design pass):
// the design's .set-section/.set-head, .conn-group/.conn-panel family,
// .conn-row list rows, .conn-form fields, the sticky .dir-bar search + category
// chips, the .skill-chip copy control, and the .stats-bar.admin-stats cards.
// Metrics are the design's exact values on the app's semantic tokens.

import { createContext, useContext, useId, useRef, useState, type ReactNode } from "react";
import { Icon } from "@/app/shared/ui/Icon";
import { copyText } from "@/app/shared/lib/clipboard";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { EmptyState, EmptyStateDescription } from "@/components/ui/empty-state";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

/* ── Section frame (.set-section / .set-head) ── */

export function SetSection({
  title,
  intro,
  children,
}: {
  title: string;
  intro?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section>
      <header className="mb-[1.1rem]">
        <h2 className="m-0 text-[1.4rem] font-semibold tracking-[-0.01em] text-[var(--color-text-primary)]">
          {title}
        </h2>
        {intro ? (
          <p className="mb-0 mt-[0.4rem] max-w-[46rem] text-[0.875rem] leading-[1.55] text-[var(--color-text-secondary)]">
            {intro}
          </p>
        ) : null}
      </header>
      {children}
    </section>
  );
}

/* ── Groups + panels (.conn-group / .conn-group-head / .panel.conn-panel) ── */

// A panel inside a ConnGroup sits under the group's h3, so its title is an h4;
// a panel directly under the page's h2 takes h3. The context keeps the outline
// correct without every call site passing a level.
const InConnGroup = createContext(false);

export function ConnGroup({ children }: { children: ReactNode }) {
  return (
    <InConnGroup.Provider value={true}>
      <div className="mb-[2.1rem]">{children}</div>
    </InConnGroup.Provider>
  );
}

export function ConnGroupHead({ title, children }: { title: ReactNode; children?: ReactNode }) {
  return (
    <div className="mb-[0.8rem]">
      <h3 className="m-0 text-[1.08rem] font-semibold text-[var(--color-text-primary)]">{title}</h3>
      {children ? (
        <p className="mb-0 mt-[0.35rem] text-[0.78rem] leading-[1.55] text-[var(--color-text-muted)] [&_b]:font-semibold [&_b]:text-[var(--color-text-secondary)]">
          {children}
        </p>
      ) : null}
    </div>
  );
}

export function ConnPanel({
  className,
  children,
}: {
  className?: string;
  children: ReactNode;
}) {
  return (
    <Card
      className={cn(
        "mb-[0.85rem] gap-0 rounded-md px-[1.15rem] pt-4 pb-[1.15rem] shadow-none",
        className,
      )}
    >
      {children}
    </Card>
  );
}

export function ConnPanelHead({ title, children }: { title: ReactNode; children?: ReactNode }) {
  const Heading = useContext(InConnGroup) ? "h4" : "h3";
  return (
    <div className="flex min-h-[1.8rem] flex-wrap items-center justify-between gap-x-4 gap-y-2">
      {/* A panel title names the block a reader is looking for, so it reads
          as a heading in the primary text color, not a faint overline. */}
      <Heading className="m-0 text-[0.95rem] font-semibold text-[var(--color-text-primary)]">
        {title}
      </Heading>
      {children}
    </div>
  );
}

export function ConnPanelSub({ children }: { children: ReactNode }) {
  return (
    <p className="mb-[0.9rem] mt-[0.4rem] text-[0.75rem] leading-[1.55] text-[var(--color-text-muted)]">
      {children}
    </p>
  );
}

// ConnEmpty — the dashed empty-state slab (.conn-empty).
export function ConnEmpty({ children }: { children: ReactNode }) {
  return (
    <EmptyState className="mt-[0.2rem] rounded-md border-border-strong px-4 py-[1.05rem]">
      <EmptyStateDescription className="max-w-none text-[0.79rem] text-muted-foreground">
        {children}
      </EmptyStateDescription>
    </EmptyState>
  );
}

/* ── List rows (.conn-rows / .conn-row) ── */

export function ConnRows({ children }: { children: ReactNode }) {
  return <div className="grid">{children}</div>;
}

export function ConnRow({
  name,
  sub,
  detail,
  actions,
  children,
}: {
  name: ReactNode;
  sub?: ReactNode;
  // Extra full-width content under the row (share panel, expanded body).
  detail?: ReactNode;
  actions?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <div className="border-b border-[var(--color-border-subtle)] px-[0.1rem] py-[0.55rem] last:border-b-0">
      <div className="flex flex-wrap items-center justify-between gap-x-[0.8rem] gap-y-[0.5rem]">
        <div className="grid min-w-[min(14rem,100%)] flex-1 gap-[0.15rem]">
          <span className="text-[0.875rem] font-medium text-[var(--color-text-primary)] [overflow-wrap:anywhere] [&_code]:font-[family-name:var(--font-code)] [&_code]:text-[0.76rem] [&_code]:text-[var(--color-text-secondary)]">
            {name}
          </span>
          {sub ? (
            <span className="text-[0.72rem] text-[var(--color-text-muted)] [overflow-wrap:anywhere]">
              {sub}
            </span>
          ) : null}
          {children}
        </div>
        {actions ? <div className="flex min-w-0 max-w-full flex-wrap items-center gap-[0.35rem]">{actions}</div> : null}
      </div>
      {detail}
    </div>
  );
}

/* ── Forms (.conn-form / .conn-field / .conn-form-actions) ── */

export function ConnForm({ className, children }: { className?: string; children: ReactNode }) {
  return (
    <div
      className={[
        "mb-[0.9rem] mt-[0.2rem] flex flex-wrap items-end gap-[0.55rem] [&>button]:min-h-[2.6rem]",
        className ?? "",
      ].join(" ")}
    >
      {children}
    </div>
  );
}

export function ConnField({
  label,
  grow = false,
  full = false,
  children,
}: {
  label: ReactNode;
  grow?: boolean;
  full?: boolean;
  children: ReactNode;
}) {
  return (
    <label
      className={[
        "grid min-w-[9rem] gap-[0.3rem]",
        grow ? "flex-[2_1_16rem]" : "flex-[1_1_10rem]",
        full ? "col-span-full" : "",
      ].join(" ")}
    >
      <span className="text-[0.72rem] font-medium text-[var(--color-text-secondary)] [&_code]:rounded-[0.3rem] [&_code]:border [&_code]:border-[var(--color-border)] [&_code]:bg-[var(--color-overlay-soft)] [&_code]:px-[0.28rem] [&_code]:py-[0.02rem] [&_code]:font-[family-name:var(--font-code)] [&_code]:text-[0.9em] [&_code]:text-[var(--color-text-secondary)]">
        {label}
      </span>
      {children}
    </label>
  );
}

export function ConnFormActions({ children }: { children: ReactNode }) {
  return <div className="mt-[0.9rem] flex justify-end gap-2">{children}</div>;
}

/* ── Directory search + category chips (.dir-search / .dir-chip) ── */

export function DirSearch({
  value,
  onChange,
  placeholder,
  label,
  className,
}: {
  value: string;
  onChange: (next: string) => void;
  placeholder: string;
  label: string;
  className?: string;
}) {
  const inputRef = useRef<HTMLInputElement | null>(null);
  // No visible label element (the search icon + placeholder carry the affordance
  // and the aria-label carries the name), so the id exists on its own —
  // useId keeps it unique if a page ever renders more than one DirSearch.
  const inputId = useId();
  return (
    <div className={cn("relative", className)}>
      <Icon
        name="search"
        className="pointer-events-none absolute left-3 top-1/2 size-[0.95rem] -translate-y-1/2 text-[var(--color-text-muted)]"
      />
      <Input
        ref={inputRef}
        id={inputId}
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        aria-label={label}
        className="min-h-[2.45rem] py-[0.4rem] pr-[2.4rem] pl-[2.3rem] text-[0.83rem]"
      />
      {value ? (
        <Button
          type="button"
          variant="ghost"
          size="icon-xs"
          aria-label={tr("Clear search")}
          onClick={() => {
            onChange("");
            inputRef.current?.focus();
          }}
          className="absolute top-1/2 right-[0.45rem] -translate-y-1/2 border-transparent"
        >
          <Icon name="close" className="size-[0.85rem]" />
        </Button>
      ) : null}
    </div>
  );
}

export function DirChip({
  active,
  onClick,
  count,
  role,
  ariaSelected,
  ariaPressed,
  leading,
  children,
}: {
  active: boolean;
  onClick: () => void;
  count?: number;
  role?: string;
  ariaSelected?: boolean;
  ariaPressed?: boolean;
  leading?: ReactNode;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      role={role}
      aria-selected={ariaSelected}
      aria-pressed={ariaPressed}
      onClick={onClick}
      className={[
        "inline-flex shrink-0 items-baseline rounded-[var(--radius-pill)] border px-3 py-[0.26rem] text-[0.75rem] transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
        active
          ? "border-[color-mix(in_srgb,var(--color-accent)_45%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-accent)_15%,transparent)] text-[var(--color-text-primary)]"
          : "border-[var(--color-border)] text-[var(--color-text-secondary)] hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]",
      ].join(" ")}
    >
      {leading}
      {children}
      {typeof count === "number" ? (
        <span className="ml-[0.32rem] font-[family-name:var(--font-code)] text-[0.66rem] text-[var(--color-text-muted)]">
          {count}
        </span>
      ) : null}
    </button>
  );
}

// DirCatHead — the uppercase category heading between directory groups.
export function DirCatHead({ children }: { children: ReactNode }) {
  return (
    <div className="mb-[0.55rem] mt-[1.15rem] text-[0.62rem] font-bold uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
      {children}
    </div>
  );
}

/* ── Skill copy chip (.skill-chip) ── */

// CopyChip — click-to-copy "/name" control with the copied check state and a
// screen-reader announcement (the brief's "announce copied").
export function CopyChip({ name }: { name: string }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const copy = async () => {
    // Clipboard denial (permissions, non-secure context, headless) shows no
    // "Copied": the check and the announcement confirm only a real copy.
    if (!(await copyText(`/${name}`))) return;
    setCopied(true);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), 1400);
  };

  return (
    <>
      <Button
        type="button"
        variant="outline"
        size="xs"
        aria-label={tr("Copy /{0} to clipboard", [name])}
        onClick={() => void copy()}
        className="h-auto gap-[0.4rem] rounded-[0.45rem] border-border bg-(--color-overlay-soft) px-[0.6rem] py-[0.26rem] font-[family-name:var(--font-code)] text-[0.78rem] font-medium whitespace-normal text-foreground [overflow-wrap:anywhere]"
      >
        <span className="text-[var(--color-accent)]">/</span>
        {name}
        <Icon
          name={copied ? "check" : "copy"}
          className={cn(
            "size-[0.8rem] shrink-0",
            copied ? "text-[var(--color-success)]" : "text-[var(--color-text-muted)]",
          )}
        />
      </Button>
      <span role="status" aria-live="polite" className="sr-only">
        {copied ? tr("Copied /{0} to clipboard", [name]) : ""}
      </span>
    </>
  );
}

/* ── Admin stat cards (.stats-bar.admin-stats / .stat-card) ── */

// AdminStats — the compact operator stat cards.
export type AdminStat = { title: string; value: string; sub?: string };

export function AdminStats({ items, testId }: { items: AdminStat[]; testId?: string }) {
  return (
    <div
      className="grid grid-cols-4 gap-[0.7rem] max-[860px]:grid-cols-2 max-[520px]:grid-cols-1"
      data-testid={testId}
    >
      {items.map((s) => (
        <Card
          key={s.title}
          className="min-w-0 gap-0 rounded-md px-[0.95rem] pt-[0.85rem] pb-[0.95rem] shadow-none"
        >
          <div className="mb-[0.55rem] flex items-center gap-2">
            <span className="text-[0.65rem] font-medium uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
              {s.title}
            </span>
          </div>
          <div className="text-[1.15rem] font-semibold leading-[1.25] tracking-[-0.01em] text-[var(--color-text-primary)] [font-variant-numeric:tabular-nums] [overflow-wrap:anywhere]">
            {s.value}
          </div>
          {s.sub ? (
            <div className="mt-[0.4rem] break-words text-[0.7rem] text-[var(--color-text-muted)] [overflow-wrap:anywhere]">
              {s.sub}
            </div>
          ) : null}
        </Card>
      ))}
    </div>
  );
}
