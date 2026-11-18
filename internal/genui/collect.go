package genui

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxListTextUnits = 1_000_000

var collectEmailRe = regexp.MustCompile("^[a-zA-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)+$")

var collectInputTypes = map[string]bool{
	"text_input":      true,
	"number":          true,
	"slider":          true,
	"select":          true,
	"choice":          true,
	"multi_select":    true,
	"toggle":          true,
	"date":            true,
	"list_input":      true,
	"include_exclude": true,
	"repeater":        true,
}

func isInputComponent(c map[string]any) bool {
	typ, _ := c["type"].(string)
	if collectInputTypes[typ] {
		return true
	}
	if typ == "table" {
		sel, _ := c["select"].(string)
		_, hasID := c["id"].(string)
		return hasID && (sel == "single" || sel == "multi")
	}
	return false
}

func compChildren(c map[string]any, key string) []map[string]any {
	raw, ok := c[key].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			if _, hasType := m["type"].(string); hasType {
				out = append(out, m)
			}
		}
	}
	return out
}

func compTabs(c map[string]any) [][]map[string]any {
	raw, ok := c["tabs"].([]any)
	if !ok {
		return nil
	}
	out := make([][]map[string]any, 0, len(raw))
	for _, t := range raw {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, compChildren(tm, "children"))
	}
	return out
}

func walkInputComponents(list []map[string]any, visit func(map[string]any)) {
	for _, c := range list {
		if isInputComponent(c) {
			visit(c)
		}
		if typ, _ := c["type"].(string); typ == "repeater" {
			continue
		}
		walkInputComponents(compChildren(c, "children"), visit)
		for _, tabChildren := range compTabs(c) {
			walkInputComponents(tabChildren, visit)
		}
	}
}

func hasPasswordInputAnywhere(list []map[string]any) bool {
	for _, c := range list {
		if isInputComponent(c) {
			if format, _ := c["format"].(string); strings.EqualFold(strings.TrimSpace(format), "password") {
				return true
			}
		}
		if hasPasswordInputAnywhere(compChildren(c, "children")) {
			return true
		}
		if hasPasswordInputAnywhere(compChildren(c, "fields")) {
			return true
		}
		for _, tabChildren := range compTabs(c) {
			if hasPasswordInputAnywhere(tabChildren) {
				return true
			}
		}
	}
	return false
}

