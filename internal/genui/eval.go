package genui

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxExprString mirrors MAX_EXPR_STRING in web/src/app/chat/ui/genui/expr.ts.
const MaxExprString = 1_000_000

const jsEpsilon = 2.220446049250313e-16

type evalNodeKind int

const (
	evalLit evalNodeKind = iota
	evalRef
	evalMember
	evalCall
	evalUnary
	evalBin
	evalCond
)

type evalNode struct {
	kind evalNodeKind
	val  any
	name string
	op   string
	obj  *evalNode
	l, r *evalNode
	c    *evalNode
	a, b *evalNode
	args []*evalNode
}

type astParser struct {
	toks  []token
	pos   int
	depth int
}

func (p *astParser) peek() token { return p.toks[p.pos] }

func (p *astParser) next() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *astParser) isOp(s string) bool {
	t := p.peek()
	return t.kind == tOp && t.text == s
}

func (p *astParser) failf(format string, args ...any) error {
	t := p.peek()
	where := "end of expression"
	if t.kind != tEOF {
		where = fmt.Sprintf("%q at %d", t.text, t.pos)
	}
	return fmt.Errorf("%s (near %s)", fmt.Sprintf(format, args...), where)
}

func (p *astParser) expect(s string) error {
	if !p.isOp(s) {
		return p.failf("expected %q", s)
	}
	p.next()
	return nil
}

func (p *astParser) parseAll() (*evalNode, error) {
	n, err := p.expr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tEOF {
		return nil, p.failf("unexpected trailing input")
	}
	return n, nil
}

func (p *astParser) expr() (*evalNode, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxExprDepth {
		return nil, p.failf("expression nested too deeply")
	}
	c, err := p.binary(0)
	if err != nil {
		return nil, err
	}
	if p.isOp("?") {
		p.next()
		a, err := p.expr()
		if err != nil {
			return nil, err
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		b, err := p.expr()
		if err != nil {
			return nil, err
		}
		return &evalNode{kind: evalCond, c: c, a: a, b: b}, nil
	}
	return c, nil
}

func (p *astParser) binary(level int) (*evalNode, error) {
	if level == len(binaryLevels) {
		return p.unary()
	}
	l, err := p.binary(level + 1)
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tOp || !slices.Contains(binaryLevels[level], t.text) {
			return l, nil
		}
		p.next()
		r, err := p.binary(level + 1)
		if err != nil {
			return nil, err
		}
		l = &evalNode{kind: evalBin, op: t.text, l: l, r: r}
	}
}

func (p *astParser) unary() (*evalNode, error) {
	if p.isOp("!") || p.isOp("-") {
		op := p.next().text
		p.depth++
		defer func() { p.depth-- }()
		if p.depth > maxExprDepth {
			return nil, p.failf("expression nested too deeply")
		}
		arg, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &evalNode{kind: evalUnary, op: op, l: arg}, nil
	}
	n, err := p.primary()
	if err != nil {
		return nil, err
	}
	for p.isOp(".") {
		p.next()
		t := p.peek()
		if t.kind != tIdent {
			return nil, p.failf("expected a field name after '.'")
		}
		p.next()
		n = &evalNode{kind: evalMember, obj: n, name: t.text}
	}
	return n, nil
}

