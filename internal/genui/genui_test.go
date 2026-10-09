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

func TestCardRowBudgetCountsRepeaterItems(t *testing.T) {
	items := strings.TrimSuffix(strings.Repeat(`{},`, MaxRepeaterItems), ",")
	reps := func(n int) string {
		rs := make([]string, n)
		for i := range rs {
			rs[i] = fmt.Sprintf(`{"type":"repeater","id":"r%d","label":"R","value":[%s],"fields":[{"type":"divider"}]}`, i, items)
		}
		return `{"title":"x","components":[` + strings.Join(rs, ",") + `],"actions":[{"id":"go","label":"Go"}]}`
	}
	if _, issues := Validate([]byte(reps(MaxCardRows / MaxRepeaterItems))); len(issues) != 0 {
		t.Fatalf("at the budget: %v", issues)
	}
	_, issues := Validate([]byte(reps(MaxCardRows/MaxRepeaterItems + 1)))
	if len(issues) != 1 || !strings.Contains(issues[0].Message, "repeater item fields") {
		t.Fatalf("over the budget: %v", issues)
	}
}

func TestRepeaterDefaultEntriesCap(t *testing.T) {
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = fmt.Sprintf("%q", strconv.Itoa(i))
	}
	card := func(items int) string {
		return fmt.Sprintf(`{"title":"x","components":[{"type":"repeater","id":"r","label":"R","min_items":%d,"max_items":%d,"fields":[{"type":"list_input","id":"l","label":"L","value":[%s]}]}],"actions":[{"id":"go","label":"Go"}]}`, items, items, strings.Join(lines, ","))
	}
	if _, issues := Validate([]byte(card(MaxListItems / 200))); len(issues) != 0 {
		t.Fatalf("at the cap: %v", issues)
	}
	_, issues := Validate([]byte(card(MaxListItems/200 + 1)))
	if len(issues) != 1 || !strings.Contains(issues[0].Message, "list entries across the") {
		t.Fatalf("over the cap: %v", issues)
	}
}

func TestRepeaterNestedDisplayCountsPerItem(t *testing.T) {
	rows := strings.TrimSuffix(strings.Repeat(`{"a":"x"},`, 20), ",")
	card := func(items int) string {
		return fmt.Sprintf(`{"title":"x","components":[{"type":"repeater","id":"r","label":"R","min_items":%d,"max_items":%d,"fields":[{"type":"text_input","id":"n","label":"N"},{"type":"table","columns":[{"key":"a"}],"rows":[%s]}]}],"actions":[{"id":"go","label":"Go"}]}`, items, items, rows)
	}
	// Each item renders its two components and the table's 20 rows: 22.
	if _, issues := Validate([]byte(card(MaxCardRows / 22))); len(issues) != 0 {
		t.Fatalf("within the budget: %v", issues)
	}
	_, issues := Validate([]byte(card(MaxCardRows/22 + 2)))
	if len(issues) == 0 || !strings.Contains(issues[0].Message, "more than") {
		t.Fatalf("over the budget: %v", issues)
	}
}

// A repeater's items can grow to max_items (200 when unset), so the most it
// can render must fit the card-wide row budget.
func TestRepeaterMaxItemsFitsRowBudget(t *testing.T) {
	fields := strings.TrimSuffix(strings.Repeat(`{"type":"divider"},`, 20), ",")
	card := func(maxItems string) string {
		return `{"title":"x","components":[{"type":"repeater","id":"r","label":"R"` + maxItems + `,"fields":[` + fields + `]}],"actions":[{"id":"go","label":"Go"}]}`
	}
	if _, issues := Validate([]byte(card(""))); len(issues) == 0 || !strings.Contains(issues[0].Message, "set max_items") {
		t.Fatalf("default max_items: %v", issues)
	}
	if _, issues := Validate([]byte(card(`,"max_items":100`))); len(issues) != 0 {
		t.Fatalf("max_items 100: %v", issues)
	}
}

