import { describe, expect, it } from "vitest";
import {
  bulkScope,
  connectorStatus,
  countOn,
  groupConnectors,
  highlightSegments,
  libraryBulkScope,
  matchAll,
  matchConnector,
  matchesStatus,
  onSnapshot,
  orderedCategories,
  popoverSections,
  seatLabel,
  seatOptions,
  type PickerConnector,
} from "./toolsPicker";

const CATS = [
  { id: "ssps", name: "SSPs" },
  { id: "deal-management", name: "Deal Management" },
  { id: "data-storage", name: "Data & Storage" },
  { id: "misc", name: "Misc Tools" },
];

const row = (r: Partial<PickerConnector> & { name: string }): PickerConnector => ({
  description: "",
  tools: [],
  tool_count: 0,
  enabled: false,
  ...r,
});

const ROWS: PickerConnector[] = [
  row({ name: "dealsheet", display_name: "Deal Sheet", category: "deal-management", always_on: true, enabled: true,
    description: "Build a branded XLSX deal sheet." }),
  row({ name: "deallib", display_name: "Deal Library", category: "deal-management", description: "Search deals." }),
  row({ name: "ix", display_name: "Index Exchange", category: "ssps", enabled: true,
    description: "Create and pause PMP deals on Index Exchange.", tools: ["ix_create_deal", "ix_clone_deal"] }),
  row({ name: "magnite", display_name: "Magnite", category: "ssps", description: "Streaming and DV+ deals.",
    tools: ["mg_clone_deal"] }),
  row({ name: "tl", display_name: "TripleLift", category: "ssps", status: "unavailable" }),
  row({ name: "bq", display_name: "BigQuery Reporting", category: "data-storage", status: "needs_setup",
    description: "Query the reporting warehouse." }),
  row({ name: "webfetch", display_name: "Web Fetch", category: "nonsense" }),
  row({ name: "broken", display_name: "Broken", always_on: true, enabled: false }),
];

describe("connectorStatus / countOn", () => {
  it("maps rows to the five statuses", () => {
    expect(ROWS.map(connectorStatus)).toEqual(["always", "off", "on", "off", "unavailable", "setup", "off", "unavailable"]);
  });

  it("counts always-on as on, never needs-setup or unreachable always-on", () => {
    expect(countOn(ROWS)).toBe(2);
  });
});

describe("matchConnector", () => {
  const score = (name: string, q: string) => matchConnector(ROWS.find((r) => r.name === name)!, q, CATS)?.score;

  it("ranks name prefix > name contains > category > description > inner tool", () => {
    expect(score("deallib", "deal")).toBe(4);
    expect(score("ix", "exchange")).toBe(3);
    expect(score("magnite", "ssp")).toBe(2);
    expect(score("bq", "warehouse")).toBe(1.5);
    expect(matchConnector(ROWS[3], "mg_clone", CATS)).toEqual({ score: 1, toolHits: ["mg_clone_deal"] });
    expect(score("magnite", "zzz")).toBeUndefined();
  });

  it("puts an unknown category under Misc Tools", () => {
    expect(score("webfetch", "misc")).toBe(2);
  });
});

describe("groupConnectors", () => {
  it("ranks groups by best match, rows by score then name", () => {
    const groups = groupConnectors(matchAll(ROWS, "deal", CATS), CATS, true);
    expect(groups.map((g) => g.category.id)).toEqual(["deal-management", "ssps"]);
    expect(groups[0].rows.map((r) => r.connector.name)).toEqual(["deallib", "dealsheet"]);
    // Index Exchange (description) above Magnite (description too) by name.
    expect(groups[1].rows.map((r) => r.connector.name)).toEqual(["ix", "magnite"]);
  });

  it("keeps category order, Misc Tools last, when browsing", () => {
    const groups = groupConnectors(matchAll(ROWS, "", CATS), CATS, false);
    expect(groups.map((g) => g.category.id)).toEqual(["ssps", "deal-management", "data-storage", "misc"]);
  });
});

