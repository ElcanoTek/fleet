import { describe, expect, it } from "vitest";
import {
  asText,
  DEFAULT_PILLS,
  formInitialValues,
  getPill,
  isPillReady,
  pillToPrompt,
  type ProtocolPill,
} from "./protocolPills";

// A small config-style fixture (the shape that arrives over JSON from
// /api/client-config) — exercises the generic helpers without any client
// specifics.
const TEMPLATE_PILL: ProtocolPill = {
  id: "report",
  section: "Reporting",
  type: "form",
  icon: "bar-chart",
  title: "Build a report",
  desc: "Fill a few fields and generate a report.",
  cta: "Run report",
  fields: [
    { key: "client", label: "Client", type: "text", required: true },
    { key: "window", label: "Window", type: "text", default: "last week" },
  ],
  promptTemplate: "Build a report for {client} covering {window}.",
};

const FALLBACK_PILL: ProtocolPill = {
  id: "wrap",
  section: "Reporting",
  type: "form",
  icon: "layers",
  title: "End-of-campaign wrap",
  desc: "Summarize a finished campaign.",
  cta: "Build wrap",
  fields: [
    { key: "client", label: "Client", type: "text", required: true },
    { key: "flight", label: "Flight", type: "daterange" },
    { key: "deck", label: "Build a deck", type: "toggle", default: false },
  ],
  // no promptTemplate → neutral fallback
};

describe("asText", () => {
  it("trims strings and stringifies numbers, blanks everything else", () => {
    expect(asText("  hi ")).toBe("hi");
    expect(asText(14)).toBe("14");
    expect(asText(true)).toBe("");
    expect(asText({ from: "a", to: "b" })).toBe("");
    expect(asText(undefined)).toBe("");
  });
});

describe("formInitialValues", () => {
  it("honors field defaults and type fallbacks", () => {
    const v = formInitialValues(FALLBACK_PILL);
    expect(v.client).toBe(""); // text, no default
    expect(v.flight).toEqual({ from: "", to: "" }); // daterange fallback
    expect(v.deck).toBe(false); // explicit default

    const t = formInitialValues(TEMPLATE_PILL);
    expect(t.window).toBe("last week"); // explicit default
  });
});

describe("isPillReady", () => {
  it("gates on required fields", () => {
    const base = formInitialValues(TEMPLATE_PILL);
    expect(isPillReady(TEMPLATE_PILL, base)).toBe(false);
    expect(isPillReady(TEMPLATE_PILL, { ...base, client: "Acme" })).toBe(true);
  });

  it("requires both ends of a required daterange", () => {
    const pill: ProtocolPill = {
      ...FALLBACK_PILL,
      fields: [{ key: "flight", label: "Flight", type: "daterange", required: true }],
    };
    const base = formInitialValues(pill);
    expect(isPillReady(pill, base)).toBe(false);
    expect(isPillReady(pill, { flight: { from: "2026-04-01", to: "" } })).toBe(false);
    expect(isPillReady(pill, { flight: { from: "2026-04-01", to: "2026-05-31" } })).toBe(true);
  });

  it("treats a pill with no required fields as always ready", () => {
    const pill: ProtocolPill = { ...TEMPLATE_PILL, fields: [], promptTemplate: undefined };
    expect(isPillReady(pill, formInitialValues(pill))).toBe(true);
  });
});

describe("getPill", () => {
  it("looks a pill up by id within a provided list", () => {
    const list = [TEMPLATE_PILL, FALLBACK_PILL];
    expect(getPill("wrap", list)?.title).toBe("End-of-campaign wrap");
    expect(getPill("nope", list)).toBeUndefined();
  });
});

describe("pillToPrompt — string template", () => {
  it("interpolates {key} tokens from filled field values", () => {
    const v = { ...formInitialValues(TEMPLATE_PILL), client: "Acme" };
    expect(pillToPrompt(TEMPLATE_PILL, v)).toBe("Build a report for Acme covering last week.");
  });

  it("leaves a token in place when its field is blank", () => {
    const v = formInitialValues(TEMPLATE_PILL); // client unset
    expect(pillToPrompt(TEMPLATE_PILL, v)).toBe("Build a report for {client} covering last week.");
  });

  it("returns a token-free template verbatim", () => {
    const pill: ProtocolPill = { ...TEMPLATE_PILL, promptTemplate: "Summarize the attached document." };
    expect(pillToPrompt(pill, {})).toBe("Summarize the attached document.");
  });
});

describe("pillToPrompt — neutral fallback (no template)", () => {
  it("builds 'Title.' plus Label: value lines for filled fields only", () => {
    const v = {
      ...formInitialValues(FALLBACK_PILL),
      client: "Acme",
      flight: { from: "2026-04-01", to: "2026-05-31" },
      deck: true,
    };
    const out = pillToPrompt(FALLBACK_PILL, v);
    expect(out).toContain("End-of-campaign wrap.");
    expect(out).toContain("Client: Acme");
    expect(out).toContain("Flight: 2026-04-01 → 2026-05-31");
    expect(out).toContain("Build a deck: yes");
  });

  it("omits blank fields entirely", () => {
    const out = pillToPrompt(FALLBACK_PILL, formInitialValues(FALLBACK_PILL));
    expect(out).toBe("End-of-campaign wrap. Build a deck: no.");
    expect(out).not.toContain("Client:");
    expect(out).not.toContain("Flight:");
  });
});

describe("DEFAULT_PILLS — neutral fallback catalog", () => {
  it("ships generic, client-agnostic quick-start cards", () => {
    expect(DEFAULT_PILLS.map((p) => p.id).sort()).toEqual(
      ["analyze-data", "draft", "summarize"].sort(),
    );
    // at least one of each interaction shape
    expect(DEFAULT_PILLS.some((p) => p.type === "form")).toBe(true);
    expect(DEFAULT_PILLS.some((p) => p.optionalForm)).toBe(true);
    // every pill has the required static fields and renders a prompt
    for (const p of DEFAULT_PILLS) {
      expect(p.id).toBeTruthy();
      expect(p.title).toBeTruthy();
      expect(p.icon).toBeTruthy();
      expect(p.cta).toBeTruthy();
      expect(typeof pillToPrompt(p, formInitialValues(p))).toBe("string");
    }
  });

  it("carries no client-specific content", () => {
    const blob = JSON.stringify(DEFAULT_PILLS).toLowerCase();
    expect(blob).not.toContain("elcano");
    expect(blob).not.toContain("victoria");
    expect(blob).not.toContain("dsp");
  });
});
