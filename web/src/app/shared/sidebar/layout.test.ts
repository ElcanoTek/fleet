import { describe, expect, it } from "vitest";
import {
  BUILTIN_LAYOUT,
  PRESETS,
  addShortcut,
  cleanText,
  findShortcut,
  isBuiltinLayout,
  isKeptAtTop,
  isStartPage,
  isValidIcon,
  itemName,
  keepAtTop,
  moveItem,
  moveItemTo,
  normalizeLayout,
  presetMatching,
  removeFromTop,
  removeItem,
  setHome,
  setStartPage,
  setTeamChatsMode,
  teamChatsMode,
  topItems,
  updateItem,
  updateSection,
  type SidebarLayout,
} from "./layout";

const ids = (l: SidebarLayout) => l.items.map((i) => i.id);

describe("normalizeLayout", () => {
  it("turns nothing into the built-in layout: Patrols and Background tasks, then Pinned, Projects, Chats", () => {
    for (const raw of [null, undefined, "x", 3, {}]) {
      const l = normalizeLayout(raw);
      expect(ids(l)).toEqual(["chat", "patrols", "tasks"]);
      expect(l.sections.map((s) => s.id)).toEqual(["pinned", "projects", "chats", "team"]);
      expect(l.home).toBe("");
      expect(isBuiltinLayout(l)).toBe(true);
    }
  });

  it("keeps the order given, appends a missing builtin or section, and carries chat first", () => {
    const l = normalizeLayout({
      version: 1,
      items: [{ id: "tasks", kind: "builtin" }, { id: "s-1", kind: "chat", ref: "c1", title: "Board" }],
      sections: [{ id: "team" }],
    });
    // "chat" is in every layout and never a row: it rides first, so the top
    // of the sidebar is items[1:] in the order the person set.
    expect(ids(l)).toEqual(["chat", "tasks", "s-1", "patrols"]);
    expect(topItems(l).map((i) => i.id)).toEqual(["tasks", "s-1", "patrols"]);
    expect(l.sections.map((s) => s.id)).toEqual(["pinned", "team", "projects", "chats"]);
  });

  it("puts Pinned first in a layout saved before the section existed, and keeps it where a newer one put it", () => {
    const old = normalizeLayout({ sections: [{ id: "chats" }, { id: "projects" }, { id: "team" }] });
    expect(old.sections.map((s) => s.id)).toEqual(["pinned", "chats", "projects", "team"]);
    const moved = normalizeLayout({ sections: [{ id: "chats" }, { id: "pinned", hidden: true }] });
    expect(moved.sections.map((s) => s.id)).toEqual(["chats", "pinned", "projects", "team"]);
    expect(moved.sections[1].hidden).toBe(true);
  });

  it("carries over a sidebar customized before the redesign, with every choice intact", () => {
    // As the old client saved it: no "pinned" section, no row options, and
    // "chat" wherever the person left it.
    const saved = {
      version: 1,
      items: [
        { id: "tasks", kind: "builtin", label: "Jobs", icon: "bar-chart" },
        { id: "s-board", kind: "chat", ref: "c-1", title: "Campaign Watch", view: "dashboard", icon: "🚀" },
        { id: "chat", kind: "builtin", label: "Ask" },
        { id: "s-proj", kind: "project", ref: "p-1", title: "Playbook", hidden: true },
        { id: "patrols", kind: "builtin", hidden: true },
        { id: "s-help", kind: "page", ref: "help" },
      ],
      sections: [
        { id: "team", label: "Colleagues" },
        { id: "chats", label: "Conversations", collapsed: true },
        { id: "projects", hidden: true },
      ],
      home: "s-board",
      compact: true,
      separate_lists: true,
    };
    const l = normalizeLayout(saved);
    // The top of the sidebar: their items, in their order, with their names,
    // icons, views, and what they had hidden still hidden.
    expect(topItems(l)).toEqual([saved.items[0], saved.items[1], saved.items[5]]);
    expect(l.items.find((i) => i.id === "patrols")?.hidden).toBe(true);
    expect(l.items.find((i) => i.id === "s-proj")).toEqual(saved.items[3]);
    // Their lists, in their order, with their names, folds and hides. Pinned
    // is new: it goes first, and starts folded because their Chats does.
    expect(l.sections).toEqual([{ id: "pinned", collapsed: true }, ...saved.sections]);
    // Where fleet opens, density, and teammates' chats in a list of their own.
    expect(l.home).toBe("s-board");
    expect(isStartPage(l, "chat", "c-1")).toBe(true);
    expect(l.compact).toBe(true);
    expect(teamChatsMode(l)).toBe("own");
    // The new row options start as they were drawn before: statuses and dates on.
    expect(l.status_off).toBeUndefined();
    expect(l.dates_off).toBeUndefined();
    // Reading it again changes nothing, so it is never saved back altered.
    expect(normalizeLayout(l)).toEqual(l);
  });

  it("does not bring pinned chats back for someone who had hidden Chats", () => {
    // Pinned chats used to be drawn inside the Chats section: hiding Chats hid them.
    const l = normalizeLayout({ sections: [{ id: "projects" }, { id: "chats", hidden: true }, { id: "team" }] });
    expect(l.sections).toEqual([{ id: "pinned", hidden: true }, { id: "projects" }, { id: "chats", hidden: true }, { id: "team" }]);
    // Once Pinned is in the layout it is the person's own setting.
    const shown = normalizeLayout(updateSection(l, "pinned", { hidden: false }));
    expect(shown.sections[0]).toEqual({ id: "pinned" });
    expect(shown.sections.find((x) => x.id === "chats")?.hidden).toBe(true);
  });

  it("a Home that was the Chat page still opens the last chat", () => {
    const l = normalizeLayout({ home: "chat" });
    // No start page of its own: fleet opens the last chat, as "Chat" did.
    expect(topItems(l).some((i) => i.id === "chat")).toBe(false);
    expect(["", "chat"]).toContain(l.home);
  });

  it("keeps the two row options only when they are set", () => {
    const plain = normalizeLayout({ version: 1 });
    expect("status_off" in plain).toBe(false);
    expect("dates_off" in plain).toBe(false);
    const off = normalizeLayout({ version: 1, status_off: true, dates_off: true });
    expect(off.status_off).toBe(true);
    expect(off.dates_off).toBe(true);
    expect(normalizeLayout({ status_off: "yes", dates_off: 1 }).status_off).toBeUndefined();
    expect(normalizeLayout({ status_off: "yes", dates_off: 1 }).dates_off).toBeUndefined();
  });

  it("drops what it cannot render: bad kinds, ids, refs, duplicates and pages", () => {
    const l = normalizeLayout({
      items: [
        { id: "s-1", kind: "link", ref: "https://x" },
        { id: "BAD", kind: "chat", ref: "c1" },
        { id: "s-2", kind: "chat", ref: "../etc" },
        { id: "s-3", kind: "page", ref: "admin" },
        { id: "s-4", kind: "page", ref: "help" },
        { id: "s-4", kind: "chat", ref: "c2" },
        { id: "chat", kind: "chat", ref: "c3" },
      ],
    });
    expect(ids(l)).toEqual(["chat", "s-4", "patrols", "tasks"]);
    expect(l.items[1].kind).toBe("page");
  });

  it("cleans names and refuses a bad icon or a dashboard that is not a chat", () => {
    const l = normalizeLayout({
      items: [
        { id: "s-1", kind: "project", ref: "p1", label: "  Road\u0000map ​ ", icon: "<script>", view: "dashboard" },
        { id: "s-2", kind: "chat", ref: "c1", icon: "🚀", view: "dashboard" },
      ],
    });
    expect(l.items[1]).toEqual({ id: "s-1", kind: "project", ref: "p1", label: "Roadmap" });
    expect(l.items[2].icon).toBe("🚀");
    expect(l.items[2].view).toBe("dashboard");
  });

  it("clears a start page that names nothing usable", () => {
    expect(normalizeLayout({ home: "nope" }).home).toBe("");
    expect(normalizeLayout({ home: "patrols" }).home).toBe("");
    expect(normalizeLayout({ items: [{ id: "tasks", kind: "builtin", hidden: true }], home: "tasks" }).home).toBe("");
    expect(normalizeLayout({ home: "tasks" }).home).toBe("tasks");
  });

  it("lets a hidden shortcut be the start page: it does not have to be at the top", () => {
    const l = normalizeLayout({
      items: [{ id: "s-1", kind: "chat", ref: "c1", hidden: true }],
      home: "s-1",
    });
    expect(l.home).toBe("s-1");
    expect(topItems(l).map((i) => i.id)).toEqual(["patrols", "tasks"]);
  });
});