describe("popoverSections", () => {
  it("pins the On snapshot first and hides rows without a switch", () => {
    const sections = popoverSections(ROWS, "", CATS, onSnapshot(ROWS));
    expect(sections.map((s) => s.label)).toEqual(["On", "SSPs", "Deal Management", "Misc Tools"]);
    expect(sections[0].rows.map((r) => r.connector.name)).toEqual(["ix", "dealsheet"]);
    expect(sections[0].count).toBe("2");
    const names = sections.flatMap((s) => s.rows.map((r) => r.connector.name));
    expect(names).not.toContain("bq");
    expect(names).not.toContain("tl");
  });

  it("does not move a row that is toggled while open", () => {
    const pinned = onSnapshot(ROWS);
    const toggled = ROWS.map((r) => (r.name === "ix" ? { ...r, enabled: false } : r));
    const sections = popoverSections(toggled, "", CATS, pinned);
    expect(sections[0].rows.map((r) => r.connector.name)).toContain("ix");
  });

  it("search includes needs-setup and unavailable rows with match counts", () => {
    const sections = popoverSections(ROWS, "ssp", CATS, new Set());
    expect(sections).toHaveLength(1);
    expect(sections[0].count).toBe("3 matches");
    expect(bulkScope(sections).sort()).toEqual(["ix", "magnite"]);
  });
});

describe("library helpers", () => {
  it("On includes always-on; the rest filter exactly", () => {
    expect(ROWS.filter((r) => matchesStatus(r, "on")).map((r) => r.name)).toEqual(["dealsheet", "ix"]);
    expect(ROWS.filter((r) => matchesStatus(r, "setup")).map((r) => r.name)).toEqual(["bq"]);
  });

  it("phrases the bulk scope", () => {
    expect(libraryBulkScope(10, { filtered: false })).toBe("Applies to 10 connections");
    expect(libraryBulkScope(5, { filtered: false, categoryName: "SSPs" })).toBe("Applies to 5 connections in SSPs");
    expect(libraryBulkScope(1, { filtered: true, categoryName: "SSPs" })).toBe("Applies to 1 connection shown");
  });

  it("forces Misc Tools last and falls back when no list came", () => {
    expect(orderedCategories([{ id: "misc", name: "Misc Tools" }, { id: "ssps", name: "SSPs" }]).map((c) => c.id)).toEqual(["ssps", "misc"]);
    expect(orderedCategories([]).map((c) => c.id)).toEqual(["ssps", "deal-management", "data-storage", "misc"]);
  });
});

describe("highlightSegments", () => {
  it("splits matches case-insensitively", () => {
    expect(highlightSegments("ix_clone_deal", "CLONE")).toEqual([
      { text: "ix_", match: false },
      { text: "clone", match: true },
      { text: "_deal", match: false },
    ]);
  });
});

describe("seatOptions", () => {
  it("labels the default seat with the deployment name", () => {
    const ix = row({ name: "ix", accounts: ["OMG_USD", "OMG_GBP"] });
    expect(seatOptions(ix, "Elcano").map((o) => o.label)).toEqual(["Elcano", "OMG_USD", "OMG_GBP"]);
    expect(seatLabel({ ...ix, account: "OMG_GBP" }, "Elcano")).toBe("OMG_GBP");
    expect(seatLabel(ix, "Reklaim")).toBe("Reklaim");
  });

  it("uses a named default and does not repeat it", () => {
    const ix = row({ name: "ix", accounts: ["OMG_USD", "OMG_GBP"], default_account: "OMG_USD" });
    expect(seatOptions(ix, "Elcano").map((o) => [o.value, o.label])).toEqual([
      ["", "OMG_USD"],
      ["OMG_GBP", "OMG_GBP"],
    ]);
  });

  it("calls a hosted connection's unlabeled login the primary login", () => {
    const gh = row({ name: "github", remote: true, accounts: ["work"] });
    expect(seatOptions(gh, "Elcano")[0].label).toBe("Primary login");
  });
});
