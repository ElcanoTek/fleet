// The generative-UI card expression language — the browser half of a pinned
// pair with internal/genui/expr.go (which only parses; this file parses AND
// evaluates). Both run the shared fixture internal/genui/testdata/expressions.json.
//
// A hand-written interpreter, never eval()/new Function(): the source is
// model-authored, and the only names an expression can reach are the card's
// own values passed in `scope`. There is no property access on anything but
// plain card data, no calls except the FUNCTIONS whitelist, no assignment.
//
// Semantics are deliberately forgiving — a card mid-edit is full of blanks —
// so nothing throws at evaluation time: a missing name is null, null is 0 in
// arithmetic, division by zero is null, and a type mismatch yields null
// rather than NaN. Parse errors DO throw (ExprError); the renderer catches
// them and draws an inline warning instead of breaking the card.

export type Value = null | boolean | number | string | Value[] | { [k: string]: Value };
export type Scope = Record<string, unknown>;

export class ExprError extends Error {}

export const MAX_EXPR_LEN = 500;
const MAX_DEPTH = 40;

type Tok = { kind: "num" | "str" | "ident" | "op" | "eof"; text: string; pos: number };

function lex(src: string): Tok[] {
  const toks: Tok[] = [];
  const rs = Array.from(src);
  let i = 0;
  const isDigit = (c: string | undefined) => c !== undefined && c >= "0" && c <= "9";
  const isIdentStart = (c: string) => c === "_" || /\p{L}/u.test(c);
  const isIdent = (c: string | undefined) => c !== undefined && (c === "_" || /[\p{L}\p{Nd}]/u.test(c));
  while (i < rs.length) {
    const c = rs[i];
    if (/\s/u.test(c)) {
      i++;
    } else if (isDigit(c) || (c === "." && isDigit(rs[i + 1]))) {
      const start = i;
      let seenDot = false;
      while (i < rs.length && (isDigit(rs[i]) || (rs[i] === "." && !seenDot))) {
        if (rs[i] === ".") {
          if (!isDigit(rs[i + 1])) break;
          seenDot = true;
        }
        i++;
      }
      const lit = rs.slice(start, i).join("");
      // A literal too large for a double would read as Infinity: refused on
      // both sides (internal/genui/expr.go lex).
      if (!Number.isFinite(Number(lit))) throw new ExprError(`number ${lit.slice(0, 20)} at ${start} is too large`);
      toks.push({ kind: "num", text: lit, pos: start });
    } else if (c === "'" || c === '"') {
      const start = i;
      i++;
      let out = "";
      let closed = false;
      while (i < rs.length) {
        if (rs[i] === "\\" && i + 1 < rs.length) {
          out += rs[i + 1];
          i += 2;
          continue;
        }
        if (rs[i] === c) {
          closed = true;
          i++;
          break;
        }
        out += rs[i];
        i++;
      }
      if (!closed) throw new ExprError(`unterminated string starting at ${start}`);
      toks.push({ kind: "str", text: out, pos: start });
    } else if (isIdentStart(c)) {
      const start = i;
      while (i < rs.length && isIdent(rs[i])) i++;
      toks.push({ kind: "ident", text: rs.slice(start, i).join(""), pos: start });
    } else {
      const two = rs.slice(i, i + 2).join("");
      if (["==", "!=", "<=", ">=", "&&", "||"].includes(two)) {
        toks.push({ kind: "op", text: two, pos: i });
        i += 2;
      } else if ("+-*/%<>!?:().,".includes(c)) {
        toks.push({ kind: "op", text: c, pos: i });
        i++;
      } else {
        throw new ExprError(`unexpected character ${JSON.stringify(c)} at ${i}`);
      }
    }
  }
  toks.push({ kind: "eof", text: "", pos: rs.length });
  return toks;
}

export type Node =
  | { t: "lit"; v: Value }
  | { t: "ref"; name: string }
  | { t: "member"; obj: Node; name: string }
  | { t: "call"; fn: string; args: Node[] }
  | { t: "unary"; op: string; arg: Node }
  | { t: "bin"; op: string; l: Node; r: Node }
  | { t: "cond"; c: Node; a: Node; b: Node };

