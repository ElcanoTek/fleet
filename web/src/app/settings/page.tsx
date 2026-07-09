"use client";

// Settings → General (fleet-unified settings pass): personal preference rows —
// Theme (mirrors the account-menu segmented control via the shared useTheme
// hook), Send on Enter (the composer's localStorage preference), and the
// browser-notifications opt-in (moved here from the Connections page; the
// operator-side VAPID setup is env-only and intentionally not rendered).
//
// The credential-account admin that used to squat on this page moved to
// Settings → Connections, where the design places it.

import { useState } from "react";
import { BrowserNotificationsRow } from "./BrowserNotificationsRow";
import { SetRow, SetSwitch, Segmented } from "./ui/atoms";
import { SetSection } from "./ui/panels";
import { useTheme, type ThemePreference } from "@/app/shared/hooks/useTheme";

// The composer's send-key preference key (chat/ui/Composer.tsx keeps the
// constant private to avoid pulling the composer into this bundle — the
// literal and its values are a shared contract: "enter" | "ctrl+enter",
// absent = "enter").
const SEND_KEY_STORAGE = "fleet.sendKey";

const THEME_OPTIONS = [
  { value: "system", label: "System" },
  { value: "light", label: "Light" },
  { value: "dark", label: "Dark" },
] as const satisfies readonly { value: ThemePreference; label: string }[];

function SendOnEnterRow() {
  // Same lazy-initializer read the composer uses: SSR (no window) and
  // private-browsing storage failures both fall back to the "enter" default.
  const [sendOnEnter, setSendOnEnter] = useState<boolean>(() => {
    if (typeof window === "undefined") return true;
    try {
      return (localStorage.getItem(SEND_KEY_STORAGE) ?? "enter") === "enter";
    } catch {
      return true;
    }
  });

  const toggle = () => {
    const next = !sendOnEnter;
    setSendOnEnter(next);
    try {
      localStorage.setItem(SEND_KEY_STORAGE, next ? "enter" : "ctrl+enter");
    } catch {
      // Storage unavailable: the choice applies to this page's state only.
    }
  };

  return (
    <SetRow
      label="Send on Enter"
      desc="Enter sends the message; Shift+Enter adds a line. When off, Enter adds a line and ⌘+Enter sends. The composer toggle mirrors this."
    >
      <SetSwitch on={sendOnEnter} onToggle={toggle} label="Send on Enter" />
    </SetRow>
  );
}

export default function GeneralSettingsPage() {
  const { themePreference, setTheme } = useTheme();

  return (
    <SetSection
      title="General"
      intro="Personal preferences — they affect only you, not your workspace."
    >
      <div className="rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-[1.15rem] py-[0.3rem]">
        <SetRow
          label="Theme"
          desc="System follows your OS appearance. Also switchable from the account menu."
        >
          <Segmented
            value={themePreference}
            options={THEME_OPTIONS}
            onChange={setTheme}
            label="Theme"
          />
        </SetRow>
        <SendOnEnterRow />
        <BrowserNotificationsRow />
      </div>
    </SetSection>
  );
}
