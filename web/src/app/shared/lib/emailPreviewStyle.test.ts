import { describe, expect, it } from "vitest";
import type { PillField } from "@/app/chat/ui/protocolPills";
import { previewRecolorTable, recolorPreview, type PreviewStyles } from "./emailPreviewStyle";

const style: PillField = {
  key: "style",
  label: "Style",
  type: "palette",
  options: ["elcano", "amazon"],
  swatches: { elcano: ["#1A0B1E", "#6262A0"], amazon: ["#333E48", "#FF9900"] },
  custom: ["Header", "Accent"],
};
const styles: PreviewStyles = {
  options: { elcano: {}, amazon: { "#1A0B1E": "#333E48", "#6262A0": "#FF9900", "#333E48": "#000000" } },
  custom: { Header: ["#1A0B1E"], Accent: ["#7272AB", "#6262A0"] },
};
const page = '<td bgcolor="#1a0b1e" style="border:1px solid #7272AB"><a style="background:#6262A0">#333E48</a></td>';

describe("the example email's style", () => {
  it("is left alone for the default look, without a table, or without a palette field", () => {
    expect(recolorPreview(page, previewRecolorTable(styles, [style], { style: "elcano" }))).toBe(page);
    expect(recolorPreview(page, previewRecolorTable(null, [style], { style: "amazon" }))).toBe(page);
    expect(recolorPreview(page, previewRecolorTable(styles, [{ ...style, type: "select" }], { style: "amazon" }))).toBe(page);
    expect(recolorPreview(page, previewRecolorTable(styles, [style], null))).toBe(page);
  });

  it("swaps a preset's colors in one pass, whatever their case", () => {
    // #1A0B1E becomes #333E48 and stays that: the table's own #333E48 entry is for the original text.
    expect(recolorPreview(page, previewRecolorTable(styles, [style], { style: "amazon" }))).toBe(
      '<td bgcolor="#333E48" style="border:1px solid #7272AB"><a style="background:#FF9900">#000000</a></td>',
    );
  });

  it("puts each custom color on the preview colors it replaces", () => {
    const table = previewRecolorTable(styles, [style], { style: "custom (Header #1F3A5F, Accent #C2410C)" });
    expect(table).toEqual({ "#1A0B1E": "#1F3A5F", "#7272AB": "#C2410C", "#6262A0": "#C2410C" });
    expect(recolorPreview(page, table)).toBe(
      '<td bgcolor="#1F3A5F" style="border:1px solid #C2410C"><a style="background:#C2410C">#333E48</a></td>',
    );
  });

  // A bundle whose default theme is a brand: the example is rendered in the
  // plain palette and the brand is the picker's default option.
  const branded: PillField = { ...style, options: ["acme", "plain"], default: "acme" };
  const brandedStyles: PreviewStyles = {
    options: { plain: {}, acme: { "#1A0B1E": "#0A0D10", "#6262A0": "#0A0D10", "#333E48": "#111111" } },
    custom: { Header: ["#1A0B1E"], Accent: ["#7272AB", "#6262A0"] },
  };

  it("shows the default look before the form is open", () => {
    expect(recolorPreview(page, previewRecolorTable(brandedStyles, [branded], null))).toBe(
      '<td bgcolor="#0A0D10" style="border:1px solid #7272AB"><a style="background:#0A0D10">#111111</a></td>',
    );
    // Once the form is open its value decides, the plain look included.
    expect(recolorPreview(page, previewRecolorTable(brandedStyles, [branded], { style: "plain" }))).toBe(page);
    // Without a default the form preselects the first option, so the example shows that one.
    const { default: _unset, ...firstIsBrand } = branded;
    expect(previewRecolorTable(brandedStyles, [firstIsBrand], null)).toEqual(brandedStyles.options?.acme);
    expect(recolorPreview(page, previewRecolorTable(styles, [style], null))).toBe(page);
  });

  it("puts custom colors on top of the default look", () => {
    const table = previewRecolorTable(brandedStyles, [branded], { style: "custom (Header #1F3A5F, Accent #C2410C)" });
    // The header and accent are the picked colors; the text keeps the brand's.
    expect(table).toEqual({ "#1A0B1E": "#1F3A5F", "#7272AB": "#C2410C", "#6262A0": "#C2410C", "#333E48": "#111111" });
  });

  it("never writes anything but a color into the markup", () => {
    const hostile = {
      options: { amazon: { "#1A0B1E": '"><script>alert(1)</script>', "</td>": "#333E48" } },
      custom: { Header: ['"onload="x'] },
    } as PreviewStyles;
    expect(previewRecolorTable(hostile, [style], { style: "amazon" })).toEqual({});
    expect(previewRecolorTable(hostile, [style], { style: "custom (Header #1F3A5F, Accent #C2410C)" })).toEqual({});
  });
});