func (p *astParser) primary() (*evalNode, error) {
	t := p.peek()
	switch t.kind {
	case tNum:
		p.next()
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, p.failf("invalid number %q", t.text)
		}
		return &evalNode{kind: evalLit, val: f}, nil
	case tStr:
		p.next()
		return &evalNode{kind: evalLit, val: t.text}, nil
	case tIdent:
		p.next()
		switch t.text {
		case "true":
			return &evalNode{kind: evalLit, val: true}, nil
		case "false":
			return &evalNode{kind: evalLit, val: false}, nil
		case "null":
			return &evalNode{kind: evalLit, val: nil}, nil
		}
		if p.isOp("(") {
			arity, ok := exprFuncs[t.text]
			if !ok {
				return nil, p.failf("unknown function %q", t.text)
			}
			p.next()
			var args []*evalNode
			if !p.isOp(")") {
				for {
					a, err := p.expr()
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.isOp(",") {
						p.next()
						continue
					}
					break
				}
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			if len(args) < arity[0] || (arity[1] >= 0 && len(args) > arity[1]) {
				return nil, p.failf("%s() takes %d..%d argument(s), got %d", t.text, arity[0], arity[1], len(args))
			}
			return &evalNode{kind: evalCall, name: t.text, args: args}, nil
		}
		return &evalNode{kind: evalRef, name: t.text}, nil
	case tOp:
		if t.text == "(" {
			p.next()
			n, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			return n, nil
		}
	case tEOF:
		return nil, p.failf("expected a value")
	}
	return nil, p.failf("expected a value")
}

func parseEvalAST(src string) (*evalNode, error) {
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("empty expression")
	}
	if utf16Len(src) > MaxExprLen {
		return nil, fmt.Errorf("expression longer than %d characters", MaxExprLen)
	}
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	return (&astParser{toks: toks}).parseAll()
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

func boundedExprString(s string) string {
	if len(s) <= MaxExprString {
		return s
	}
	units := 0
	for idx, r := range s {
		w := utf16.RuneLen(r)
		if units+w > MaxExprString {
			return s[:idx]
		}
		units += w
	}
	return s
}

func normEvalVal(v any) any {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case bool, string:
		return x
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		if x == 0 {
			return float64(0)
		}
		return x
	case float32:
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
		return f
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = normEvalVal(x[i])
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i := range x {
			out[i] = x[i]
		}
		return out
	case map[string]any:
		return x
	default:
		return nil
	}
}

// Truthy reports whether v is truthy in the card expression language.
func Truthy(v any) bool {
	v = normEvalVal(v)
	if v == nil {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	default:
		return true
	}
}

func evalNum(v any) float64 {
	v = normEvalVal(v)
	if v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		return parseJSNumberString(x)
	default:
		return math.NaN()
	}
}

func parseJSNumberString(raw string) float64 {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if strings.ContainsRune(s, '_') {
		return math.NaN()
	}
	if len(s) >= 3 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X' || s[1] == 'b' || s[1] == 'B' || s[1] == 'o' || s[1] == 'O') {
		var base float64
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'b', 'B':
			base = 2
		case 'o', 'O':
			base = 8
		}
		var acc float64
		for i := 2; i < len(s); i++ {
			ch := s[i]
			var digit int
			switch {
			case ch >= '0' && ch <= '9':
				digit = int(ch - '0')
			case base == 16 && ch >= 'a' && ch <= 'f':
				digit = int(ch-'a') + 10
			case base == 16 && ch >= 'A' && ch <= 'F':
				digit = int(ch-'A') + 10
			default:
				return math.NaN()
			}
			if float64(digit) >= base {
				return math.NaN()
			}
			acc = acc*base + float64(digit)
			if math.IsInf(acc, 0) {
				return math.NaN()
			}
		}
		return acc
	}
	if !isDecimalFloatSyntax(s) {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return math.NaN()
	}
	return f
}

func isDecimalFloatSyntax(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		digits++
		i++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			digits++
			i++
		}
	}
	if digits == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		expDigits := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			expDigits++
			i++
		}
		if expDigits == 0 {
			return false
		}
	}
	return i == len(s)
}

func numOrNil(n float64) any {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	if n == 0 {
		return float64(0)
	}
	return n
}

