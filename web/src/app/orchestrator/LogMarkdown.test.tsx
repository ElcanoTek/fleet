import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import LogMarkdown from "./LogMarkdown";

afterEach(() => cleanup());

describe("LogMarkdown", () => {
  it("keeps an image's DOM node when the log grows (stable components map)", () => {
    const first = "![chart](out/chart.png)\n";
    const { rerender } = render(<LogMarkdown content={first} taskId="t1" />);
    const img = screen.getByTestId("log-image");
    expect(img.getAttribute("src")).toContain("t1");

    // A live log appends lines; the image above must not remount.
    rerender(<LogMarkdown content={first + "\nmore output\n"} taskId="t1" />);
    expect(screen.getByTestId("log-image")).toBe(img);
    expect(screen.getByText("more output")).toBeTruthy();
  });

  it("re-resolves workspace hrefs when the task changes", () => {
    const content = "![chart](out/chart.png)\n";
    const { rerender } = render(<LogMarkdown content={content} taskId="t1" />);
    rerender(<LogMarkdown content={content} taskId="t2" />);
    expect(screen.getByTestId("log-image").getAttribute("src")).toContain("t2");
  });

  // The same reply reads the same in the run log as in the chat: one shared
  // plugin list (shared/lib/markdownPlugins), so a table cell's line breaks
  // are line breaks here too and a tag outside a table is shown as typed.
  it("treats a literal <br> the way the chat does", () => {
    const { container } = render(
      <LogMarkdown content={"| Plan |\n|---|\n| 5 seats<br>Email support |\n\nUse the <br> tag."} taskId="t1" />,
    );
    expect(container.querySelector("td")?.querySelectorAll("br")).toHaveLength(1);
    expect(container.querySelector("td")?.textContent).not.toContain("<br>");
    expect(container.textContent).toContain("Use the <br> tag.");
  });
});
