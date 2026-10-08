package genui

import (
	"fmt"
	"strings"
	"unicode"
)

// The card expression language. A generated card is data, never code, but a
// form that cannot compute anything is a poor substitute for the bespoke UIs
// it replaces (a live deal count, a budget total, a field shown only when
// another has a given value). So the spec carries a deliberately tiny,
// side-effect-free expression language: literals, field references, the
// usual arithmetic / comparison / boolean operators, a ternary, member access
// and a fixed function whitelist. There is no assignment, no loop, no lambda,
// no property access on anything but the card's own values, and no way to
// name a browser global — the web evaluator (web/src/app/chat/ui/genui/expr.ts)
// is a hand-written interpreter over the same grammar, never eval().
//
// This file is the Go half of a pinned pair: it only PARSES (syntax + which
// names an expression reads) so a bad expression is refused back to the model
// at tool-call time, where it can fix it, instead of rendering as a broken
// widget the model never hears about. Evaluation lives in the browser. Both
// halves run the shared fixture testdata/expressions.json.

// MaxExprLen bounds one expression's source. Expressions are one-liners by
// design; anything longer is the model writing a program, which this
// language is not for.
const MaxExprLen = 500

// exprFuncs is the function whitelist with its arity bounds (max -1 =
// variadic). Keep in step with FUNCTIONS in expr.ts.
var exprFuncs = map[string][2]int{
	"len":      {1, 1},
	"count":    {1, 1},
	"sum":      {1, 1},
	"avg":      {1, 1},
	"min":      {1, -1},
	"max":      {1, -1},
	"abs":      {1, 1},
	"floor":    {1, 1},
	"ceil":     {1, 1},
	"round":    {1, 2},
	"fixed":    {2, 2},
	"number":   {1, 1},
	"string":   {1, 1},
	"upper":    {1, 1},
	"lower":    {1, 1},
	"join":     {1, 2},
	"contains": {2, 2},
	"empty":    {1, 1},
	"unique":   {1, 1},
}