// Function whitelist with arity bounds (max -1 = variadic). Keep in step with
// exprFuncs in internal/genui/expr.go.
const ARITY: Record<string, [number, number]> = {
  len: [1, 1],
  count: [1, 1],
  sum: [1, 1],
  avg: [1, 1],
  min: [1, -1],
  max: [1, -1],
  abs: [1, 1],
  floor: [1, 1],
  ceil: [1, 1],
  round: [1, 2],
  fixed: [2, 2],
  number: [1, 1],
  string: [1, 1],
  upper: [1, 1],
  lower: [1, 1],
  join: [1, 2],
  contains: [2, 2],
  empty: [1, 1],
  unique: [1, 1],
};

const LEVELS: string[][] = [["||"], ["&&"], ["==", "!="], ["<", "<=", ">", ">="], ["+", "-"], ["*", "/", "%"]];

class Parser {
  private pos = 0;
  private depth = 0;
  constructor(private toks: Tok[]) {}

  private peek() {
    return this.toks[this.pos];
  }
  private next() {
    const t = this.toks[this.pos];
    if (this.pos < this.toks.length - 1) this.pos++;
    return t;
  }
  private isOp(s: string) {
    const t = this.peek();
    return t.kind === "op" && t.text === s;
  }
  private fail(msg: string): never {
    const t = this.peek();
    throw new ExprError(`${msg} (near ${t.kind === "eof" ? "end of expression" : `"${t.text}" at ${t.pos}`})`);
  }
  private expect(s: string) {
    if (!this.isOp(s)) this.fail(`expected "${s}"`);
    this.next();
  }
  private enter() {
    if (++this.depth > MAX_DEPTH) this.fail("expression nested too deeply");
  }

  parseAll(): Node {
    const n = this.expr();
    if (this.peek().kind !== "eof") this.fail("unexpected trailing input");
    return n;
  }

  expr(): Node {
    this.enter();
    try {
      const c = this.binary(0);
      if (this.isOp("?")) {
        this.next();
        const a = this.expr();
        this.expect(":");
        const b = this.expr();
        return { t: "cond", c, a, b };
      }
      return c;
    } finally {
      this.depth--;
    }
  }

  binary(level: number): Node {
    if (level === LEVELS.length) return this.unary();
    let l = this.binary(level + 1);
    for (;;) {
      const t = this.peek();
      if (t.kind !== "op" || !LEVELS[level].includes(t.text)) return l;
      this.next();
      const r = this.binary(level + 1);
      l = { t: "bin", op: t.text, l, r };
    }
  }

  unary(): Node {
    if (this.isOp("!") || this.isOp("-")) {
      const op = this.next().text;
      this.enter();
      try {
        return { t: "unary", op, arg: this.unary() };
      } finally {
        this.depth--;
      }
    }
    let n = this.primary();
    while (this.isOp(".")) {
      this.next();
      const t = this.peek();
      if (t.kind !== "ident") this.fail("expected a field name after '.'");
      this.next();
      n = { t: "member", obj: n, name: t.text };
    }
    return n;
  }

  primary(): Node {
    const t = this.peek();
    if (t.kind === "num") {
      this.next();
      return { t: "lit", v: Number(t.text) };
    }
    if (t.kind === "str") {
      this.next();
      return { t: "lit", v: t.text };
    }
    if (t.kind === "ident") {
      this.next();
      if (t.text === "true") return { t: "lit", v: true };
      if (t.text === "false") return { t: "lit", v: false };
      if (t.text === "null") return { t: "lit", v: null };
      if (this.isOp("(")) {
        const arity = Object.prototype.hasOwnProperty.call(ARITY, t.text) ? ARITY[t.text] : undefined;
        if (!arity) throw new ExprError(`unknown function "${t.text}"`);
        this.next();
        const args: Node[] = [];
        if (!this.isOp(")")) {
          for (;;) {
            args.push(this.expr());
            if (this.isOp(",")) {
              this.next();
              continue;
            }
            break;
          }
        }
        this.expect(")");
        if (args.length < arity[0] || (arity[1] >= 0 && args.length > arity[1])) {
          throw new ExprError(`${t.text}() takes ${arity[0]}${arity[1] < 0 ? "+" : arity[1] !== arity[0] ? `-${arity[1]}` : ""} argument(s), got ${args.length}`);
        }
        return { t: "call", fn: t.text, args };
      }
      return { t: "ref", name: t.text };
    }
    if (this.isOp("(")) {
      this.next();
      const n = this.expr();
      this.expect(")");
      return n;
    }
    this.fail("expected a value");
  }
}

