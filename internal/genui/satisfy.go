package genui

import (
	"fmt"
	"strings"
)

// Propositional satisfiability over conditions. A visible_if / disabled_if
// is reduced to its boolean skeleton: &&, || and ! stay operators, true and
// false (and null) are constants, and every other operand (a field, a
// comparison, a call, a ternary...) becomes an opaque atom, the same atom
// wherever its tokens are the same. Atoms are treated as independent, which
// can only find MORE satisfying states than really exist, so "never true"
// here is never a false alarm: it holds whatever the inputs are. It catches
// what a model can actually write by mistake — "gate && !gate", a button
// disabled by "gate || !gate", or shown on "gate" but disabled on
// "gate || block" — without an evaluator for the value domain.

// maxSatAtoms bounds the brute-force check (2^n states); a condition with
// more distinct atoms is not judged.
const maxSatAtoms = 12

type bnode struct {
	op   byte // 'o' or, 'a' and, 'n' not, 'v' atom, 'c' constant
	l, r *bnode
	atom int
	val  bool
}

type skelParser struct {
	toks  []token
	pos   int
	atoms map[string]int
}

func (p *skelParser) peek() token { return p.toks[p.pos] }

func (p *skelParser) next() {
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
}

func (p *skelParser) isOp(s string) bool {
	t := p.peek()
	return t.kind == tOp && t.text == s
}

// atomOf names the tokens from start up to the current position.
func (p *skelParser) atomOf(start int) *bnode {
	var b strings.Builder
	for _, t := range p.toks[start:p.pos] {
		fmt.Fprintf(&b, "%d:%s ", t.kind, t.text)
	}
	key := b.String()
	id, ok := p.atoms[key]
	if !ok {
		id = len(p.atoms)
		p.atoms[key] = id
	}
	return &bnode{op: 'v', atom: id}
}

func (p *skelParser) expr() *bnode {
	start := p.pos
	n := p.or()
	if p.isOp("?") {
		p.next()
		p.expr()
		if p.isOp(":") {
			p.next()
		}
		p.expr()
		return p.atomOf(start)
	}
	return n
}

func (p *skelParser) or() *bnode {
	n := p.and()
	for p.isOp("||") {
		p.next()
		n = &bnode{op: 'o', l: n, r: p.and()}
	}
	return n
}

func (p *skelParser) and() *bnode {
	n := p.operand()
	for p.isOp("&&") {
		p.next()
		n = &bnode{op: 'a', l: n, r: p.operand()}
	}
	return n
}

// operand is everything tighter than &&: a unary term, or a chain of
// comparison / arithmetic operators, which is one atom.
func (p *skelParser) operand() *bnode {
	start := p.pos
	n := p.unary()
	chained := false
	for {
		t := p.peek()
		if t.kind != tOp || !isValueOp(t.text) {
			break
		}
		chained = true
		p.next()
		p.unary()
	}
	if chained {
		return p.atomOf(start)
	}
	return n
}

func isValueOp(s string) bool {
	for _, lvl := range binaryLevels[2:] {
		if containsStr(lvl, s) {
			return true
		}
	}
	return false
}

func (p *skelParser) unary() *bnode {
	start := p.pos
	if p.isOp("!") {
		p.next()
		return &bnode{op: 'n', l: p.unary()}
	}
	if p.isOp("-") {
		p.next()
		p.unary()
		return p.atomOf(start)
	}
	n := p.primary()
	if p.isOp(".") {
		for p.isOp(".") {
			p.next()
			p.next() // the member name
		}
		return p.atomOf(start)
	}
	return n
}

func (p *skelParser) primary() *bnode {
	start := p.pos
	t := p.peek()
	switch {
	case t.kind == tIdent && (t.text == "true" || t.text == "false" || t.text == "null"):
		p.next()
		return &bnode{op: 'c', val: t.text == "true"}
	case t.kind == tIdent:
		p.next()
		if p.isOp("(") {
			p.next()
			for !p.isOp(")") && p.peek().kind != tEOF {
				p.expr()
				if p.isOp(",") {
					p.next()
				}
			}
			p.next()
		}
		return p.atomOf(start)
	case t.kind == tOp && t.text == "(":
		p.next()
		n := p.expr()
		p.next() // ")"
		return n
	default:
		p.next()
		return p.atomOf(start)
	}
}

func (n *bnode) eval(state uint) bool {
	switch n.op {
	case 'o':
		return n.l.eval(state) || n.r.eval(state)
	case 'a':
		return n.l.eval(state) && n.r.eval(state)
	case 'n':
		return !n.l.eval(state)
	case 'v':
		return state&(1<<uint(n.atom)) != 0
	default:
		return n.val
	}
}

// skeleton parses conditions that already passed ParseExpr into one shared
// atom space. A source that fails to lex yields nil (the validator reports
// the parse error elsewhere).
type skeleton struct {
	atoms map[string]int
}

func (s *skeleton) parse(src string) *bnode {
	toks, err := lex(src)
	if err != nil {
		return nil
	}
	p := &skelParser{toks: toks, atoms: s.atoms}
	return p.expr()
}

// satisfiable reports whether some assignment of the atoms makes every
// `want` true and every `deny` false. Too many atoms counts as satisfiable.
func satisfiable(atoms int, want, deny []*bnode) bool {
	if atoms > maxSatAtoms {
		return true
	}
	for state := uint(0); state < 1<<uint(atoms); state++ {
		ok := true
		for _, w := range want {
			if w != nil && !w.eval(state) {
				ok = false
				break
			}
		}
		for _, d := range deny {
			if ok && d != nil && d.eval(state) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// neverTrue: a condition that holds in no state at all.
func neverTrue(src string) bool {
	s := &skeleton{atoms: map[string]int{}}
	n := s.parse(src)
	return n != nil && !satisfiable(len(s.atoms), []*bnode{n}, nil)
}

// neverUsable: an action that is disabled in every state where it is shown.
func neverUsable(visibleIf, disabledIf string) bool {
	s := &skeleton{atoms: map[string]int{}}
	var want, deny []*bnode
	if visibleIf != "" {
		want = append(want, s.parse(visibleIf))
	}
	if disabledIf != "" {
		deny = append(deny, s.parse(disabledIf))
	}
	return !satisfiable(len(s.atoms), want, deny)
}
