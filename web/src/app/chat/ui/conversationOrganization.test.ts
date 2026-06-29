import { describe, expect, it } from "vitest";
import {
  MAX_LABELS,
  addLabel,
  canAddLabel,
  deriveFolders,
  deriveLabels,
  filterConversations,
  isFiltering,
  normalizeLabel,
  pinnedUnfiled,
  recentUnfiled,
  removeLabel,
  type OrganizableConversation,
} from "./conversationOrganization";

const conv = (over: Partial<OrganizableConversation>): OrganizableConversation => ({
  title: "Untitled",
  pinned: false,
  ...over,
});

describe("normalizeLabel", () => {
  it("trims whitespace and clamps to 32 chars", () => {
    expect(normalizeLabel("  hello  ")).toBe("hello");
    expect(normalizeLabel("x".repeat(40))).toHaveLength(32);
  });
});

describe("canAddLabel", () => {
  it("rejects empty, duplicate, and over-cap additions", () => {
    expect(canAddLabel([], "")).toBe(false);
    expect(canAddLabel([], "   ")).toBe(false);
    expect(canAddLabel(["work"], "work")).toBe(false);
    const full = Array.from({ length: MAX_LABELS }, (_, i) => `l${i}`);
    expect(canAddLabel(full, "another")).toBe(false);
  });

  it("accepts a fresh, non-empty label under the cap", () => {
    expect(canAddLabel(["work"], "urgent")).toBe(true);
  });
});

describe("addLabel / removeLabel", () => {
  it("appends a normalized label without mutating the input", () => {
    const existing = ["work"];
    const next = addLabel(existing, "  urgent ");
    expect(next).toEqual(["work", "urgent"]);
    expect(existing).toEqual(["work"]);
  });

  it("is a no-op when the label cannot be added", () => {
    expect(addLabel(["work"], "work")).toEqual(["work"]);
  });

  it("removes a label", () => {
    expect(removeLabel(["work", "urgent"], "work")).toEqual(["urgent"]);
  });
});

describe("deriveFolders", () => {
  it("returns distinct non-empty folders with counts, sorted by name", () => {
    const convs = [
      conv({ folder: "Research" }),
      conv({ folder: "Work Projects" }),
      conv({ folder: "Work Projects" }),
      conv({ folder: "" }),
      conv({}),
    ];
    expect(deriveFolders(convs)).toEqual([
      { name: "Research", count: 1 },
      { name: "Work Projects", count: 2 },
    ]);
  });
});

describe("deriveLabels", () => {
  it("counts labels across conversations, sorted by name", () => {
    const convs = [
      conv({ labels: ["client", "urgent"] }),
      conv({ labels: ["client"] }),
      conv({ labels: [] }),
      conv({}),
    ];
    expect(deriveLabels(convs)).toEqual([
      { name: "client", count: 2 },
      { name: "urgent", count: 1 },
    ]);
  });
});

describe("filterConversations", () => {
  const convs = [
    conv({ title: "Acme renewal", folder: "Clients", labels: ["client", "urgent"] }),
    conv({ title: "Omnicom pacing", folder: "Clients", labels: ["client"] }),
    conv({ title: "Schema notes", labels: ["research"] }),
  ];

  it("filters by folder (exact)", () => {
    expect(filterConversations(convs, { folder: "Clients" })).toHaveLength(2);
  });

  it("AND-filters by labels", () => {
    const res = filterConversations(convs, { labels: ["client", "urgent"] });
    expect(res.map((c) => c.title)).toEqual(["Acme renewal"]);
  });

  it("filters by case-insensitive title query", () => {
    expect(filterConversations(convs, { query: "omni" }).map((c) => c.title)).toEqual([
      "Omnicom pacing",
    ]);
  });

  it("combines folder + label + query", () => {
    const res = filterConversations(convs, { folder: "Clients", labels: ["client"], query: "acme" });
    expect(res.map((c) => c.title)).toEqual(["Acme renewal"]);
  });
});

describe("isFiltering", () => {
  it("is false for an empty filter and true when any facet is set", () => {
    expect(isFiltering({})).toBe(false);
    expect(isFiltering({ query: "  " })).toBe(false);
    expect(isFiltering({ folder: "Clients" })).toBe(true);
    expect(isFiltering({ labels: ["x"] })).toBe(true);
    expect(isFiltering({ query: "ac" })).toBe(true);
  });
});

describe("pinnedUnfiled / recentUnfiled", () => {
  const convs = [
    conv({ title: "Pinned loose", pinned: true }),
    conv({ title: "Pinned filed", pinned: true, folder: "Work" }),
    conv({ title: "Recent loose", pinned: false }),
    conv({ title: "Recent filed", pinned: false, folder: "Work" }),
  ];

  it("excludes filed conversations from both sections", () => {
    expect(pinnedUnfiled(convs).map((c) => c.title)).toEqual(["Pinned loose"]);
    expect(recentUnfiled(convs).map((c) => c.title)).toEqual(["Recent loose"]);
  });
});