describe("edits", () => {
  it("adds a shortcut once; adding it again un-hides it and takes the new view", () => {
    let l = addShortcut(BUILTIN_LAYOUT, { kind: "chat", ref: "c1", title: "Board" });
    expect(l.items).toHaveLength(4);
    const id = l.items[3].id;
    l = updateItem(l, id, { hidden: true });
    l = addShortcut(l, { kind: "chat", ref: "c1", view: "dashboard" });
    expect(l.items).toHaveLength(4);
    expect(l.items[3]).toMatchObject({ id, view: "dashboard" });
    expect(l.items[3].hidden).toBeUndefined();
  });

  it("never removes a builtin, and removing the start page clears it", () => {
    let l = addShortcut(BUILTIN_LAYOUT, { kind: "project", ref: "p1", title: "Roadmap" });
    const id = l.items[3].id;
    l = setHome(l, id);
    expect(l.home).toBe(id);
    // The start page stays where the person put it.
    expect(ids(l)).toEqual(["chat", "patrols", "tasks", id]);
    expect(ids(removeItem(l, "chat"))).toContain("chat");
    const gone = removeItem(l, id);
    expect(gone.home).toBe("");
    expect(ids(gone)).toEqual(["chat", "patrols", "tasks"]);
  });

  it("setHome takes only an item in the layout, never Patrols, and \"\" clears it", () => {
    expect(setHome(BUILTIN_LAYOUT, "tasks").home).toBe("tasks");
    expect(setHome(BUILTIN_LAYOUT, "patrols").home).toBe("");
    expect(setHome(BUILTIN_LAYOUT, "s-nope").home).toBe("");
    expect(setHome(setHome(BUILTIN_LAYOUT, "tasks"), "").home).toBe("");
  });

  it("moves the top items by a step or to a place; chat stays first", () => {
    expect(ids(moveItem(BUILTIN_LAYOUT, "patrols", 1))).toEqual(["chat", "tasks", "patrols"]);
    expect(ids(moveItem(BUILTIN_LAYOUT, "tasks", -5))).toEqual(["chat", "tasks", "patrols"]);
    expect(ids(moveItemTo(BUILTIN_LAYOUT, "tasks", 0))).toEqual(["chat", "tasks", "patrols"]);
    expect(ids(moveItem(BUILTIN_LAYOUT, "tasks", 5))).toEqual(["chat", "patrols", "tasks"]);
  });

  it("patching a name to empty returns to the default name", () => {
    let l = updateItem(BUILTIN_LAYOUT, "chat", { label: "Ask" });
    expect(itemName(l.items[0])).toBe("Ask");
    l = updateItem(l, "chat", { label: "" });
    expect(l.items[0]).toEqual({ id: "chat", kind: "builtin" });
    expect(itemName(l.items[0])).toBe("Chat");
  });

  it("sections take a name, a fold and a hide", () => {
    const l = updateSection(BUILTIN_LAYOUT, "projects", { label: "Clients", hidden: true, collapsed: true });
    expect(l.sections.find((x) => x.id === "projects")).toEqual({ id: "projects", label: "Clients", hidden: true, collapsed: true });
  });
});

