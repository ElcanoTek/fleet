"use client";

// AccountMenu — the rail footer's account control (#169). A single button shows
// the avatar + email; opening it reveals the account menu (Settings · Theme ·
// Sign out) built on the shared Menu surface, so it is the same component family
// as the conversation-row kebab.
//
// Surface-specific wiring:
//   - onSettings is optional: omitted on the chat surface (Settings lives only
//     in the Operations Center account menu), present in the orchestrator.
//   - onSignOut is supplied per surface (chat posts a logout form; the
//     orchestrator calls its session.logout()).
//   - Theme is driven by the shared useTheme hook via a Light/Dark segmented
//     control, replacing the standalone header ThemeToggle on both surfaces.

import { useRef, useState } from "react";
import { useTheme } from "@/app/shared/hooks/useTheme";
import { Icon } from "./Icon";
import { Menu, MenuItem, MenuSeparator } from "./Menu";

function Avatar({ email, className }: { email: string; className?: string }) {
  const initial = (email || "?").charAt(0).toUpperCase();
  return (
    <span
      aria-hidden="true"
      className={[
        "grid shrink-0 place-items-center rounded-full bg-[var(--color-primary)] text-[0.78rem] font-semibold text-white",
        className ?? "size-7",
      ].join(" ")}
    >
      {initial}
    </span>
  );
}

export function AccountMenu({
  email,
  onSignOut,
  onSettings,
}: {
  email: string;
  onSignOut: () => void;
  onSettings?: () => void;
}) {
  const [open, setOpen] = useState(false);
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  const { theme, setTheme } = useTheme();
  const close = () => setOpen(false);

  return (
    <div className="relative border-t border-[var(--color-border)] pt-2">
      <button
        ref={anchorRef}
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="Account menu"
        data-testid="account-menu-button"
        onClick={() => setOpen((o) => !o)}
        className={[
          "flex w-full items-center gap-2.5 rounded-[var(--radius-md)] px-2 py-2 text-left transition",
          "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
          open ? "bg-[var(--rail-active)] text-[var(--color-text-primary)]" : "",
        ].join(" ")}
      >
        <Avatar email={email} />
        <span className="min-w-0 flex-1 truncate text-[0.8125rem]">{email || "Loading…"}</span>
        <Icon name="selector" className="size-4 shrink-0 text-[var(--color-text-muted)]" />
      </button>

      <Menu open={open} onClose={close} anchorRef={anchorRef} placement="top-stretch" label="Account">
        <div className="flex items-center gap-2.5 px-2 py-1.5">
          <Avatar email={email} />
          <span className="min-w-0 flex-1 truncate text-[0.8125rem] font-medium text-[var(--color-text-primary)]">
            {email}
          </span>
        </div>
        <MenuSeparator />
        {onSettings ? (
          <MenuItem
            icon={<Icon name="settings" className="size-4" />}
            onClick={() => {
              close();
              onSettings();
            }}
          >
            Settings
          </MenuItem>
        ) : null}
        <div className="flex items-center gap-2 px-2 py-1.5 text-[0.8125rem] text-[var(--color-text-secondary)]">
          <Icon name="moon" className="size-4 shrink-0 text-[var(--color-text-muted)]" />
          <span className="min-w-0 flex-1">Theme</span>
          <span
            role="group"
            aria-label="Theme"
            className="inline-flex overflow-hidden rounded-[var(--radius-pill)] border border-[var(--color-border)]"
          >
            {(["light", "dark"] as const).map((value) => (
              <button
                key={value}
                type="button"
                aria-pressed={theme === value}
                onClick={() => setTheme(value)}
                className={[
                  "px-2.5 py-0.5 text-[0.72rem] font-medium capitalize transition focus-visible:outline-none",
                  theme === value
                    ? "bg-[var(--color-primary)] text-white"
                    : "text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]",
                ].join(" ")}
              >
                {value}
              </button>
            ))}
          </span>
        </div>
        <MenuSeparator />
        <MenuItem
          icon={<Icon name="logout" className="size-4" />}
          onClick={() => {
            close();
            onSignOut();
          }}
        >
          Sign out
        </MenuItem>
      </Menu>
    </div>
  );
}

export default AccountMenu;
