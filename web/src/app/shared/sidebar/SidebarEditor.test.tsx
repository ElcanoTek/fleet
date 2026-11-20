import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useEffect, useState } from "react";
import { SidebarEditor, startPageOptions } from "./SidebarEditor";
import {
  BUILTIN_LAYOUT,
  addShortcut,
  isStartPage,
  presetMatching,
  teamChatsMode,
  topItems,
  type SidebarLayout,
} from "./layout";
import { SidebarTargetsContext, type SidebarPins, type SidebarTargets } from "./targets";

// The full Customize panel's body (docs/SIDEBAR.md): where fleet opens, a
// layout to start from, the top of the sidebar, what is pinned, the lists,
// and how dense the rows are.

vi.mock("./pickerData", () => ({
  loadPickerEntries: () =>
    Promise.resolve([
      { group: "chats", kind: "chat", ref: "c1", title: "Campaign Watch", shared: false },
      { group: "chats", kind: "chat", ref: "c5", title: "Budget notes", shared: false },
      { group: "chats", kind: "chat", ref: "c6", title: "Old vendor comparison", detail: "Archived", shared: false },
      { group: "projects", kind: "project", ref: "p2", title: "Reklaim", shared: false },
      { group: "pages", kind: "page", ref: "help", shared: true },
    ]),
}));

const pinsFor = (over: Partial<SidebarPins> = {}): SidebarPins => ({
  entries: [
    { id: "project:p1", kind: "project", ref: "p1", title: "TWC emails", canUnpin: true },
    { id: "c9", kind: "chat", ref: "c9", title: "FW26 Campaign", canUnpin: true },
    { id: "project:p7", kind: "project", ref: "p7", title: "Team playbook", canUnpin: false },
  ],
  reorder: vi.fn(),
  unpin: vi.fn(),
  pin: vi.fn(() => true),
  ...over,
});

let latest: SidebarLayout = BUILTIN_LAYOUT;

function Editor({ start, pins, mode = "user" }: { start: SidebarLayout; pins?: SidebarPins; mode?: "user" | "workspace" }) {
  const [layout, setLayout] = useState(start);
  // What the editor last handed up, for the assertions.
  useEffect(() => {
    latest = layout;
  }, [layout]);
  const targets: SidebarTargets = {
    activeConversationId: null,
    activeProjectId: null,
    pins,
    titleFor: () => undefined,
    availability: () => "yes",
    open: () => false,
  };
  return (
    <SidebarTargetsContext.Provider value={targets}>
      <SidebarEditor layout={layout} onChange={setLayout} mode={mode} />
    </SidebarTargetsContext.Provider>
  );
}

const section = (heading: string) => screen.getByRole("heading", { name: heading }).closest("section") as HTMLElement;

beforeEach(() => {
  latest = BUILTIN_LAYOUT;
});
afterEach(() => cleanup());

describe("Customize — Fleet opens to", () => {
  it("is the last chat until a start page is chosen, and says which it is", async () => {
    render(<Editor start={addShortcut(BUILTIN_LAYOUT, { kind: "chat", ref: "c1", title: "Saved name", view: "dashboard" })} />);
    const select = screen.getByTestId("sidebar-home-select") as HTMLSelectElement;
    expect(select.selectedOptions[0].textContent).toBe("The last chat you had open");
    expect(screen.getByTestId("sidebar-home-summary")).toHaveTextContent("No start page set: fleet opens the last chat you had open.");
    // Names come from the live lists once they are read.
    await waitFor(() => expect(within(select).getByRole("option", { name: "Campaign Watch · Dashboard" })).toBeInTheDocument());
    fireEvent.change(select, { target: { value: latest.items.find((i) => i.ref === "c1")!.id } });
    expect(isStartPage(latest, "chat", "c1")).toBe(true);
    expect(screen.getByTestId("sidebar-home-summary")).toHaveTextContent("Opening fleet takes you to Campaign Watch.");
    fireEvent.change(select, { target: { value: "" } });
    expect(latest.home).toBe("");
  });

  it("offers what is pinned, and never Patrols or the chat page itself", () => {
    const options = startPageOptions(BUILTIN_LAYOUT, () => undefined, new Set(), pinsFor());
    expect(options.map((o) => o.label)).toEqual(["The last chat you had open", "Background tasks", "TWC emails", "FW26 Campaign", "Team playbook"]);
    expect(options.filter((o) => o.selected).map((o) => o.label)).toEqual(["The last chat you had open"]);
    const picked = options[3].apply(BUILTIN_LAYOUT);
    expect(isStartPage(picked, "chat", "c9")).toBe(true);
    expect(topItems(picked).map((i) => i.id)).toEqual(["patrols", "tasks"]);
  });

  it("lists a pinned item that is also a top item once", () => {
    const layout = addShortcut(BUILTIN_LAYOUT, { kind: "chat", ref: "c9", title: "FW26 Campaign" });
    const labels = startPageOptions(layout, () => undefined, new Set(), pinsFor()).map((o) => o.label);
    expect(labels.filter((l) => l === "FW26 Campaign")).toHaveLength(1);
  });
});

