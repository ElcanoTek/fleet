package genui

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Satisfiability over conditions. A visible_if / disabled_if is reduced to
// its boolean skeleton: &&, || and ! stay operators, true and false (and
// null) are constants, and every other operand becomes an atom, the same atom
// wherever its tokens are the same. Two kinds of atom are understood:
//
//   - A comparison between one number input (number or slider) and a number
//     literal ("n > 0", "5 <= n", "n == 3"), or a bare number input read for
//     truthiness, is a fact about that input's value. Such atoms are
//     evaluated together by giving the input concrete values: every literal
//     it is compared with, a point between each pair and beyond both ends,
//     and (for a number input, which can be blank) null. That set meets
//     every region the comparisons can tell apart, so "n > 0" shown and
//     "n >= 0" disabled is caught.
//   - Everything else (a toggle compared with true/false aside, which shares
//     the toggle's atom) is opaque and free: any truth value, independent of
//     the rest.
//
// Free atoms can only find MORE satisfying states than really exist, so a
// "never" verdict is never a false alarm: it holds whatever the inputs are.
// What is deliberately not judged: arithmetic ("a + b > 3"), comparisons
// between two inputs, text and list values.

// maxSatStates bounds the brute-force check; a condition set with more
// states than this is not judged.
const maxSatStates = 1 << 12

type bnode struct {
	op   byte // 'o' or, 'a' and, 'n' not, 'v' atom, 'c' constant
	l, r *bnode
	atom int
	val  bool
}

// numAtom is an atom that is a fact about one number input's value.
type numAtom struct {
	field string
	op    string // < <= > >= == != , or "truthy" for a bare read
	c     float64
}

// inputKind is what the check knows about one input: its kind and, for a
// slider, the values its range control can actually hold.
type inputKind struct {
	kind   string
	domain *sliderDomain
}

type sliderDomain struct{ min, max, step float64 }

// maxDomainPoints bounds the slider positions scanned for representatives;
// a finer slider is treated as any number (less precise, never wrong).
const maxDomainPoints = 100000

// Input kinds the check understands (see validator.inputKinds).
const (
	kindBool   = "bool"   // toggle: always true or false
	kindNumber = "number" // number input: a number, or null when blank
	kindSlider = "slider" // slider: always a number
)

type skelParser struct {
	toks []token
	pos  int
	sk   *skeleton
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
	return p.sk.atom(tokenKey(p.toks[start:p.pos]), nil)
}

func tokenKey(toks []token) string {
	var b strings.Builder
	for _, t := range toks {
		fmt.Fprintf(&b, "%d:%s ", t.kind, t.text)
	}
	return b.String()
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
	if !chained {
		return n
	}
	if b := p.compare(p.toks[start:p.pos]); b != nil {
		return b
	}
	return p.atomOf(start)
}

// compare understands one comparison between an input and a literal:
// a toggle against true/false, or a number input against a number.
func (p *skelParser) compare(t []token) *bnode {
	// field OP literal, or literal OP field (a literal may be negative).
	var field token
	var op string
	var lit []token
	known := func(k token) bool { return k.kind == tIdent && p.sk.kinds[k.text].kind != "" }
	switch {
	case len(t) >= 3 && known(t[0]) && t[1].kind == tOp:
		field, op, lit = t[0], t[1].text, t[2:]
	case len(t) >= 3 && known(t[len(t)-1]) && t[len(t)-2].kind == tOp:
		field, op, lit = t[len(t)-1], flipOp(t[len(t)-2].text), t[:len(t)-2]
	default:
		return nil
	}
	switch p.sk.kinds[field.text].kind {
	case kindBool:
		if len(lit) != 1 || lit[0].kind != tIdent || (lit[0].text != "true" && lit[0].text != "false") || (op != "==" && op != "!=") {
			return nil
		}
		n := p.sk.atom(tokenKey([]token{field}), nil)
		if (lit[0].text == "true") != (op == "==") {
			return &bnode{op: 'n', l: n}
		}
		return n
	case kindNumber, kindSlider:
		c, ok := numberLiteral(lit)
		if !ok || !containsStr([]string{"<", "<=", ">", ">=", "==", "!="}, op) {
			return nil
		}
		return p.sk.atom(fmt.Sprintf("num:%s %s %v", field.text, op, c), &numAtom{field: field.text, op: op, c: c})
	}
	return nil
}

// flipOp turns "c OP x" into "x OP' c".
func flipOp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return op
}

