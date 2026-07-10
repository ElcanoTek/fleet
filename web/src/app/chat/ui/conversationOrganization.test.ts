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
  projectGroups,
  recentUnfiled,
  removeLabel,
  visibleConversationOrder,
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

describe("visibleConversationOrder", () => {
  // This is the shared source of truth for keyboard j/k navigation (chat-
  // experience) and the sidebar's rendered rows — the two must never drift.
  const all = [
    conv({ title: "Pinned loose", pinned: true }),
    conv({ title: "Filed", pinned: true, folder: "Work" }),
    conv({ title: "Recent loose", pinned: false }),
  ];

  it("returns pinned-unfiled then recent-unfiled when not filtering", () => {
    const filtered = filterConversations(all, {});
    expect(
      visibleConversationOrder({ all, filtered, filtering: false }).map((c) => c.title),
    ).toEqual(["Pinned loose", "Recent loose"]);
  });

  it("excludes filed conversations from the unfiltered order", () => {
    const order = visibleConversationOrder({ all, filtered: all, filtering: false });
    expect(order.map((c) => c.title)).not.toContain("Filed");
  });

  it("returns the filtered list verbatim (including filed rows) when filtering", () => {
    const filtered = filterConversations(all, { folder: "Work" });
    expect(
      visibleConversationOrder({ all, filtered, filtering: true }).map((c) => c.title),
    ).toEqual(["Filed"]);
  });

  it("does not mutate its inputs", () => {
    const order = visibleConversationOrder({ all, filtered: all, filtering: false });
    order.push(conv({ title: "extra" }));
    expect(all).toHaveLength(3);
  });
});

describe("project grouping (#509 follow-up)", () => {
  const proj = (id: string, updated_at: number) => ({ id, name: id, updated_at });

  it("a project conversation lives only under its project — excluded from Pinned and Chats", () => {
    const all = [
      conv({ title: "p", pinned: true, project_id: "alpha" }),
      conv({ title: "r", project_id: "alpha" }),
      conv({ title: "plain" }),
      conv({ title: "pinned-plain", pinned: true }),
    ];
    expect(pinnedUnfiled(all).map((c) => c.title)).toEqual(["pinned-plain"]);
    expect(recentUnfiled(all).map((c) => c.title)).toEqual(["plain"]);
    expect(visibleConversationOrder({ all, filtered: [], filtering: false }).map((c) => c.title)).toEqual([
      "pinned-plain",
      "plain",
    ]);
  });

  it("projectGroups returns the top N projects by recent update, each with its chats in input order", () => {
    const projects = [proj("old", 1), proj("newest", 9), proj("mid", 5)];
    const all = [
      conv({ title: "a", project_id: "newest" }),
      conv({ title: "b", project_id: "old" }),
      conv({ title: "c", project_id: "newest" }),
    ];
    const groups = projectGroups(all, projects, 2);
    expect(groups.map((g) => g.project.id)).toEqual(["newest", "mid"]);
    expect(groups[0].chats.map((c) => c.title)).toEqual(["a", "c"]);
    // "mid" has no chats yet but still gets a group — it must exist in the
    // rail to be a drag target.
    expect(groups[1].chats).toEqual([]);
  });

  it("search still reaches project conversations (filters are explicit)", () => {
    const all = [conv({ title: "quarterly report", project_id: "alpha" }), conv({ title: "misc" })];
    expect(filterConversations(all, { query: "quarterly" }).map((c) => c.title)).toEqual(["quarterly report"]);
  });
});
