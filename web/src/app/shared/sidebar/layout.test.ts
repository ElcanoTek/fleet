import { describe, expect, it } from "vitest";
import {
  BUILTIN_LAYOUT,
  PRESETS,
  addShortcut,
  cleanText,
  isBuiltinLayout,
  isValidIcon,
  itemName,
  moveItem,
  moveItemTo,
  normalizeLayout,
  presetMatching,
  removeItem,
  setHome,
  updateItem,
  updateSection,
  type SidebarLayout,
} from "./layout";

const ids = (l: SidebarLayout) => l.items.map((i) => i.id);

describe("normalizeLayout", () => {
  it("turns nothing into the built-in layout, which is the rail as it always was", () => {
    for (const raw of [null, undefined, "x", 3, {}]) {
      const l = normalizeLayout(raw);
      expect(ids(l)).toEqual(["chat", "patrols", "tasks"]);
      expect(l.sections.map((s) => s.id)).toEqual(["projects", "chats", "team"]);
      expect(l.home).toBe("");
      expect(isBuiltinLayout(l)).toBe(true);
    }
  });

  it("keeps the order given and appends a missing builtin or section", () => {
    const l = normalizeLayout({
      version: 1,
      items: [{ id: "tasks", kind: "builtin" }, { id: "s-1", kind: "chat", ref: "c1", title: "Board" }],
      sections: [{ id: "team" }],
    });
    expect(ids(l)).toEqual(["tasks", "s-1", "chat", "patrols"]);
    expect(l.sections.map((s) => s.id)).toEqual(["team", "projects", "chats"]);
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
    expect(ids(l)).toEqual(["s-4", "chat", "patrols", "tasks"]);
    expect(l.items[0].kind).toBe("page");
  });

  it("cleans names and refuses a bad icon or a dashboard that is not a chat", () => {
    const l = normalizeLayout({
      items: [
        { id: "s-1", kind: "project", ref: "p1", label: "  Road\u0000map ​ ", icon: "<script>", view: "dashboard" },
        { id: "s-2", kind: "chat", ref: "c1", icon: "🚀", view: "dashboard" },
      ],
    });
    expect(l.items[0]).toEqual({ id: "s-1", kind: "project", ref: "p1", label: "Roadmap" });
    expect(l.items[1].icon).toBe("🚀");
    expect(l.items[1].view).toBe("dashboard");
  });

  it("clears a Home that names nothing usable", () => {
    expect(normalizeLayout({ home: "nope" }).home).toBe("");
    expect(normalizeLayout({ home: "patrols" }).home).toBe("");
    expect(normalizeLayout({ items: [{ id: "tasks", kind: "builtin", hidden: true }], home: "tasks" }).home).toBe("");
    expect(normalizeLayout({ home: "tasks" }).home).toBe("tasks");
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

  it("never removes a builtin, and removing Home clears it", () => {
    let l = addShortcut(BUILTIN_LAYOUT, { kind: "project", ref: "p1", title: "Roadmap" });
    const id = l.items[3].id;
    l = setHome(l, id);
    expect(l.home).toBe(id);
    expect(ids(l)[0]).toBe(id); // Home floats to the top
    expect(ids(removeItem(l, "chat"))).toContain("chat");
    const gone = removeItem(l, id);
    expect(gone.home).toBe("");
    expect(ids(gone)).toEqual(["chat", "patrols", "tasks"]);
  });

  it("moves by a step or to a place, clamped to the list", () => {
    expect(ids(moveItem(BUILTIN_LAYOUT, "chat", 1))).toEqual(["patrols", "chat", "tasks"]);
    expect(ids(moveItem(BUILTIN_LAYOUT, "chat", -5))).toEqual(["chat", "patrols", "tasks"]);
    expect(ids(moveItemTo(BUILTIN_LAYOUT, "tasks", 0))).toEqual(["tasks", "chat", "patrols"]);
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
    expect(l.sections[0]).toEqual({ id: "projects", label: "Clients", hidden: true, collapsed: true });
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

  it("one list for all chats is the default; separate lists survive normalizing and every preset", () => {
    // A layout that says nothing (the built-in one, and every layout saved
    // before the choice existed) is one list.
    expect("separate_lists" in normalizeLayout({ version: 1 })).toBe(false);
    expect(BUILTIN_LAYOUT.separate_lists).toBeUndefined();
    expect(normalizeLayout({ version: 1, separate_lists: true }).separate_lists).toBe(true);
    expect(normalizeLayout({ version: 1, separate_lists: "yes" }).separate_lists).toBeUndefined();
    const mine = { ...BUILTIN_LAYOUT, separate_lists: true };
    for (const preset of PRESETS) expect(preset.apply(mine).separate_lists).toBe(true);
    for (const preset of PRESETS) expect(preset.apply(BUILTIN_LAYOUT).separate_lists).toBeUndefined();
  });

  it("Classic on the built-in layout is the built-in layout", () => {
    expect(PRESETS[0].apply(BUILTIN_LAYOUT)).toEqual(normalizeLayout(BUILTIN_LAYOUT));
  });

  it("Dashboards first makes the first dashboard Home", () => {
    const base = addShortcut(BUILTIN_LAYOUT, { kind: "chat", ref: "c1", view: "dashboard" });
    const l = PRESETS.find((p) => p.id === "dashboards")!.apply(base);
    expect(l.items.find((i) => i.id === l.home)?.ref).toBe("c1");
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
