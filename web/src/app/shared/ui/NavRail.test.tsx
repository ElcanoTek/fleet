import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { NavRail, keepFocusForDrawerShortcut, type RailCollapse } from "./NavRail";

afterEach(() => cleanup());

// CrossViewNav renders router links; the rail itself needs no routing behavior for
// these assertions, so stub the two nav buttons out.
vi.mock("./CrossViewNav", () => ({
  NavToChat: () => <a href="/chat">Chat</a>,
  NavToOrchestrator: () => <a href="/orchestrator">Operations</a>,
  NavToHelp: () => <a href="/help">Guides</a>,
}));

const collapse: RailCollapse = {
  collapsed: false,
  isNarrow: false,
  railReady: true,
  setCollapsed: () => {},
};

function renderRail(brandLogoSrc?: string) {
  return render(
    <NavRail
      activeView="chat"
      brandName="Acme"
      brandLogoSrc={brandLogoSrc}
      sidebarOpen={false}
      setSidebarOpen={() => {}}
      collapse={collapse}
      account={{ email: "someone@example.com", onSignOut: () => {} }}
    />,
  );
}

describe("NavRail brand mark", () => {
  // Regression guard for the bundle-logo defect: next/image bypasses its
  // optimizer ONLY for a src path literally ending in ".svg". A bundle mark
  // arrives as "/api/brand/logo" with no extension, so without `unoptimized` it
  // was rewritten to "/_next/image?url=..." — and that endpoint rejects
  // image/svg+xml unless images.dangerouslyAllowSVG is set (it is not), so every
  // page rendered a broken mark for any bundle whose branding.logo was an SVG.
  it("serves an extensionless bundle logo as-is, not through the image optimizer", () => {
    renderRail("/api/brand/logo");
    const img = screen.getByAltText("Acme");
    expect(img.getAttribute("src")).toBe("/api/brand/logo");
    expect(img.getAttribute("src")).not.toContain("/_next/image");
  });

  it("serves fleet's own default mark as-is too", () => {
    renderRail();
    const img = screen.getByAltText("Acme");
    expect(img.getAttribute("src")).toBe("/logos/fleet-mark.svg");
    expect(img.getAttribute("src")).not.toContain("/_next/image");
  });

  it("does not emit a srcset — an unoptimized mark has no generated variants", () => {
    renderRail("/api/brand/logo");
    expect(screen.getByAltText("Acme").getAttribute("srcset")).toBeNull();
  });
});