describe("Customize — Start from", () => {
  it("marks the layout in use, and choosing another rearranges without touching pins or the start page", () => {
    render(<Editor start={BUILTIN_LAYOUT} />);
    expect(screen.getByTestId("sidebar-preset-classic")).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText("Pins and start page stay")).toBeInTheDocument();
    fireEvent.click(screen.getByTestId("sidebar-preset-chat"));
    expect(presetMatching(latest)).toBe("chat");
    expect(topItems(latest)).toEqual([]);
    expect(screen.getByTestId("sidebar-preset-chat")).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByTestId("sidebar-preset-classic")).toHaveAttribute("aria-pressed", "false");
  });
});

describe("Customize — Top of the sidebar", () => {
  it("lists Patrols, Background tasks and kept shortcuts; the chat page is never a row", () => {
    render(<Editor start={addShortcut(BUILTIN_LAYOUT, { kind: "chat", ref: "c1", title: "Campaign Watch", view: "dashboard" })} />);
    const rows = within(section("Top of the sidebar")).getAllByTestId("sidebar-item-name");
    expect(rows.map((r) => r.getAttribute("placeholder"))).toEqual(["Patrols", "Background tasks", "Campaign Watch"]);
  });

  it("moves a row and says where it went, counting only the rows shown", () => {
    render(<Editor start={BUILTIN_LAYOUT} />);
    const top = section("Top of the sidebar");
    expect(within(top).getByRole("button", { name: "Move Patrols up" })).toBeDisabled();
    expect(within(top).getByRole("button", { name: "Move Background tasks down" })).toBeDisabled();
    fireEvent.click(within(top).getByRole("button", { name: "Move Patrols down" }));
    expect(topItems(latest).map((i) => i.id)).toEqual(["tasks", "patrols"]);
    expect(top).toHaveTextContent("Patrols moved to position 2 of 2.");
  });

  it("hides a row with its switch, and Patrols cannot be the start page", () => {
    render(<Editor start={BUILTIN_LAYOUT} />);
    const top = section("Top of the sidebar");
    fireEvent.click(within(top).getByRole("switch", { name: "Show Background tasks in the sidebar" }));
    expect(topItems(latest).map((i) => i.id)).toEqual(["patrols"]);
    expect(within(top).getByRole("button", { name: "Make Patrols your start page" })).toBeDisabled();
  });
});

describe("Customize — Pinned", () => {
  it("lists what is pinned and reorders it with the arrows", () => {
    const pins = pinsFor();
    render(<Editor start={BUILTIN_LAYOUT} pins={pins} />);
    const pinned = section("Pinned");
    expect(within(pinned).getAllByRole("listitem").map((li) => li.getAttribute("data-pin-id"))).toEqual(["project:p1", "c9", "project:p7"]);
    fireEvent.click(within(pinned).getByRole("button", { name: "Move FW26 Campaign up" }));
    expect(pins.reorder).toHaveBeenCalledWith(["c9", "project:p1", "project:p7"]);
    expect(pinned).toHaveTextContent("FW26 Campaign moved to position 1 of 3.");
    expect(within(pinned).getByRole("button", { name: "Move TWC emails up" })).toBeDisabled();
    expect(within(pinned).getByRole("button", { name: "Move Team playbook down" })).toBeDisabled();
  });

  it("unpins, except a project whose pin is its owner's", () => {
    const pins = pinsFor();
    render(<Editor start={BUILTIN_LAYOUT} pins={pins} />);
    const pinned = section("Pinned");
    fireEvent.click(within(pinned).getByRole("button", { name: "Unpin FW26 Campaign" }));
    expect(pins.unpin).toHaveBeenCalledWith(expect.objectContaining({ id: "c9" }));
    expect(within(pinned).getByRole("button", { name: "Unpin Team playbook" })).toBeDisabled();
  });

  it("has no arrows for an account that cannot reorder, and says so when nothing is pinned", () => {
    const view = render(<Editor start={BUILTIN_LAYOUT} pins={pinsFor({ reorder: undefined })} />);
    expect(within(section("Pinned")).queryByRole("button", { name: /^Move / })).toBeNull();
    view.unmount();
    render(<Editor start={BUILTIN_LAYOUT} pins={pinsFor({ entries: [] })} />);
    expect(section("Pinned")).toHaveTextContent("Nothing pinned yet.");
  });

  it("pins a chat or a project from the list, leaving out archived chats and what is pinned already", async () => {
    const pins = pinsFor({ entries: [{ id: "c1", kind: "chat", ref: "c1", title: "Campaign Watch", canUnpin: true }] });
    render(<Editor start={BUILTIN_LAYOUT} pins={pins} />);
    fireEvent.click(screen.getByTestId("sidebar-pin-add"));
    await waitFor(() => expect(screen.getAllByTestId("sidebar-pin-item").length).toBeGreaterThan(0));
    expect(screen.getAllByTestId("sidebar-pin-item").map((i) => i.textContent)).toEqual(["Budget notes", "Reklaim"]);
    fireEvent.click(screen.getByText("Reklaim"));
    expect(pins.pin).toHaveBeenCalledWith("project", "p2");
  });

  it("is a person's own: the workspace default has no Pinned list, nor a page without the lists", () => {
    const view = render(<Editor start={BUILTIN_LAYOUT} pins={pinsFor()} mode="workspace" />);
    expect(screen.queryByRole("heading", { name: "Pinned" })).toBeNull();
    view.unmount();
    render(<Editor start={BUILTIN_LAYOUT} />);
    expect(screen.queryByRole("heading", { name: "Pinned" })).toBeNull();
  });
});

