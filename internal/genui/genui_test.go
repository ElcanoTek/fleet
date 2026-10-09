package genui

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type exprCase struct {
	Expr  string   `json:"expr"`
	Valid bool     `json:"valid"`
	Refs  []string `json:"refs"`
}

func TestExpressionsFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/expressions.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Cases []exprCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.Cases {
		refs, err := ParseExpr(c.Expr)
		if c.Valid != (err == nil) {
			t.Errorf("%q: valid=%v, got err=%v", c.Expr, c.Valid, err)
			continue
		}
		if c.Valid && c.Refs != nil {
			if refs == nil {
				refs = []string{}
			}
			if !reflect.DeepEqual(refs, c.Refs) {
				t.Errorf("%q: refs %v, want %v", c.Expr, refs, c.Refs)
			}
		}
	}
}

func TestExprBounds(t *testing.T) {
	if _, err := ParseExpr(strings.Repeat("1+", 300) + "1"); err == nil {
		t.Error("over-long expression accepted")
	}
	if _, err := ParseExpr(strings.Repeat("(", 60) + "1" + strings.Repeat(")", 60)); err == nil {
		t.Error("over-deep expression accepted")
	}
	if _, err := ParseExpr(strings.Repeat("!", 100) + "a"); err == nil {
		t.Error("over-deep unary chain accepted")
	}
}

func TestTemplateExprs(t *testing.T) {
	got, err := TemplateExprs("a {{ x }} b {{y+1}}")
	if err != nil || !reflect.DeepEqual(got, []string{" x ", "y+1"}) {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := TemplateExprs("plain"); err != nil || got != nil {
		t.Fatalf("plain: %q %v", got, err)
	}
	if _, err := TemplateExprs("{{ x"); err == nil {
		t.Fatal("unclosed template accepted")
	}
}

type cardCase struct {
	Name          string          `json:"name"`
	Valid         bool            `json:"valid"`
	ErrorContains string          `json:"error_contains"`
	Card          json.RawMessage `json:"card"`
}

func loadCards(t *testing.T) []cardCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/cards.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Cases []cardCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return fx.Cases
}

func TestCardsFixture(t *testing.T) {
	for _, c := range loadCards(t) {
		t.Run(c.Name, func(t *testing.T) {
			card, issues := Validate(c.Card)
			if c.Valid {
				if len(issues) != 0 {
					t.Fatalf("expected valid, got %v", issues)
				}
				if card.Title == "" || len(card.Components) == 0 {
					t.Fatalf("valid card did not decode: %+v", card)
				}
				return
			}
			if len(issues) == 0 {
				t.Fatalf("expected issues containing %q, got none", c.ErrorContains)
			}
			var all []string
			for _, is := range issues {
				all = append(all, is.String())
			}
			joined := strings.Join(all, "\n")
			if !strings.Contains(joined, c.ErrorContains) {
				t.Fatalf("issues %q do not mention %q", joined, c.ErrorContains)
			}
		})
	}
}

func TestLimits(t *testing.T) {
	big := `{"title":"x","components":[{"type":"text","text":"` + strings.Repeat("a", MaxSpecBytes) + `"}]}`
	if _, issues := Validate([]byte(big)); len(issues) == 0 {
		t.Error("oversized card accepted")
	}
	var comps []string
	for i := 0; i < MaxNodes+5; i++ {
		comps = append(comps, `{"type":"divider"}`)
	}
	many := `{"title":"x","components":[` + strings.Join(comps, ",") + `]}`
	_, issues := Validate([]byte(many))
	if len(issues) != 1 || !strings.Contains(issues[0].Message, "more than") {
		t.Errorf("node cap: %v", issues)
	}
	deep := `{"type":"text","text":"x"}`
	for i := 0; i < MaxDepth+2; i++ {
		deep = `{"type":"section","children":[` + deep + `]}`
	}
	if _, issues := Validate([]byte(`{"title":"x","components":[` + deep + `]}`)); len(issues) == 0 {
		t.Error("over-deep card accepted")
	}
	if _, issues := Validate([]byte(`[]`)); len(issues) == 0 {
		t.Error("non-object accepted")
	}
}

// A huge min_items is refused without sizing anything by it: the browser
// materializes min_items items, and the validator models them too.
func TestHugeMinItemsIsRefusedCheaply(t *testing.T) {
	for _, n := range []string{"1000000000", "1e300"} {
		card := `{"title":"x","components":[{"type":"repeater","id":"r","label":"R","min_items":` + n + `,"fields":[{"type":"toggle","id":"ok","label":"OK","required":true,"disabled":true}]}],"actions":[{"id":"go","label":"Go"}]}`
		_, issues := Validate([]byte(card))
		found := false
		for _, is := range issues {
			found = found || strings.Contains(is.Message, fmt.Sprintf("at most %d", MaxRepeaterItems))
		}
		if !found {
			t.Errorf("min_items %s: %v", n, issues)
		}
	}
}

func TestDisplayListCap(t *testing.T) {
	items := make([]string, MaxDisplayItems+1)
	for i := range items {
		items[i] = `{"text":"x"}`
	}
	card := `{"title":"x","components":[{"type":"badges","items":[` + strings.Join(items, ",") + `]}]}`
	_, issues := Validate([]byte(card))
	if len(issues) == 0 || !strings.Contains(issues[0].Message, fmt.Sprintf("at most %d entries", MaxDisplayItems)) {
		t.Fatalf("issues: %v", issues)
	}
	card = `{"title":"x","components":[{"type":"badges","items":[` + strings.Join(items[:MaxDisplayItems], ",") + `]}]}`
	if _, issues := Validate([]byte(card)); len(issues) != 0 {
		t.Fatalf("at the cap: %v", issues)
	}
}

