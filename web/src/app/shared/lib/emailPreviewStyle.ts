import type { PillField, PillValues } from "@/app/chat/ui/protocolPills";
import type { EmailTemplate } from "./orchestratorApi";
import { parseCustomPalette } from "./formPickers";

// Shows the look picked in a template's Style field on its example email.
//
// The example is the bundle's pre-rendered sample, and Fleet does not know
// what a theme is. The bundle ships a recolor table with the preview
// (`preview_styles`, docs/EMAIL-TEMPLATES.md): for each palette option, which
// colors of the preview become which, and for each custom color, the preview
// colors it replaces. Recoloring is one pass over the markup's #RRGGBB values,
// the same swap the bundle's renderer makes, so the result is the email that
// look produces.
//
// Only a #RRGGBB value is ever written into the markup: the server refuses a
// table holding anything else, and both sides of every swap are checked again
// here, because the result goes into the preview frame.

export type PreviewStyles = NonNullable<EmailTemplate["preview_styles"]>;

const HEX = /#[0-9A-Fa-f]{6}\b/g;
const HEX_ONLY = /^#[0-9A-Fa-f]{6}$/;

/** {preview color: new color} for the form's current palette value; empty
 *  when the template has no palette field or the table does not cover it.
 *
 *  Before the form is open (`values` null) the look is the field's default,
 *  so the example starts as the email a person gets by changing nothing. A
 *  bundle whose default theme is a brand renders its examples in a plain
 *  palette and lists the brand as the default option. Custom colors go on
 *  top of that default look, which is what the renderer does: custom colors
 *  are passed without a theme, and the bundle's default theme still applies. */
export function previewRecolorTable(
  styles: PreviewStyles | null | undefined,
  fields: PillField[] | undefined,
  values: PillValues | null,
): Record<string, string> {
  const table: Record<string, string> = {};
  const field = fields?.find((f) => f.type === "palette");
  const fallback = typeof field?.default === "string" ? field.default : undefined;
  const picked = field && values ? values[field.key] : undefined;
  const value = values ? picked : fallback;
  if (!styles || !field || typeof value !== "string") return table;
  const put = (from: string, to: string) => {
    if (HEX_ONLY.test(from) && HEX_ONLY.test(to)) table[from.toUpperCase()] = to.toUpperCase();
  };
  const custom = parseCustomPalette(value, field.custom);
  if (custom) {
    for (const [from, to] of Object.entries((fallback && styles.options?.[fallback]) || {})) put(from, to);
    for (const [name, color] of Object.entries(custom)) {
      for (const from of styles.custom?.[name] ?? []) put(from, color);
    }
    return table;
  }
  for (const [from, to] of Object.entries(styles.options?.[value] ?? {})) put(from, to);
  return table;
}

/** The preview with every color in the table swapped, in one pass so a
 *  chain (A→B, B→C) never cascades. */
export function recolorPreview(html: string, table: Record<string, string>): string {
  if (Object.keys(table).length === 0) return html;
  return html.replace(HEX, (hex) => table[hex.toUpperCase()] ?? hex);
}