describe("the top of the sidebar", () => {
  it("shows Patrols, Background tasks and kept shortcuts, never chat, never what is hidden", () => {
    let l = addShortcut(BUILTIN_LAYOUT, { kind: "chat", ref: "c1", title: "Board", view: "dashboard" });
    expect(topItems(l).map((i) => i.ref ?? i.id)).toEqual(["patrols", "tasks", "c1"]);
    l = updateItem(l, "tasks", { hidden: true });
    expect(topItems(l).map((i) => i.ref ?? i.id)).toEqual(["patrols", "c1"]);
  });

  it("Move to the top keeps a target there; Remove from the top takes it out", () => {
    let l = keepAtTop(BUILTIN_LAYOUT, { kind: "project", ref: "p1", title: "Roadmap" });
    expect(isKeptAtTop(l, "project", "p1")).toBe(true);
    l = removeFromTop(l, "project", "p1");
    expect(isKeptAtTop(l, "project", "p1")).toBe(false);
    expect(findShortcut(l, "project", "p1")).toBeUndefined();
  });

  it("removing the start page from the top keeps it as the start page, off the top", () => {
    let l = keepAtTop(BUILTIN_LAYOUT, { kind: "chat", ref: "c1", title: "Board" });
    l = setStartPage(l, { kind: "chat", ref: "c1" });
    l = removeFromTop(l, "chat", "c1");
    expect(isKeptAtTop(l, "chat", "c1")).toBe(false);
    expect(isStartPage(l, "chat", "c1")).toBe(true);
    // Moving it back to the top shows it again, still the start page.
    l = keepAtTop(l, { kind: "chat", ref: "c1" });
    expect(isKeptAtTop(l, "chat", "c1")).toBe(true);
    expect(isStartPage(l, "chat", "c1")).toBe(true);
  });
});

