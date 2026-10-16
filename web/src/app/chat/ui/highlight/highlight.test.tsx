import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import path from "node:path";
import { renderToStaticMarkup } from "react-dom/server";
import HighlightedCode from "../CodeHighlight";
import { tokenize, Token } from "./prism";
import { grammarFor } from "./languages";

type Golden = {
  language: string;
  code: string;
  props: { showLineNumbers?: boolean; wrapLongLines?: boolean; fontSize?: string };
  html: string;
};

// __fixtures__/golden.json is the HTML react-syntax-highlighter 16.1.2
// (PrismLight + refractor 5.0.0 + Prism 1.30.0) rendered for corpus.json,
// with this component's props, captured before the library was removed. The
// vendored highlighter must reproduce it byte for byte.
const goldens = JSON.parse(
  readFileSync(path.join(__dirname, "__fixtures__", "golden.json"), "utf8"),
) as Golden[];

describe("HighlightedCode matches react-syntax-highlighter's output", () => {
  it("covers every corpus entry in four layouts", () => {
    expect(goldens.length).toBeGreaterThanOrEqual(13 * 4);
  });
  it.each(goldens.map((g, i) => [`${i} ${g.language} ${JSON.stringify(g.props)}`, g] as const))("%s", (_, g) => {
    expect(renderToStaticMarkup(<HighlightedCode code={g.code} language={g.language} {...g.props} />)).toBe(g.html);
  });
});

describe("tokenize", () => {
  it("nests interpolations inside a Python f-string", () => {
    const tokens = tokenize('f"{x}"', grammarFor("python")!);
    expect(tokens).toHaveLength(1);
    const t = tokens[0] as Token;
    expect(t.type).toBe("string-interpolation");
    expect(JSON.stringify(t.content)).toContain('"interpolation"');
  });
  it("registers the aliases fleet relies on", () => {
    for (const name of ["python", "py", "bash", "sh", "shell", "json", "yaml", "yml", "markup", "html", "xml"]) {
      expect(grammarFor(name), name).toBeDefined();
    }
    expect(grammarFor("toString")).toBeUndefined();
    expect(grammarFor("javascript")).toBeUndefined();
  });
});