func compOptionSet(c map[string]any) map[string]bool {
	raw, ok := c["options"].([]any)
	if !ok {
		return nil
	}
	out := make(map[string]bool, len(raw))
	for _, o := range raw {
		switch v := o.(type) {
		case string:
			out[v] = true
		case map[string]any:
			if s, ok := v["value"].(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

func strSliceOf(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func scanListText(text string, dedupe bool) []string {
	text = clipUTF16Units(text, maxListTextUnits)
	out := []string{}
	seen := map[string]bool{}
	start := 0
	for start < len(text) {
		stop := start
		for stop < len(text) && text[stop] != '\n' && text[stop] != '\r' {
			stop++
		}
		t := strings.TrimSpace(text[start:stop])
		start = stop + 1
		if t == "" || (dedupe && seen[t]) {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) > MaxListItems {
			return out
		}
	}
	return out
}

func clipUTF16Units(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	units := 0
	for idx, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > limit {
			return s[:idx]
		}
		units += w
	}
	return s
}

func isValidCalendarDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i := 0; i < 10; i++ {
		if i == 4 || i == 7 {
			continue
		}
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	if s[:4] == "0000" {
		return false
	}
	dt, err := time.Parse("2006-01-02", s)
	if err != nil {
		return false
	}
	return dt.Format("2006-01-02") == s
}

func isValidWebURL(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t\r\n<>\"`") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x1f || s[i] == 0x7f {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, " \t\r\n<>\"`#/?@\\^|{}[]") {
		return false
	}
	if strings.HasPrefix(u.Host, "[") {
		addr, perr := netip.ParseAddr(host)
		if perr != nil || !addr.Is6() {
			return false
		}
	}
	rawHost := u.Host
	if strings.HasPrefix(rawHost, "[") {
		closeIdx := strings.IndexByte(rawHost, ']')
		if closeIdx < 0 {
			return false
		}
		rawHost = rawHost[closeIdx+1:]
	} else if colonIdx := strings.IndexByte(rawHost, ':'); colonIdx >= 0 {
		rawHost = rawHost[colonIdx:]
	} else {
		rawHost = ""
	}
	if strings.HasPrefix(rawHost, ":") {
		portStr := rawHost[1:]
		if portStr != "" {
			if len(portStr) > 5 {
				return false
			}
			for i := 0; i < len(portStr); i++ {
				if portStr[i] < '0' || portStr[i] > '9' {
					return false
				}
			}
			portNum, aerr := strconv.Atoi(portStr)
			if aerr != nil || portNum < 1 || portNum > 65535 {
				return false
			}
		}
	} else if rawHost != "" {
		return false
	}
	return true
}

func defaultComponentValue(c map[string]any) any {
	v := c["value"]
	typ, _ := c["type"].(string)
	custom, _ := c["allow_custom"].(bool)

	switch typ {
	case "text_input":
		if s, ok := v.(string); ok {
			return s
		}
		return ""
	case "date":
		if s, ok := v.(string); ok && isValidCalendarDate(s) {
			return s
		}
		return ""
	case "select", "choice":
		if s, ok := v.(string); ok && compOptionSet(c)[s] {
			return s
		}
		return ""
	case "number":
		return finiteFloatOrNil(v)
	case "slider":
		if f := finiteFloatOrNil(v); f != nil {
			return f
		}
		if f := finiteFloatOrNil(c["min"]); f != nil {
			return f
		}
		return float64(0)
	case "toggle":
		b, _ := v.(bool)
		return b
	case "multi_select":
		return defaultMultiSelectValue(c, v, custom)
	case "list_input":
		return defaultListInputValue(c, v)
	case "include_exclude":
		return defaultIncludeExcludeValue(c, v, custom)
	case "table":
		return defaultTableValue(c, v)
	case "repeater":
		return defaultRepeaterValue(c, v)
	default:
		return nil
	}
}

func finiteFloatOrNil(v any) any {
	if f, ok := v.(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
		return f
	}
	return nil
}

func defaultMultiSelectValue(c map[string]any, v any, custom bool) []string {
	opts := compOptionSet(c)
	seen := map[string]bool{}
	out := []string{}
	for _, x := range strSliceOf(v) {
		if seen[x] {
			continue
		}
		seen[x] = true
		if opts[x] || (custom && strings.TrimSpace(x) != "") {
			out = append(out, x)
			if len(out) >= MaxChoiceItems {
				break
			}
		}
	}
	return out
}

func defaultListInputValue(c map[string]any, v any) []string {
	dedupe := true
	if d, ok := c["dedupe"].(bool); ok && !d {
		dedupe = false
	}
	return scanListText(strings.Join(strSliceOf(v), "\n"), dedupe)
}

func defaultIncludeExcludeValue(c map[string]any, v any, custom bool) map[string]any {
	obj, _ := v.(map[string]any)
	opts := compOptionSet(c)
	okOpt := func(x string) bool {
		return opts[x] || (custom && strings.TrimSpace(x) != "")
	}
	inc := collectAllowedUniqueChoices(strSliceOf(obj["include"]), okOpt, nil, MaxChoiceItems)
	incSet := make(map[string]bool, len(inc))
	for _, x := range inc {
		incSet[x] = true
	}
	exc := collectAllowedUniqueChoices(strSliceOf(obj["exclude"]), okOpt, incSet, MaxChoiceItems-len(inc))
	return map[string]any{
		"include": inc,
		"exclude": exc,
	}
}

func collectAllowedUniqueChoices(items []string, allow func(string) bool, skip map[string]bool, limit int) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, x := range items {
		if limit <= 0 {
			break
		}
		if seen[x] || skip[x] {
			continue
		}
		seen[x] = true
		if allow(x) {
			out = append(out, x)
			limit--
		}
	}
	return out
}

func defaultTableValue(c map[string]any, v any) any {
	rowKey, _ := c["row_key"].(string)
	rowKeys := map[string]bool{}
	if rows, ok := c["rows"].([]any); ok {
		for _, r := range rows {
			if rm, ok := r.(map[string]any); ok {
				if ks, ok := rm[rowKey].(string); ok {
					rowKeys[ks] = true
				}
			}
		}
	}
	if sel, _ := c["select"].(string); sel == "multi" {
		seen := map[string]bool{}
		out := []string{}
		for _, k := range strSliceOf(v) {
			if !seen[k] && rowKeys[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
		return out
	}
	if s, ok := v.(string); ok && rowKeys[s] {
		return s
	}
	return ""
}

func defaultRepeaterValue(c map[string]any, v any) []any {
	fields := compChildren(c, "fields")
	if arr, ok := v.([]any); ok {
		out := make([]any, 0, min(len(arr), MaxRepeaterItems))
		for _, rawItem := range arr {
			im, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			if len(out) >= MaxRepeaterItems {
				break
			}
			merged := newRepeaterItem(fields)
			walkInputComponents(fields, func(f map[string]any) {
				fid, _ := f["id"].(string)
				if fid == "" {
					return
				}
				if iv, exists := im[fid]; exists {
					probe := maps.Clone(f)
					probe["value"] = iv
					merged[fid] = defaultComponentValue(probe)
				}
			})
			out = append(out, merged)
		}
		return out
	}
	n := 1
	if minItems, ok := c["min_items"].(float64); ok {
		n = max(1, min(MaxRepeaterItems, int(minItems)))
	}
	out := make([]any, n)
	for i := 0; i < n; i++ {
		out[i] = newRepeaterItem(fields)
	}
	return out
}

func newRepeaterItem(fields []map[string]any) map[string]any {
	item := map[string]any{}
	walkInputComponents(fields, func(f map[string]any) {
		if id, _ := f["id"].(string); id != "" {
			item[id] = defaultComponentValue(f)
		}
	})
	return item
}

func initialCardValues(components []map[string]any) map[string]any {
	values := map[string]any{}
	walkInputComponents(components, func(c map[string]any) {
		if id, _ := c["id"].(string); id != "" {
			values[id] = defaultComponentValue(c)
		}
	})
	return values
}

func scopeForCollect(values map[string]any, item map[string]any, index int) map[string]any {
	if item == nil {
		return values
	}
	merged := make(map[string]any, len(values)+len(item)+1)
	maps.Copy(merged, values)
	maps.Copy(merged, item)
	merged["index"] = float64(index + 1)
	return merged
}

func isNodeVisible(obj map[string]any, scope map[string]any) bool {
	vis, ok := obj["visible_if"].(string)
	if !ok || strings.TrimSpace(vis) == "" {
		return true
	}
	return Truthy(EvaluateSafe(vis, scope, true))
}

func isFieldEmpty(c map[string]any, v any) bool {
	if v == nil {
		return true
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) == ""
	case []string:
		return len(x) == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		if typ, _ := c["type"].(string); typ == "include_exclude" {
			inc, _ := x["include"].([]string)
			exc, _ := x["exclude"].([]string)
			return len(inc) == 0 && len(exc) == 0
		}
	}
	return false
}

func checkCollectedField(c map[string]any, v any) string {
	typ, _ := c["type"].(string)
	reqField, _ := c["required"].(bool)
	if reqField && typ == "toggle" {
		if b, _ := v.(bool); b {
			return ""
		}
		return "Required"
	}
	if reqField && isFieldEmpty(c, v) {
		return "Required"
	}
	if isFieldEmpty(c, v) {
		return ""
	}
	switch typ {
	case "text_input":
		return checkTextInputField(c, v)
	case "number", "slider":
		return checkNumericField(c, typ, v)
	case "multi_select":
		return checkMultiSelectField(c, v)
	case "list_input":
		return checkListInputField(c, v)
	case "date":
		return checkDateField(c, v)
	case "repeater":
		return checkRepeaterField(c, v)
	default:
		return ""
	}
}

func checkTextInputField(c map[string]any, v any) string {
	s, _ := v.(string)
	uLen := utf16Len(s)
	if minLen, ok := c["min_length"].(float64); ok && float64(uLen) < minLen {
		return "min_length"
	}
	if maxLen, ok := c["max_length"].(float64); ok && float64(uLen) > maxLen {
		return "max_length"
	}
	format, _ := c["format"].(string)
	if format == "email" && !collectEmailRe.MatchString(strings.TrimSpace(s)) {
		return "email"
	}
	if format == "url" && !isValidWebURL(strings.TrimSpace(s)) {
		return "url"
	}
	return ""
}

func checkNumericField(c map[string]any, typ string, v any) string {
	n, ok := v.(float64)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
		return "number"
	}
	if minVal, ok := c["min"].(float64); ok && n < minVal {
		return "min"
	}
	if maxVal, ok := c["max"].(float64); ok && n > maxVal {
		return "max"
	}
	step, hasStep := c["step"].(float64)
	if !hasStep && typ == "slider" {
		step = 1
		hasStep = true
	}
	if hasStep && step > 0 {
		base := 0.0
		if minVal, ok := c["min"].(float64); ok {
			base = minVal
		}
		k := (n - base) / step
		if math.Abs(k-math.Round(k)) > 1e-9 {
			return "step"
		}
	}
	return ""
}

func checkMultiSelectField(c map[string]any, v any) string {
	if maxItems, ok := c["max_items"].(float64); ok {
		if arr, ok := v.([]string); ok && float64(len(arr)) > maxItems {
			return "max_items"
		}
	}
	return ""
}

func checkListInputField(c map[string]any, v any) string {
	capItems := float64(MaxListItems)
	if maxItems, ok := c["max_items"].(float64); ok && maxItems < capItems {
		capItems = maxItems
	}
	if arr, ok := v.([]string); ok && float64(len(arr)) > capItems {
		return "max_items"
	}
	return ""
}

func checkDateField(c map[string]any, v any) string {
	s, _ := v.(string)
	if minDate, ok := c["min"].(string); ok && s < minDate {
		return "min_date"
	}
	if maxDate, ok := c["max"].(string); ok && s > maxDate {
		return "max_date"
	}
	return ""
}

func checkRepeaterField(c map[string]any, v any) string {
	n := 0
	if arr, ok := v.([]any); ok {
		n = len(arr)
	}
	if minItems, ok := c["min_items"].(float64); ok && float64(n) < minItems {
		return "min_items"
	}
	if maxItems, ok := c["max_items"].(float64); ok && float64(n) > maxItems {
		return "max_items"
	}
	return ""
}

func collectCardSubmission(components []map[string]any, values map[string]any) (submitted map[string]any, errs map[string]string, hasVisibleInputs bool) {
	submitted = map[string]any{}
	errs = map[string]string{}

	var visit func(list []map[string]any, scope map[string]any, read map[string]any, write map[string]any, prefix string, disabled bool)
	visit = func(list []map[string]any, scope map[string]any, read map[string]any, write map[string]any, prefix string, disabled bool) {
		for _, c := range list {
			if !isNodeVisible(c, scope) {
				continue
			}
			id, _ := c["id"].(string)
			if isInputComponent(c) && id != "" {
				compDisabled, _ := c["disabled"].(bool)
				off := disabled || compDisabled
				v := read[id]
				if typ, _ := c["type"].(string); typ == "repeater" {
					fields := compChildren(c, "fields")
					rawItems, _ := v.([]any)
					items := make([]any, len(rawItems))
					for i, rawItem := range rawItems {
						im, _ := rawItem.(map[string]any)
						if im == nil {
							im = map[string]any{}
						}
						w := map[string]any{}
						visit(fields, scopeForCollect(values, im, i), im, w, fmt.Sprintf("%s[%d].", id, i), off)
						items[i] = w
					}
					write[id] = items
					if !off {
						hasVisibleInputs = true
						if errMsg := checkCollectedField(c, rawItems); errMsg != "" {
							errs[prefix+id] = errMsg
						}
					}
					continue
				}
				if !off {
					hasVisibleInputs = true
					if errMsg := checkCollectedField(c, v); errMsg != "" {
						errs[prefix+id] = errMsg
					}
				}
				write[id] = v
			}
			visit(compChildren(c, "children"), scope, read, write, prefix, disabled)
			for _, tabChildren := range compTabs(c) {
				visit(tabChildren, scope, read, write, prefix, disabled)
			}
		}
	}

	visit(components, scopeForCollect(values, nil, 0), values, submitted, "", false)
	return submitted, errs, hasVisibleInputs
}

func selectEligibleSubmitAction(rawActions []any, scope map[string]any) (string, bool) {
	var (
		visibleSubmits        []map[string]any
		enabledSubmits        []map[string]any
		visiblePrimarySubmits []map[string]any
		enabledPrimarySubmits []map[string]any
	)
	for _, raw := range rawActions {
		a, ok := raw.(map[string]any)
		if !ok || !isNodeVisible(a, scope) {
			continue
		}
		kindStr, _ := a["kind"].(string)
		if kindStr != "" && kindStr != "submit" {
			continue
		}
		visibleSubmits = append(visibleSubmits, a)
		style, _ := a["style"].(string)
		isPrimary := style == "" || style == "primary"
		if isPrimary {
			visiblePrimarySubmits = append(visiblePrimarySubmits, a)
		}
		disabled := false
		if disExpr, ok := a["disabled_if"].(string); ok && strings.TrimSpace(disExpr) != "" {
			disabled = Truthy(EvaluateSafe(disExpr, scope, false))
		}
		if !disabled {
			enabledSubmits = append(enabledSubmits, a)
			if isPrimary {
				enabledPrimarySubmits = append(enabledPrimarySubmits, a)
			}
		}
	}

	var chosen map[string]any
	switch {
	case len(visibleSubmits) == 1 && len(enabledSubmits) == 1:
		chosen = enabledSubmits[0]
	case len(visiblePrimarySubmits) == 1 && len(enabledPrimarySubmits) == 1:
		chosen = enabledPrimarySubmits[0]
	default:
		return "", false
	}

	id, _ := chosen["id"].(string)
	if strings.TrimSpace(id) == "" {
		return "", false
	}
	if style, _ := chosen["style"].(string); strings.EqualFold(strings.TrimSpace(style), "danger") {
		return "", false
	}
	if confirm, ok := chosen["confirm"].(string); ok && strings.TrimSpace(confirm) != "" {
		return "", false
	}
	if validate, ok := chosen["validate"].(bool); ok && !validate {
		return "", false
	}
	return id, true
}

// CollectInitialSubmit evaluates whether a show_ui card spec is eligible for
// server-side 10-second auto-submission in Autonomous mode and returns the
// selected submit action ID and initial serialized values JSON.
//
// Mirrors initialValues(spec) + collect(spec, values) + blockingPaths in
// web/src/app/chat/ui/genui/model.ts and GenerativeCard.tsx.
func CollectInitialSubmit(specJSON []byte) (actionID string, valuesJSON []byte, eligible bool) {
	card, issues := Validate(specJSON)
	if len(issues) > 0 {
		return "", nil, false
	}
	if card.AutoSubmit != nil && !*card.AutoSubmit {
		return "", nil, false
	}
	if strings.TrimSpace(card.Panel) != "" {
		return "", nil, false
	}
	if len(card.FieldErrors) > 0 {
		return "", nil, false
	}

	components := make([]map[string]any, 0, len(card.Components))
	for _, raw := range card.Components {
		if m, ok := raw.(map[string]any); ok {
			components = append(components, m)
		}
	}
	if hasPasswordInputAnywhere(components) {
		return "", nil, false
	}

	values := initialCardValues(components)
	submitted, errs, hasVisibleInputs := collectCardSubmission(components, values)
	if len(errs) > 0 || !hasVisibleInputs {
		return "", nil, false
	}

	actID, ok := selectEligibleSubmitAction(card.Actions, scopeForCollect(values, nil, 0))
	if !ok {
		return "", nil, false
	}

	valBytes, err := json.Marshal(submitted)
	if err != nil {
		return "", nil, false
	}

	// Verify the built submission message fits within MaxSubmissionBytes when
	// JSON-encoded (matching submissionBytes in model.ts). Use a generous 64-byte
	// placeholder for call_id so long provider call IDs never push it over.
	msg := fmt.Sprintf("[UI submission] card=%s action=%s auto=1\n```json\n%s\n```",
		strings.Repeat("x", 64), actID, string(valBytes))
	msgEncoded, err := json.Marshal(msg)
	if err != nil || len(msgEncoded) > MaxSubmissionBytes {
		return "", nil, false
	}

	return actID, valBytes, true
}

// IsDisplayOnly reports whether a card spec only shows something: no footer
// actions and no input component at any depth. It is the rule dashboard view
// uses to put a card with no panel key on the board (isDisplayOnly in
// web/src/app/chat/ui/dashboard/board.ts); a card that can send something is
// a step in the conversation instead. A spec that does not parse is not
// display-only.
func IsDisplayOnly(specJSON []byte) bool {
	var spec struct {
		Components []map[string]any `json:"components"`
		Actions    []any            `json:"actions"`
	}
	if json.Unmarshal(specJSON, &spec) != nil || len(spec.Actions) > 0 {
		return false
	}
	hasInput := false
	walkInputComponents(spec.Components, func(map[string]any) { hasInput = true })
	return !hasInput
}