describe("the start page", () => {
  it("any chat, dashboard or project can be it without moving to the top", () => {
    const l = setStartPage(BUILTIN_LAYOUT, { kind: "project", ref: "p1", title: "Roadmap" });
    expect(isStartPage(l, "project", "p1")).toBe(true);
    expect(isKeptAtTop(l, "project", "p1")).toBe(false);
    expect(topItems(l).map((i) => i.id)).toEqual(["patrols", "tasks"]);
  });

  it("choosing another drops the one kept only to be the start page", () => {
    let l = setStartPage(BUILTIN_LAYOUT, { kind: "project", ref: "p1" });
    l = setStartPage(l, { kind: "chat", ref: "c1", view: "dashboard" });
    expect(isStartPage(l, "chat", "c1")).toBe(true);
    expect(findShortcut(l, "project", "p1")).toBeUndefined();
    expect(findShortcut(l, "chat", "c1")?.view).toBe("dashboard");
    // Back to the last chat: nothing hidden is left behind.
    l = setHome(l, "");
    expect(l.home).toBe("");
    expect(ids(l)).toEqual(["chat", "patrols", "tasks"]);
  });

  it("a top item made the start page stays at the top when another takes over", () => {
    let l = keepAtTop(BUILTIN_LAYOUT, { kind: "chat", ref: "c1" });
    l = setStartPage(l, { kind: "chat", ref: "c1" });
    l = setStartPage(l, { kind: "project", ref: "p1" });
    expect(isKeptAtTop(l, "chat", "c1")).toBe(true);
    expect(isStartPage(l, "project", "p1")).toBe(true);
  });
});

describe("teammates' shared chats", () => {
  it("are in Chats by default, in a list of their own, or hidden", () => {
    expect(teamChatsMode(BUILTIN_LAYOUT)).toBe("chats");
    const own = setTeamChatsMode(BUILTIN_LAYOUT, "own");
    expect(teamChatsMode(own)).toBe("own");
    expect(own.separate_lists).toBe(true);
    const hidden = setTeamChatsMode(own, "hidden");
    expect(teamChatsMode(hidden)).toBe("hidden");
    expect(hidden.sections.find((x) => x.id === "team")?.hidden).toBe(true);
    // Hiding keeps which list they were in, for when they are shown again.
    expect(hidden.separate_lists).toBe(true);
    const back = setTeamChatsMode(hidden, "chats");
    expect(teamChatsMode(back)).toBe("chats");
    expect(back.separate_lists).toBeUndefined();
    expect(back.sections.find((x) => x.id === "team")?.hidden).toBeUndefined();
  });
});