// FunctionNames lists the whitelist, for the tool description and errors.
func FunctionNames() []string {
	out := make([]string, 0, len(exprFuncs))
	for k := range exprFuncs {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

type tokKind int

const (
	tEOF tokKind = iota
	tNum
	tStr
	tIdent
	tOp
)

type token struct {
	kind tokKind
	text string
	pos  int
}

func lex(src string) ([]token, error) {
	var toks []token
	rs := []rune(src)
	i := 0
	for i < len(rs) {
		c := rs[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case unicode.IsDigit(c) || (c == '.' && i+1 < len(rs) && unicode.IsDigit(rs[i+1])):
			start := i
			seenDot := false
			for i < len(rs) && (unicode.IsDigit(rs[i]) || (rs[i] == '.' && !seenDot)) {
				if rs[i] == '.' {
					// "a.b" member access after a number is not a thing; a
					// second dot ends the literal.
					if i+1 >= len(rs) || !unicode.IsDigit(rs[i+1]) {
						break
					}
					seenDot = true
				}
				i++
			}
			toks = append(toks, token{tNum, string(rs[start:i]), start})
		case c == '\'' || c == '"':
			quote := c
			start := i
			i++
			var b strings.Builder
			closed := false
			for i < len(rs) {
				if rs[i] == '\\' && i+1 < len(rs) {
					b.WriteRune(rs[i+1])
					i += 2
					continue
				}
				if rs[i] == quote {
					closed = true
					i++
					break
				}
				b.WriteRune(rs[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string starting at %d", start)
			}
			toks = append(toks, token{tStr, b.String(), start})
		case c == '_' || unicode.IsLetter(c):
			start := i
			for i < len(rs) && (rs[i] == '_' || unicode.IsLetter(rs[i]) || unicode.IsDigit(rs[i])) {
				i++
			}
			toks = append(toks, token{tIdent, string(rs[start:i]), start})
		default:
			two := ""
			if i+1 < len(rs) {
				two = string(rs[i : i+2])
			}
			switch two {
			case "==", "!=", "<=", ">=", "&&", "||":
				toks = append(toks, token{tOp, two, i})
				i += 2
				continue
			}
			if strings.ContainsRune("+-*/%<>!?:().,", c) {
				toks = append(toks, token{tOp, string(c), i})
				i++
				continue
			}
			return nil, fmt.Errorf("unexpected character %q at %d", c, i)
		}
	}
	toks = append(toks, token{tEOF, "", len(rs)})
	return toks, nil
}

// exprParser is a recursive-descent recognizer. It builds no tree — the Go
// side only needs "does it parse" and "which root names does it read".
type exprParser struct {
	toks []token
	pos  int
	refs []string
	// paths holds each field reference with the member names read off it
	// ("lines.cpm" → ["lines", "cpm"]), so the validator can check members
	// too, not just roots.
	paths [][]string
	// vpath is the field path of the value the last complete operand
	// produced ("lines" for `lines` or `(lines)`, nil for `a + b`, a call or
	// a literal), so `(lines).cpm` is checked like `lines.cpm`.
	vpath []string
	depth int
}

func (p *exprParser) peek() token { return p.toks[p.pos] }

// advance consumes the current token (never past EOF).
func (p *exprParser) next() {
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
}

func (p *exprParser) isOp(s string) bool {
	t := p.peek()
	return t.kind == tOp && t.text == s
}

func (p *exprParser) expect(s string) error {
	if !p.isOp(s) {
		return p.errf("expected %q", s)
	}
	p.next()
	return nil
}

func (p *exprParser) errf(format string, args ...any) error {
	t := p.peek()
	where := "end of expression"
	if t.kind != tEOF {
		where = fmt.Sprintf("%q at %d", t.text, t.pos)
	}
	return fmt.Errorf("%s (near %s)", fmt.Sprintf(format, args...), where)
}

// maxExprDepth bounds nesting so a pathological input cannot blow the stack.
const maxExprDepth = 40

func (p *exprParser) expr() error {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxExprDepth {
		return p.errf("expression nested too deeply")
	}
	if err := p.binary(0); err != nil {
		return err
	}
	if p.isOp("?") {
		p.next()
		if err := p.expr(); err != nil {
			return err
		}
		if err := p.expect(":"); err != nil {
			return err
		}
		err := p.expr()
		p.vpath = nil
		return err
	}
	return nil
}

// binaryLevels is the precedence ladder, loosest first.
var binaryLevels = [][]string{
	{"||"},
	{"&&"},
	{"==", "!="},
	{"<", "<=", ">", ">="},
	{"+", "-"},
	{"*", "/", "%"},
}

func (p *exprParser) binary(level int) error {
	if level == len(binaryLevels) {
		return p.unary()
	}
	if err := p.binary(level + 1); err != nil {
		return err
	}
	for {
		t := p.peek()
		if t.kind != tOp || !containsStr(binaryLevels[level], t.text) {
			return nil
		}
		p.next()
		if err := p.binary(level + 1); err != nil {
			return err
		}
		p.vpath = nil // an operator's result is not a field
	}
}

func (p *exprParser) unary() error {
	if p.isOp("!") || p.isOp("-") {
		p.next()
		p.depth++
		defer func() { p.depth-- }()
		if p.depth > maxExprDepth {
			return p.errf("expression nested too deeply")
		}
		err := p.unary()
		p.vpath = nil
		return err
	}
	return p.postfix()
}

func (p *exprParser) postfix() error {
	if err := p.primary(); err != nil {
		return err
	}
	// primary left vpath at the field path of its value, if it has one.
	var path []string
	if p.vpath != nil {
		path = append([]string(nil), p.vpath...)
	}
	for p.isOp(".") {
		p.next()
		if p.peek().kind != tIdent {
			return p.errf("expected a field name after '.'")
		}
		if path != nil {
			path = append(path, p.peek().text)
		}
		p.next()
	}
	if len(path) > 1 {
		p.paths = append(p.paths, path)
	}
	p.vpath = path
	return nil
}

func (p *exprParser) primary() error {
	t := p.peek()
	switch t.kind {
	case tNum, tStr:
		p.next()
		p.vpath = nil
		return nil
	case tIdent:
		p.next()
		p.vpath = nil
		switch t.text {
		case "true", "false", "null":
			return nil
		}
		if p.isOp("(") {
			arity, ok := exprFuncs[t.text]
			if !ok {
				return fmt.Errorf("unknown function %q (allowed: %s)", t.text, strings.Join(FunctionNames(), ", "))
			}
			p.next()
			n := 0
			if !p.isOp(")") {
				for {
					if err := p.expr(); err != nil {
						return err
					}
					n++
					if p.isOp(",") {
						p.next()
						continue
					}
					break
				}
			}
			if err := p.expect(")"); err != nil {
				return err
			}
			if n < arity[0] || (arity[1] >= 0 && n > arity[1]) {
				return fmt.Errorf("%s() takes %s, got %d", t.text, arityText(arity), n)
			}
			p.vpath = nil // a call's result is not a field
			return nil
		}
		p.refs = append(p.refs, t.text)
		p.vpath = []string{t.text}
		return nil
	case tEOF:
		return p.errf("expected a value")
	case tOp:
		if t.text == "(" {
			p.next()
			// The inner expression leaves vpath at its own field path, so a
			// parenthesized field keeps it.
			if err := p.expr(); err != nil {
				return err
			}
			return p.expect(")")
		}
	}
	return p.errf("expected a value")
}

func arityText(a [2]int) string {
	switch {
	case a[1] < 0:
		return fmt.Sprintf("at least %d argument(s)", a[0])
	case a[0] == a[1]:
		return fmt.Sprintf("%d argument(s)", a[0])
	default:
		return fmt.Sprintf("%d-%d arguments", a[0], a[1])
	}
}

// ParseExpr checks one expression and returns the root names it reads (field
// ids, or a repeater's per-item names) in source order, duplicates kept.
func ParseExpr(src string) ([]string, error) {
	refs, _, err := parseExprPaths(src)
	return refs, err
}

// parseExprPaths is ParseExpr plus the member path of every field reference.
func parseExprPaths(src string) ([]string, [][]string, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil, fmt.Errorf("empty expression")
	}
	if len(src) > MaxExprLen {
		return nil, nil, fmt.Errorf("expression longer than %d characters", MaxExprLen)
	}
	toks, err := lex(src)
	if err != nil {
		return nil, nil, err
	}
	p := &exprParser{toks: toks}
	if err := p.expr(); err != nil {
		return nil, nil, err
	}
	if p.peek().kind != tEOF {
		return nil, nil, p.errf("unexpected trailing input")
	}
	return p.refs, p.paths, nil
}

// TemplateExprs splits a display string into its {{ expr }} holes. A string
// without "{{" has none. An unclosed "{{" is an error rather than literal
// text: it is always a model mistake, and rendering it raw would show the
// user template syntax.
func TemplateExprs(s string) ([]string, error) {
	var out []string
	rest := s
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			return out, nil
		}
		j := strings.Index(rest[i+2:], "}}")
		if j < 0 {
			return nil, fmt.Errorf("unclosed {{ in %q", truncateRunes(s, 80))
		}
		out = append(out, rest[i+2:i+2+j])
		rest = rest[i+2+j+2:]
	}
}
