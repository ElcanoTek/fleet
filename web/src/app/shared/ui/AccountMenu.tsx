"use client";

// AccountMenu — the rail footer's account control (#169, settings pass). A
// single button shows the avatar + email; opening it reveals the account menu
// built on the shared Menu surface, so it is the same component family as the
// conversation-row kebab. Contents per the design: account header (avatar +
// email) · Settings with its subtext line ("Connections, skills & workspace
// settings") · Theme (System/Light/Dark segmented) · Sign out. Connections,
// Skills, and Admin are no longer separate menu items — they are sections of
// the settings area's own sub-nav (Admin only for admins, resolved there).
//
// Surface-specific wiring:
//   - onSignOut is supplied per surface (chat posts a logout form; the
//     orchestrator calls its session.logout()).
//   - Theme is driven by the shared useTheme hook via a System/Light/Dark
//     segmented control (System follows the OS preference live).
//   - `current` marks the settings surface: the anchor button and the
//     Settings item take the primary tint (the design's `.current` state).

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
        "grid shrink-0 place-items-center rounded-full bg-[var(--color-primary)] text-[0.78rem] font-semibold text-[var(--color-on-primary)]",
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
  railCollapsed = false,
  current = false,
}: {
  email: string;
  onSignOut: () => void;
  // Collapsed-rail mode (≥sm): the button shrinks to an avatar-only 2.5rem
  // square and the menu opens at a fixed 15rem instead of stretching to the
  // (now tiny) anchor. The <sm drawer always shows the full button.
  railCollapsed?: boolean;
  // True on the settings surface — tints the anchor + Settings item.
  current?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  const { themePreference, setTheme } = useTheme();
  const close = () => setOpen(false);

  // Collapsing the rail dismisses the menu (the design's setCollapsed
  // behavior) — the strip has no room for an anchored stretch menu.
  // Render-time adjustment (not an effect) per the React "adjusting state
  // when a prop changes" pattern.
  const [prevCollapsed, setPrevCollapsed] = useState(railCollapsed);
  if (railCollapsed !== prevCollapsed) {
    setPrevCollapsed(railCollapsed);
    if (railCollapsed) setOpen(false);
  }

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
          current && !open ? "account-current" : "",
          railCollapsed ? "sm:mx-auto sm:size-10 sm:justify-center sm:gap-0 sm:p-0" : "",
        ].join(" ")}
      >
        <Avatar email={email} />
        <span
          className={[
            "min-w-0 flex-1 truncate text-[0.82rem]",
            railCollapsed ? "sm:hidden" : "",
          ].join(" ")}
        >
          {email || "Loading…"}
        </span>
        <Icon
          name="selector"
          className={[
            "size-4 shrink-0 text-[var(--color-text-muted)]",
            railCollapsed ? "sm:hidden" : "",
          ].join(" ")}
        />
      </button>

      <Menu
        open={open}
        onClose={close}
        anchorRef={anchorRef}
        placement={railCollapsed ? "top-start" : "top-stretch"}
        label="Account"
      >
        <div className="flex items-center gap-2.5 px-2 py-1.5">
          <Avatar email={email} />
          <span className="min-w-0 flex-1 truncate text-[0.82rem] font-medium text-[var(--color-text-primary)]">
            {email}
          </span>
        </div>
        <MenuSeparator />
        <MenuItem
          icon={<Icon name="settings" className="size-4" />}
          description="Connections, skills & workspace settings"
          className={current ? "menu-item-current" : ""}
          onClick={() => {
            close();
            window.location.assign("/settings");
          }}
        >
          Settings
        </MenuItem>
        <MenuSeparator />
        <div className="flex items-center gap-2 px-2 py-1.5 text-[0.82rem] text-[var(--color-text-secondary)]">
          <Icon name="moon" className="size-4 shrink-0 text-[var(--color-text-muted)]" />
          <span className="min-w-0 flex-1">Theme</span>
          <span
            role="group"
            aria-label="Theme"
            className="inline-flex overflow-hidden rounded-[var(--radius-pill)] border border-[var(--color-border)]"
          >
            {(["system", "light", "dark"] as const).map((value) => (
              <button
                key={value}
                type="button"
                aria-pressed={themePreference === value}
                onClick={() => setTheme(value)}
                className={[
                  "px-[0.6rem] py-[0.18rem] text-[0.72rem] font-medium capitalize transition focus-visible:outline-none",
                  themePreference === value
                    ? "bg-[var(--color-primary)] text-[var(--color-on-primary)]"
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