func jsNumberString(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ""
	}
	if v == 0 {
		return "0"
	}
	abs := math.Abs(v)
	if math.Trunc(v) == v && abs < 1e21 {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	if abs >= 1e21 || abs < 1e-6 {
		s := strconv.FormatFloat(v, 'e', -1, 64)
		s = strings.Replace(s, "e+0", "e+", 1)
		s = strings.Replace(s, "e-0", "e-", 1)
		return s
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ToText formats v using the same rules as toText in expr.ts.
func ToText(v any) string {
	v = normEvalVal(v)
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case float64:
		if math.Trunc(x) == x && math.Abs(x) < 1e21 {
			return strconv.FormatFloat(x, 'f', 0, 64)
		}
		if math.Abs(x) >= 1e13 {
			return jsNumberString(x)
		}
		short, err := strconv.ParseFloat(strconv.FormatFloat(x, 'g', 15, 64), 64)
		if err == nil && math.Abs(short-x) <= 4*jsEpsilon*math.Abs(x) {
			return jsNumberString(short)
		}
		return jsNumberString(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case string:
		return x
	case []any:
		return evalJoinList(x, ", ")
	default:
		return ""
	}
}

func evalJoinList(items []any, glue string) string {
	var b strings.Builder
	first := true
	for _, item := range items {
		t := ToText(item)
		if t == "" {
			continue
		}
		if !first {
			b.WriteString(glue)
		}
		b.WriteString(t)
		first = false
		if b.Len() > MaxExprString*4 {
			break
		}
	}
	return boundedExprString(b.String())
}

func evalAsList(v any) []any {
	v = normEvalVal(v)
	if v == nil {
		return []any{}
	}
	if arr, ok := v.([]any); ok {
		return arr
	}
	return []any{v}
}

func evalNumbers(args []any) []float64 {
	flat := args
	if len(args) == 1 {
		flat = evalAsList(args[0])
	}
	out := make([]float64, 0, len(flat))
	for _, x := range flat {
		x = normEvalVal(x)
		if x == nil {
			continue
		}
		if s, ok := x.(string); ok && s == "" {
			continue
		}
		n := evalNum(x)
		if !math.IsNaN(n) && !math.IsInf(n, 0) {
			out = append(out, n)
		}
	}
	return out
}

func jsMathRound(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) || x == 0 {
		return x
	}
	if x > 0 {
		return math.Floor(x + 0.5)
	}
	if x >= -0.5 {
		return 0
	}
	return math.Floor(x + 0.5)
}

func evalRoundTo(n float64, digits float64) float64 {
	d := 0
	if !math.IsNaN(digits) && !math.IsInf(digits, 0) {
		d = int(math.Trunc(digits))
	}
	if d < 0 {
		d = 0
	} else if d > 10 {
		d = 10
	}
	f := math.Pow10(d)
	sign := 0.0
	if n > 0 {
		sign = 1
	} else if n < 0 {
		sign = -1
	}
	scaled := (n + jsEpsilon*sign) * f
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) {
		return n
	}
	r := jsMathRound(scaled) / f
	if r == 0 {
		return 0
	}
	return r
}

func evalEqual(a, b any) bool {
	a = normEvalVal(a)
	b = normEvalVal(b)
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	_, aIsNum := a.(float64)
	_, bIsNum := b.(float64)
	if aIsNum || bIsNum {
		x := evalNum(a)
		y := evalNum(b)
		aBlankStr := false
		if s, ok := a.(string); ok && strings.TrimSpace(s) == "" {
			aBlankStr = true
		}
		bBlankStr := false
		if s, ok := b.(string); ok && strings.TrimSpace(s) == "" {
			bBlankStr = true
		}
		if !math.IsNaN(x) && !math.IsInf(x, 0) && !math.IsNaN(y) && !math.IsInf(y, 0) && !aBlankStr && !bBlankStr {
			return x == y
		}
	}
	switch a.(type) {
	case []any, map[string]any:
		return false
	}
	switch b.(type) {
	case []any, map[string]any:
		return false
	}
	return a == b
}