const cache = new Map<string, Node | ExprError>();

/** Parse (memoized). Throws ExprError on a syntax error. */
export function parseExpr(src: string): Node {
  const hit = cache.get(src);
  if (hit instanceof ExprError) throw hit;
  if (hit) return hit;
  try {
    if (!src.trim()) throw new ExprError("empty expression");
    if (src.length > MAX_EXPR_LEN) throw new ExprError(`expression longer than ${MAX_EXPR_LEN} characters`);
    const node = new Parser(lex(src)).parseAll();
    if (cache.size > 2000) cache.clear();
    cache.set(src, node);
    return node;
  } catch (e) {
    const err = e instanceof ExprError ? e : new ExprError(String(e));
    cache.set(src, err);
    throw err;
  }
}

// ── evaluation ──

/** Normalize arbitrary card data into the Value domain (drops functions etc.). */
// A scope list is normalized once per array: card values are replaced, never
// mutated, so the same array always normalizes to the same result — and
// keeping that result one array lets the join and unique caches below hit
// across every template that reads the list.
const normCache = new WeakMap<unknown[], Value[]>();

function norm(v: unknown): Value {
  if (v === null || v === undefined) return null;
  if (typeof v === "boolean" || typeof v === "string") return v;
  if (typeof v === "number") return Number.isFinite(v) ? v : null;
  if (Array.isArray(v)) {
    const hit = normCache.get(v);
    if (hit) return hit;
    const out = v.map(norm);
    normCache.set(v, out);
    return out;
  }
  if (typeof v === "object") return v as { [k: string]: Value };
  return null;
}

export function truthy(v: Value): boolean {
  if (v === null || v === false || v === 0 || v === "") return false;
  if (Array.isArray(v)) return v.length > 0;
  return true;
}

/** Numeric view: null → 0, numeric strings → numbers, otherwise NaN. */
function num(v: Value): number {
  if (v === null) return 0;
  if (typeof v === "number") return v;
  if (typeof v === "boolean") return v ? 1 : 0;
  if (typeof v === "string") {
    const s = v.trim();
    if (s === "") return 0;
    const n = Number(s);
    return Number.isFinite(n) ? n : NaN;
  }
  return NaN;
}

function numOrNull(n: number): Value {
  return Number.isFinite(n) ? n : null;
}

/** Display formatting shared by templates and string(). */
export function toText(v: Value): string {
  if (v === null) return "";
  if (typeof v === "number") {
    if (Number.isInteger(v)) return String(v);
    // Arithmetic noise (0.1 + 0.2 = 0.30000000000000004) sits within a few
    // units in the last place of a 15-digit value; show that value then.
    // Any other number keeps its shortest exact form, so 1e-11 and
    // 1.234567890123456 are shown as they are. From 1e13 up a 15-digit
    // value keeps under two decimals, and a few units in the last place are
    // a real fraction (1000000000000000.1), so large values are always exact.
    if (Math.abs(v) >= 1e13) return String(v);
    const short = Number(v.toPrecision(15));
    return String(Math.abs(short - v) <= 4 * Number.EPSILON * Math.abs(v) ? short : v);
  }
  if (typeof v === "boolean") return v ? "true" : "false";
  if (typeof v === "string") return v;
  if (Array.isArray(v)) return joinList(v, ", ");
  return "";
}

/**
 * The longest string an expression produces (join, a list shown as text,
 * text concatenation). A list can hold 20,000 lines and a separator 20,000
 * characters; without a bound one evaluation could build hundreds of MB.
 * Every context (display, conditions, computed values) sees the same
 * bounded value, so they cannot disagree.
 */
export const MAX_EXPR_STRING = 1_000_000;

function bounded(s: string): string {
  return s.length > MAX_EXPR_STRING ? s.slice(0, MAX_EXPR_STRING) : s;
}

