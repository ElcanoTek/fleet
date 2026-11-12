import { changeLanguage, getLanguage, LANGUAGES, t, type Language } from "@/app/lib/i18n";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";

/** Native select backed by Flag's @flag/native-select primitive. */
export function LanguageSelect() {
  return (
    <NativeSelect
      size="sm"
      aria-label={t("Language")}
      value={getLanguage()}
      onChange={(event) => changeLanguage(event.target.value as Language)}
      className="w-auto bg-[var(--color-surface-1)]"
    >
      {LANGUAGES.map(({ value, label }) => (
        <NativeSelectOption key={value} value={value} lang={value}>
          {label}
        </NativeSelectOption>
      ))}
    </NativeSelect>
  );
}
