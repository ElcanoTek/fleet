"use client";

// Settings-area panels + composite pieces (fleet-unified settings design pass):
// the design's .set-section/.set-head, .conn-group/.conn-panel family,
// .conn-row list rows, .conn-form fields, the sticky .dir-bar search + category
// chips, the .skill-chip copy control, and the .stats-bar.admin-stats cards.
// Metrics are the design's exact values on the app's semantic tokens.

import { useRef, useState, type ReactNode } from "react";
import { Icon } from "@/app/shared/ui/Icon";

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
        <h2 className="m-0 text-[1.15rem] font-semibold text-[var(--color-text-primary)]">
          {title}
        </h2>
        {intro ? (
          <p className="mb-0 mt-[0.35rem] text-[0.82rem] leading-[1.55] text-[var(--color-text-muted)]">
            {intro}
          </p>
        ) : null}
      </header>
      {children}
    </section>
  );
}

/* ── Groups + panels (.conn-group / .conn-group-head / .panel.conn-panel) ── */

export function ConnGroup({ children }: { children: ReactNode }) {
  return <div className="mb-[2.1rem]">{children}</div>;
}

export function ConnGroupHead({ title, children }: { title: ReactNode; children?: ReactNode }) {
  return (
    <div className="mb-[0.8rem]">
      <h3 className="m-0 text-[0.98rem] font-semibold text-[var(--color-text-primary)]">{title}</h3>
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
    <div
      className={[
        "mb-[0.85rem] rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-[1.15rem] pb-[1.15rem] pt-4",
        className ?? "",
      ].join(" ")}
    >
      {children}
    </div>
  );
}

export function ConnPanelHead({ title, children }: { title: ReactNode; children?: ReactNode }) {
  return (
    <div className="flex min-h-[1.8rem] items-center justify-between gap-4">
      <span className="text-[0.68rem] font-bold uppercase tracking-[0.09em] text-[var(--color-text-secondary)]">
        {title}
      </span>
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
    <p className="mt-[0.2rem] rounded-[var(--radius-md)] border border-dashed border-[var(--color-border-strong)] px-4 py-[1.05rem] text-center text-[0.79rem] text-[var(--color-text-muted)]">
      {children}
    </p>
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
      <div className="flex items-center gap-[0.8rem]">
        <div className="grid min-w-0 flex-1 gap-[0.15rem]">
          <span className="text-[0.83rem] font-medium text-[var(--color-text-primary)] [overflow-wrap:anywhere] [&_code]:font-[family-name:var(--font-code)] [&_code]:text-[0.76rem] [&_code]:text-[var(--color-text-secondary)]">
            {name}
          </span>
          {sub ? (
            <span className="text-[0.72rem] text-[var(--color-text-muted)] [overflow-wrap:anywhere]">
              {sub}
            </span>
          ) : null}
          {children}
        </div>
        {actions ? <div className="flex shrink-0 items-center gap-[0.35rem]">{actions}</div> : null}
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
  return (
    <div className={["relative", className ?? ""].join(" ")}>
      <Icon
        name="search"
        className="pointer-events-none absolute left-3 top-1/2 size-[0.95rem] -translate-y-1/2 text-[var(--color-text-muted)]"
      />
      <input
        ref={inputRef}
        type="text"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        aria-label={label}
        className="min-h-[2.45rem] w-full rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] py-[0.4rem] pl-[2.3rem] pr-[2.4rem] text-[0.83rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)]"
      />
      {value ? (
        <button
          type="button"
          aria-label="Clear search"
          onClick={() => {
            onChange("");
            inputRef.current?.focus();
          }}
          className="absolute right-[0.45rem] top-1/2 inline-flex size-[1.6rem] -translate-y-1/2 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
        >
          <Icon name="close" className="size-[0.85rem]" />
        </button>
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

  const copy = () => {
    // Clipboard denial rejects the PROMISE (permissions, non-secure context,
    // headless) — a try/catch around the call would miss it and surface an
    // unhandled rejection. The visual state still confirms the intent.
    navigator.clipboard?.writeText(`/${name}`).catch(() => {});
    setCopied(true);
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), 1400);
  };

  return (
    <>
      <button
        type="button"
        aria-label={`Copy /${name} to clipboard`}
        onClick={copy}
        className={[
          "inline-flex items-center gap-[0.4rem] rounded-[0.45rem] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-[0.6rem] py-[0.26rem] font-[family-name:var(--font-code)] text-[0.78rem] font-medium text-[var(--color-text-primary)] transition [overflow-wrap:anywhere]",
          "hover:border-[var(--color-accent)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
        ].join(" ")}
      >
        <span className="text-[var(--color-accent)]">/</span>
        {name}
        <Icon
          name={copied ? "check" : "copy"}
          className={[
            "size-[0.8rem] shrink-0",
            copied ? "text-[var(--color-success)]" : "text-[var(--color-text-muted)]",
          ].join(" ")}
        />
      </button>
      <span role="status" aria-live="polite" className="sr-only">
        {copied ? `Copied /${name} to clipboard` : ""}
      </span>
    </>
  );
}

/* ── Admin stat cards (.stats-bar.admin-stats / .stat-card) ── */

// AdminStats — the compact operator stat cards. Markup duplicated from the
// ops StatsGrid's card by decision (the orchestrator surface stays untouched);
// metrics are the design's .admin-stats overrides of .stat-card.
export type AdminStat = { title: string; value: string; sub?: string };

export function AdminStats({ items, testId }: { items: AdminStat[]; testId?: string }) {
  return (
    <div className="grid grid-cols-4 gap-[0.7rem] max-[860px]:grid-cols-2" data-testid={testId}>
      {items.map((s) => (
        <div
          key={s.title}
          className="rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-[0.95rem] pb-[0.95rem] pt-[0.85rem]"
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
            <div className="mt-[0.4rem] text-[0.7rem] text-[var(--color-text-muted)]">{s.sub}</div>
          ) : null}
        </div>
      ))}
    </div>
  );
}

/* ── Secrets fieldset (.secrets-set / .secret-row) ── */

export function SecretsFieldset({
  legend = "Secrets (write-only)",
  children,
}: {
  legend?: string;
  children: ReactNode;
}) {
  return (
    <fieldset className="mt-[0.35rem] rounded-[var(--radius-md)] border border-[var(--color-border)] px-[0.9rem] pb-4 pt-[0.4rem]">
      <legend className="px-[0.4rem] text-[0.78rem] font-medium text-[var(--color-text-secondary)]">
        {legend}
      </legend>
      {children}
    </fieldset>
  );
}
