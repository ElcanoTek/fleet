import { describe, expect, it } from "vitest";
import {
  asText,
  formInitialValues,
  getPill,
  isPillReady,
  PROTOCOL_PILLS,
  type ProtocolPill,
} from "./protocolPills";

function pill(id: string): ProtocolPill {
  const p = getPill(id);
  if (!p) throw new Error(`missing pill: ${id}`);
  return p;
}

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
    const weekly = formInitialValues(pill("weekly"));
    expect(weekly.window).toBe("Last full Mon–Sun"); // explicit default
    expect(weekly.breakout).toBe("Channel"); // explicit default
    expect(weekly.client).toBe(""); // text, no default
    expect(weekly.pacing).toBe(""); // text, no default

    const wrap = formInitialValues(pill("wrap"));
    expect(wrap.gammaDeck).toBe(false);
    expect(wrap.format).toBe("PPTX");
    expect(wrap.flight).toEqual({ from: "", to: "" });
    expect(typeof wrap.year).toBe("number");

    const opt = formInitialValues(pill("optimization"));
    expect(opt.lookback).toBe(14);
  });
});

describe("isPillReady", () => {
  it("gates on required fields", () => {
    const weekly = pill("weekly");
    const base = formInitialValues(weekly);
    expect(isPillReady(weekly, base)).toBe(false);
    expect(isPillReady(weekly, { ...base, client: "Meridian Auto" })).toBe(false);
    expect(isPillReady(weekly, { ...base, client: "Meridian Auto", elc: "ELC-4821" })).toBe(true);
  });

  it("requires both ends of a required daterange", () => {
    const wrap = pill("wrap");
    const base = { ...formInitialValues(wrap), client: "Meridian Auto", elc: "ELC-4821" };
    expect(isPillReady(wrap, base)).toBe(false);
    expect(isPillReady(wrap, { ...base, flight: { from: "2026-04-01", to: "" } })).toBe(false);
    expect(isPillReady(wrap, { ...base, flight: { from: "2026-04-01", to: "2026-05-31" } })).toBe(true);
  });

  it("treats the diagnostic's optional form as always ready", () => {
    const diag = pill("diagnostic");
    expect(isPillReady(diag, formInitialValues(diag))).toBe(true);
  });
});

describe("promptTemplate — Weekly Performance Report (dsp-reporting trigger)", () => {
  const weekly = pill("weekly");

  it("emits the protocol trigger and includes only filled details", () => {
    const v = { ...formInitialValues(weekly), client: "Meridian Auto", elc: "ELC-4821" };
    const out = weekly.promptTemplate!(v);
    expect(out).toContain("Run the DSP reporting protocol for Meridian Auto (ELC-4821).");
    // defaulted advanced fields carry through
    expect(out).toContain("Reporting window: Last full Mon–Sun");
    expect(out).toContain("Breakout: Channel");
    // unfilled optional fields are omitted, not stubbed
    expect(out).not.toContain("Mailbox:");
    expect(out).not.toContain("Primary KPI:");
  });

  it("appends mailbox / recipient / KPI when provided", () => {
    const v = {
      ...formInitialValues(weekly),
      client: "Meridian Auto",
      elc: "ELC-4821",
      mailbox: "meridian@victoria.elcanotek.com",
      recipient: "dana@meridianauto.com",
      kpi: "CTR = clicks / impressions",
    };
    const out = weekly.promptTemplate!(v);
    expect(out).toContain("Mailbox: meridian@victoria.elcanotek.com");
    expect(out).toContain("Recipient(s): dana@meridianauto.com");
    expect(out).toContain("Primary KPI: CTR = clicks / impressions");
  });
});

