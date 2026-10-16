// Prism's tokenizer: Token, tokenize and matchGrammar from prismjs 1.30.0
// (components/prism-core.js), ported to TypeScript and trimmed to the
// tokenizer — no DOM highlighting, plugins, hooks or language helpers.
// Copyright (c) 2012 Lea Verou; see LICENSE.prism. Modified.
// SPDX-License-Identifier: MIT

/** A grammar: token name -> pattern(s). Loosely typed, as Prism's grammars are. */
// The values are RegExps, pattern objects or arrays of either; `rest` merges
// another grammar in on first use.
// oxlint-disable-next-line typescript/no-explicit-any -- grammar data is heterogeneous by design
export type Grammar = Record<string, any>;

/** The registry the grammar files write into. */
export interface PrismLike {
  languages: Record<string, Grammar>;
}

/** A matched token: its type, alias(es) and content (text or nested stream). */
export class Token {
  type: string;
  content: TokenStream | string;
  alias: string | string[] | undefined;
  length: number;

  constructor(type: string, content: TokenStream | string, alias: string | string[] | undefined, matchedStr: string) {
    this.type = type;
    this.content = content;
    this.alias = alias;
    this.length = matchedStr.length;
  }
}

export type TokenStream = Array<string | Token>;

interface ListNode {
  value: string | Token | null;
  prev: ListNode | null;
  next: ListNode | null;
}

interface List {
  head: ListNode;
  tail: ListNode;
  length: number;
}

function newList(): List {
  const head: ListNode = { value: null, prev: null, next: null };
  const tail: ListNode = { value: null, prev: head, next: null };
  head.next = tail;
  return { head, tail, length: 0 };
}

function addAfter(list: List, node: ListNode, value: string | Token): ListNode {
  const next = node.next as ListNode;
  const newNode: ListNode = { value, prev: node, next };
  node.next = newNode;
  next.prev = newNode;
  list.length++;
  return newNode;
}

function removeRange(list: List, node: ListNode, count: number): void {
  let next = node.next as ListNode;
  let i = 0;
  for (; i < count && next !== list.tail; i++) {
    next = next.next as ListNode;
  }
  node.next = next;
  next.prev = node;
  list.length -= i;
}

function toArray(list: List): TokenStream {
  const out: TokenStream = [];
  let node = list.head.next as ListNode;
  while (node !== list.tail) {
    out.push(node.value as string | Token);
    node = node.next as ListNode;
  }
  return out;
}

const valueLength = (v: string | Token | null): number => (v === null ? 0 : v.length);

function matchPattern(pattern: RegExp, pos: number, text: string, lookbehind: boolean): RegExpExecArray | null {
  pattern.lastIndex = pos;
  const match = pattern.exec(text);
  if (match && lookbehind && match[1]) {
    // Drop the text matched by Prism's lookbehind group.
    const lookbehindLength = match[1].length;
    match.index += lookbehindLength;
    match[0] = match[0].slice(lookbehindLength);
  }
  return match;
}

interface Rematch {
  cause: string;
  reach: number;
}

/** Tokenizes text with grammar into a stream of strings and Tokens. */
export function tokenize(text: string, grammar: Grammar): TokenStream {
  const rest = grammar.rest as Grammar | undefined;
  if (rest) {
    for (const token in rest) {
      grammar[token] = rest[token];
    }
    delete grammar.rest;
  }
  const list = newList();
  addAfter(list, list.head, text);
  matchGrammar(text, list, grammar, list.head, 0);
  return toArray(list);
}

// matchGrammar is Prism's, line for line; see prism-core.js for the commentary.
function matchGrammar(text: string, list: List, grammar: Grammar, startNode: ListNode, startPos: number, rematch?: Rematch): void {
  for (const token in grammar) {
    if (!Object.prototype.hasOwnProperty.call(grammar, token) || !grammar[token]) {
      continue;
    }
    const patterns = Array.isArray(grammar[token]) ? grammar[token] : [grammar[token]];

    for (let j = 0; j < patterns.length; ++j) {
      if (rematch && rematch.cause === token + "," + j) {
        return;
      }
      const patternObj = patterns[j];
      const inside = patternObj.inside as Grammar | undefined;
      const lookbehind = !!patternObj.lookbehind;
      const greedy = !!patternObj.greedy;
      const alias = patternObj.alias as string | string[] | undefined;

      if (greedy && !patternObj.pattern.global) {
        // Without the global flag, lastIndex won't work.
        const flags = (patternObj.pattern.toString().match(/[imsuy]*$/) as RegExpMatchArray)[0];
        patternObj.pattern = RegExp(patternObj.pattern.source, flags + "g");
      }
      const pattern: RegExp = patternObj.pattern || patternObj;

      for (
        let currentNode = startNode.next as ListNode, pos = startPos;
        currentNode !== list.tail;
        pos += valueLength(currentNode.value), currentNode = currentNode.next as ListNode
      ) {
        if (rematch && pos >= rematch.reach) {
          break;
        }
        let str = currentNode.value;
        if (list.length > text.length) {
          // Something went terribly wrong, ABORT, ABORT!
          return;
        }
        if (str instanceof Token) {
          continue;
        }

        let removeCount = 1;
        let match: RegExpExecArray | null;

        if (greedy) {
          match = matchPattern(pattern, pos, text, lookbehind);
          if (!match || match.index >= text.length) {
            break;
          }
          const from = match.index;
          const to = match.index + match[0].length;
          let p = pos;

          // Find the node that contains the match.
          p += valueLength(currentNode.value);
          while (from >= p) {
            currentNode = currentNode.next as ListNode;
            p += valueLength(currentNode.value);
          }
          p -= valueLength(currentNode.value);
          pos = p;

          // A match starting inside another Token is invalid.
          if (currentNode.value instanceof Token) {
            continue;
          }

          // Find the last node affected by this match.
          for (let k = currentNode; k !== list.tail && (p < to || typeof k.value === "string"); k = k.next as ListNode) {
            removeCount++;
            p += valueLength(k.value);
          }
          removeCount--;

          str = text.slice(pos, p);
          match.index -= pos;
        } else {
          match = matchPattern(pattern, 0, str as string, lookbehind);
          if (!match) {
            continue;
          }
        }

        const s = str as string;
        const from = match.index;
        const matchStr = match[0];
        const before = s.slice(0, from);
        const after = s.slice(from + matchStr.length);

        const reach = pos + s.length;
        if (rematch && reach > rematch.reach) {
          rematch.reach = reach;
        }

        let removeFrom = currentNode.prev as ListNode;
        if (before) {
          removeFrom = addAfter(list, removeFrom, before);
          pos += before.length;
        }
        removeRange(list, removeFrom, removeCount);

        const wrapped = new Token(token, inside ? tokenize(matchStr, inside) : matchStr, alias, matchStr);
        currentNode = addAfter(list, removeFrom, wrapped);
        if (after) {
          addAfter(list, currentNode, after);
        }

        if (removeCount > 1) {
          // A Token was removed, so rematch (only a greedy pattern gets here).
          const nestedRematch: Rematch = { cause: token + "," + j, reach };
          matchGrammar(text, list, grammar, currentNode.prev as ListNode, pos, nestedRematch);
          if (rematch && nestedRematch.reach > rematch.reach) {
            rematch.reach = nestedRematch.reach;
          }
        }
      }
    }
  }
}