func numberLiteral(t []token) (float64, bool) {
	neg := false
	if len(t) == 2 && t[0].kind == tOp && t[0].text == "-" {
		neg, t = true, t[1:]
	}
	if len(t) != 1 || t[0].kind != tNum {
		return 0, false
	}
	f, err := strconv.ParseFloat(t[0].text, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	if neg {
		f = -f
	}
	return f, true
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
			return p.atomOf(start)
		}
		// A bare number input read for truthiness: a fact about its value.
		if k := p.sk.kinds[t.text].kind; k == kindNumber || k == kindSlider {
			return p.sk.atom("num:"+t.text+" truthy", &numAtom{field: t.text, op: "truthy"})
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

// skeleton parses conditions that already passed ParseExpr into one shared
// atom space. A source that fails to lex yields nil (the validator reports
// the parse error elsewhere).
type skeleton struct {
	keys  map[string]int
	nums  []*numAtom // by atom id; nil for a free atom
	kinds map[string]inputKind
}

func newSkeleton(kinds map[string]inputKind) *skeleton {
	return &skeleton{keys: map[string]int{}, kinds: kinds}
}

func (s *skeleton) atom(key string, num *numAtom) *bnode {
	id, ok := s.keys[key]
	if !ok {
		id = len(s.nums)
		s.keys[key] = id
		s.nums = append(s.nums, num)
	}
	return &bnode{op: 'v', atom: id}
}

func (s *skeleton) parse(src string) *bnode {
	toks, err := lex(src)
	if err != nil {
		return nil
	}
	p := &skelParser{toks: toks, sk: s}
	return p.expr()
}

// value is a number input's value in one state: a number, or blank (null).
type value struct {
	null bool
	n    float64
}

// candidates are the values worth trying for a number input: each literal
// it is compared with, the points between and beyond them, and blank. A
// slider with a known domain is tried only at positions it can hold: one
// per distinct outcome of the comparisons, found by scanning its positions.
func (s *skeleton) candidates(field string) []value {
	var cs []float64
	for _, a := range s.nums {
		if a != nil && a.field == field && a.op != "truthy" {
			cs = append(cs, a.c)
		}
	}
	cs = append(cs, 0) // truthiness turns on zero
	sort.Float64s(cs)
	if d := s.kinds[field].domain; d != nil && (d.max-d.min)/d.step < maxDomainPoints {
		return s.domainCandidates(field, d)
	}
	var out []value
	for i, c := range cs {
		if i > 0 && c == cs[i-1] {
			continue
		}
		if i == 0 {
			out = append(out, value{n: c - 1})
		} else {
			out = append(out, value{n: (cs[i-1] + c) / 2})
		}
		out = append(out, value{n: c})
	}
	out = append(out, value{n: cs[len(cs)-1] + 1})
	if s.kinds[field].kind == kindNumber {
		out = append(out, value{null: true})
	}
	return out
}

// domainCandidates picks, among a slider's positions, one per distinct
// truth assignment of the field's atoms.
func (s *skeleton) domainCandidates(field string, d *sliderDomain) []value {
	var mine []*numAtom
	for _, a := range s.nums {
		if a != nil && a.field == field {
			mine = append(mine, a)
		}
	}
	seen := map[string]bool{}
	var out []value
	n := int(math.Floor((d.max-d.min)/d.step + 1e-9))
	for k := 0; k <= n; k++ {
		// The browser reports a position as a short decimal (0.3, not
		// 0.30000000000000004), so round away float noise before comparing.
		v := value{n: math.Round((d.min+float64(k)*d.step)*1e9) / 1e9}
		sig := make([]byte, len(mine))
		for i, a := range mine {
			sig[i] = '0'
			if a.holds(v) {
				sig[i] = '1'
			}
		}
		if !seen[string(sig)] {
			seen[string(sig)] = true
			out = append(out, v)
		}
	}
	return out
}

// holds evaluates a number atom the way expr.ts does: ordering treats a
// blank (null) as 0, equality never matches it, and blank is falsy.
func (a *numAtom) holds(v value) bool {
	if a.op == "truthy" {
		return !v.null && v.n != 0
	}
	if v.null && (a.op == "==" || a.op == "!=") {
		return a.op == "!="
	}
	x := v.n
	if v.null {
		x = 0
	}
	switch a.op {
	case "<":
		return x < a.c
	case "<=":
		return x <= a.c
	case ">":
		return x > a.c
	case ">=":
		return x >= a.c
	case "==":
		return x == a.c
	default:
		return x != a.c
	}
}

func (n *bnode) eval(truth []bool) bool {
	switch n.op {
	case 'o':
		return n.l.eval(truth) || n.r.eval(truth)
	case 'a':
		return n.l.eval(truth) && n.r.eval(truth)
	case 'n':
		return !n.l.eval(truth)
	case 'v':
		return truth[n.atom]
	default:
		return n.val
	}
}

// satisfiable reports whether some state makes every `want` true and every
// `deny` false. A state assigns each free atom a truth value and each number
// input one of its candidate values. Too many states counts as satisfiable.
func (s *skeleton) satisfiable(want, deny []*bnode) bool {
	var free []int
	var fields []string
	cands := map[string][]value{}
	for id, a := range s.nums {
		if a == nil {
			free = append(free, id)
		} else if _, seen := cands[a.field]; !seen {
			cands[a.field] = s.candidates(a.field)
			fields = append(fields, a.field)
		}
	}
	states := 1 << len(free)
	for _, f := range fields {
		states *= len(cands[f])
		if states > maxSatStates {
			return true
		}
	}
	if len(free) > 12 || states > maxSatStates {
		return true
	}
	truth := make([]bool, len(s.nums))
	vals := map[string]value{}
	for st := 0; st < states; st++ {
		rest := st
		for _, id := range free {
			truth[id] = rest&1 != 0
			rest >>= 1
		}
		for _, f := range fields {
			c := cands[f]
			vals[f] = c[rest%len(c)]
			rest /= len(c)
		}
		for id, a := range s.nums {
			if a != nil {
				truth[id] = a.holds(vals[a.field])
			}
		}
		ok := true
		for _, w := range want {
			if w != nil && !w.eval(truth) {
				ok = false
				break
			}
		}
		for _, d := range deny {
			if ok && d != nil && d.eval(truth) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// neverTrue: a condition that holds in no state at all. kinds names the
// inputs the check understands (see the constants above); nil is allowed.
func neverTrue(src string, kinds map[string]inputKind) bool {
	s := newSkeleton(kinds)
	n := s.parse(src)
	return n != nil && !s.satisfiable([]*bnode{n}, nil)
}

// neverUsable: an action that is disabled in every state where it is shown.
func neverUsable(visibleIf, disabledIf string, kinds map[string]inputKind) bool {
	s := newSkeleton(kinds)
	var want, deny []*bnode
	if visibleIf != "" {
		want = append(want, s.parse(visibleIf))
	}
	if disabledIf != "" {
		deny = append(deny, s.parse(disabledIf))
	}
	return !s.satisfiable(want, deny)
}