// Recent case conversions each way. Many templates may upper() or lower()
// the same long text (a joined list is one cached string), and Unicode case
// mapping can grow it, so each conversion is done once and stored bounded
// rather than rebuilt per template per render. A few texts are kept each
// way (oldest dropped first), so templates alternating between texts hit.
const CASE_CACHE_ENTRIES = 4;
const caseCache = { upper: new Map<string, string>(), lower: new Map<string, string>() };

function convertCase(s: string, upper: boolean): string {
  const cache = upper ? caseCache.upper : caseCache.lower;
  const hit = cache.get(s);
  if (hit !== undefined) return hit;
  const out = bounded(upper ? s.toUpperCase() : s.toLowerCase());
  if (cache.size >= CASE_CACHE_ENTRIES) cache.delete(cache.keys().next().value as string);
  cache.set(s, out);
  return out;
}

// The latest joins of each list: many templates may join (or show) the same
// list, which is one array object for as long as it is unchanged, so each
// joined string is built once, not once per template per render. A few
// separators are kept per list, so templates alternating between them do
// not rebuild it each time; the oldest goes first, so a separator the user
// is typing cannot pile up results.
const JOIN_CACHE_SEPARATORS = 8;
const joined = new WeakMap<object, Map<string, string>>();

function joinList(v: Value[], glue: string): string {
  let byGlue = joined.get(v);
  const hit = byGlue?.get(glue);
  if (hit !== undefined) return hit;
  let out = "";
  for (const x of v) {
    const t = toText(x);
    if (t === "") continue;
    out = out === "" ? t : out + glue + t;
    if (out.length > MAX_EXPR_STRING) {
      out = out.slice(0, MAX_EXPR_STRING);
      break;
    }
  }
  if (!byGlue) joined.set(v, (byGlue = new Map()));
  if (byGlue.size >= JOIN_CACHE_SEPARATORS) byGlue.delete(byGlue.keys().next().value as string);
  byGlue.set(glue, out);
  return out;
}

function list(v: Value): Value[] {
  if (Array.isArray(v)) return v;
  if (v === null) return [];
  return [v];
}

function numbers(args: Value[]): number[] {
  const flat = args.length === 1 ? list(args[0]) : args;
  return flat.filter((x) => x !== null && x !== "").map(num).filter((n) => Number.isFinite(n));
}

function roundTo(n: number, digits: number): number {
  const f = 10 ** Math.max(0, Math.min(10, Math.trunc(digits)));
  // A huge input overflows when scaled; it has no fractional digits to
  // round anyway (every double above 2^53 is an integer).
  const scaled = (n + Number.EPSILON * Math.sign(n)) * f;
  if (!Number.isFinite(scaled)) return n;
  return Math.round(scaled) / f;
}

function equal(a: Value, b: Value): boolean {
  if (a === null || b === null) return a === b;
  if (typeof a === "number" || typeof b === "number") {
    const x = num(a);
    const y = num(b);
    if (Number.isFinite(x) && Number.isFinite(y) && (typeof a !== "string" || a.trim() !== "") && (typeof b !== "string" || b.trim() !== "")) {
      return x === y;
    }
  }
  if (typeof a === "object" || typeof b === "object") return false;
  return a === b;
}

// unique() keeps each item no earlier item is `equal` to, in order — the
// same result as scanning the output for every item, but indexed, because a
// list_input can hold 20,000 entries and expressions re-run on every render.
// `equal` matches a number to the same number, to a non-blank numeric string
// of that value, and to true/false as 1/0; any other string or boolean only
// to itself; null to null; an object or list to nothing. Each set below
// indexes one of those cases.
// One result per source list: many templates may unique() the same list,
// which is one array object for as long as it is unchanged, so it is
// deduplicated once and the result (also one array) reaches joinList's cache.
const uniqueCache = new WeakMap<Value[], Value[]>();

function uniqueOf(items: Value[]): Value[] {
  const hit = uniqueCache.get(items);
  if (hit) return hit;
  const out = uniqueValues(items);
  uniqueCache.set(items, out);
  return out;
}