describe("promptTemplate — Optimization Report (optimization trigger)", () => {
  it("matches the optimization protocol's exact usage string", () => {
    const opt = pill("optimization");
    const v = {
      ...formInitialValues(opt),
      alias: "twc.johndeere.elc00096@victoria.elcanotek.com",
      recipient: "brad@elcanotek.com",
    };
    expect(opt.promptTemplate!(v)).toBe(
      "Run the optimization protocol for emails sent to " +
        "'twc.johndeere.elc00096@victoria.elcanotek.com' and send recommendations to " +
        "'brad@elcanotek.com'. Lookback: 14d.",
    );
  });

  it("folds in the campaign code when the optional field is filled", () => {
    const opt = pill("optimization");
    const v = {
      ...formInitialValues(opt),
      alias: "twc.johndeere.elc00096@victoria.elcanotek.com",
      recipient: "brad@elcanotek.com",
      elc: "AUTO-2024",
    };
    expect(opt.promptTemplate!(v)).toBe(
      "Run the optimization protocol for emails sent to " +
        "'twc.johndeere.elc00096@victoria.elcanotek.com' and send recommendations to " +
        "'brad@elcanotek.com'. Campaign code: AUTO-2024. Lookback: 14d.",
    );
  });
});

describe("promptTemplate — Performance Diagnostic (optional form)", () => {
  const diag = pill("diagnostic");

  it("works with no inputs (still names the unallocated default)", () => {
    const out = diag.promptTemplate!(formInitialValues(diag));
    expect(out).toContain("Run a performance diagnostic.");
    expect(out).toContain("Include unallocated conversions: no");
    expect(out).toContain("wins, risks, and what to change");
  });

  it("folds in the optional details when filled", () => {
    const v = {
      ...formInitialValues(diag),
      client: "Meridian Auto",
      elc: "ELC-4821",
      kpi: "CPA",
      range: { from: "2026-01-01", to: "2026-01-31" },
      unallocated: true,
    };
    const out = diag.promptTemplate!(v);
    expect(out).toContain("Campaign: Meridian Auto (ELC-4821)");
    expect(out).toContain("Primary KPI: CPA");
    expect(out).toContain("Date range: 2026-01-01 → 2026-01-31");
    expect(out).toContain("Include unallocated conversions: yes");
  });

  it("ships a conversational starter for the chat-first path", () => {
    expect(diag.starterPrompt ?? "").toContain("performance diagnostic");
  });
});

describe("promptTemplate — End-of-Campaign Wrap (Gamma toggle)", () => {
  const wrap = pill("wrap");
  const base = {
    ...formInitialValues(wrap),
    client: "Meridian Auto",
    elc: "ELC-4821",
    flight: { from: "2026-04-01", to: "2026-05-31" },
  };

  it("defaults to a written summary (Gamma off)", () => {
    const out = wrap.promptTemplate!(base);
    expect(out).toContain(
      "Build an end-of-campaign wrap for Meridian Auto (ELC-4821), full flight " +
        "2026-04-01–2026-05-31, for the client audience.",
    );
    expect(out).toContain("Deliver it as a written end-of-campaign summary.");
    expect(out).not.toContain("Gamma");
  });

  it("requests the Gamma deck when the toggle is on", () => {
    const out = wrap.promptTemplate!({ ...base, gammaDeck: true });
    expect(out).toContain("Campaign Wrap-Up presentation deck via Gamma");
    expect(out).toContain("export as PPTX");
    expect(out).not.toContain("written end-of-campaign summary");
  });
});

describe("catalog shape", () => {
  it("delivers the form + conversation hybrid the four interviewed workflows need", () => {
    expect(PROTOCOL_PILLS.map((p) => p.id).sort()).toEqual(
      ["diagnostic", "optimization", "weekly", "wrap"].sort(),
    );
    // at least one of each interaction shape ships
    expect(PROTOCOL_PILLS.some((p) => p.type === "form")).toBe(true);
    expect(PROTOCOL_PILLS.some((p) => p.optionalForm)).toBe(true);
    // every form pill can produce a prompt
    for (const p of PROTOCOL_PILLS) {
      expect(typeof p.promptTemplate).toBe("function");
    }
  });
});
