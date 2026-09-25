import { readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import { SharedConversationView } from "./SharedConversationView";

// The public share page is the one route that scrolls the whole document
// instead of an inner pane, so it opts out of the app shell's body rules
// (overflow-x: hidden on body, a background-attachment: fixed gradient) that
// made a long transcript scroll badly. The opt-out is a class on <main> plus a
// :has() block in globals.css; renaming either side silently brings the jank
// back, so this pins the pair.

describe("the shared conversation page's scroll opt-out", () => {
  it("marks its <main> with the class globals.css keys off", () => {
    const { container } = render(
      <SharedConversationView
        snapshot={{ title: "t", persona: "", model: "", created_at: 0, shared_at: 0, messages: [] }}
      />,
    );
    expect(container.querySelector("main.shared-page")).not.toBeNull();
  });

  it("clips instead of hiding overflow and moves the gradient off the scrolling body", () => {
    const css = readFileSync(path.join(__dirname, "../../globals.css"), "utf8");
    // Every declaration block whose selector list ends with `selector`.
    const block = (selector: string) => {
      let out = "";
      for (let i = css.indexOf(`${selector} {`); i !== -1; i = css.indexOf(`${selector} {`, i + 1)) {
        out += css.slice(i, css.indexOf("}", i));
      }
      expect(out, `missing rule for ${selector}`).not.toBe("");
      return out;
    };
    expect(block("body:has(> .shared-page)")).toContain("overflow-x: clip");
    expect(block("body:has(> .shared-page)")).toContain("background-image: none");
    const layer = block("body:has(> .shared-page)::before");
    expect(layer).toContain("position: fixed");
    expect(layer).toContain("background-image: var(--gradient-bg)");
  });
});