// Repeaters are budgeted at the items they can grow to, card-wide: two
// ten-field repeaters at the default 200 items cannot share 2,000 rows.
func TestRepeaterGrowthIsBudgetedCardWide(t *testing.T) {
	fields := strings.TrimSuffix(strings.Repeat(`{"type":"divider"},`, 10), ",")
	rep := func(id, maxItems string) string {
		return `{"type":"repeater","id":"` + id + `","label":"R"` + maxItems + `,"fields":[` + fields + `]}`
	}
	two := `{"title":"x","components":[` + rep("a", "") + `,` + rep("b", "") + `],"actions":[{"id":"go","label":"Go"}]}`
	if _, issues := Validate([]byte(two)); len(issues) == 0 {
		t.Fatal("two 200-item repeaters accepted")
	}
	fit := `{"title":"x","components":[` + rep("a", `,"max_items":100`) + `,` + rep("b", `,"max_items":100`) + `],"actions":[{"id":"go","label":"Go"}]}`
	if _, issues := Validate([]byte(fit)); len(issues) != 0 {
		t.Fatalf("two 100-item repeaters: %v", issues)
	}
	chart := `{"title":"x","components":[{"type":"repeater","id":"c","label":"C","fields":[{"type":"chart","kind":"bar","labels":[` + strings.TrimSuffix(strings.Repeat(`"l",`, 200), ",") + `],"series":[{"name":"s","values":[` + strings.TrimSuffix(strings.Repeat(`1,`, 200), ",") + `]}]}]}],"actions":[{"id":"go","label":"Go"}]}`
	if _, issues := Validate([]byte(chart)); len(issues) == 0 {
		t.Fatal("a chart in a 200-item repeater accepted")
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

// Codex round 41: repeater budgets charge what the browser can actually
// render, no more and no less.
func TestRepeaterBudgetEdges(t *testing.T) {
	act := `],"actions":[{"id":"go","label":"Go"}]}`
	// A disabled repeater cannot grow: it is budgeted at its opening items.
	fields := strings.TrimSuffix(strings.Repeat(`{"type":"divider"},`, 11), ",")
	dis := `{"title":"x","components":[{"type":"repeater","id":"r","label":"R","disabled":true,"fields":[` + fields + `]}` + act
	if _, issues := Validate([]byte(dis)); len(issues) != 0 {
		t.Fatalf("disabled repeater: %v", issues)
	}
	// A nested chart is charged once per reachable item, not once extra:
	// two items × 8 series × 200 points is exactly the card budget.
	labels := strings.TrimSuffix(strings.Repeat(`"l",`, 200), ",")
	vals := strings.TrimSuffix(strings.Repeat(`1,`, 200), ",")
	series := strings.TrimSuffix(strings.Repeat(`{"name":"s","values":[`+vals+`]},`, 8), ",")
	chart := `{"title":"x","components":[{"type":"repeater","id":"c","label":"C","max_items":2,"fields":[{"type":"chart","kind":"line","labels":[` + labels + `],"series":[` + series + `]}]}` + act
	if _, issues := Validate([]byte(chart)); len(issues) != 0 {
		t.Fatalf("chart at the budget: %v", issues)
	}
	// Each item renders every choice option as a button.
	opts := make([]string, 20)
	for i := range opts {
		opts[i] = strconv.Quote(strconv.Itoa(i))
	}
	choice := func(maxItems int) string {
		return fmt.Sprintf(`{"title":"x","components":[{"type":"repeater","id":"r","label":"R","max_items":%d,"fields":[{"type":"choice","id":"c","label":"C","options":[%s]}]}`+act, maxItems, strings.Join(opts, ","))
	}
	// 21 rows per item (the choice and its 20 options).
	if _, issues := Validate([]byte(choice(MaxCardRows / 21))); len(issues) != 0 {
		t.Fatalf("choice within the budget: %v", issues)
	}
	if _, issues := Validate([]byte(choice(MaxCardRows/21 + 1))); len(issues) == 0 {
		t.Fatal("choice options over the budget accepted")
	}
	// A list_input default is counted by the lines it splits into.
	lines := make([]string, 200)
	for i := range lines {
		lines[i] = strconv.Itoa(i)
	}
	one, _ := json.Marshal([]string{strings.Join(lines, "\n")})
	list := func(items int) string {
		return fmt.Sprintf(`{"title":"x","components":[{"type":"repeater","id":"r","label":"R","max_items":%d,"fields":[{"type":"list_input","id":"l","label":"L","value":%s}]}`+act, items, one)
	}
	if _, issues := Validate([]byte(list(MaxListItems / 200))); len(issues) != 0 {
		t.Fatalf("split list within the cap: %v", issues)
	}
	if _, issues := Validate([]byte(list(MaxListItems/200 + 1))); len(issues) == 0 || !strings.Contains(issues[0].Message, "list entries across the") {
		t.Fatalf("split list over the cap: %v", issues)
	}
}

// A required multi_select or include_exclude without free entry must send at
// least one of its options, so the shortest one counts toward the answer.
func TestRequiredCollectionChoiceCountsInAnswer(t *testing.T) {
	long := strings.Repeat("a", 2000)
	card := func(typ, extra string) string {
		return `{"title":"x","components":[{"type":"repeater","id":"r","label":"R","min_items":200,"fields":[{"type":"` + typ + `","id":"m","label":"M","required":true` + extra + `,"options":["` + long + `"]}]}],"actions":[{"id":"go","label":"Go"}]}`
	}
	for _, typ := range []string{"multi_select", "include_exclude"} {
		if _, issues := Validate([]byte(card(typ, ""))); len(issues) == 0 || !strings.Contains(fmt.Sprint(issues), "more than one answer carries") {
			t.Fatalf("%s: %v", typ, issues)
		}
		if _, issues := Validate([]byte(card(typ, `,"allow_custom":true`))); len(issues) != 0 {
			t.Fatalf("%s with allow_custom: %v", typ, issues)
		}
	}
}

// A required selectable table sends at least one row key per item.
func TestRequiredTableSelectionCountsInAnswer(t *testing.T) {
	long := strings.Repeat("k", 2000)
	card := func(req string) string {
		return `{"title":"x","components":[{"type":"repeater","id":"r","label":"R","min_items":200,"max_items":200,"fields":[{"type":"table","id":"t","label":"T","select":"single"` + req + `,"row_key":"id","columns":[{"key":"id"}],"rows":[{"id":"` + long + `"}]}]}],"actions":[{"id":"go","label":"Go"}]}`
	}
	if _, issues := Validate([]byte(card(`,"required":true`))); len(issues) == 0 || !strings.Contains(fmt.Sprint(issues), "more than one answer carries") {
		t.Fatalf("required: %v", issues)
	}
	if _, issues := Validate([]byte(card(""))); len(issues) != 0 {
		t.Fatalf("optional: %v", issues)
	}
}

// A select renders every option as an <option> in each repeater item, so its
// options count toward the item's rows like a choice's buttons.
func TestRepeaterBudgetCountsSelectOptions(t *testing.T) {
	opts := make([]string, 99)
	for i := range opts {
		opts[i] = strconv.Quote(strconv.Itoa(i))
	}
	card := func(maxItems int) string {
		return fmt.Sprintf(`{"title":"x","components":[{"type":"repeater","id":"r","label":"R","max_items":%d,"fields":[{"type":"select","id":"s","label":"S","options":[%s]}]}],"actions":[{"id":"go","label":"Go"}]}`, maxItems, strings.Join(opts, ","))
	}
	// 100 rows an item (the select and its 99 options).
	if _, issues := Validate([]byte(card(MaxCardRows / 100))); len(issues) != 0 {
		t.Fatalf("within the budget: %v", issues)
	}
	if _, issues := Validate([]byte(card(MaxCardRows/100 + 1))); len(issues) == 0 {
		t.Fatal("select options over the budget accepted")
	}
}

// The minimum answer counts each input's real empty value: an
// include/exclude pair is an object with two lists, not "".
func TestMinimumAnswerCountsStructuredValues(t *testing.T) {
	long := strings.Repeat("a", 1260)
	card := `{"title":"x","components":[{"type":"repeater","id":"r","label":"R","min_items":200,"max_items":200,"fields":[` +
		`{"type":"select","id":"s","label":"S","required":true,"options":["` + long + `"]},` +
		`{"type":"include_exclude","id":"g","label":"G","required":true,"allow_custom":true}` +
		`]}],"actions":[{"id":"go","label":"Go"}]}`
	if _, issues := Validate([]byte(card)); len(issues) == 0 || !strings.Contains(fmt.Sprint(issues), "more than one answer carries") {
		t.Fatalf("issues: %v", issues)
	}
}

// A member can follow only an input name, whose fields the validator checks:
// on a call's result a misspelt field would render blank instead of failing.
func TestMemberAfterCallRefused(t *testing.T) {
	card := func(expr string) string {
		return `{"title":"x","components":[{"type":"repeater","id":"rows","label":"R","fields":[{"type":"number","id":"cpm","label":"C"}],"max_items":5},{"type":"text","text":"{{ ` + expr + ` }}"}],"actions":[{"id":"go","label":"Go"}]}`
	}
	if _, issues := Validate([]byte(card("unique(rows).cpn"))); len(issues) == 0 || !strings.Contains(fmt.Sprint(issues), "can follow only an input name") {
		t.Fatalf("call member: %v", issues)
	}
	if _, issues := Validate([]byte(card("sum(unique(rows.cpm))"))); len(issues) != 0 {
		t.Fatalf("member then call: %v", issues)
	}
	if _, issues := Validate([]byte(card("sum((rows).cpm)"))); len(issues) != 0 {
		t.Fatalf("parenthesized name: %v", issues)
	}
}