function uniqueValues(items: Value[]): Value[] {
  const out: Value[] = [];
  let sawNull = false;
  const nums = new Set<number>();
  const numericStrs = new Set<number>();
  const strs = new Set<string>();
  const bools = new Set<boolean>();
  for (const x of items) {
    if (x === null) {
      if (sawNull) continue;
      sawNull = true;
    } else if (typeof x === "number") {
      if (nums.has(x) || numericStrs.has(x) || (x === 1 && bools.has(true)) || (x === 0 && bools.has(false))) continue;
      nums.add(x);
    } else if (typeof x === "string") {
      const n = x.trim() === "" ? NaN : num(x);
      if (strs.has(x) || (Number.isFinite(n) && nums.has(n))) continue;
      strs.add(x);
      if (Number.isFinite(n)) numericStrs.add(n);
    } else if (typeof x === "boolean") {
      if (bools.has(x) || nums.has(x ? 1 : 0)) continue;
      bools.add(x);
    }
    out.push(x);
  }
  return out;
}

const FUNCS: Record<string, (args: Value[]) => Value> = {
  len: ([v]) => (typeof v === "string" ? Array.from(v).length : list(v).length),
  count: ([v]) => list(v).length,
  // Results stay finite like every other value (norm): an overflowing sum is
  // null, and the mean is a running mean — each step a weighted average of
  // the mean so far and the next value, so it never exceeds the largest
  // input when the inputs are finite.
  sum: (a) => numOrNull(numbers(a).reduce((s, n) => s + n, 0)),
  avg: (a) => {
    const ns = numbers(a);
    return ns.length
      ? numOrNull(
          ns.reduce((m, n, i) => {
            const d = n - m;
            // n - m overflows only when the two have opposite signs and are
            // both huge; weigh them separately then.
            return Number.isFinite(d) ? m + d / (i + 1) : m - m / (i + 1) + n / (i + 1);
          }, 0),
        )
      : null;
  },
  min: (a) => {
    const ns = numbers(a);
    return ns.length ? Math.min(...ns) : null;
  },
  max: (a) => {
    const ns = numbers(a);
    return ns.length ? Math.max(...ns) : null;
  },
  abs: ([v]) => numOrNull(Math.abs(num(v))),
  floor: ([v]) => numOrNull(Math.floor(num(v))),
  ceil: ([v]) => numOrNull(Math.ceil(num(v))),
  round: ([v, d]) => {
    const n = num(v);
    return Number.isFinite(n) ? roundTo(n, d === undefined ? 0 : num(d)) : null;
  },
  fixed: ([v, d]) => {
    const n = num(v);
    const digits = Math.max(0, Math.min(10, Math.trunc(num(d)) || 0));
    return Number.isFinite(n) ? roundTo(n, digits).toFixed(digits) : null;
  },
  number: ([v]) => numOrNull(num(v)),
  string: ([v]) => toText(v),
  // Case mapping can lengthen text (ß becomes SS): bounded like any string.
  upper: ([v]) => convertCase(toText(v), true),
  lower: ([v]) => convertCase(toText(v), false),
  join: ([v, sep]) => joinList(list(v), sep === undefined ? ", " : toText(sep)),
  contains: ([hay, needle]) => {
    if (typeof hay === "string") return hay.includes(toText(needle));
    return list(hay).some((x) => equal(x, needle));
  },
  empty: ([v]) => {
    if (v === null) return true;
    if (typeof v === "string") return v.trim() === "";
    if (Array.isArray(v)) return v.length === 0;
    if (typeof v === "object") return Object.values(v).every((x) => Array.isArray(x) && x.length === 0);
    return false;
  },
  unique: ([v]) => uniqueOf(list(v)),
};

function member(obj: Value, name: string): Value {
  if (Array.isArray(obj)) return obj.map((x) => member(x, name));
  if (obj !== null && typeof obj === "object") {
    return Object.prototype.hasOwnProperty.call(obj, name) ? norm(obj[name]) : null;
  }
  return null;
}

