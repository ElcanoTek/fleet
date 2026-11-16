import type { PillField, PillValues } from "@/app/chat/ui/protocolPills";
import { isValidEmail } from "./format";
import { readPref, writePref } from "./storage";

// Value formats and browser memory for the two form pickers (`recipients` and
// `palette`, docs/PROMPT-LIBRARY.md "Pickers"). Both fields hold a plain
// string, so a form's template renders them like any text field.
//
// The memory is a per-browser convenience kept in localStorage: the addresses
// a person typed into a recipients picker, and the last few styles they used
// in a palette. Nothing here is sent to the server, and every read tolerates
// missing or damaged storage.

const RECIPIENTS_KEY = "fleet.form.recipients";
const PALETTE_KEY_PREFIX = "fleet.form.palette.";
const MAX_REMEMBERED_RECIPIENTS = 30;
export const MAX_RECENT_PALETTES = 4;

const HEX_COLOR = /^#[0-9A-F]{6}$/;

function readList(key: string): string[] {
  try {
    const parsed: unknown = JSON.parse(readPref(key) ?? "[]");
    return Array.isArray(parsed) ? parsed.filter((v): v is string => typeof v === "string") : [];
  } catch {
    return [];
  }
}

// ── recipients ────────────────────────────────────────────────────────────

/** The addresses in a recipients value, lower-cased, in order, each once. */
export function splitRecipients(value: unknown): string[] {
  if (typeof value !== "string") return [];
  const out: string[] = [];
  for (const part of value.split(/[,;\s]+/)) {
    const email = part.trim().toLowerCase();
    if (email && !out.includes(email)) out.push(email);
  }
  return out;
}

export function joinRecipients(emails: string[]): string {
  return emails.join(", ");
}

/** Addresses this browser added in a recipients picker, newest first. */
export function rememberedRecipients(): string[] {
  return readList(RECIPIENTS_KEY).filter(isValidEmail);
}

export function rememberRecipients(emails: string[]): void {
  const fresh = emails.map((e) => e.trim().toLowerCase()).filter(isValidEmail);
  if (fresh.length === 0) return;
  const merged = [...fresh, ...rememberedRecipients().filter((e) => !fresh.includes(e))];
  writePref(RECIPIENTS_KEY, JSON.stringify(merged.slice(0, MAX_REMEMBERED_RECIPIENTS)));
}

// ── palette ───────────────────────────────────────────────────────────────

/** "custom (Header #1F3A5F, Accent #C2410C)": the value of a palette field
 *  whose colors the person picked themselves, in the field's `custom` order. */
export function formatCustomPalette(names: string[], colors: Record<string, string>): string {
  return `custom (${names.map((name) => `${name} ${(colors[name] ?? "").toUpperCase()}`).join(", ")})`;
}

/** The colors of a custom palette value, or null when the value is not one
 *  for exactly these names. */
export function parseCustomPalette(value: unknown, names: string[] | undefined): Record<string, string> | null {
  if (typeof value !== "string" || !names || names.length === 0) return null;
  const match = /^custom \((.*)\)$/.exec(value.trim());
  if (!match) return null;
  const parts = match[1].split(", ");
  if (parts.length !== names.length) return null;
  const colors: Record<string, string> = {};
  for (const [i, name] of names.entries()) {
    const color = parts[i].startsWith(`${name} `) ? parts[i].slice(name.length + 1) : "";
    if (!HEX_COLOR.test(color)) return null;
    colors[name] = color;
  }
  return colors;
}

/** The colors a palette value is drawn with: an option's swatches, or the
 *  picked colors of a custom value. Null for a value the field cannot show. */
export function paletteColors(field: PillField, value: unknown): string[] | null {
  if (typeof value !== "string") return null;
  if (field.options?.includes(value)) return field.swatches?.[value] ?? [];
  const custom = parseCustomPalette(value, field.custom);
  return custom ? (field.custom ?? []).map((name) => custom[name]) : null;
}

/** Palette values this browser used last for a field of this key, newest
 *  first, limited to the ones the field can still show. */
export function recentPalettes(field: PillField): string[] {
  return readList(PALETTE_KEY_PREFIX + field.key)
    .filter((value) => paletteColors(field, value) !== null)
    .slice(0, MAX_RECENT_PALETTES);
}

function rememberPalette(field: PillField, value: string): void {
  if (paletteColors(field, value) === null) return;
  const key = PALETTE_KEY_PREFIX + field.key;
  const merged = [value, ...readList(key).filter((v) => v !== value)];
  writePref(key, JSON.stringify(merged.slice(0, MAX_RECENT_PALETTES)));
}

/** Called when a form is used: remembers what its pickers held. */
export function rememberPickerValues(fields: PillField[], values: PillValues): void {
  for (const field of fields) {
    const value = values[field.key];
    if (field.type === "recipients") rememberRecipients(splitRecipients(value));
    else if (field.type === "palette" && typeof value === "string") rememberPalette(field, value);
  }
}
