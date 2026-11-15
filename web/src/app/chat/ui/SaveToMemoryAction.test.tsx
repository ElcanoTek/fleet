import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { placeReplyMenu, ReplyMoreActions, SaveToMemoryAction } from "./ChatTranscript";

// Item D1's SECOND capture path: a Save action beside Copy · Regenerate ·
// Branch, for keeping something the agent said without composing a "remember
// this" turn. It shipped with no test at all — MemoryProposalCard covers only
// the approval-card path, which is a different component with a different
// default.

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function mockPost() {
  const calls: { url: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), body: String(init?.body ?? "") });
      return new Response(JSON.stringify({}), { status: 200 });
    }),
  );
  return calls;
}

describe("SaveToMemoryAction", () => {
  it("saves straight to personal memory outside a project — no choice worth showing", async () => {
    const calls = mockPost();
    const onSaved = vi.fn();
    render(<SaveToMemoryAction content="the spread is 12bps" project={null} onSaved={onSaved} />);

    fireEvent.click(screen.getByRole("button", { name: "Save to memory" }));
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].url).toBe("/api/memories");
    expect(JSON.parse(calls[0].body)).toEqual({
      content: "the spread is 12bps",
      kind: "fact",
    });
    expect(onSaved).toHaveBeenCalled();
    expect(await screen.findByText("Saved ✓")).toBeInTheDocument();
  });

  it("preselects the team in a team-shared project — the user picked this text", async () => {
    const calls = mockPost();
    render(
      <SaveToMemoryAction
        content="quote spreads in bps"
        project={{ id: "p1", name: "Quant", teamShared: true }}
        onSaved={vi.fn()}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Save to memory" }));
    // The destination is shown BEFORE saving, never applied silently.
    expect(screen.getByRole("button", { name: "Team learnings" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].url).toBe("/api/projects/p1/memories");
  });

  it("defaults to my memory in a personal project, and honours a flip", async () => {
    const calls = mockPost();
    render(
      <SaveToMemoryAction
        content="a note"
        project={{ id: "p2", name: "Mine", teamShared: false }}
        onSaved={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Save to memory" }));
    expect(screen.getByRole("button", { name: "My memory" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    fireEvent.click(screen.getByRole("button", { name: "Team learnings" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].url).toBe("/api/projects/p2/memories");
  });

  it("says the save failed instead of claiming it worked", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("nope", { status: 403 })),
    );
    const onSaved = vi.fn();
    render(<SaveToMemoryAction content="x" project={null} onSaved={onSaved} />);
    fireEvent.click(screen.getByRole("button", { name: "Save to memory" }));

    expect(await screen.findByRole("button", { name: "Save failed" })).toBeInTheDocument();
    expect(onSaved).not.toHaveBeenCalled();
  });
});

// The reply footer's occasional actions: inline on a desktop, inside a
// "more actions" menu on a phone (ReplyMoreActions decides by window width).
describe("ReplyMoreActions", () => {
  function narrowScreen(matches: boolean) {
    vi.stubGlobal(
      "matchMedia",
      vi.fn(() => ({ matches, addEventListener: vi.fn(), removeEventListener: vi.fn() })),
    );
  }

  it("renders its actions inline on a desktop, with no menu button", () => {
    narrowScreen(false);
    render(
      <ReplyMoreActions>
        <button type="button" className="reply-action">Branch</button>
      </ReplyMoreActions>,
    );
    expect(screen.getByRole("button", { name: "Branch" })).toBeVisible();
    expect(screen.queryByRole("button", { name: "More actions" })).toBeNull();
    expect(screen.queryByTestId("reply-more-menu")).toBeNull();
  });

  it("on a phone, opens the actions as a menu and closes it once one is chosen", () => {
    narrowScreen(true);
    const onBranch = vi.fn();
    render(
      <ReplyMoreActions>
        {null}
        <button type="button" className="reply-action" onClick={onBranch}>Branch</button>
      </ReplyMoreActions>,
    );
    const toggle = screen.getByRole("button", { name: "More actions" });
    const menu = screen.getByTestId("reply-more-menu");
    expect(menu).not.toBeVisible();
    expect(toggle).toHaveAttribute("aria-expanded", "false");

    fireEvent.click(toggle);
    expect(menu).toBeVisible();
    expect(toggle).toHaveAttribute("aria-expanded", "true");

    fireEvent.click(screen.getByRole("button", { name: "Branch" }));
    expect(onBranch).toHaveBeenCalledTimes(1);
    expect(menu).not.toBeVisible();
  });

  it("stays open for an action that answers in place, and closes on Escape or a tap outside", () => {
    narrowScreen(true);
    render(
      <ReplyMoreActions>
        <button type="button" className="reply-action" data-keep-open="">Save to memory</button>
      </ReplyMoreActions>,
    );
    const toggle = screen.getByRole("button", { name: "More actions" });
    const menu = screen.getByTestId("reply-more-menu");
    fireEvent.click(toggle);
    fireEvent.click(screen.getByRole("button", { name: "Save to memory" }));
    expect(menu).toBeVisible();

    fireEvent.keyDown(document, { key: "Escape" });
    expect(menu).not.toBeVisible();

    fireEvent.click(toggle);
    expect(menu).toBeVisible();
    fireEvent.pointerDown(document.body);
    expect(menu).not.toBeVisible();
  });

  // The sheet is portalled to <body>: Tab from its last item used to carry
  // on to the top of the page and leave it open behind.
  it("closes and returns to its button when Tab leaves either end of the menu", () => {
    narrowScreen(true);
    render(
      <ReplyMoreActions>
        <button type="button" className="reply-action">Branch</button>
        <button type="button" className="reply-action">Save as workflow</button>
      </ReplyMoreActions>,
    );
    const toggle = screen.getByRole("button", { name: "More actions" });
    const menu = screen.getByTestId("reply-more-menu");
    // jsdom lays nothing out, so every element reports no offsetParent; the
    // menu's Tab handling uses it to skip hidden items.
    const offsetParent = vi
      .spyOn(HTMLElement.prototype, "offsetParent", "get")
      .mockReturnValue(document.body);
    try {
      fireEvent.click(toggle);
      const first = screen.getByRole("button", { name: "Branch" });
      const last = screen.getByRole("button", { name: "Save as workflow" });

      // Tab between items is left to the browser.
      first.focus();
      fireEvent.keyDown(first, { key: "Tab" });
      expect(menu).toBeVisible();

      last.focus();
      fireEvent.keyDown(last, { key: "Tab" });
      expect(menu).not.toBeVisible();
      expect(toggle).toHaveFocus();

      fireEvent.click(toggle);
      first.focus();
      fireEvent.keyDown(first, { key: "Tab", shiftKey: true });
      expect(menu).not.toBeVisible();
      expect(toggle).toHaveFocus();
    } finally {
      offsetParent.mockRestore();
    }
  });

  // Save to memory answers in place: its button becomes a "Saved" note, and
  // the focus it held falls to <body> with the sheet still open.
  it("closes and returns to its button when Tab is pressed after focus fell out of the open menu", () => {
    narrowScreen(true);
    render(
      <ReplyMoreActions>
        <button type="button" className="reply-action" data-keep-open="">Save to memory</button>
      </ReplyMoreActions>,
    );
    const toggle = screen.getByRole("button", { name: "More actions" });
    const menu = screen.getByTestId("reply-more-menu");
    fireEvent.click(toggle);
    (document.activeElement as HTMLElement | null)?.blur();
    expect(document.body).toHaveFocus();

    fireEvent.keyDown(document.body, { key: "Tab" });
    expect(menu).not.toBeVisible();
    expect(toggle).toHaveFocus();
  });

  // Phone turned sideways, or the window widened: the actions go back inline.
  it("closes an open menu when the actions return inline, so it is closed when they fold again", () => {
    let matches = true;
    const listeners = new Set<() => void>();
    vi.stubGlobal(
      "matchMedia",
      vi.fn(() => ({
        get matches() {
          return matches;
        },
        addEventListener: (_: string, fn: () => void) => void listeners.add(fn),
        removeEventListener: (_: string, fn: () => void) => void listeners.delete(fn),
      })),
    );
    const setNarrow = (next: boolean) => {
      matches = next;
      act(() => {
        for (const fn of listeners) fn();
      });
    };
    render(
      <ReplyMoreActions>
        <button type="button" className="reply-action">Branch</button>
      </ReplyMoreActions>,
    );
    fireEvent.click(screen.getByRole("button", { name: "More actions" }));
    expect(screen.getByTestId("reply-more-menu")).toBeVisible();

    setNarrow(false);
    expect(screen.queryByRole("button", { name: "More actions" })).toBeNull();
    setNarrow(true);
    expect(screen.getByRole("button", { name: "More actions" })).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByTestId("reply-more-menu")).not.toBeVisible();
  });

  it("hands focus back to its button when an action closes the menu", () => {
    narrowScreen(true);
    render(
      <ReplyMoreActions>
        <button type="button" className="reply-action">Branch</button>
      </ReplyMoreActions>,
    );
    const toggle = screen.getByRole("button", { name: "More actions" });
    fireEvent.click(toggle);
    const branch = screen.getByRole("button", { name: "Branch" });
    branch.focus();
    fireEvent.click(branch);
    expect(screen.getByTestId("reply-more-menu")).not.toBeVisible();
    expect(toggle).toHaveFocus();
  });

  it("keeps Escape to itself while open", () => {
    narrowScreen(true);
    const pageShortcut = vi.fn();
    document.addEventListener("keydown", pageShortcut);
    try {
      render(
        <ReplyMoreActions>
          <button type="button" className="reply-action">Branch</button>
        </ReplyMoreActions>,
      );
      fireEvent.click(screen.getByRole("button", { name: "More actions" }));
      fireEvent.keyDown(screen.getByRole("button", { name: "Branch" }), { key: "Escape" });
      expect(screen.getByTestId("reply-more-menu")).not.toBeVisible();
      expect(pageShortcut).not.toHaveBeenCalled();
    } finally {
      document.removeEventListener("keydown", pageShortcut);
    }
  });

  it("keeps an action's own state across closing and reopening the menu", async () => {
    narrowScreen(true);
    mockPost();
    render(
      <ReplyMoreActions>
        <SaveToMemoryAction content="keep this" project={null} onSaved={vi.fn()} />
      </ReplyMoreActions>,
    );
    const toggle = screen.getByRole("button", { name: "More actions" });
    fireEvent.click(toggle);
    fireEvent.click(screen.getByRole("button", { name: "Save to memory" }));
    expect(await screen.findByText("Saved ✓")).toBeInTheDocument();
    fireEvent.keyDown(document, { key: "Escape" });
    fireEvent.click(toggle);
    expect(screen.getByText("Saved ✓")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Save to memory" })).toBeNull();
  });

  it("renders nothing when no secondary action applies", () => {
    narrowScreen(true);
    const { container } = render(
      <ReplyMoreActions>
        {null}
        {false}
      </ReplyMoreActions>,
    );
    expect(container).toBeEmptyDOMElement();
    expect(screen.queryByTestId("reply-more-menu")).toBeNull();
  });
});

// Where the phone menu opens: above the reply it belongs to, so the reply is
// not covered — with fallbacks for when that spot is off screen.
describe("placeReplyMenu", () => {
  const base = { menuHeight: 240, limitTop: 120, limitBottom: 660, gap: 8 };

  it("opens above the reply when the sheet fits there", () => {
    // Reply at 560–640, its action row at 610–650: sheet ends 8px above the reply.
    expect(placeReplyMenu({ ...base, messageTop: 560, rowTop: 610, rowBottom: 650 })).toBe(312);
  });

  it("opens below the action row when there is no room above the reply", () => {
    expect(placeReplyMenu({ ...base, messageTop: 200, rowTop: 250, rowBottom: 290 })).toBe(298);
  });

  it("sits above the action row, inside the transcript, for a reply taller than the screen", () => {
    expect(placeReplyMenu({ ...base, messageTop: -900, rowTop: 610, rowBottom: 650 })).toBe(362);
    // Never above the transcript's top edge, however tall the sheet.
    expect(placeReplyMenu({ ...base, menuHeight: 600, messageTop: -900, rowTop: 610, rowBottom: 650 })).toBe(120);
  });
});