describe("presets", () => {
  it("each keeps the person's shortcuts and is recognised afterwards", () => {
    const base = addShortcut(BUILTIN_LAYOUT, { kind: "chat", ref: "c1", title: "Board", view: "dashboard" });
    for (const p of PRESETS) {
      const applied = p.apply(base);
      expect(applied.items.some((i) => i.ref === "c1")).toBe(true);
      expect(presetMatching(applied)).toBe(p.id);
    }
  });

  it("one list for all chats is the default; separate lists survive normalizing", () => {
    // A layout that says nothing (the built-in one, and every layout saved
    // before the choice existed) is one list.
    expect("separate_lists" in normalizeLayout({ version: 1 })).toBe(false);
    expect(BUILTIN_LAYOUT.separate_lists).toBeUndefined();
    expect(normalizeLayout({ version: 1, separate_lists: true }).separate_lists).toBe(true);
    expect(normalizeLayout({ version: 1, separate_lists: "yes" }).separate_lists).toBeUndefined();
  });

  it("a preset arranges the lists: only Team room gives teammates' chats a list of their own", () => {
    const mine = { ...BUILTIN_LAYOUT, separate_lists: true };
    for (const preset of PRESETS) {
      expect(teamChatsMode(preset.apply(mine))).toBe(
        preset.id === "team" ? "own" : preset.id === "chat" ? "hidden" : "chats",
      );
    }
  });

  it("Classic on the built-in layout is the built-in layout", () => {
    expect(PRESETS[0].apply(BUILTIN_LAYOUT)).toEqual(normalizeLayout(BUILTIN_LAYOUT));
  });

  it("pins and the start page stay: a preset moves neither, and restyles nothing", () => {
    let base = addShortcut(BUILTIN_LAYOUT, { kind: "chat", ref: "c1", view: "dashboard" });
    base = setStartPage(base, { kind: "project", ref: "p1" });
    base = { ...base, compact: true, status_off: true, dates_off: true };
    for (const preset of PRESETS) {
      const l = preset.apply(base);
      expect(isStartPage(l, "project", "p1")).toBe(true);
      expect(isKeptAtTop(l, "chat", "c1")).toBe(true);
      expect(l.compact).toBe(true);
      expect(l.status_off).toBe(true);
      expect(l.dates_off).toBe(true);
    }
  });

  it("a start page the preset hides is cleared, not left pointing at a hidden destination", () => {
    const base = setHome(BUILTIN_LAYOUT, "tasks");
    expect(PRESETS.find((p) => p.id === "chat")!.apply(base).home).toBe("");
    expect(PRESETS.find((p) => p.id === "automation")!.apply(base).home).toBe("tasks");
  });

  it("Just chat hides Patrols, Background tasks, Projects and teammates' chats", () => {
    const l = PRESETS.find((p) => p.id === "chat")!.apply(BUILTIN_LAYOUT);
    expect(topItems(l)).toEqual([]);
    expect(l.sections.filter((x) => x.hidden).map((x) => x.id).sort()).toEqual(["projects", "team"]);
  });
});

describe("text and icons", () => {
  it("cleanText strips controls and cuts to length", () => {
    expect(cleanText("a\u0007b c", 10)).toBe("abc");
    expect(cleanText("x".repeat(80), 60)).toHaveLength(60);
  });

  it("an icon is a sprite choice or a short emoji", () => {
    expect(isValidIcon("bar-chart")).toBe(true);
    expect(isValidIcon("🛰️")).toBe(true);
    expect(isValidIcon("👩‍💻")).toBe(true);
    expect(isValidIcon("not-an-icon")).toBe(false);
    expect(isValidIcon("a")).toBe(false);
    expect(isValidIcon("🚀🚀🚀🚀🚀🚀🚀🚀🚀")).toBe(false);
    // The server's rules: no separators or stray format characters.
    expect(isValidIcon("🚀\u00a0🚀")).toBe(false); // no-break space
    expect(isValidIcon("🚀\u200b")).toBe(false); // zero-width space
    expect(isValidIcon("\u202e🚀")).toBe(false); // bidi override
    expect(isValidIcon("🏴\u{e0067}\u{e0062}\u{e0073}\u{e0063}\u{e0074}\u{e007f}")).toBe(true); // tag flag
    expect(isValidIcon("🧑\u200d🚀")).toBe(true); // joined with ZWJ
  });
});
