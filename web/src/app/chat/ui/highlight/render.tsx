// Token stream -> React, laid out exactly as react-syntax-highlighter 16.1.2
// rendered fleet's code blocks: refractor 5.0.0's token-to-tree step
// (lib/core.js stringify) followed by react-syntax-highlighter's line
// splitting, line numbers and inline-style resolution (highlight.js,
// create-element.js), trimmed to the options fleet passes. The HTML is pinned
// byte for byte against the library's by highlight.test.tsx.
// Copyright (c) 2019 Conor Hastings (react-syntax-highlighter) and Titus
// Wormer (refractor); see LICENSE.react-syntax-highlighter and
// LICENSE.refractor. Modified. SPDX-License-Identifier: MIT

import { createElement as h, type CSSProperties, type ReactNode } from "react";
import { Token, type TokenStream } from "./prism";

export type Stylesheet = Record<string, CSSProperties>;

// A hast-like tree, as refractor produced and react-syntax-highlighter walked.
type TextNode = { type: "text"; value: string };
type ElementNode = {
  type: "element";
  tagName: "span";
  properties: { className: string[]; style?: CSSProperties; title?: string; key?: string };
  children: Array<TextNode | ElementNode>;
};
type Node = TextNode | ElementNode;

// --- refractor: token stream -> tree ---

// An entity token is titled with the character it stands for (Prism's
// markup "wrap" hook). Numeric references and the five XML entities are
// decoded; any other named entity keeps its source text as the title.
const NAMED: Record<string, string> = { amp: "&", lt: "<", gt: ">", quot: '"', apos: "'" };
function entityTitle(source: string): string {
  const s = source.replace(/&amp;/, "&");
  return s.replace(/&(#x[\da-f]+|#\d+|[a-z]+);/gi, (whole, ref: string) => {
    if (ref[0] === "#") {
      const code = ref[1] === "x" || ref[1] === "X" ? parseInt(ref.slice(2), 16) : parseInt(ref.slice(1), 10);
      return Number.isFinite(code) && code > 0 && code <= 0x10ffff ? String.fromCodePoint(code) : whole;
    }
    return NAMED[ref.toLowerCase()] ?? whole;
  });
}

function toTree(stream: TokenStream | string): Node[] {
  if (typeof stream === "string") {
    return [{ type: "text", value: stream }];
  }
  const out: Node[] = [];
  for (const t of stream) {
    if (t === "") continue;
    if (typeof t === "string") {
      out.push({ type: "text", value: t });
      continue;
    }
    out.push(tokenNode(t));
  }
  return out;
}

function tokenNode(t: Token): ElementNode {
  const classes = ["token", t.type];
  if (t.alias) classes.push(...(typeof t.alias === "string" ? [t.alias] : t.alias));
  const children = toTree(t.content);
  const properties: ElementNode["properties"] = { className: classes };
  if (t.type === "entity" && children.length === 1 && children[0].type === "text") {
    properties.title = entityTitle(children[0].value);
  }
  return { type: "element", tagName: "span", properties, children };
}

// --- react-syntax-highlighter: tree -> lines ---

function lineElement(children: Node[], className: string[] = [], style?: CSSProperties): ElementNode {
  const properties: ElementNode["properties"] = { className };
  if (style) properties.style = style;
  return { type: "element", tagName: "span", properties, children };
}

// flattenCodeTree: every text leaf becomes a span carrying all its ancestors'
// classes (deduplicated, in order).
function flatten(tree: Node[], className: string[] = [], out: ElementNode[] = []): ElementNode[] {
  for (const node of tree) {
    if (node.type === "text") {
      out.push(lineElement([node], [...new Set(className)]));
    } else {
      flatten(node.children, className.concat(node.properties.className), out);
    }
  }
  return out;
}

interface LineOptions {
  showLineNumbers: boolean;
  wrapLines: boolean;
  wrapLongLines: boolean;
  lineNumberStyle: CSSProperties;
  largestLineNumber: number;
}

function lineNumberNode(lineNumber: number, o: LineOptions): ElementNode {
  const style: CSSProperties = {
    display: "inline-block",
    minWidth: `${String(o.largestLineNumber).length}.25em`,
    paddingRight: "1em",
    textAlign: "right",
    userSelect: "none",
    ...o.lineNumberStyle,
  };
  return {
    type: "element",
    tagName: "span",
    properties: { key: `line-number--${lineNumber}`, className: ["comment", "linenumber", "react-syntax-highlighter-line-number"], style },
    children: [{ type: "text", value: String(lineNumber) }],
  };
}

