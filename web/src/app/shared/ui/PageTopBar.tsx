"use client";

import { useEffect, useRef, useState, type ReactNode } from "react";
import {
  useClientConfig,
  type ClientSupportContact,
} from "@/app/lib/useClientConfig";
import { DialogShell } from "./DialogShell";
import { Icon } from "./Icon";

// PageTopBar — the shared top header for Chat and the Operations Center (#169).
// One bordered bar so the two views read identically: a mobile sidebar toggle +
// the view title on the left, and optional right-aligned actions. The
// Operations Center renders it with just a title; Chat renders it with the same
// title treatment plus its header controls (search, shortcuts, memories,
// details) in the actions slot. When the client bundle configures
// branding.support, a prominent support contact button is rendered in the
// right-hand bar across every surface (Chat, Operations Center, Settings, and
// Guides) and opens a dialog with the configured message, copyable email, and
// mailto: action.

export type PageTopBarProps = {
  title: string;
  // Opens the off-canvas conversation/nav sidebar on narrow viewports.
  onMenu: () => void;
  // Right-aligned controls (Chat supplies its header buttons here; Ops omits).
  actions?: ReactNode;
  // Optional override for tests; defaults to branding.support from useClientConfig.
  support?: ClientSupportContact;
};

function buildMailtoHref(contact: ClientSupportContact): string {
  const email = contact.email.trim().replaceAll("%", "%25");
  const subject = contact.subject?.trim();
  if (!subject) {
    return `mailto:${email}`;
  }
  return `mailto:${email}?subject=${encodeURIComponent(subject)}`;
}