func evalUnique(items []any) []any {
	out := make([]any, 0, len(items))
	sawNull := false
	nums := map[float64]bool{}
	numericStrs := map[float64]bool{}
	strs := map[string]bool{}
	bools := map[bool]bool{}
	for _, raw := range items {
		x := normEvalVal(raw)
		switch v := x.(type) {
		case nil:
			if sawNull {
				continue
			}
			sawNull = true
		case float64:
			if nums[v] || numericStrs[v] || (v == 1 && bools[true]) || (v == 0 && bools[false]) {
				continue
			}
			nums[v] = true
		case string:
			n := math.NaN()
			if strings.TrimSpace(v) != "" {
				n = evalNum(v)
			}
			finite := !math.IsNaN(n) && !math.IsInf(n, 0)
			if strs[v] || (finite && nums[n]) {
				continue
			}
			strs[v] = true
			if finite {
				numericStrs[n] = true
			}
		case bool:
			numEq := 0.0
			if v {
				numEq = 1.0
			}
			if bools[v] || nums[numEq] {
				continue
			}
			bools[v] = true
		}
		out = append(out, x)
	}
	return out
}

func evalAddCommas(s string) string {
	if strings.ContainsAny(s, "eE") {
		return s
	}
	rawInt, dec, hasDec := strings.Cut(s, ".")
	sign := ""
	digits := rawInt
	if strings.HasPrefix(rawInt, "-") {
		sign = "-"
		digits = rawInt[1:]
	}
	var b strings.Builder
	b.WriteString(sign)
	for i := 0; i < len(digits); i++ {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(digits[i])
	}
	if hasDec {
		b.WriteByte('.')
		b.WriteString(dec)
	}
	return b.String()
}

func evalDigitsArg(v any, def int) int {
	if v == nil {
		return def
	}
	f := evalNum(v)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	d := int(math.Trunc(f))
	if d < 0 {
		return 0
	}
	if d > 10 {
		return 10
	}
	return d
}

func evalArgAt(args []any, i int) any {
	if i >= 0 && i < len(args) {
		return args[i]
	}
	return nil
}

func evalCallFunc(fn string, args []any) any {
	if out, handled := evalNumericCallFunc(fn, args); handled {
		return out
	}
	if out, handled := evalStringAndFormatCallFunc(fn, args); handled {
		return out
	}
	return evalCollectionCallFunc(fn, args)
}

func evalNumericCallFunc(fn string, args []any) (any, bool) {
	switch fn {
	case "sum":
		ns := evalNumbers(args)
		s := 0.0
		for _, n := range ns {
			s += n
		}
		return numOrNil(s), true
	case "avg":
		return evalAvgCall(evalNumbers(args)), true
	case "min":
		return evalMinMaxCall(evalNumbers(args), false), true
	case "max":
		return evalMinMaxCall(evalNumbers(args), true), true
	case "abs":
		return numOrNil(math.Abs(evalNum(evalArgAt(args, 0)))), true
	case "floor":
		return numOrNil(math.Floor(evalNum(evalArgAt(args, 0)))), true
	case "ceil":
		return numOrNil(math.Ceil(evalNum(evalArgAt(args, 0)))), true
	case "round":
		n := evalNum(evalArgAt(args, 0))
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, true
		}
		d := 0.0
		if len(args) > 1 {
			d = evalNum(args[1])
		}
		return numOrNil(evalRoundTo(n, d)), true
	case "number":
		return numOrNil(evalNum(evalArgAt(args, 0))), true
	default:
		return nil, false
	}
}

func evalAvgCall(ns []float64) any {
	if len(ns) == 0 {
		return nil
	}
	m := 0.0
	for i, n := range ns {
		d := n - m
		denom := float64(i + 1)
		if !math.IsInf(d, 0) && !math.IsNaN(d) {
			m += d / denom
		} else {
			m = m - m/denom + n/denom
		}
	}
	return numOrNil(m)
}

func evalMinMaxCall(ns []float64, wantMax bool) any {
	if len(ns) == 0 {
		return nil
	}
	best := ns[0]
	for _, n := range ns[1:] {
		if (wantMax && n > best) || (!wantMax && n < best) {
			best = n
		}
	}
	return numOrNil(best)
}