// processLines, for showInlineLineNumbers (the default) and startingLineNumber 1.
function processLines(tree: Node[], o: LineOptions): Node[] {
  const flat = flatten(tree);
  const lines: Node[][] = [];
  let lastLineBreakIndex = -1;

  const createLine = (children: Node[], lineNumber: number | false, className: string[] = []): Node[] => {
    if (o.wrapLines || className.length > 0) {
      if (lineNumber && o.showLineNumbers) children.unshift(lineNumberNode(lineNumber, o));
      const style = o.wrapLongLines && o.showLineNumbers ? { display: "flex" } : undefined;
      return [lineElement(children, className, style)];
    }
    if (o.showLineNumbers && lineNumber) children.unshift(lineNumberNode(lineNumber, o));
    return children;
  };

  for (let index = 0; index < flat.length; index++) {
    const node = flat[index];
    const value = (node.children[0] as TextNode).value;
    if (!value.includes("\n")) continue;
    const split = value.split("\n");
    split.forEach((text, i) => {
      const lineNumber = o.showLineNumbers && lines.length + 1;
      const newChild: TextNode = { type: "text", value: `${text}\n` };
      if (i === 0) {
        const children: Node[] = flat
          .slice(lastLineBreakIndex + 1, index)
          .concat(lineElement([newChild], node.properties.className));
        lines.push(createLine(children, lineNumber));
      } else if (i === split.length - 1) {
        const last: TextNode = { type: "text", value: text };
        const next = flat[index + 1];
        if (next && next.children && next.children[0]) {
          flat.splice(index + 1, 0, lineElement([last], node.properties.className));
        } else {
          lines.push(createLine([last], lineNumber, node.properties.className));
        }
      } else {
        lines.push(createLine([newChild], lineNumber, node.properties.className));
      }
    });
    lastLineBreakIndex = index;
  }
  if (lastLineBreakIndex !== flat.length - 1) {
    const children = flat.slice(lastLineBreakIndex + 1);
    if (children.length) {
      const lineNumber = o.showLineNumbers && lines.length + 1;
      lines.push(createLine(children, lineNumber));
    }
  }
  return lines.flat();
}

// --- react-syntax-highlighter: tree -> React, styles inlined ---

// All orderings of up to four classes, as react-syntax-highlighter merges
// compound selectors ("a.b") after single ones.
function combinations(classes: string[]): string[] {
  if (classes.length <= 1) return classes;
  const arr = classes.slice(0, 4);
  const out: string[] = [];
  const perms = (prefix: string[], rest: string[], size: number) => {
    if (prefix.length === size) {
      out.push(prefix.join("."));
      return;
    }
    rest.forEach((c, i) => perms([...prefix, c], [...rest.slice(0, i), ...rest.slice(i + 1)], size));
  };
  for (let size = 1; size <= arr.length; size++) perms([], arr, size);
  return out;
}

function styleFor(classes: string[], elementStyle: CSSProperties, sheet: Stylesheet): CSSProperties {
  return combinations(classes.filter((c) => c !== "token")).reduce<CSSProperties>(
    (style, c) => ({ ...style, ...sheet[c] }),
    elementStyle,
  );
}

function renderNode(node: Node, sheet: Stylesheet, selectorClasses: Set<string>, key: string): ReactNode {
  if (node.type === "text") return node.value;
  const { className: classes, style, key: ownKey, ...rest } = node.properties;
  const kept = (classes.includes("token") ? ["token"] : []).concat(classes.filter((c) => !selectorClasses.has(c)));
  const children = node.children.map((child, i) => renderNode(child, sheet, selectorClasses, `${key}-${i}`));
  return h(
    node.tagName,
    { key: ownKey ?? key, ...rest, className: kept.join(" ") || undefined, style: styleFor(classes, { ...style }, sheet) },
    ...children,
  );
}

export interface RenderOptions {
  showLineNumbers: boolean;
  wrapLongLines: boolean;
  lineNumberStyle: CSSProperties;
}

/** Renders a token stream (or plain text, for no grammar) as the code's children. */
export function renderCode(code: string, tokens: TokenStream | null, sheet: Stylesheet, o: RenderOptions): ReactNode[] {
  const tree: Node[] = tokens ? toTree(tokens) : [{ type: "text", value: code }];
  const lineBreaks = code.match(/\n/g)?.length ?? 0;
  const lines = processLines(tree, {
    showLineNumbers: o.showLineNumbers,
    wrapLines: o.wrapLongLines, // react-syntax-highlighter turns wrapLines on for wrapLongLines
    wrapLongLines: o.wrapLongLines,
    lineNumberStyle: o.lineNumberStyle,
    largestLineNumber: 1 + lineBreaks,
  });
  const selectorClasses = new Set(Object.keys(sheet).flatMap((s) => s.split(".")));
  return lines.map((node, i) => renderNode(node, sheet, selectorClasses, `code-segment-${i}`));
}
