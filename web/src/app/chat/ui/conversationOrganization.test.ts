import { describe, expect, it } from "vitest";
import {
  MAX_LABELS,
  addLabel,
  canAddLabel,
  deriveLabels,
  filterConversations,
  foldSearchText,
  isFiltering,
  matchesSearch,
  searchMatchRanges,
  searchTokens,
  applyChatOrder,
  movePinned,
  normalizeLabel,
  pinnedGroup,
  dragTargetIndex,
  pinnedUnfiled,
  projectOrderScope,
  sortRecent,
  projectGroups,
  railProjectEmptyState,
  recentUnfiled,
  removeLabel,
  visibleConversationOrder,
  type OrganizableConversation,
  pinnedEntries,
  pinnedProjectToken,
  dateGroupOf,
  groupByDate,
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

describe("search matching", () => {
  it("matches every word in any order, ignoring case and accents", () => {
    const convs = [conv({ title: "Acme renewal" }), conv({ title: "Résumé review" }), conv({ title: "Renewal dates" })];
    expect(filterConversations(convs, { query: "renewal ACME" }).map((c) => c.title)).toEqual(["Acme renewal"]);
    expect(filterConversations(convs, { query: "resume" }).map((c) => c.title)).toEqual(["Résumé review"]);
    expect(filterConversations(convs, { query: "acme dates" })).toEqual([]);
  });

  it("a word may be found in any of the texts given", () => {
    const tokens = searchTokens("  Pacing   dana ");
    expect(tokens).toEqual(["pacing", "dana"]);
    expect(matchesSearch(tokens, "Omnicom pacing", "dana@example.com")).toBe(true);
    expect(matchesSearch(tokens, "Omnicom pacing", undefined)).toBe(false);
    expect(matchesSearch([], "anything")).toBe(true);
  });

  it("marks the matched ranges in the original text, merged and in order", () => {
    expect(searchMatchRanges("Acme renewal", searchTokens("renew ac"))).toEqual([
      [0, 2],
      [5, 10],
    ]);
    // Accented text: the range covers the accented letters themselves.
    expect(searchMatchRanges("Résumé review", searchTokens("resume"))).toEqual([[0, 6]]);
    // Overlapping tokens merge into one range.
    expect(searchMatchRanges("pacing", searchTokens("pac acin"))).toEqual([[0, 5]]);
    expect(searchMatchRanges("pacing", [])).toEqual([]);
    expect(foldSearchText("ÀÉÎ")).toBe("aei");
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

  it("projectGroups returns every project by default, pinned first then most recent", () => {
    // The rail once cut this to five; a sixth project and its chats then
    // vanished from the rail with no way to reach them.
    const projects = Array.from({ length: 12 }, (_, i) => proj(`p${i}`, i));
    const groups = projectGroups([conv({ title: "deep", project_id: "p0" })], projects);
    expect(groups).toHaveLength(12);
    expect(groups[0].project.id).toBe("p11");
    expect(groups[11].project.id).toBe("p0");
    expect(groups[11].chats.map((c) => c.title)).toEqual(["deep"]);
  });

  it("projectGroups returns the top N projects by recent update when capped, each with its chats in input order", () => {
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

// The rail lists only the viewer's OWN chats (ADR-0057 keeps the project home
// as the single discovery surface for team-shared ones), so "No chats yet" was
// a false statement to a teammate whose colleagues had filed two shared chats
// into the project: the rail called it empty while its home listed content.
describe("railProjectEmptyState", () => {
  it("keeps the plain filing copy for a personal project", () => {
    // No count consulted: team sharing is refused outside a team-shared
    // project and every way of losing that home clears the flag, so zero is
    // certain here rather than unknown.
    expect(railProjectEmptyState({ teamShared: false })).toEqual({
      kind: "no-chats",
    });
    expect(
      railProjectEmptyState({ teamShared: false, teamSharedChatCount: 0 }),
    ).toEqual({ kind: "no-chats" });
  });

  it("quotes the team's chats when the project has some", () => {
    expect(
      railProjectEmptyState({ teamShared: true, teamSharedChatCount: 2 }),
    ).toEqual({ kind: "team-has-chats", count: 2 });
    expect(
      railProjectEmptyState({ teamShared: true, teamSharedChatCount: 1 }),
    ).toEqual({ kind: "team-has-chats", count: 1 });
  });

  it("keeps the plain filing copy when a team-shared project really is empty", () => {
    expect(
      railProjectEmptyState({ teamShared: true, teamSharedChatCount: 0 }),
    ).toEqual({ kind: "no-chats" });
  });

  it("asserts no number when the count is unknown", () => {
    // Never invent a count — and never fall back to "empty", which is the one
    // claim that was wrong.
    expect(railProjectEmptyState({ teamShared: true })).toEqual({
      kind: "team-count-unknown",
    });
  });
});

describe("chat order (docs/CHAT-ORDER.md)", () => {
  const p = (id: string, pinned = true) => conv({ id, title: id, pinned });

  it("applies the saved order, with fresh pins above it and stale ids ignored", () => {
    const list = [p("new1"), p("a"), p("b"), p("c"), p("new2")];
    expect(applyChatOrder(list, ["c", "gone", "a", "b"]).map((c) => c.id)).toEqual(["new1", "new2", "c", "a", "b"]);
  });

  it("is the order pinnedUnfiled and visibleConversationOrder use", () => {
    const all = [p("a"), p("b"), p("loose", false)];
    expect(pinnedUnfiled(all, undefined, ["b", "a"]).map((c) => c.id)).toEqual(["b", "a"]);
    expect(pinnedUnfiled(all).map((c) => c.id)).toEqual(["a", "b"]);
    expect(
      visibleConversationOrder({ all, filtered: [], filtering: false, pinOrder: ["b", "a"] }).map((c) => c.id),
    ).toEqual(["b", "a", "loose"]);
  });

  it("moves one id, clamped to the list", () => {
    expect(movePinned(["a", "b", "c"], 0, 2)).toEqual(["b", "c", "a"]);
    expect(movePinned(["a", "b", "c"], 2, 0)).toEqual(["c", "a", "b"]);
    expect(movePinned(["a", "b", "c"], 1, 9)).toEqual(["a", "c", "b"]);
    expect(movePinned(["a", "b", "c"], 0, -1)).toEqual(["a", "b", "c"]);
    expect(movePinned(["a", "b", "c"], 5, 0)).toEqual(["a", "b", "c"]);
  });

  it("orders each project's chats by that project's own saved order", () => {
    const chats = [
      conv({ id: "x", title: "x", project_id: "p1" }),
      conv({ id: "y", title: "y", project_id: "p1" }),
      conv({ id: "z", title: "z", project_id: "p2" }),
    ];
    const projects = [
      { id: "p1", name: "One", updated_at: 2 },
      { id: "p2", name: "Two", updated_at: 1 },
    ];
    const groups = projectGroups(chats, projects, Number.POSITIVE_INFINITY, { [projectOrderScope("p1")]: ["y", "x"] });
    expect(groups.map((g) => g.chats.map((c) => c.id))).toEqual([["y", "x"], ["z"]]);
    expect(projectGroups(chats, projects)[0].chats.map((c) => c.id)).toEqual(["x", "y"]);
  });
});

describe("pinnedGroup (own and teammates' pinned chats in one group)", () => {
  const own = [
    { id: "a", updated_at: 30 },
    { id: "b", updated_at: 10 },
  ];
  const shared = [
    { id: "t-old", updated_at: 5 },
    { id: "t-new", updated_at: 20 },
  ];

  it("interleaves by recent activity before any order is saved", () => {
    expect(pinnedGroup(own, shared).map((c) => c.id)).toEqual(["a", "t-new", "b", "t-old"]);
    expect(pinnedGroup(own, []).map((c) => c.id)).toEqual(["a", "b"]);
    expect(pinnedGroup([], shared).map((c) => c.id)).toEqual(["t-new", "t-old"]);
  });

  it("follows the saved order, with chats it does not name on top", () => {
    expect(pinnedGroup(own, shared, ["t-old", "b", "a"]).map((c) => c.id)).toEqual(["t-new", "t-old", "b", "a"]);
  });

  it("never reorders the person's own chats among themselves, so j/k matches the rows", () => {
    // Own chats arrive pinned-first from the server, not strictly by time.
    const unsorted = [
      { id: "x", updated_at: 1 },
      { id: "y", updated_at: 99 },
    ];
    for (const order of [undefined, ["y"], ["t-new", "x"]]) {
      const group = pinnedGroup(unsorted, shared, order).filter((c) => !c.id.startsWith("t-"));
      const walk = pinnedUnfiled(
        unsorted.map((c) => ({ ...c, title: c.id, pinned: true })),
        undefined,
        order,
      );
      expect(group.map((c) => c.id)).toEqual(walk.map((c) => c.id));
    }
  });
});

describe("dragTargetIndex (the reorder list's pointer drag)", () => {
  it("lands on the nearest slot and never leaves the list", () => {
    expect(dragTargetIndex(2, 0, 40, 5)).toBe(2);
    expect(dragTargetIndex(2, 19, 40, 5)).toBe(2);
    expect(dragTargetIndex(2, 21, 40, 5)).toBe(3);
    expect(dragTargetIndex(2, -61, 40, 5)).toBe(0);
    expect(dragTargetIndex(2, -4000, 40, 5)).toBe(0);
    expect(dragTargetIndex(2, 4000, 40, 5)).toBe(4);
  });

  it("stays put when the rows have no height to measure", () => {
    expect(dragTargetIndex(1, 500, 0, 3)).toBe(1);
    expect(dragTargetIndex(1, Number.NaN, 40, 3)).toBe(1);
    expect(dragTargetIndex(0, 10, 40, 0)).toBe(0);
  });
});

describe("sortRecent (Temporary)", () => {
  const t = (id: string, title: string, updated_at: number, created_at?: number) =>
    conv({ id, title, updated_at, created_at });
  // created_at is never after updated_at; "d" comes from a server that sends no created_at.
  const list = [t("a", "banana", 30, 5), t("b", "Apple", 20, 15), t("c", "cherry 10", 10, 8), t("d", "cherry 9", 10)];

  it("defaults to most recent activity, stable on ties", () => {
    expect(sortRecent(list).map((c) => c.id)).toEqual(["a", "b", "c", "d"]);
  });
  it("sorts newest-started first, using activity time when created_at is missing", () => {
    expect(sortRecent(list, "created").map((c) => c.id)).toEqual(["b", "d", "c", "a"]);
  });
  it("sorts by title, ignoring case and reading numbers as numbers", () => {
    expect(sortRecent(list, "title").map((c) => c.id)).toEqual(["b", "a", "d", "c"]);
  });
  it("is the order visibleConversationOrder walks", () => {
    expect(
      visibleConversationOrder({ all: list, filtered: [], filtering: false, recentSort: "title" }).map((c) => c.id),
    ).toEqual(["b", "a", "d", "c"]);
  });
});

describe("pinnedEntries (the Pinned section)", () => {
  const chats = [{ id: "c1" }, { id: "c2" }];
  const projects = [{ id: "p1" }, { id: "p2" }];

  it("lists pinned projects, then pinned chats, until the person sets an order", () => {
    expect(pinnedEntries(chats, projects).map((e) => e.id)).toEqual(["project:p1", "project:p2", "c1", "c2"]);
    expect(pinnedEntries(chats, projects, []).map((e) => e.id)).toEqual(["project:p1", "project:p2", "c1", "c2"]);
  });

  it("mixes projects and chats in the saved order, a project named by its token", () => {
    const order = ["c2", pinnedProjectToken("p2"), "c1", pinnedProjectToken("p1")];
    const rows = pinnedEntries(chats, projects, order);
    expect(rows.map((e) => e.id)).toEqual(order);
    expect(rows.map((e) => e.kind)).toEqual(["chat", "project", "chat", "project"]);
  });

  it("puts what the order does not name yet above what it does, and ignores what is gone", () => {
    const rows = pinnedEntries(chats, projects, ["c1", "gone", pinnedProjectToken("p1")]);
    expect(rows.map((e) => e.id)).toEqual(["project:p2", "c2", "c1", "project:p1"]);
  });
});

describe("the Chats list's date groups", () => {
  // Local noon, so the groups are the reader's own days whatever the zone.
  const now = new Date(2026, 10, 19, 12, 0, 0);
  const at = (daysAgo: number, hour = 9) => new Date(2026, 10, 19 - daysAgo, hour, 0, 0).getTime() / 1000;

  it("places a moment by the reader's calendar days", () => {
    expect(dateGroupOf(at(0, 0), now)).toBe("today");
    expect(dateGroupOf(at(0, 11), now)).toBe("today");
    expect(dateGroupOf(at(1, 23), now)).toBe("yesterday");
    expect(dateGroupOf(at(2), now)).toBe("week");
    expect(dateGroupOf(at(6, 0), now)).toBe("week");
    expect(dateGroupOf(at(7, 23), now)).toBe("older");
    expect(dateGroupOf(0, now)).toBe("older");
  });

  it("splits a sorted list into runs, by last activity or by when the chat was started", () => {
    const list = [
      { id: "a", updated_at: at(0), created_at: at(9) },
      { id: "b", updated_at: at(1), created_at: at(1) },
      { id: "c", updated_at: at(3), created_at: at(3) },
      { id: "d", updated_at: at(40) },
    ];
    expect(groupByDate(list, "activity", now)?.map((g) => [g.id, g.chats.map((c) => c.id)])).toEqual([
      ["today", ["a"]],
      ["yesterday", ["b"]],
      ["week", ["c"]],
      ["older", ["d"]],
    ]);
    // By start date "a" is an old chat, and one without created_at goes by its activity.
    const byCreated = [list[1], list[2], list[0], list[3]];
    expect(groupByDate(byCreated, "created", now)?.map((g) => [g.id, g.chats.map((c) => c.id)])).toEqual([
      ["yesterday", ["b"]],
      ["week", ["c"]],
      ["older", ["a", "d"]],
    ]);
  });

  it("an A–Z list has no dates to group by", () => {
    expect(groupByDate([{ id: "a", updated_at: at(0) }], "title", now)).toBeNull();
    expect(groupByDate([], "activity", now)).toEqual([]);
  });
});