function ev(n: Node, scope: Scope): Value {
  switch (n.t) {
    case "lit":
      return n.v;
    case "ref":
      return Object.prototype.hasOwnProperty.call(scope, n.name) ? norm(scope[n.name]) : null;
    case "member":
      return member(ev(n.obj, scope), n.name);
    case "call": {
      // One choke point keeps every function's result in the Value domain:
      // a number that is not finite (an overflow, a NaN) becomes null, so no
      // function can leak "Infinity" or "NaN" into a template or condition.
      const out = FUNCS[n.fn](n.args.map((a) => ev(a, scope)));
      return typeof out === "number" ? numOrNull(out) : out;
    }
    case "unary": {
      const v = ev(n.arg, scope);
      return n.op === "!" ? !truthy(v) : numOrNull(-num(v));
    }
    case "cond":
      return truthy(ev(n.c, scope)) ? ev(n.a, scope) : ev(n.b, scope);
    case "bin": {
      if (n.op === "&&") return truthy(ev(n.l, scope)) && truthy(ev(n.r, scope));
      if (n.op === "||") return truthy(ev(n.l, scope)) || truthy(ev(n.r, scope));
      const l = ev(n.l, scope);
      const r = ev(n.r, scope);
      switch (n.op) {
        case "==":
          return equal(l, r);
        case "!=":
          return !equal(l, r);
        case "+": {
          // Two strings join. One string beside a number adds when the string
          // is numeric (a text input holding "3") and joins otherwise.
          const ls = typeof l === "string";
          const rs = typeof r === "string";
          if (ls && rs) return bounded(l + r);
          if (ls || rs) {
            const s = (ls ? l : r) as string;
            const numeric = s.trim() !== "" && Number.isFinite(num(s)) && Number.isFinite(num(ls ? r : l));
            if (!numeric) return bounded(toText(l) + toText(r));
          }
          return numOrNull(num(l) + num(r));
        }
        case "-":
          return numOrNull(num(l) - num(r));
        case "*":
          return numOrNull(num(l) * num(r));
        case "/": {
          const d = num(r);
          return d === 0 ? null : numOrNull(num(l) / d);
        }
        case "%": {
          const d = num(r);
          return d === 0 ? null : numOrNull(num(l) % d);
        }
        default: {
          // < <= > >= : numeric when both sides are numeric, else text order.
          // null (a blank number) is a scalar 0 here as in arithmetic; only
          // lists and objects are never numeric.
          const ln = num(l);
          const rn = num(r);
          const scalar = (v: Value) => v === null || typeof v !== "object";
          const numeric = Number.isFinite(ln) && Number.isFinite(rn) && scalar(l) && scalar(r);
          const [x, y] = numeric ? [ln, rn] : [toText(l), toText(r)];
          if (n.op === "<") return x < y;
          if (n.op === "<=") return x <= y;
          if (n.op === ">") return x > y;
          return x >= y;
        }
      }
    }
  }
}

/** Evaluate an expression. Throws ExprError only on a parse error. */
export function evaluate(src: string, scope: Scope): Value {
  return ev(parseExpr(src), scope);
}

/** Like evaluate, but a parse error becomes `fallback`. */
export function evaluateSafe(src: string, scope: Scope, fallback: Value = null): Value {
  try {
    return evaluate(src, scope);
  } catch {
    return fallback;
  }
}

/**
 * Render a display string's {{ expr }} holes. A hole that fails to parse
 * renders as "⚠" rather than throwing — the server validated the spec, so
 * this only guards a renderer/validator drift.
 */
export function renderTemplate(s: string | undefined, scope: Scope): string {
  if (!s || !s.includes("{{")) return s ?? "";
  let left = MAX_TEMPLATE_CHARS;
  return s.replace(/\{\{([\s\S]*?)\}\}/g, (_, src: string) => {
    let t: string;
    try {
      t = toText(evaluate(src, scope));
    } catch {
      return "⚠";
    }
    if (t.length > left) t = `${t.slice(0, Math.max(0, left))}…`;
    left -= t.length;
    return t;
  });
}

/**
 * The most text one template's holes add. A hole can expand an input far
 * past the spec's own size (join over a 20,000-line list), and a card can
 * repeat it in hundreds of components; displayed hole text is clipped, while
 * conditions and computed values still see the whole result.
 */
export const MAX_TEMPLATE_CHARS = 2000;
