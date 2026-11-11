import es from "./es.json";
import pt from "./pt.json";
import { readPref, writePref } from "@/app/shared/lib/storage";

export type Language = "en" | "es" | "pt";
export const LANGUAGE_KEY = "elcano.language";
export const LANGUAGE_COOKIE = "elcano_language";
export const LANGUAGES: readonly { value: Language; label: string }[] = [
  { value: "en", label: "English" },
  { value: "es", label: "Español" },
  { value: "pt", label: "Português" },
];

export function supportedLanguage(value: string | null | undefined): Language | null {
  const base = value?.trim().toLowerCase().split(/[-_]/)[0];
  return base === "en" || base === "es" || base === "pt" ? base : null;
}

export function detectLanguage(): Language {
  if (typeof window === "undefined") return "en";
  const requested = supportedLanguage(new URLSearchParams(window.location.search).get("lang"));
  const saved = supportedLanguage(readPref(LANGUAGE_KEY));
  const cookie = typeof document === "undefined" ? null : document.cookie.split(";")
    .map((part) => part.trim()).find((part) => part.startsWith(`${LANGUAGE_COOKIE}=`))?.split("=")[1];
  return requested ?? saved ?? supportedLanguage(cookie) ?? supportedLanguage(navigator.language) ?? "en";
}

// Language changes reload this document: module-level menu definitions and
// cached API labels then use the same language as components. No translated
// value is written back to a record or used as a protocol identifier.
const language = detectLanguage();
export function getLanguage(): Language { return language; }

export function rememberLanguage(next: Language): void {
  writePref(LANGUAGE_KEY, next);
  // This is a host-only display preference, not a session credential. The
  // cookie also carries it across a full-page SSO round trip when localStorage
  // is denied, and lets the Go gateway pass the preference to central Auth.
  try {
    document.cookie = `${LANGUAGE_COOKIE}=${next}; Path=/; Max-Age=31536000; SameSite=Lax${window.location.protocol === "https:" ? "; Secure" : ""}`;
  } catch { /* A denied preference store must not prevent sign-in. */ }
}

export function languageURL(next: Language, href: string): string {
  const url = new URL(href);
  url.searchParams.set("lang", next);
  return url.href;
}

export function changeLanguage(next: Language): void {
  if (!supportedLanguage(next)) return;
  rememberLanguage(next);
  // The URL also preserves the choice when the browser denies storage.
  window.location.assign(languageURL(next, window.location.href));
}

const catalogs: Record<Language, Readonly<Record<string, string>>> = { en: {}, es, pt };

// Bootstrap contains public branding copy for everyone, and the full bundle
// catalog only after /client-config has authorized this session. Read it
// without importing bootstrap.ts: its default strings also use this module.
function bundleMessages(): Readonly<Record<string, string>> {
  if (typeof document === "undefined") return {};
  try {
    const source = document.getElementById("fleet-bootstrap")?.textContent;
    if (!source) return {};
    const boot = JSON.parse(source) as {
      localizations?: Record<string, Record<string, string>>;
      branding?: { localizations?: Record<string, Record<string, string>> };
    };
    return boot.localizations?.[language] ?? boot.branding?.localizations?.[language] ?? {};
  } catch {
    return {};
  }
}

let bundleCatalog = bundleMessages();

export function setBundleLocalizations(localizations?: Record<string, Record<string, string>>): void {
  bundleCatalog = localizations?.[language] ?? {};
}

function lookup(catalog: Readonly<Record<string, string>>, source: string): string | undefined {
  if (!Object.hasOwn(catalog, source)) return undefined;
  const value = catalog[source];
  return typeof value === "string" ? value : undefined;
}

/** Engine-owned UI copy never consults the deployment bundle catalog. */
export function t(source: string, values: readonly unknown[] = []): string {
  return translate(language, source, values);
}

export function translate(locale: Language, source: string, values: readonly unknown[] = []): string {
  if (typeof source !== "string") return "";
  return interpolate(lookup(catalogs[locale], source) ?? source, values);
}

/** Only for bundle-owned branding and metadata, never engine UI or user records.
 * A deployment's wording wins over the engine's translation of the same key. */
export function bundleText(source: string, values: readonly unknown[] = []): string {
  if (typeof source !== "string") return "";
  return interpolate(lookup(bundleCatalog, source) ?? lookup(catalogs[language], source) ?? source, values);
}

function interpolate(message: string, values: readonly unknown[]): string {
  return message.replace(/\{(\d+)\}/g, (whole, index: string) => {
    return Number(index) < values.length ? String(values[Number(index)]) : whole;
  });
}

export function formatNumber(value: number, options?: Intl.NumberFormatOptions): string {
  return new Intl.NumberFormat(language, options).format(value);
}

export function formatFixed(value: number, digits: number): string {
  // Keep the existing toFixed rounding contract; only presentation changes.
  return formatNumber(Number(value.toFixed(digits)), { minimumFractionDigits: digits, maximumFractionDigits: digits, useGrouping: false });
}

export function formatDate(value: Date | number, options?: Intl.DateTimeFormatOptions): string {
  return new Intl.DateTimeFormat(language, options).format(value);
}