func evalStringAndFormatCallFunc(fn string, args []any) (any, bool) {
	switch fn {
	case "fixed":
		n := evalNum(evalArgAt(args, 0))
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, true
		}
		digits := evalDigitsArg(evalArgAt(args, 1), 0)
		return strconv.FormatFloat(evalRoundTo(n, float64(digits)), 'f', digits, 64), true
	case "commas":
		n := evalNum(evalArgAt(args, 0))
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, true
		}
		if len(args) < 2 {
			return evalAddCommas(ToText(n)), true
		}
		digits := evalDigitsArg(args[1], 0)
		return evalAddCommas(strconv.FormatFloat(evalRoundTo(n, float64(digits)), 'f', digits, 64)), true
	case "compact":
		return evalCompactCall(args), true
	case "string":
		return ToText(evalArgAt(args, 0)), true
	case "upper":
		return boundedExprString(strings.ToUpper(ToText(evalArgAt(args, 0)))), true
	case "lower":
		return boundedExprString(strings.ToLower(ToText(evalArgAt(args, 0)))), true
	default:
		return nil, false
	}
}

func evalCompactCall(args []any) any {
	n := evalNum(evalArgAt(args, 0))
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	digits := 1
	if len(args) >= 2 {
		digits = evalDigitsArg(args[1], 0)
	}
	abs := math.Abs(n)
	units := []struct {
		scale      float64
		suffix     string
		upperScale float64
		upperSuf   string
	}{
		{1e12, "T", 0, ""},
		{1e9, "B", 1e12, "T"},
		{1e6, "M", 1e9, "B"},
		{1e3, "K", 1e6, "M"},
	}
	for _, u := range units {
		if abs >= u.scale {
			r := evalRoundTo(n/u.scale, float64(digits))
			if math.Abs(r) >= 1000 && u.upperScale > 0 {
				return ToText(evalRoundTo(n/u.upperScale, float64(digits))) + u.upperSuf
			}
			return ToText(r) + u.suffix
		}
	}
	r := evalRoundTo(n, float64(digits))
	if math.Abs(r) >= 1000 {
		return ToText(evalRoundTo(n/1e3, float64(digits))) + "K"
	}
	return ToText(r)
}

func evalCollectionCallFunc(fn string, args []any) any {
	switch fn {
	case "len":
		v := normEvalVal(evalArgAt(args, 0))
		if s, ok := v.(string); ok {
			return float64(utf8.RuneCountInString(s))
		}
		return float64(len(evalAsList(v)))
	case "count":
		return float64(len(evalAsList(evalArgAt(args, 0))))
	case "join":
		sep := ", "
		if len(args) >= 2 {
			sep = ToText(args[1])
		}
		return evalJoinList(evalAsList(evalArgAt(args, 0)), sep)
	case "contains":
		return evalContainsCall(normEvalVal(evalArgAt(args, 0)), normEvalVal(evalArgAt(args, 1)))
	case "empty":
		return evalEmptyCall(normEvalVal(evalArgAt(args, 0)))
	case "unique":
		return evalUnique(evalAsList(evalArgAt(args, 0)))
	default:
		return nil
	}
}

func evalContainsCall(hay, needle any) bool {
	if s, ok := hay.(string); ok {
		return strings.Contains(s, ToText(needle))
	}
	for _, item := range evalAsList(hay) {
		if evalEqual(item, needle) {
			return true
		}
	}
	return false
}

