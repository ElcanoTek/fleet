import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { GuideMarkdown } from "./GuideMarkdown";
import { GuideNav } from "./GuideNav";

afterEach(() => cleanup());

vi.mock("next/navigation", () => ({ usePathname: () => "/help/chat" }));

describe("GuideMarkdown", () => {
  it("anchors headings with the slug the guides' own links point at", () => {
    render(<GuideMarkdown content={"## 4. Run states\n\ntext\n"} />);
    expect(screen.getByRole("heading", { name: "4. Run states" }).id).toBe("4-run-states");
  });

  it("drops the leading h1 — the page header already carries the title", () => {
    render(<GuideMarkdown content={"# Chat\n\n## Section\n"} />);
    expect(screen.queryByRole("heading", { name: "Chat" })).toBeNull();
    expect(screen.getByRole("heading", { name: "Section" })).toBeTruthy();
  });

  it("wraps tables in their own scroller so a wide one never widens the page", () => {
    const { container } = render(
      <GuideMarkdown content={"| A | B |\n| --- | --- |\n| 1 | 2 |\n"} />,
    );
    expect(container.querySelector(".guide-table-shell > table")).toBeTruthy();
  });

  it("opens external links in a new tab without handing over the opener", () => {
    render(<GuideMarkdown content={"[out](https://example.com) and [in](#4-run-states)\n"} />);
    const external = screen.getByRole("link", { name: "out" });
    expect(external.getAttribute("target")).toBe("_blank");
    expect(external.getAttribute("rel")).toContain("noopener");
    // A same-page anchor must stay a plain in-page jump.
    const internal = screen.getByRole("link", { name: "in" });
    expect(internal.getAttribute("target")).toBeNull();
  });
});

describe("GuideNav", () => {
  const guides = [
    { slug: "chat", title: "Chat", blurb: "", file: "chat.md" },
    { slug: "operations-center", title: "Operations Center", blurb: "", file: "ops.md" },
  ];

  it("marks the open guide as the current page", () => {
    render(<GuideNav guides={guides} />);
    expect(screen.getByRole("link", { name: "Chat" }).getAttribute("aria-current")).toBe("page");
    expect(
      screen.getByRole("link", { name: "Operations Center" }).getAttribute("aria-current"),
    ).toBeNull();
  });

  it("lists the open guide's sections, but not its subsections", () => {
    render(
      <GuideNav
        guides={guides}
        headings={[
          { id: "one", text: "One", depth: 2 },
          { id: "deep", text: "Deep", depth: 3 },
        ]}
      />,
    );
    expect(screen.getByRole("link", { name: "One" }).getAttribute("href")).toBe("#one");
    expect(screen.queryByRole("link", { name: "Deep" })).toBeNull();
  });
});
