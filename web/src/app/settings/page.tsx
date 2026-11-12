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
import { Card } from "@/components/ui/card";
import { useTheme, type ThemePreference } from "@/app/shared/hooks/useTheme";
import { readSendOnEnter, writeSendOnEnter } from "@/app/shared/lib/storage";
import { t } from "@/app/lib/i18n";
import { LanguageSelect } from "@/app/shared/ui/LanguageSelect";

// The composer's send-key preference key (chat/ui/Composer.tsx keeps the
// constant private to avoid pulling the composer into this bundle — the
// literal and its values are a shared contract: "enter" | "ctrl+enter",
// absent = "enter").

const THEME_OPTIONS = [
  { value: "system", label: t("System") },
  { value: "light", label: t("Light") },
  { value: "dark", label: t("Dark") },
] as const satisfies readonly { value: ThemePreference; label: string }[];

function SendOnEnterRow() {
  // Same read the composer uses: a storage failure falls back to the
  // "enter" default, and a failed write leaves the choice page-only.
  const [sendOnEnter, setSendOnEnter] = useState<boolean>(readSendOnEnter);

  const toggle = () => {
    const next = !sendOnEnter;
    setSendOnEnter(next);
    writeSendOnEnter(next);
  };

  return (
    <SetRow
      label={t("Send on Enter")}
      desc={t("On: Enter sends and Shift+Enter starts a new line. Off: Enter starts a new line and Ctrl+Enter (⌘+Enter on a Mac) sends.")}
    >
      <SetSwitch on={sendOnEnter} onToggle={toggle} label={t("Send on Enter")} />
    </SetRow>
  );
}

export default function GeneralSettingsPage() {
  const { themePreference, setTheme } = useTheme();

  return (
    <SetSection
      title={t("General")}
      intro={t("Your own preferences. They only change things for you.")}
    >
      <Card className="gap-0 rounded-md px-[1.15rem] py-[0.3rem] shadow-none">
        <SetRow label={t("Language")} desc={t("Choose the language for this app. Changing it reloads the page.")}>
          <LanguageSelect />
        </SetRow>
        <SetRow
          label={t("Theme")}
          desc={t("System follows your OS appearance. Also switchable from the account menu.")}
        >
          <Segmented
            value={themePreference}
            options={THEME_OPTIONS}
            onChange={setTheme}
            label={t("Theme")}
          />
        </SetRow>
        <SendOnEnterRow />
        <BrowserNotificationsRow />
      </Card>
    </SetSection>
  );
}