func evalEmptyCall(v any) bool {
	if v == nil {
		return true
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		for _, mv := range x {
			arr, ok := normEvalVal(mv).([]any)
			if !ok || len(arr) > 0 {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func evalMemberVal(obj any, name string) any {
	obj = normEvalVal(obj)
	switch x := obj.(type) {
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = evalMemberVal(x[i], name)
		}
		return out
	case map[string]any:
		v, exists := x[name]
		if !exists {
			return nil
		}
		return normEvalVal(v)
	default:
		return nil
	}
}

func evalAST(n *evalNode, scope map[string]any) any {
	switch n.kind {
	case evalLit:
		return n.val
	case evalRef:
		if scope == nil {
			return nil
		}
		v, exists := scope[n.name]
		if !exists {
			return nil
		}
		return normEvalVal(v)
	case evalMember:
		return evalMemberVal(evalAST(n.obj, scope), n.name)
	case evalCall:
		args := make([]any, len(n.args))
		for i, a := range n.args {
			args[i] = evalAST(a, scope)
		}
		out := evalCallFunc(n.name, args)
		if f, ok := out.(float64); ok {
			return numOrNil(f)
		}
		return out
	case evalUnary:
		v := evalAST(n.l, scope)
		if n.op == "!" {
			return !Truthy(v)
		}
		return numOrNil(-evalNum(v))
	case evalCond:
		if Truthy(evalAST(n.c, scope)) {
			return evalAST(n.a, scope)
		}
		return evalAST(n.b, scope)
	case evalBin:
		return evalBinNode(n, scope)
	}
	return nil
}

func evalBinNode(n *evalNode, scope map[string]any) any {
	if n.op == "&&" {
		return Truthy(evalAST(n.l, scope)) && Truthy(evalAST(n.r, scope))
	}
	if n.op == "||" {
		return Truthy(evalAST(n.l, scope)) || Truthy(evalAST(n.r, scope))
	}
	l := evalAST(n.l, scope)
	r := evalAST(n.r, scope)
	switch n.op {
	case "==":
		return evalEqual(l, r)
	case "!=":
		return !evalEqual(l, r)
	case "+":
		return evalPlusOp(l, r)
	case "-":
		return numOrNil(evalNum(l) - evalNum(r))
	case "*":
		return numOrNil(evalNum(l) * evalNum(r))
	case "/":
		d := evalNum(r)
		if d == 0 {
			return nil
		}
		return numOrNil(evalNum(l) / d)
	case "%":
		d := evalNum(r)
		if d == 0 {
			return nil
		}
		return numOrNil(math.Mod(evalNum(l), d))
	default:
		return evalRelationalOp(n.op, l, r)
	}
}

func evalPlusOp(l, r any) any {
	ls, lIsStr := l.(string)
	rs, rIsStr := r.(string)
	if lIsStr && rIsStr {
		return boundedExprString(ls + rs)
	}
	if lIsStr || rIsStr {
		s, other := ls, r
		if rIsStr {
			s, other = rs, l
		}
		sn := evalNum(s)
		on := evalNum(other)
		numeric := strings.TrimSpace(s) != "" && !math.IsNaN(sn) && !math.IsInf(sn, 0) && !math.IsNaN(on) && !math.IsInf(on, 0)
		if !numeric {
			return boundedExprString(ToText(l) + ToText(r))
		}
	}
	return numOrNil(evalNum(l) + evalNum(r))
}

func isScalarEvalVal(v any) bool {
	switch v.(type) {
	case []any, map[string]any:
		return false
	default:
		return true
	}
}

func evalRelationalOp(op string, l, r any) bool {
	ln := evalNum(l)
	rn := evalNum(r)
	if !math.IsNaN(ln) && !math.IsInf(ln, 0) && !math.IsNaN(rn) && !math.IsInf(rn, 0) && isScalarEvalVal(l) && isScalarEvalVal(r) {
		switch op {
		case "<":
			return ln < rn
		case "<=":
			return ln <= rn
		case ">":
			return ln > rn
		default:
			return ln >= rn
		}
	}
	lt := ToText(l)
	rt := ToText(r)
	switch op {
	case "<":
		return lt < rt
	case "<=":
		return lt <= rt
	case ">":
		return lt > rt
	default:
		return lt >= rt
	}
}

// Evaluate parses and evaluates src against scope, returning an error only when
// src fails to parse. Mirrors evaluate() in web/src/app/chat/ui/genui/expr.ts.
func Evaluate(src string, scope map[string]any) (any, error) {
	ast, err := parseEvalAST(src)
	if err != nil {
		return nil, err
	}
	return evalAST(ast, scope), nil
}

// EvaluateSafe evaluates src against scope, returning fallback on parse error.
func EvaluateSafe(src string, scope map[string]any, fallback any) any {
	v, err := Evaluate(src, scope)
	if err != nil {
		return fallback
	}
	return v
}
