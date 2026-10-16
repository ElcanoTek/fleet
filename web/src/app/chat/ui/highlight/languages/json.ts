// @ts-nocheck -- vendored grammar data, typed loosely as upstream ships it.
// Prism's json grammar (prismjs 1.30.0, components/prism-json.js) in the
// ES-module form refractor 5.0.0 ships as lang/json.js, modified: registered
// on fleet's tokenizer (../prism.ts) instead of refractor. Copyright (c) 2012
// Lea Verou (Prism) and Titus Wormer (refractor); see ../LICENSE.prism and
// ../LICENSE.refractor. SPDX-License-Identifier: MIT

/* eslint-disable no-useless-escape, no-control-regex -- the regexes are Prism's, kept byte for byte so the tokens stay upstream's. */

import type { PrismLike } from "../prism";

export function json(Prism: PrismLike): void {
  // https://www.json.org/json-en.html
  Prism.languages.json = {
    property: {
      pattern: /(^|[^\\])"(?:\\.|[^\\"\r\n])*"(?=\s*:)/,
      lookbehind: true,
      greedy: true
    },
    string: {
      pattern: /(^|[^\\])"(?:\\.|[^\\"\r\n])*"(?!\s*:)/,
      lookbehind: true,
      greedy: true
    },
    comment: {
      pattern: /\/\/.*|\/\*[\s\S]*?(?:\*\/|$)/,
      greedy: true
    },
    number: /-?\b\d+(?:\.\d+)?(?:e[+-]?\d+)?\b/i,
    punctuation: /[{}[\],]/,
    operator: /:/,
    boolean: /\b(?:false|true)\b/,
    null: {
      pattern: /\bnull\b/,
      alias: 'keyword'
    }
  }
  Prism.languages.webmanifest = Prism.languages.json
}
