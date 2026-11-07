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
});