describe("NavRail account menu", () => {
  // The Sign out confirm names the deployment as the brand row does
  // (branding.app_name), not as "Fleet".
  it("names the brand in the Sign out confirm", () => {
    renderRail();
    fireEvent.click(screen.getByRole("button", { name: /account menu/i }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
    expect(screen.getByRole("dialog", { name: "Sign out?" })).toHaveTextContent(
      "This signs you out of Acme and of every app that shares your sign-in.",
    );
  });
});

describe("NavRail phone drawer", () => {
  // The closed drawer is invisible, so focus left inside it would fall to
  // <body>. Closing it hands focus to the button that opens it.
  it("returns focus to the visible sidebar button when it closes with focus inside", () => {
    // jsdom lays nothing out: every element reports no offsetParent, which the
    // hand-back reads as "hidden".
    const offsetParent = vi
      .spyOn(HTMLElement.prototype, "offsetParent", "get")
      .mockReturnValue(document.body);
    try {
      const rail = (open: boolean) => (
        <>
          <button type="button" data-sidebar-toggle="">Open sidebar</button>
          <NavRail
            activeView="chat"
            brandName="Acme"
            sidebarOpen={open}
            setSidebarOpen={() => {}}
            collapse={collapse}
            account={{ email: "someone@example.com", onSignOut: () => {} }}
          />
        </>
      );
      const { rerender } = render(rail(true));
      screen.getByRole("button", { name: "Close sidebar" }).focus();
      rerender(rail(false));
      expect(screen.getByRole("button", { name: "Open sidebar" })).toHaveFocus();
    } finally {
      offsetParent.mockRestore();
    }
  });

  // Below sm the open drawer covers the page: focus moves into it, Escape
  // closes it and Tab stays inside, on every page (only Chat used to close it).
  describe("keyboard, at phone width", () => {
    // jsdom has no matchMedia at all.
    const phone = (matches: boolean) => {
      vi.stubGlobal("matchMedia", (query: string) => ({ matches, media: query, addEventListener() {}, removeEventListener() {} }));
      return { mockRestore: () => vi.unstubAllGlobals() };
    };
    const rail = (open: boolean, setSidebarOpen: (open: boolean) => void) => (
      <>
        <button type="button" data-sidebar-toggle="">Open sidebar</button>
        <a href="/behind">Behind the drawer</a>
        {/* Focusable, but not a control: Enter is not its key. */}
        {/* eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex */}
        <div role="group" aria-label="A wide table" tabIndex={0} />
        <NavRail
          activeView="orchestrator"
          brandName="Acme"
          sidebarOpen={open}
          setSidebarOpen={setSidebarOpen}
          collapse={collapse}
          account={{ email: "someone@example.com", onSignOut: () => {} }}
        />
      </>
    );

    it("moves focus in, closes on Escape and keeps Tab inside", () => {
      const offsetParent = vi.spyOn(HTMLElement.prototype, "offsetParent", "get").mockReturnValue(document.body);
      const media = phone(true);
      try {
        const setSidebarOpen = vi.fn();
        const { rerender, container } = render(rail(false, setSidebarOpen));
        screen.getByRole("button", { name: "Open sidebar" }).focus();
        rerender(rail(true, setSidebarOpen));
        const aside = container.querySelector("aside")!;
        expect(aside.contains(document.activeElement)).toBe(true);

        // Focus that ended up on the page behind is brought back by Tab.
        screen.getByRole("link", { name: "Behind the drawer" }).focus();
        fireEvent.keyDown(window, { key: "Tab" });
        expect(aside.contains(document.activeElement)).toBe(true);

        fireEvent.keyDown(window, { key: "Escape" });
        expect(setSidebarOpen).toHaveBeenCalledWith(false);
      } finally {
        media.mockRestore();
        offsetParent.mockRestore();
      }
    });

    // One test per row of the table in NavRail's drawer effect.
    const opened = (how: "button" | "shortcut", focusOn: "control" | "region" | "nothing") => {
      const offsetParent = vi.spyOn(HTMLElement.prototype, "offsetParent", "get").mockReturnValue(document.body);
      const media = phone(true);
      try {
        const { rerender, container } = render(rail(false, () => {}));
        if (focusOn === "control") screen.getByRole("link", { name: "Behind the drawer" }).focus();
        else if (focusOn === "region") screen.getByRole("group", { name: "A wide table" }).focus();
        else (document.activeElement as HTMLElement | null)?.blur();
        if (how === "shortcut") keepFocusForDrawerShortcut();
        rerender(rail(true, () => {}));
        return container.querySelector("aside")!.contains(document.activeElement) ? "drawer" : "page";
      } finally {
        media.mockRestore();
        offsetParent.mockRestore();
      }
    };

    it.each([
      // Safari does not focus a button when it is tapped: nothing is focused.
      ["button", "nothing", "drawer"],
      ["button", "region", "drawer"],
      ["button", "control", "drawer"],
      // A focused control is now under the drawer: Enter must not reach it.
      ["shortcut", "control", "drawer"],
      // Enter has to open the chat under the cursor, which it does whenever
      // no control has focus: with nothing focused, and with focus resting on
      // something that does not own Enter (a scrollable region).
      ["shortcut", "nothing", "page"],
      ["shortcut", "region", "page"],
    ] as const)("opened by %s with %s focused: focus ends in the %s", (how, focusOn, where) => {
      expect(opened(how, focusOn)).toBe(where);
    });

    it("does not let an unused shortcut mark carry over to a later opening", () => {
      const offsetParent = vi.spyOn(HTMLElement.prototype, "offsetParent", "get").mockReturnValue(document.body);
      const media = phone(true);
      try {
        // The shortcut is pressed while the drawer is already open (nothing
        // opens, so nothing uses the mark), then the drawer is closed and
        // opened again from its button.
        const { rerender, container } = render(rail(true, () => {}));
        keepFocusForDrawerShortcut();
        rerender(rail(false, () => {}));
        (document.activeElement as HTMLElement | null)?.blur();
        rerender(rail(true, () => {}));
        expect(container.querySelector("aside")!.contains(document.activeElement)).toBe(true);
      } finally {
        media.mockRestore();
        offsetParent.mockRestore();
      }
    });

    it("leaves Escape to a menu or dialog that is open over the drawer", () => {
      const offsetParent = vi.spyOn(HTMLElement.prototype, "offsetParent", "get").mockReturnValue(document.body);
      const media = phone(true);
      try {
        const setSidebarOpen = vi.fn();
        render(
          <>
            {rail(true, setSidebarOpen)}
            <div role="dialog" aria-label="Rename">
              <button type="button">In the dialog</button>
            </div>
          </>,
        );
        screen.getByRole("button", { name: "In the dialog" }).focus();
        fireEvent.keyDown(window, { key: "Escape" });
        expect(setSidebarOpen).not.toHaveBeenCalled();
        // A dialog that closes itself on this very Escape and hands focus back
        // to its trigger in the rail has marked the key as used.
        const usedByDialog = (e: KeyboardEvent) => {
          if (e.key === "Escape") e.preventDefault();
        };
        document.addEventListener("keydown", usedByDialog, true);
        try {
          screen.getByRole("button", { name: "Close sidebar" }).focus();
          fireEvent.keyDown(document.body, { key: "Escape" });
          expect(setSidebarOpen).not.toHaveBeenCalled();
        } finally {
          document.removeEventListener("keydown", usedByDialog, true);
        }
        screen.getByRole("button", { name: "In the dialog" }).focus();
        // Tab is the dialog's too: focus is not pulled back into the drawer.
        fireEvent.keyDown(window, { key: "Tab" });
        expect(screen.getByRole("button", { name: "In the dialog" })).toHaveFocus();
      } finally {
        media.mockRestore();
        offsetParent.mockRestore();
      }
    });

    it("does nothing from sm up, where the rail is part of the page", () => {
      const media = phone(false);
      try {
        const setSidebarOpen = vi.fn();
        render(rail(true, setSidebarOpen));
        screen.getByRole("link", { name: "Behind the drawer" }).focus();
        fireEvent.keyDown(window, { key: "Escape" });
        expect(setSidebarOpen).not.toHaveBeenCalled();
        expect(screen.getByRole("link", { name: "Behind the drawer" })).toHaveFocus();
      } finally {
        media.mockRestore();
      }
    });
  });

  it("leaves focus alone when the drawer closes with focus elsewhere", () => {
    const rail = (open: boolean) => (
      <>
        <button type="button" data-sidebar-toggle="">Open sidebar</button>
        <input aria-label="Message" />
        <NavRail
          activeView="chat"
          brandName="Acme"
          sidebarOpen={open}
          setSidebarOpen={() => {}}
          collapse={collapse}
          account={{ email: "someone@example.com", onSignOut: () => {} }}
        />
      </>
    );
    const { rerender } = render(rail(true));
    screen.getByLabelText("Message").focus();
    rerender(rail(false));
    expect(screen.getByLabelText("Message")).toHaveFocus();
  });
});