export function PageTopBar({ title, onMenu, actions, support }: PageTopBarProps) {
  const clientConfig = useClientConfig(false);
  const activeSupport = support ?? clientConfig?.branding?.support;
  const hasSupport = Boolean(
    activeSupport?.label?.trim() && activeSupport?.email?.trim(),
  );
  const [dialogOpen, setDialogOpen] = useState(false);
  const [copied, setCopied] = useState(false);
  const copyTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    return () => {
      if (copyTimerRef.current) {
        clearTimeout(copyTimerRef.current);
      }
    };
  }, []);

  const handleCopyEmail = async () => {
    if (!activeSupport?.email) return;
    if (typeof navigator === "undefined" || typeof navigator.clipboard?.writeText !== "function") {
      return;
    }
    try {
      await navigator.clipboard.writeText(activeSupport.email.trim());
      setCopied(true);
      if (copyTimerRef.current) clearTimeout(copyTimerRef.current);
      copyTimerRef.current = setTimeout(() => setCopied(false), 2000);
    } catch {
      // Clipboard write failed; email remains selectable in the card.
    }
  };

  return (
    <>
      <header className="flex items-center justify-between gap-3 border-b border-[var(--color-border)] px-4 pb-3 pt-[max(0.75rem,env(safe-area-inset-top))] sm:px-6">
        {/* min-h matches the action-button size (size-11 / sm:size-8) so the bar
            is the same height whether or not a view supplies actions — Chat (with
            its header controls) and the Operations Center (none) line up exactly,
            with no height/title jump when switching between them. */}
        <div className="flex min-h-11 min-w-0 items-center gap-3 sm:min-h-8">
          <button
            aria-label="Open sidebar"
            className="inline-flex size-9 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:hidden"
            type="button"
            onClick={onMenu}
          >
            <svg
              aria-hidden="true"
              className="size-4"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.9"
              strokeLinecap="round"
              strokeLinejoin="round"
            >
              <path d="M4 6h16" />
              <path d="M4 12h16" />
              <path d="M4 18h16" />
            </svg>
          </button>
          <h1 className="truncate text-[1.05rem] font-semibold text-[var(--color-text-primary)]">
            {title}
          </h1>
        </div>
        {hasSupport || actions ? (
          <div className="flex min-w-0 items-center gap-1.5">
            {hasSupport && activeSupport ? (
              <button
                type="button"
                data-testid="support-contact-button"
                aria-label={activeSupport.label}
                title={activeSupport.label}
                onClick={() => {
                  setCopied(false);
                  setDialogOpen(true);
                }}
                className="inline-flex h-8 max-w-[9.5rem] shrink-0 items-center gap-1.5 rounded-full border border-[color-mix(in_srgb,var(--color-primary)_42%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-primary)_12%,var(--color-surface-1))] px-2.5 text-xs font-medium text-[var(--color-text-primary)] transition hover:border-[var(--color-primary)] hover:bg-[color-mix(in_srgb,var(--color-primary)_20%,var(--color-surface-1))] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:max-w-[16rem] sm:px-3"
              >
                <Icon
                  name="mail"
                  className="size-3.5 shrink-0 text-[var(--color-primary)]"
                />
                <span className="truncate">{activeSupport.label}</span>
              </button>
            ) : null}
            {actions ? <div className="flex shrink-0 items-center gap-1">{actions}</div> : null}
          </div>
        ) : null}
      </header>
      {dialogOpen && hasSupport && activeSupport ? (
        <DialogShell
          label={activeSupport.title || activeSupport.label}
          scrimLabel="Close support dialog"
          onDismiss={() => setDialogOpen(false)}
          testId="support-contact-dialog"
          className="max-h-[calc(100dvh-2rem)] w-full max-w-md overflow-y-auto p-6"
        >
          <div className="flex items-start justify-between gap-3">
            <div className="flex items-center gap-2.5">
              <span className="inline-flex size-9 shrink-0 items-center justify-center rounded-lg border border-[color-mix(in_srgb,var(--color-primary)_35%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-primary)_14%,var(--color-surface-2))] text-[var(--color-primary)]">
                <Icon name="mail" className="size-4" />
              </span>
              <h2 className="text-base font-semibold text-[var(--color-text-primary)]">
                {activeSupport.title || activeSupport.label}
              </h2>
            </div>
            <button
              type="button"
              aria-label="Close"
              onClick={() => setDialogOpen(false)}
              className="inline-flex size-8 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            >
              <Icon name="close" className="size-4" />
            </button>
          </div>
          {activeSupport.message ? (
            <div className="mt-3 space-y-2 text-sm leading-relaxed text-[var(--color-text-secondary)]">
              {activeSupport.message
                .split(/\n{2,}/)
                .map((para) => para.trim())
                .filter(Boolean)
                .map((para, idx) => (
                  <p key={idx}>{para}</p>
                ))}
            </div>
          ) : null}
          <div className="mt-4 flex items-center justify-between gap-2 rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3.5 py-2.5">
            <span
              data-testid="support-contact-email"
              className="select-all truncate font-mono text-sm font-medium text-[var(--color-text-primary)]"
            >
              {activeSupport.email}
            </span>
            <button
              type="button"
              data-testid="support-contact-copy"
              onClick={() => void handleCopyEmail()}
              className="inline-flex shrink-0 items-center gap-1.5 rounded-md border border-[var(--color-border)] bg-[var(--color-surface-1)] px-2.5 py-1 text-xs font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--rail-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            >
              {copied ? "Copied" : "Copy email"}
            </button>
          </div>
          <div className="mt-5 flex items-center justify-end gap-2">
            <button
              type="button"
              onClick={() => setDialogOpen(false)}
              className="inline-flex h-9 items-center justify-center rounded-lg border border-[var(--color-border)] bg-[var(--color-surface-1)] px-3.5 text-sm font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            >
              Close
            </button>
            <a
              href={buildMailtoHref(activeSupport)}
              data-testid="support-contact-mailto"
              className="inline-flex h-9 items-center justify-center gap-1.5 rounded-lg bg-[var(--color-primary-fill)] px-4 text-sm font-medium text-[var(--color-on-primary-fill)] transition hover:bg-[var(--color-primary-fill-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            >
              <span>Send email</span>
            </a>
          </div>
        </DialogShell>
      ) : null}
    </>
  );
}

export default PageTopBar;
