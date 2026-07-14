"use client";

// Syntax-highlighted code block body — the ONLY module that imports
// react-syntax-highlighter. It is loaded lazily (React.lazy in
// ToolChips.tsx) because the highlighter + grammars are the single
// largest dependency in the initial /chat bundle (~75 KiB transfer)
// while nothing on screen needs them until the user expands a tool
// chip: CodeBlock renders a plain <pre> until this chunk arrives.
//
// Prism light build so we only ship the languages we actually render.
// PrismLight shares a single module-level grammar registry across
// imports, so registering the languages here (the only module that
// renders a SyntaxHighlighter) makes them available wherever the
// highlighter is used. Keep the registered set in sync with
// syntaxSupportedLanguages in ToolChips.tsx — that set gates which
// languages reach this component at all (it stays there so the gate
// doesn't statically pull this module back into the initial bundle).

import { PrismLight as SyntaxHighlighter } from "react-syntax-highlighter";
import pythonGrammar from "react-syntax-highlighter/dist/esm/languages/prism/python";
import bashGrammar from "react-syntax-highlighter/dist/esm/languages/prism/bash";
import jsonGrammar from "react-syntax-highlighter/dist/esm/languages/prism/json";
import yamlGrammar from "react-syntax-highlighter/dist/esm/languages/prism/yaml";
SyntaxHighlighter.registerLanguage("python", pythonGrammar);
SyntaxHighlighter.registerLanguage("bash", bashGrammar);
SyntaxHighlighter.registerLanguage("shell", bashGrammar);
SyntaxHighlighter.registerLanguage("json", jsonGrammar);
SyntaxHighlighter.registerLanguage("yaml", yamlGrammar);

// syntaxStyle is a react-syntax-highlighter style object: keys are
// Prism token classes, values are CSS-in-JS objects. We use CSS var
// references so the colors flip with the app's light/dark theme.
const syntaxStyle: Record<string, React.CSSProperties> = {
  'code[class*="language-"]': {
    color: "var(--color-text-primary)",
    background: "transparent",
    fontFamily: "var(--font-code)",
  },
  'pre[class*="language-"]': {
    color: "var(--color-text-primary)",
    background: "transparent",
    fontFamily: "var(--font-code)",
    margin: 0,
    padding: 0,
  },
  comment: { color: "var(--color-syntax-comment)", fontStyle: "italic" },
  prolog: { color: "var(--color-syntax-comment)" },
  doctype: { color: "var(--color-syntax-comment)" },
  cdata: { color: "var(--color-syntax-comment)" },
  punctuation: { color: "var(--color-syntax-punctuation)" },
  property: { color: "var(--color-syntax-builtin)" },
  tag: { color: "var(--color-syntax-keyword)" },
  boolean: { color: "var(--color-syntax-number)" },
  number: { color: "var(--color-syntax-number)" },
  constant: { color: "var(--color-syntax-number)" },
  symbol: { color: "var(--color-syntax-builtin)" },
  deleted: { color: "var(--color-danger)" },
  selector: { color: "var(--color-syntax-string)" },
  "attr-name": { color: "var(--color-syntax-builtin)" },
  string: { color: "var(--color-syntax-string)" },
  char: { color: "var(--color-syntax-string)" },
  builtin: { color: "var(--color-syntax-builtin)" },
  inserted: { color: "var(--color-success)" },
  operator: { color: "var(--color-syntax-operator)" },
  entity: { color: "var(--color-syntax-builtin)", cursor: "help" },
  url: { color: "var(--color-syntax-builtin)" },
  variable: { color: "var(--color-text-primary)" },
  atrule: { color: "var(--color-syntax-keyword)" },
  "attr-value": { color: "var(--color-syntax-string)" },
  function: { color: "var(--color-syntax-function)" },
  "class-name": { color: "var(--color-syntax-function)" },
  keyword: { color: "var(--color-syntax-keyword)" },
  regex: { color: "var(--color-syntax-number)" },
  important: { color: "var(--color-danger)", fontWeight: "bold" },
  bold: { fontWeight: "bold" },
  italic: { fontStyle: "italic" },
  decorator: { color: "var(--color-syntax-function)" },
  "triple-quoted-string": { color: "var(--color-syntax-string)" },
};

// Default export so ToolChips can `React.lazy(() => import("./CodeHighlight"))`
// without an intermediate `.then`.
export default function HighlightedCode({
  code,
  language,
}: {
  code: string;
  language: string;
}) {
  return (
    <SyntaxHighlighter
      language={language}
      style={syntaxStyle}
      PreTag="pre"
      CodeTag="code"
      wrapLongLines={false}
      customStyle={{
        background: "transparent",
        padding: 0,
        margin: 0,
        fontSize: "0.72rem",
        lineHeight: 1.4,
        fontFamily: "var(--font-code)",
      }}
      codeTagProps={{ style: { fontFamily: "var(--font-code)" } }}
    >
      {code}
    </SyntaxHighlighter>
  );
}