describe("Customize — Lists", () => {
  it("orders, folds and hides Pinned, Projects and Chats", () => {
    render(<Editor start={BUILTIN_LAYOUT} />);
    const lists = section("Lists");
    const ids = () => Array.from(lists.querySelectorAll("[data-section-id]")).map((li) => li.getAttribute("data-section-id"));
    expect(ids()).toEqual(["pinned", "projects", "chats"]);
    fireEvent.click(within(lists).getByRole("button", { name: "Move Chats up" }));
    expect(ids()).toEqual(["pinned", "chats", "projects"]);
    fireEvent.click(within(lists).getByTestId("sidebar-section-show-projects"));
    expect(latest.sections.find((s) => s.id === "projects")?.hidden).toBe(true);
    fireEvent.click(within(lists).getByRole("button", { name: "Start Pinned folded" }));
    expect(latest.sections.find((s) => s.id === "pinned")?.collapsed).toBe(true);
  });

  it("puts teammates' shared chats in Chats, in a list of their own, or nowhere", () => {
    render(<Editor start={BUILTIN_LAYOUT} />);
    const lists = section("Lists");
    const mode = within(lists).getByTestId("sidebar-editor-team-mode");
    expect(mode).toHaveAttribute("data-value", "chats");
    expect(lists).toHaveTextContent("Mixed into Chats with your own");
    fireEvent.click(within(mode).getByRole("button", { name: "Own list" }));
    expect(teamChatsMode(latest)).toBe("own");
    // A list of its own is ordered, named and folded like the others.
    expect(lists.querySelector('[data-section-id="team"]')).not.toBeNull();
    fireEvent.click(within(mode).getByRole("button", { name: "Hidden" }));
    expect(teamChatsMode(latest)).toBe("hidden");
    expect(lists.querySelector('[data-section-id="team"]')).toBeNull();
    expect(lists).toHaveTextContent("Not listed. Search still finds them.");
  });

  it("groups chats by date and shows a status on rows, each a switch", () => {
    render(<Editor start={BUILTIN_LAYOUT} />);
    const dates = screen.getByTestId("sidebar-editor-dates");
    const status = screen.getByTestId("sidebar-editor-status");
    expect(dates).toBeChecked();
    expect(status).toBeChecked();
    fireEvent.click(dates);
    expect(latest.dates_off).toBe(true);
    fireEvent.click(status);
    expect(latest.status_off).toBe(true);
    fireEvent.click(dates);
    expect(latest.dates_off).toBeUndefined();
  });
});

describe("Customize — Density", () => {
  it("is Comfortable or Compact", () => {
    render(<Editor start={BUILTIN_LAYOUT} />);
    const density = screen.getByTestId("sidebar-density");
    expect(within(density).getByRole("button", { name: "Comfortable" })).toHaveAttribute("aria-pressed", "true");
    fireEvent.click(within(density).getByRole("button", { name: "Compact" }));
    expect(latest.compact).toBe(true);
    fireEvent.click(within(density).getByRole("button", { name: "Comfortable" }));
    expect(latest.compact).toBeUndefined();
  });
});
