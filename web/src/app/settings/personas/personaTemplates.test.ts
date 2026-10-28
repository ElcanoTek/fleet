import { describe, expect, it } from "vitest";
import { cadenceLabel, PERSONA_TEMPLATES, slugify, twinBuilderMessage } from "./personaTemplates";

describe("personaTemplates", () => {
  it("slugify matches the server's lowercase-kebab, ≤48-char name shape", () => {
    const shape = /^[a-z0-9]([a-z0-9-]{0,46}[a-z0-9])?$/;
    for (const name of ["My Twin", "  Brad's  Deal Desk!! ", "Café Résumé", "x".repeat(80) + " tail", "a-"]) {
      expect(slugify(name)).toMatch(shape);
    }
    expect(slugify("Brad's Twin")).toBe("brad-s-twin");
  });

  it("names preset cadences in words and shows anything else as cron", () => {
    expect(cadenceLabel("0 */4 * * *")).toBe("Every 4 hours");
    expect(cadenceLabel("15 7 * * *")).toBe("15 7 * * *");
  });

  it("templates fill every field the editor needs", () => {
    for (const t of PERSONA_TEMPLATES) {
      const d = t.draft();
      expect(d.display_name).toBeTruthy();
      expect(d.instructions).toBeTruthy();
    }
  });

  it("the twin request honours the history opt-out and the chosen autonomy", () => {
    const off = twinBuilderMessage({ about: "", useHistory: false, autonomy: "act" });
    expect(off).toContain("Don't read my chat history");
    expect(off).toContain('autonomy "act"');
    expect(off).toContain("About me: (ask me)");
  });
});
