// AccountMenu — the rail footer's account control (#169, settings pass). A
// single button shows the avatar + email; opening it reveals the account menu
// built on the shared Menu surface, so it is the same component family as the
// conversation-row kebab. Contents per the design: account header (avatar +
// email) · Settings with its subtext line ("Connections, skills & workspace
// settings") · Guides (the user guides at /help) · Theme (System/Light/Dark segmented) · Sign out. Connections,
// Skills, and Admin are no longer separate menu items — they are sections of
// the settings area's own sub-nav (Admin only for admins, resolved there).
//
// Surface-specific wiring:
//   - onSignOut is supplied per surface (chat posts a logout form; the
//     orchestrator calls its session.logout()). Sign out asks first: the
//     item opens a centred "Sign out?" confirm and only its Sign out button
//     calls onSignOut, since signing out also ends the other apps that share
//     the sign-in. The confirm is portalled to <body>: the rail is
//     transformed and backdrop-filtered, which would otherwise make it the
//     containing block of the dialog's fixed overlay and pin it to the rail.
//     It takes DialogShell's "rail" layer, so it also paints above the rail
//     where the rail floats over the page (the 640–900px overlay) and Cancel
//     returns to the rail as it was.
//   - Theme is driven by the shared useTheme hook via a System/Light/Dark
//     segmented control (System follows the OS preference live).
//   - `current` marks the settings surface: the anchor button and the
//     Settings item take the primary tint (the design's `.current` state).

import { useRef, useState } from "react";
import { createPortal } from "react-dom";
import { ConfirmDialog } from "@/app/chat/ui/ConfirmDialog";
import { useTheme } from "@/app/shared/hooks/useTheme";
import { Icon } from "./Icon";
import { Menu, MenuItem, MenuSeparator } from "./Menu";
import { Segmented } from "./Segmented";

function Avatar({ email, className }: { email: string; className?: string }) {
  const initial = (email || "?").charAt(0).toUpperCase();
  return (
    <span
      aria-hidden="true"
      className={[
        "grid shrink-0 place-items-center rounded-full bg-[var(--color-primary-fill)] text-[0.78rem] font-semibold text-[var(--color-on-primary-fill)]",
        className ?? "size-7",
      ].join(" ")}
    >
      {initial}
    </span>
  );
}

const THEME_OPTIONS = [
  { value: "system", label: "System" },
  { value: "light", label: "Light" },
  { value: "dark", label: "Dark" },
] as const;

export function AccountMenu({
  email,
  appName,
  onSignOut,
  railCollapsed = false,
  current = false,
  analyticsCurrent = false,
  guidesCurrent = false,
}: {
  email: string;
  // The deployment's name (branding.app_name), as the rail's brand row shows
  // it, for the Sign out confirm.
  appName: string;
  onSignOut: () => void;
  // Collapsed-rail mode (≥sm): the button shrinks to an avatar-only 2.5rem
  // square and the menu opens at a fixed 15rem instead of stretching to the
  // (now tiny) anchor. The <sm drawer always shows the full button.
  railCollapsed?: boolean;
  // True on the settings surface — tints the anchor + Settings item.
  current?: boolean;
  // True on the analytics surface — tints the anchor + Analytics item.
  analyticsCurrent?: boolean;
  // True on the guides surface — tints the anchor + Guides item.
  guidesCurrent?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [confirmingSignOut, setConfirmingSignOut] = useState(false);
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  const { themePreference, setTheme } = useTheme();
  const close = () => setOpen(false);
  const shown = email || "Loading…";

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
        data-testid="account-menu-button"
        onClick={() => setOpen((o) => !o)}
        className={[
          "flex w-full items-center gap-2.5 rounded-[var(--radius-md)] px-2 py-2 text-left transition",
          "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
          open ? "bg-[var(--rail-active)] text-[var(--color-text-primary)]" : "",
          (current || analyticsCurrent || guidesCurrent) && !open ? "account-current" : "",
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
          {shown}
        </span>
        {/* The name is the button's own text (WCAG 2.5.3 Label in Name): the
            visible email, then what the button does, for screen readers. */}
        <span className="sr-only">, account menu</span>
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
          current={current}
          onClick={() => {
            close();
            window.location.assign("/settings");
          }}
        >
          Settings
        </MenuItem>
        <MenuItem
          icon={<Icon name="bar-chart" className="size-4" />}
          description="Usage, model mix, and adoption"
          className={analyticsCurrent ? "menu-item-current" : ""}
          current={analyticsCurrent}
          testId="nav-to-analytics"
          onClick={() => {
            close();
            window.location.assign("/analytics");
          }}
        >
          Analytics
        </MenuItem>
        {/* The user guides live here, beside Settings, rather than in the
            rail: the rail is for the ways of working (chat, operations,
            patrols), and Chat itself answers "how do I…" questions — the
            guides are the reference behind it. */}
        <MenuItem
          icon={<Icon name="book" className="size-4" />}
          description="How chat, patrols & background tasks work"
          className={guidesCurrent ? "menu-item-current" : ""}
          current={guidesCurrent}
          testId="nav-to-help"
          onClick={() => {
            close();
            window.location.assign("/help");
          }}
        >
          Guides
        </MenuItem>
        <MenuSeparator />
        <div className="flex items-center gap-2 px-2 py-1.5 text-[0.82rem] text-[var(--color-text-secondary)]">
          <Icon name="moon" className="size-4 shrink-0 text-[var(--color-text-muted)]" />
          <span className="min-w-0 flex-1">Theme</span>
          <Segmented
            label="Theme"
            value={themePreference}
            options={THEME_OPTIONS}
            onChange={setTheme}
          />
        </div>
        <MenuSeparator />
        <MenuItem
          icon={<Icon name="logout" className="size-4" />}
          onClick={() => {
            // Focus the anchor before the item unmounts: the confirm hands
            // focus back to whatever held it when it opened.
            anchorRef.current?.focus();
            close();
            setConfirmingSignOut(true);
          }}
        >
          Sign out
        </MenuItem>
      </Menu>

      {confirmingSignOut && typeof document !== "undefined"
        ? createPortal(
            <ConfirmDialog
              title="Sign out?"
              confirmLabel="Sign out"
              layer="rail"
              testId="sign-out-confirm"
              onCancel={() => setConfirmingSignOut(false)}
              onConfirm={() => {
                setConfirmingSignOut(false);
                onSignOut();
              }}
            >
              <p>This signs you out of {appName} and of every app that shares your sign-in.</p>
            </ConfirmDialog>,
            document.body,
          )
        : null}
    </div>
  );
}

export default AccountMenu;
