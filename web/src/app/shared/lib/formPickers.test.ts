import { beforeEach, describe, expect, it } from "vitest";
import type { PillField } from "@/app/chat/ui/protocolPills";
import {
  formatCustomPalette,
  joinRecipients,
  paletteColors,
  parseCustomPalette,
  recentPalettes,
  rememberPickerValues,
  rememberRecipients,
  rememberedRecipients,
  splitRecipients,
} from "./formPickers";

const style: PillField = {
  key: "style",
  label: "Style",
  type: "palette",
  options: ["elcano", "amazon"],
  swatches: { elcano: ["#1A0B1E", "#6262A0"], amazon: ["#333E48", "#FF9900"] },
  custom: ["Header", "Accent"],
};

beforeEach(() => window.localStorage.clear());

describe("recipients", () => {
  it("splits on commas, semicolons and spaces, lower-cases and de-duplicates", () => {
    expect(splitRecipients(" A@x.com, b@x.com;a@x.com  c@x.com ")).toEqual(["a@x.com", "b@x.com", "c@x.com"]);
    expect(splitRecipients("")).toEqual([]);
    expect(splitRecipients(undefined)).toEqual([]);
    expect(joinRecipients(["a@x.com", "b@x.com"])).toBe("a@x.com, b@x.com");
  });

  it("remembers added addresses newest first and drops anything that is not one", () => {
    rememberRecipients(["a@x.com", "not-an-address"]);
    rememberRecipients(["B@x.com", "a@x.com"]);
    expect(rememberedRecipients()).toEqual(["b@x.com", "a@x.com"]);
  });

  it("reads damaged storage as empty", () => {
    window.localStorage.setItem("fleet.form.recipients", "{not json");
    expect(rememberedRecipients()).toEqual([]);
    window.localStorage.setItem("fleet.form.recipients", JSON.stringify({ a: 1 }));
    expect(rememberedRecipients()).toEqual([]);
  });
});

describe("palette", () => {
  it("round-trips a custom value in the field's color order", () => {
    const value = formatCustomPalette(["Header", "Accent"], { Accent: "#c2410c", Header: "#1f3a5f" });
    expect(value).toBe("custom (Header #1F3A5F, Accent #C2410C)");
    expect(parseCustomPalette(value, style.custom)).toEqual({ Header: "#1F3A5F", Accent: "#C2410C" });
  });

  it("does not read a custom value made for other colors, or a preset", () => {
    expect(parseCustomPalette("custom (Header #1F3A5F)", style.custom)).toBeNull();
    expect(parseCustomPalette("custom (Top #1F3A5F, Accent #C2410C)", style.custom)).toBeNull();
    expect(parseCustomPalette("custom (Header red, Accent #C2410C)", style.custom)).toBeNull();
    expect(parseCustomPalette("elcano", style.custom)).toBeNull();
    expect(parseCustomPalette("custom (Header #1F3A5F)", undefined)).toBeNull();
  });

  it("knows the colors of a preset and of a custom value", () => {
    expect(paletteColors(style, "amazon")).toEqual(["#333E48", "#FF9900"]);
    expect(paletteColors(style, "custom (Header #1F3A5F, Accent #C2410C)")).toEqual(["#1F3A5F", "#C2410C"]);
    expect(paletteColors(style, "gone")).toBeNull();
  });

  it("keeps the last four used, newest first, and hides ones the field no longer has", () => {
    for (const value of ["elcano", "amazon", "custom (Header #111111, Accent #222222)", "elcano"]) {
      rememberPickerValues([style], { style: value });
    }
    expect(recentPalettes(style)).toEqual(["elcano", "custom (Header #111111, Accent #222222)", "amazon"]);
    expect(recentPalettes({ ...style, options: ["elcano"], custom: undefined })).toEqual(["elcano"]);
    // Another field's history is its own.
    expect(recentPalettes({ ...style, key: "other" })).toEqual([]);
  });

  it("remembers a used form's recipients too", () => {
    const to: PillField = { key: "to", label: "Send to", type: "recipients" };
    rememberPickerValues([to, style], { to: "a@x.com, b@x.com", style: "amazon" });
    expect(rememberedRecipients()).toEqual(["a@x.com", "b@x.com"]);
    expect(recentPalettes(style)).toEqual(["amazon"]);
  });
});
