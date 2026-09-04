import { describe, expect, it } from "vitest";
import {
  MAX_LABELS,
  addLabel,
  canAddLabel,
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
    conv({ title: "Acme renewal", labels: ["client", "urgent"] }),
    conv({ title: "Omnicom pacing", labels: ["client"] }),
    conv({ title: "Schema notes", labels: ["research"] }),
  ];

  it("AND-filters by labels", () => {
    const res = filterConversations(convs, { labels: ["client", "urgent"] });
    expect(res.map((c) => c.title)).toEqual(["Acme renewal"]);
  });

  it("filters by case-insensitive title query", () => {
    expect(filterConversations(convs, { query: "omni" }).map((c) => c.title)).toEqual([
      "Omnicom pacing",
    ]);
  });

  it("combines label + query", () => {
    const res = filterConversations(convs, { labels: ["client"], query: "acme" });
    expect(res.map((c) => c.title)).toEqual(["Acme renewal"]);
  });
});

describe("isFiltering", () => {
  it("is false for an empty filter and true when any facet is set", () => {
    expect(isFiltering({})).toBe(false);
    expect(isFiltering({ query: "  " })).toBe(false);
    expect(isFiltering({ labels: ["x"] })).toBe(true);
    expect(isFiltering({ query: "ac" })).toBe(true);
  });
});

describe("pinnedUnfiled / recentUnfiled", () => {
  const convs = [
    conv({ title: "Pinned loose", pinned: true }),
    conv({ title: "Pinned project", pinned: true, project_id: "p1" }),
    conv({ title: "Recent loose", pinned: false }),
    conv({ title: "Recent project", pinned: false, project_id: "p1" }),
  ];

  it("excludes project conversations from both sections", () => {
    expect(pinnedUnfiled(convs).map((c) => c.title)).toEqual(["Pinned loose"]);
    expect(recentUnfiled(convs).map((c) => c.title)).toEqual(["Recent loose"]);
  });
});

describe("visibleConversationOrder", () => {
  // This is the shared source of truth for keyboard j/k navigation (chat-
  // experience) and the sidebar's rendered rows — the two must never drift.
  const all = [
    conv({ title: "Pinned loose", pinned: true }),
    conv({ title: "Project chat", pinned: true, project_id: "p1" }),
    conv({ title: "Recent loose", pinned: false }),
  ];

  it("returns pinned-unfiled then recent-unfiled when not filtering", () => {
    const filtered = filterConversations(all, {});
    expect(
      visibleConversationOrder({ all, filtered, filtering: false }).map((c) => c.title),
    ).toEqual(["Pinned loose", "Recent loose"]);
  });

  it("excludes project conversations from the unfiltered order", () => {
    const order = visibleConversationOrder({ all, filtered: all, filtering: false });
    expect(order.map((c) => c.title)).not.toContain("Project chat");
  });

  it("returns the filtered list verbatim (including project rows) when filtering", () => {
    const filtered = filterConversations(all, { query: "project" });
    expect(
      visibleConversationOrder({ all, filtered, filtering: true }).map((c) => c.title),
    ).toEqual(["Project chat"]);
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

  it("pinned projects sort first in place and always make the cut", () => {
    const projects = [
      proj("recent-a", 9),
      proj("recent-b", 8),
      { ...proj("pinned-old", 1), pinned: true },
    ];
    const groups = projectGroups([], projects, 2);
    // The pinned project beats both fresher ones for the top slot; the
    // remaining slot goes to the freshest unpinned.
    expect(groups.map((g) => g.project.id)).toEqual(["pinned-old", "recent-a"]);
  });

  it("search still reaches project conversations (filters are explicit)", () => {
    const all = [conv({ title: "quarterly report", project_id: "alpha" }), conv({ title: "misc" })];
    expect(filterConversations(all, { query: "quarterly" }).map((c) => c.title)).toEqual(["quarterly report"]);
  });
});

// A chat filed in a project the viewer can no longer see must still show
// SOMEWHERE. Pinned and Temporary both excluded every chat with a project_id,
// and the project section only iterates projects the viewer has — so losing
// access to a project (leaving the team, an admin move, the owner re-sharing
// it elsewhere) made the viewer's OWN chats vanish from the rail, findable
// only by search, with nothing on screen to explain it.
describe("chats whose project the viewer cannot see", () => {
  const convs = [
    { id: "a", title: "loose", pinned: false, updated_at: 3 },
    { id: "b", title: "in a visible project", pinned: false, updated_at: 2, project_id: "p1" },
    { id: "c", title: "in a lost project", pinned: false, updated_at: 1, project_id: "gone" },
    { id: "d", title: "pinned, lost project", pinned: true, updated_at: 1, project_id: "gone" },
  ];

  it("treats them as unfiled once the visible projects are known", () => {
    const known = new Set(["p1"]);
    expect(recentUnfiled(convs, known).map((c) => c.title)).toEqual([
      "loose",
      "in a lost project",
    ]);
    expect(pinnedUnfiled(convs, known).map((c) => c.title)).toEqual([
      "pinned, lost project",
    ]);
  });

  it("changes nothing when the caller doesn't pass a set", () => {
    // The rail withholds the set until the projects list has loaded, so a
    // first paint must not sweep every project chat into Temporary.
    expect(recentUnfiled(convs).map((c) => c.title)).toEqual(["loose"]);
    expect(pinnedUnfiled(convs).map((c) => c.title)).toEqual([]);
  });

  it("keeps j/k in step with what the sidebar renders", () => {
    const known = new Set(["p1"]);
    expect(
      visibleConversationOrder({
        all: convs,
        filtered: [],
        filtering: false,
        knownProjectIds: known,
      }).map((c) => c.title),
    ).toEqual(["pinned, lost project", "loose", "in a lost project"]);
  });
});
