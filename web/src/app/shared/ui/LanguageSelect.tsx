import { changeLanguage, getLanguage, LANGUAGES, t, type Language } from "@/app/lib/i18n";

/** Native select, matching the settings' existing form controls. */
export function LanguageSelect() {
  return (
    <select
      aria-label={t("Language")}
      value={getLanguage()}
      onChange={(event) => changeLanguage(event.target.value as Language)}
      className="rounded-[var(--radius-sm)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-3 py-2 text-sm text-[var(--color-text-primary)]"
    >
      {LANGUAGES.map(({ value, label }) => <option key={value} value={value} lang={value}>{label}</option>)}
    </select>
  );
}
