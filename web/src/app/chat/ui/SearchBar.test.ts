import { isValidElement, type ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { previewNodes } from "./SearchBar";

type Simple = string | { mark: unknown };

function simplify(nodes: ReturnType<typeof previewNodes>): Simple[] {
  return nodes.map((n) => {
    if (typeof n === "string") return n;
    if (isValidElement(n)) {
      const el = n as ReactElement<{ children?: unknown }>;
      expect(el.type).toBe("mark");
      return { mark: el.props.children };
    }
    throw new Error(`unexpected node ${String(n)}`);
  });
}

describe("previewNodes", () => {
  it("emits text and <mark> elements for balanced pairs", () => {
    expect(simplify(previewNodes("the async <mark>function</mark> you <mark>asked</mark>"))).toEqual([
      "the async ",
      { mark: "function" },
      " you ",
      { mark: "asked" },
    ]);
  });

  it("keeps other markup as literal text (no HTML interpretation)", () => {
    const raw = `<img src=x onerror=alert(1)> & <b>bold</b> <mark><script>x</script></mark>`;
    expect(simplify(previewNodes(raw))).toEqual([
      "<img src=x onerror=alert(1)> & <b>bold</b> ",
      { mark: "<script>x</script>" },
    ]);
  });

  it("degrades unbalanced tags to text", () => {
    expect(simplify(previewNodes("open <mark>never closed"))).toEqual(["open <mark>never closed"]);
    expect(simplify(previewNodes("stray </mark> close"))).toEqual(["stray </mark> close"]);
    expect(simplify(previewNodes("<mark>a</mark> then </mark><mark>"))).toEqual([
      { mark: "a" },
      " then </mark><mark>",
    ]);
  });

  it("returns no nodes for an empty snippet", () => {
    expect(previewNodes("")).toEqual([]);
  });
});