func TestCardRowBudget(t *testing.T) {
	rows := strings.TrimSuffix(strings.Repeat(`{},`, MaxTableRows), ",")
	table := `{"type":"table","columns":[{"key":"a"}],"rows":[` + rows + `]}`
	tables := func(n int) string {
		ts := make([]string, n)
		for i := range ts {
			ts[i] = table
		}
		return `{"title":"x","components":[` + strings.Join(ts, ",") + `]}`
	}
	if _, issues := Validate([]byte(tables(MaxCardRows / MaxTableRows))); len(issues) != 0 {
		t.Fatalf("at the budget: %v", issues)
	}
	_, issues := Validate([]byte(tables(MaxCardRows/MaxTableRows + 1)))
	if len(issues) != 1 || !strings.Contains(issues[0].Message, fmt.Sprintf("more than %d table rows", MaxCardRows)) {
		t.Fatalf("over the budget: %v", issues)
	}
}

func TestCardChartPointBudget(t *testing.T) {
	labels := make([]string, MaxChartPoints)
	vals := make([]string, MaxChartPoints)
	for i := range labels {
		labels[i] = `"l"`
		vals[i] = "0"
	}
	series := make([]string, MaxChartSeries)
	for i := range series {
		series[i] = `{"name":"s","values":[` + strings.Join(vals, ",") + `]}`
	}
	chart := `{"type":"chart","kind":"bar","labels":[` + strings.Join(labels, ",") + `],"series":[` + strings.Join(series, ",") + `]}`
	charts := func(n int) string {
		cs := make([]string, n)
		for i := range cs {
			cs[i] = chart
		}
		return `{"title":"x","components":[` + strings.Join(cs, ",") + `]}`
	}
	full := MaxCardChartPoints / (MaxChartSeries * MaxChartPoints)
	if _, issues := Validate([]byte(charts(full))); len(issues) != 0 {
		t.Fatalf("at the budget: %v", issues)
	}
	_, issues := Validate([]byte(charts(full + 1)))
	if len(issues) != 1 || !strings.Contains(issues[0].Message, "points in all") {
		t.Fatalf("over the budget: %v", issues)
	}
}

// A list default is capped by the entries the browser keeps from it: one
// string splits on its line breaks (listCount), so it can carry more than
// MaxListItems entries in a single element.
func TestListCapCountsSplitLines(t *testing.T) {
	lines := make([]string, MaxListItems+1)
	for i := range lines {
		lines[i] = strconv.FormatInt(int64(i), 36)
	}
	val, _ := json.Marshal([]string{strings.Join(lines, "\n")})
	card := `{"title":"x","components":[{"type":"list_input","id":"l","label":"L","disabled":true,"value":` + string(val) + `}],"actions":[{"id":"go","label":"Go"}]}`
	_, issues := Validate([]byte(card))
	if len(issues) == 0 || !strings.Contains(issues[0].Message, fmt.Sprintf("at most %d items", MaxListItems)) {
		t.Fatalf("issues: %v", issues)
	}
	val, _ = json.Marshal([]string{strings.Join(lines[:MaxListItems], "\n")})
	card = `{"title":"x","components":[{"type":"list_input","id":"l","label":"L","disabled":true,"value":` + string(val) + `}],"actions":[{"id":"go","label":"Go"}]}`
	if _, issues := Validate([]byte(card)); len(issues) != 0 {
		t.Fatalf("at the cap: %v", issues)
	}
}

func TestIssueCapIsReported(t *testing.T) {
	var comps []string
	for i := 0; i < 40; i++ {
		comps = append(comps, `{"type":"nope"}`)
	}
	_, issues := Validate([]byte(`{"title":"x","components":[` + strings.Join(comps, ",") + `]}`))
	if len(issues) != maxIssuesReported+1 || !strings.Contains(issues[len(issues)-1].Message, "more issue") {
		t.Fatalf("got %d issues: %v", len(issues), issues[len(issues)-1])
	}
}

// The catalog is a three-way contract: this validator, the tool description
// the model reads, and the web renderer registry. The fixture below is the
// list the web test also asserts against (genui/catalog.json).
func TestCatalogMatchesFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if got := ComponentTypes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog drift:\n got  %v\n want %v", got, want)
	}
}

// TestDisabledDefaultsFixture pins the validator to the browser's checkField:
// a disabled field is submitted unvalidated, so its default must be accepted
// here exactly when checkField would pass it. The browser half runs the same
// file (web/src/app/chat/ui/genui/model.test.ts).
func TestDisabledDefaultsFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/disabled_defaults.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Cases []struct {
			Name      string         `json:"name"`
			Component map[string]any `json:"component"`
			Valid     bool           `json:"valid"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			comp := map[string]any{"disabled": true}
			for k, v := range c.Component {
				comp[k] = v
			}
			card, _ := json.Marshal(map[string]any{
				"title":      "x",
				"components": []any{comp},
				"actions":    []any{map[string]any{"id": "go", "label": "Go"}},
			})
			_, issues := Validate(card)
			if c.Valid && len(issues) != 0 {
				t.Fatalf("expected the default accepted, got %v", issues)
			}
			if !c.Valid && len(issues) == 0 {
				t.Fatal("expected the default refused, got no issues")
			}
		})
	}
}
