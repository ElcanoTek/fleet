package agentcore

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	fantasyschema "charm.land/fantasy/schema"
	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/metrics"
)

// MCP tool input schemas reach the model rebuilt as {type, properties,
// required} (mcpTool.Info), so the server's `$schema` marker never travels.
// Anthropic then validates every tool as JSON Schema draft 2020-12 and rejects
// the WHOLE request when one tool fails ("tools.N.custom.input_schema: JSON
// schema is invalid"), which fails every turn in any conversation that has the
// offending connector on. OpenAI and Gemini accept the older forms, so the
// failure is Claude-only and looks model-specific. Two layers:
//
//  1. normalizeToolSchema rewrites the older-draft constructs that are INVALID
//     under 2020-12 into their 2020-12 equivalents, preserving what the tool
//     accepts. The MCP TypeScript SDK emits draft-07, where a zod tuple becomes
//     `items: [...]` (Pages, 2026-09-30).
//  2. toolInputSchemaProblem validates the final schema against the 2020-12
//     metaschema, so buildFantasyToolsWithRoster can skip a tool that is still
//     invalid instead of letting it take every other tool down with it.

// Keywords whose value is a schema, an array of schemas, or a map of name ->
// schema. Only these positions are schemas: property NAMES and the values of
// annotation/validation keywords (default, examples, const, enum, required)
// are data and are copied verbatim, so a property literally named "items" or a
// default object with a "pattern" key is never rewritten.
var (
	schemaValuedKeywords = map[string]bool{
		"items": true, "additionalItems": true, "additionalProperties": true,
		"contains": true, "propertyNames": true, "not": true,
		"if": true, "then": true, "else": true,
		"unevaluatedItems": true, "unevaluatedProperties": true, "contentSchema": true,
	}
	schemaArrayKeywords = map[string]bool{
		"allOf": true, "anyOf": true, "oneOf": true, "prefixItems": true,
	}
	schemaMapKeywords = map[string]bool{
		"properties": true, "patternProperties": true, "$defs": true,
		"definitions": true, "dependentSchemas": true,
		// draft-07 `dependencies`: each value is a schema or a string array.
		// normalizeToolSchema passes non-schema values through unchanged.
		"dependencies": true,
	}
)

// normalizeToolSchema returns a deep copy of an MCP tool's input schema with
// older-draft constructs rewritten for draft 2020-12:
//
//   - positional `items: [a, b]` becomes `prefixItems: [a, b]`, and
//     `additionalItems: X` becomes `items: X` (without additionalItems, extra
//     elements stay allowed, exactly as in draft-07). `additionalItems` next
//     to a non-array `items` was ignored by draft-07 and is dropped.
//   - draft-04 boolean `exclusiveMinimum`/`exclusiveMaximum` fold into the
//     numeric 2020-12 form (`minimum: 5, exclusiveMinimum: true` becomes
//     `exclusiveMinimum: 5`); `false` is dropped.
//   - draft-03 boolean `required` on a property is hoisted into the parent's
//     `required` array.
//   - `pattern` values using `\p{…}` Unicode property escapes are dropped:
//     OpenAI's function-calling validator accepts ECMA-262 only.
//   - local `$ref` pointers into top-level `#/$defs/<Name>` or
//     `#/definitions/<Name>` (emitted by FastMCP / Pydantic for structured
//     tool parameters) are inlined into the schema tree so `mcpTool.Info()` —
//     which extracts `{properties, required}` without `$defs` — never sends a
//     dangling `$ref` to Gemini (`400 INVALID_ARGUMENT: reference to undefined
//     schema`) or hides nested fields from `tool_describe`.
func normalizeToolSchema(schema map[string]any) map[string]any {
	out, _ := normalizeToolSchemaWithNotes(schema)
	return out
}

// normalizeToolSchemaWithNotes is normalizeToolSchema plus one note per
// older-draft rewrite, each prefixed with the JSON Pointer of the schema it
// changed ("/properties/expect/…: positional items array rewritten to
// prefixItems"), so an operator can tell a connector's authors exactly what to
// update. Stripping a `\p{` pattern and inlining top-level `$defs`/`definitions`
// references are provider/boundary compatibility steps, not outdated constructs,
// and are not noted.
func normalizeToolSchemaWithNotes(schema map[string]any) (map[string]any, []string) {
	var notes []string
	out, _ := normalizeSchemaNode(schema, "", &notes).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	return inlineLocalRefs(out), notes
}

func normalizeSchemaNode(node any, path string, notes *[]string) any {
	obj, ok := node.(map[string]any)
	if !ok {
		// A boolean schema, or a malformed value the validator will judge.
		return copyJSONValue(node)
	}
	out := make(map[string]any, len(obj))
	var hoisted []string
	for key, value := range obj {
		switch {
		case key == "pattern":
			if s, ok := value.(string); ok && strings.Contains(s, `\p{`) {
				continue
			}
			out[key] = value
		case key == "required":
			if _, isBool := value.(bool); isBool {
				continue // draft-03 form; the parent hoists it
			}
			out[key] = copyJSONValue(value)
		case schemaValuedKeywords[key]:
			if items, isArray := value.([]any); isArray && key == "items" {
				out[key] = normalizeSchemaArray(items, path+"/items", notes) // rewritten below
				continue
			}
			out[key] = normalizeSchemaNode(value, path+"/"+key, notes)
		case schemaArrayKeywords[key]:
			if items, isArray := value.([]any); isArray {
				out[key] = normalizeSchemaArray(items, path+"/"+key, notes)
			} else {
				out[key] = copyJSONValue(value)
			}
		case schemaMapKeywords[key]:
			members, isMap := value.(map[string]any)
			if !isMap {
				out[key] = copyJSONValue(value)
				continue
			}
			normalized := make(map[string]any, len(members))
			for name, member := range members {
				memberPath := path + "/" + key + "/" + jsonPointerToken(name)
				if key == "properties" {
					if child, ok := member.(map[string]any); ok && child["required"] == true {
						hoisted = append(hoisted, name)
						noteRewrite(notes, memberPath, "boolean required moved into the parent's required list")
					}
				}
				normalized[name] = normalizeSchemaNode(member, memberPath, notes)
			}
			out[key] = normalized
		default:
			out[key] = copyJSONValue(value)
		}
	}

	if tuple, isArray := out["items"].([]any); isArray {
		out["prefixItems"] = tuple
		delete(out, "items")
		if rest, has := out["additionalItems"]; has {
			out["items"] = rest
			noteRewrite(notes, path, "positional items array rewritten to prefixItems (additionalItems moved to items)")
		} else {
			noteRewrite(notes, path, "positional items array rewritten to prefixItems")
		}
	} else if _, has := out["additionalItems"]; has {
		noteRewrite(notes, path, "additionalItems beside a single items schema dropped (draft-07 ignored it)")
	}
	delete(out, "additionalItems")

	if foldExclusiveBound(out, "exclusiveMinimum", "minimum") {
		noteRewrite(notes, path, "boolean exclusiveMinimum rewritten to the numeric form")
	}
	if foldExclusiveBound(out, "exclusiveMaximum", "maximum") {
		noteRewrite(notes, path, "boolean exclusiveMaximum rewritten to the numeric form")
	}

	if len(hoisted) > 0 {
		out["required"] = mergeRequired(out["required"], hoisted)
	}
	return out
}

func normalizeSchemaArray(items []any, path string, notes *[]string) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = normalizeSchemaNode(item, fmt.Sprintf("%s/%d", path, i), notes)
	}
	return out
}

func noteRewrite(notes *[]string, path, what string) {
	if notes == nil {
		return
	}
	if path == "" {
		path = "/"
	}
	*notes = append(*notes, path+": "+what)
}

// jsonPointerToken escapes one JSON Pointer reference token (RFC 6901).
func jsonPointerToken(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// unescapeJSONPointerToken decodes one RFC 6901 JSON Pointer reference token.
func unescapeJSONPointerToken(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

var schemaAnnotationKeywords = map[string]bool{
	"title": true, "description": true, "default": true, "examples": true,
	"$comment": true, "deprecated": true, "readOnly": true, "writeOnly": true,
}

var interactingKeywordFamilies = [][]string{
	{"properties", "patternProperties", "additionalProperties", "unevaluatedProperties"},
	{"items", "prefixItems", "additionalItems", "unevaluatedItems", "contains", "minContains", "maxContains"},
	{"if", "then", "else"},
	{"dependencies", "dependentRequired", "dependentSchemas"},
}

func siblingInteractsWithTarget(key string, target map[string]any) bool {
	if _, exists := target[key]; exists {
		return true
	}
	for _, family := range interactingKeywordFamilies {
		inFamily := false
		for _, member := range family {
			if member == key {
				inFamily = true
				break
			}
		}
		if !inFamily {
			continue
		}
		for _, member := range family {
			if _, exists := target[member]; exists {
				return true
			}
		}
	}
	return false
}

type defScope struct {
	root        map[string]any
	defs        map[string]any
	definitions map[string]any
}

func newDefScope(resource map[string]any) defScope {
	var defs, definitions map[string]any
	if d, ok := resource["$defs"].(map[string]any); ok && len(d) > 0 {
		defs, _ = copyJSONValue(d).(map[string]any)
	}
	if d, ok := resource["definitions"].(map[string]any); ok && len(d) > 0 {
		definitions, _ = copyJSONValue(d).(map[string]any)
	}
	rootCopy, _ := copyJSONValue(resource).(map[string]any)
	return defScope{root: rootCopy, defs: defs, definitions: definitions}
}

func hasNonFragmentID(obj map[string]any) bool {
	id, ok := obj["$id"].(string)
	return ok && id != "" && !strings.HasPrefix(id, "#")
}

// inlineLocalRefs resolves `#/$defs/<Name>` and `#/definitions/<Name>`
// references inside root against the active resource's `$defs` / `definitions`
// maps. Recursive back-references are converted to shallow provider-safe stubs
// without `$ref` so mcpTool.Info() never forwards a dangling reference.
func inlineLocalRefs(root map[string]any) map[string]any {
	if len(root) == 0 {
		return root
	}
	scope := newDefScope(root)
	resolved := inlineSchemaNodeRefs(root, scope, map[string]bool{"#": true}, true)
	switch r := resolved.(type) {
	case map[string]any:
		return r
	case bool:
		if !r {
			return map[string]any{"not": map[string]any{}}
		}
		return map[string]any{}
	default:
		return root
	}
}

func inlineSchemaNodeRefs(node any, scope defScope, active map[string]bool, isResourceRoot bool) any {
	obj, ok := node.(map[string]any)
	if !ok {
		return node
	}
	if !isResourceRoot && hasNonFragmentID(obj) {
		scope = newDefScope(obj)
		active = map[string]bool{"#": true}
	}
	if ref, hasRef := obj["$ref"].(string); hasRef {
		if target, canonKey, found := lookupLocalDef(ref, scope); found {
			var resolved any
			if active[canonKey] {
				resolved = recursiveRefStub(target, scope)
			} else {
				nextActive := cloneActiveRefs(active, canonKey)
				resolved = inlineSchemaNodeRefs(copyJSONValue(target), scope, nextActive, false)
			}
			if len(obj) > 1 {
				return mergeResolvedWithSiblings(resolved, obj, scope, active)
			}
			return resolved
		}
	}
	inlineSchemaMapFields(obj, scope, active)
	return obj
}

func inlineSchemaMapFields(obj map[string]any, scope defScope, active map[string]bool) {
	for key, value := range obj {
		switch {
		case schemaValuedKeywords[key]:
			obj[key] = inlineSchemaNodeRefs(value, scope, active, false)
		case schemaArrayKeywords[key]:
			if items, isArray := value.([]any); isArray {
				for i, item := range items {
					items[i] = inlineSchemaNodeRefs(item, scope, active, false)
				}
			}
		case schemaMapKeywords[key]:
			if members, isMap := value.(map[string]any); isMap {
				for name, member := range members {
					memberActive := active
					if key == "$defs" || key == "definitions" {
						memberActive = cloneActiveRefs(active, key+":"+name)
					}
					members[name] = inlineSchemaNodeRefs(member, scope, memberActive, false)
				}
			}
		}
	}
}

func cloneActiveRefs(active map[string]bool, addKey string) map[string]bool {
	next := make(map[string]bool, len(active)+1)
	for k, v := range active {
		next[k] = v
	}
	next[addKey] = true
	return next
}

func recursiveRefStub(target any, scope defScope) any {
	seen := map[string]bool{}
	cur := target
	for {
		if b, ok := cur.(bool); ok {
			return b
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return map[string]any{}
		}
		if ref, hasRef := obj["$ref"].(string); hasRef {
			if next, key, found := lookupLocalDef(ref, scope); found && !seen[key] {
				seen[key] = true
				cur = next
				continue
			}
		}
		stub := map[string]any{}
		if t, ok := obj["type"].(string); ok && t != "array" {
			stub["type"] = t
		}
		if title, ok := obj["title"].(string); ok {
			stub["title"] = title
		}
		if desc, ok := obj["description"].(string); ok {
			stub["description"] = desc
		}
		return stub
	}
}

func mergeResolvedWithSiblings(resolved any, obj map[string]any, scope defScope, active map[string]bool) any {
	siblings := make(map[string]any, len(obj)-1)
	for k, v := range obj {
		if k != "$ref" {
			siblings[k] = copyJSONValue(v)
		}
	}
	inlineSchemaMapFields(siblings, scope, active)

	if b, isBool := resolved.(bool); isBool {
		if !b {
			return false
		}
		return siblings
	}
	resMap, ok := resolved.(map[string]any)
	if !ok {
		return siblings
	}
	out, _ := copyJSONValue(resMap).(map[string]any)
	var conflicts map[string]any
	for k, v := range siblings {
		switch {
		case schemaAnnotationKeywords[k] || k == "$defs" || k == "definitions":
			out[k] = v
		case k == "required":
			if existing, has := out["required"]; has {
				out["required"] = mergeRequiredAny(existing, v)
			} else {
				out["required"] = v
			}
		case k == "allOf":
			if existing, ok := out["allOf"].([]any); ok {
				if extra, ok := v.([]any); ok {
					out["allOf"] = append(existing, extra...)
					continue
				}
			}
			out["allOf"] = v
		default:
			if !siblingInteractsWithTarget(k, resMap) {
				out[k] = v
			} else if existing, exists := resMap[k]; !exists || !reflect.DeepEqual(existing, v) {
				if conflicts == nil {
					conflicts = map[string]any{}
				}
				conflicts[k] = v
			}
		}
	}
	if len(conflicts) > 0 {
		allOf, _ := out["allOf"].([]any)
		out["allOf"] = append(allOf, conflicts)
	}
	return out
}

func mergeRequiredAny(a, b any) []any {
	var extra []string
	switch names := b.(type) {
	case []any:
		for _, v := range names {
			if s, ok := v.(string); ok {
				extra = append(extra, s)
			}
		}
	case []string:
		extra = append(extra, names...)
	}
	return mergeRequired(a, extra)
}

func isSchemaValue(v any) bool {
	switch v.(type) {
	case map[string]any, bool:
		return true
	default:
		return false
	}
}

func lookupLocalDef(ref string, scope defScope) (any, string, bool) {
	if ref == "#" && scope.root != nil {
		return scope.root, "#", true
	}
	if name, ok := strings.CutPrefix(ref, "#/$defs/"); ok && !strings.Contains(name, "/") {
		decoded := unescapeJSONPointerToken(name)
		if d, exists := scope.defs[decoded]; exists && isSchemaValue(d) {
			return d, "$defs:" + decoded, true
		}
	}
	if name, ok := strings.CutPrefix(ref, "#/definitions/"); ok && !strings.Contains(name, "/") {
		decoded := unescapeJSONPointerToken(name)
		if d, exists := scope.definitions[decoded]; exists && isSchemaValue(d) {
			return d, "definitions:" + decoded, true
		}
	}
	return nil, "", false
}

// foldExclusiveBound rewrites the draft-04 boolean exclusive bound and reports
// whether it did. A numeric value is already the 2020-12 form and is left
// alone.
func foldExclusiveBound(node map[string]any, exclusiveKey, boundKey string) bool {
	flag, isBool := node[exclusiveKey].(bool)
	if !isBool {
		return false
	}
	delete(node, exclusiveKey)
	if bound, has := node[boundKey]; flag && has {
		node[exclusiveKey] = bound
		delete(node, boundKey)
	}
	return true
}

func mergeRequired(existing any, hoisted []string) []any {
	merged := []any{}
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			merged = append(merged, name)
		}
	}
	switch names := existing.(type) {
	case []any:
		for _, v := range names {
			if s, ok := v.(string); ok {
				add(s)
			}
		}
	case []string:
		for _, s := range names {
			add(s)
		}
	}
	for _, name := range hoisted {
		add(name)
	}
	return merged
}

// copyJSONValue deep-copies a decoded JSON value so a normalized schema never
// aliases the catalog entry it came from.
func copyJSONValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = copyJSONValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = copyJSONValue(vv)
		}
		return out
	case []string:
		return append([]string(nil), t...)
	default:
		return v
	}
}

var draft2020Metaschema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	return c.Compile("https://json-schema.org/draft/2020-12/schema")
})

// toolInputSchemaProblem reports why a tool's input schema, in the exact shape
// fantasy sends (the {type, properties, required} object after
// fantasyschema.Normalize), is not valid JSON Schema draft 2020-12. nil means
// valid. It checks the schema against the metaschema only: it does not resolve
// $ref or assert formats, so it never refuses a schema the providers accept
// today on those grounds.
func toolInputSchemaProblem(info fantasy.ToolInfo) error {
	meta, err := draft2020Metaschema()
	if err != nil {
		// Fail open: an unavailable validator must never remove tools.
		log.Printf("tool schema check disabled: compile draft 2020-12 metaschema: %v", err)
		return nil
	}
	required := info.Required
	if required == nil {
		required = []string{}
	}
	doc := map[string]any{
		"type":       "object",
		"properties": copyJSONValue(info.Parameters),
		"required":   required,
	}
	fantasyschema.Normalize(doc)
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode input schema: %w", err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("decode input schema: %w", err)
	}
	if err := meta.Validate(decoded); err != nil {
		return schemaValidationSummary(err)
	}
	return nil
}

// schemaValidationSummary reduces the validator's error tree to its first leaf
// ("at /properties/x/items: expected object or boolean"), which names the
// offending location; the full tree is noise in a log line.
func schemaValidationSummary(err error) error {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err
	}
	for len(ve.Causes) > 0 {
		ve = ve.Causes[0]
	}
	location := ve.InstanceLocation
	if location == "" {
		location = "/"
	}
	return fmt.Errorf("at %s: %s", location, ve.Message)
}

// emitToolSchemaInvalid reports a tool skipped by the schema gate, mirroring
// persona_tool_blocked.
func emitToolSchemaInvalid(obs Observer, tool, reason string) {
	if obs == nil {
		return
	}
	obs.Observe("mcp_tool_schema_invalid", map[string]any{
		"tool":   tool,
		"reason": reason,
	})
}

// Tool schema findings, as reported by CheckMCPToolSchema.
const (
	// ToolSchemaRewritten: the schema used older-draft constructs that Fleet
	// translated. The tool works; its connector should still be updated.
	ToolSchemaRewritten = "rewritten"
	// ToolSchemaInvalid: the schema is not valid draft 2020-12 even after
	// translation, so the tool is withheld from the model.
	ToolSchemaInvalid = "invalid"
)

// ToolSchemaIssue is one MCP tool whose input schema needed attention.
type ToolSchemaIssue struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
	// Status is ToolSchemaRewritten or ToolSchemaInvalid.
	Status string `json:"status"`
	// Detail names what was rewritten, or why the schema is invalid, with the
	// JSON Pointer of each location.
	Detail string `json:"detail"`
	// LastSeen is when a running turn last evaluated the tool with this
	// finding. Zero outside the runtime record (ToolSchemaIssues).
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// CheckMCPToolSchema runs one MCP tool's input schema through exactly the
// translation and validation the model boundary applies, for callers outside
// a turn (`fleet mcp test`, the remote-connector probe). ok is false when the
// schema needs nothing.
func CheckMCPToolSchema(server string, tool mcp.Tool) (issue ToolSchemaIssue, ok bool) {
	return checkMCPToolSchema(&mcpTool{serverName: server, tool: tool})
}

func checkMCPToolSchema(mt *mcpTool) (ToolSchemaIssue, bool) {
	key, cacheable := toolSchemaCacheKey(mt)
	if cacheable {
		toolSchemaVerdicts.Lock()
		verdict, hit := toolSchemaVerdicts.m[key]
		toolSchemaVerdicts.Unlock()
		if hit {
			return verdict.issueFor(mt), verdict.ok
		}
	}
	verdict := evaluateToolSchema(mt)
	if cacheable {
		toolSchemaVerdicts.Lock()
		if len(toolSchemaVerdicts.m) >= toolSchemaVerdictCap {
			toolSchemaVerdicts.m = map[[sha256.Size]byte]toolSchemaVerdict{}
		}
		toolSchemaVerdicts.m[key] = verdict
		toolSchemaVerdicts.Unlock()
	}
	return verdict.issueFor(mt), verdict.ok
}

// toolSchemaVerdict is the schema-determined part of a finding. The verdict
// depends only on the schema's content (and whether file references are
// advertised), never on the tool's name, so it is cached by content hash.
// Uncached, the check costs a few hundred microseconds per tool (about 10 ms
// for Pages' 45 tools), and the catalog is re-listed as fresh maps on every
// turn; cached, it is the key's encode + hash (about 13 µs per tool).
type toolSchemaVerdict struct {
	ok     bool
	status string
	detail string
}

func (v toolSchemaVerdict) issueFor(mt *mcpTool) ToolSchemaIssue {
	if !v.ok {
		return ToolSchemaIssue{}
	}
	return ToolSchemaIssue{Server: mt.serverName, Tool: mt.tool.Name, Status: v.status, Detail: v.detail}
}

// toolSchemaVerdictCap bounds the cache; past it the cache restarts empty
// (a catalog churning through thousands of distinct schemas only loses
// memoization, never correctness).
const toolSchemaVerdictCap = 4096

var toolSchemaVerdicts = struct {
	sync.Mutex
	m map[[sha256.Size]byte]toolSchemaVerdict
}{m: map[[sha256.Size]byte]toolSchemaVerdict{}}

// toolSchemaCacheKey hashes everything the verdict depends on. encoding/json
// sorts map keys, so equal schemas hash equally. An unencodable schema is
// simply not cached.
func toolSchemaCacheKey(mt *mcpTool) ([sha256.Size]byte, bool) {
	raw, err := json.Marshal(struct {
		Schema   map[string]any `json:"s"`
		FileRefs bool           `json:"f"`
	}{mt.tool.InputSchema, mt.readWorkspaceFile != nil})
	if err != nil {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256(raw), true
}

func evaluateToolSchema(mt *mcpTool) toolSchemaVerdict {
	_, notes := normalizeToolSchemaWithNotes(mt.tool.InputSchema)
	if problem := toolInputSchemaProblem(mt.Info()); problem != nil {
		detail := problem.Error()
		if len(notes) > 0 {
			detail += " (after rewriting: " + strings.Join(notes, "; ") + ")"
		}
		return toolSchemaVerdict{ok: true, status: ToolSchemaInvalid, detail: detail}
	}
	if len(notes) > 0 {
		return toolSchemaVerdict{ok: true, status: ToolSchemaRewritten, detail: strings.Join(notes, "; ")}
	}
	return toolSchemaVerdict{}
}

// The runtime record of schema findings, keyed by server and tool. Every turn
// that builds a tool refreshes its entry and a clean tool clears it, so the
// record follows a connector fix without a restart. A tool that disappears
// from the catalog is never evaluated again and keeps its entry until restart;
// LastSeen shows how stale it is. The record is bounded by the catalog size and
// lives in process memory: after a restart it refills as turns run, which is
// why the admin read reports ToolSchemaRecordSince alongside it.
var toolSchemaRecord = struct {
	sync.Mutex
	issues map[string]ToolSchemaIssue
}{issues: map[string]ToolSchemaIssue{}}

// toolSchemaRecordSince is when this process's record started (process start).
var toolSchemaRecordSince = time.Now().UTC()

// ToolSchemaRecordSince reports when the runtime record started filling: a
// finding older than a restart is not in it until a turn builds that tool.
func ToolSchemaRecordSince() time.Time { return toolSchemaRecordSince }

// noteToolSchemaIssue records (ok) or clears (!ok) the finding for one tool.
// A new or changed finding is logged once and counted in
// fleet_mcp_tool_schema_issues_total; repeats of the same finding, every turn,
// are silent.
func noteToolSchemaIssue(server, tool string, issue ToolSchemaIssue, ok bool) {
	key := server + "\x00" + tool
	toolSchemaRecord.Lock()
	prev, had := toolSchemaRecord.issues[key]
	if !ok {
		delete(toolSchemaRecord.issues, key)
		toolSchemaRecord.Unlock()
		if had {
			log.Printf("MCP tool mcp_%s_%s: schema issue cleared (%s before)", server, tool, prev.Status)
		}
		return
	}
	issue.LastSeen = time.Now().UTC()
	toolSchemaRecord.issues[key] = issue
	toolSchemaRecord.Unlock()
	if had && prev.Status == issue.Status && prev.Detail == issue.Detail {
		return
	}
	switch issue.Status {
	case ToolSchemaInvalid:
		log.Printf("MCP tool mcp_%s_%s skipped: input schema is not valid JSON Schema draft 2020-12: %s — the connector must fix its schema", server, tool, issue.Detail)
	default:
		log.Printf("MCP tool mcp_%s_%s: input schema uses older JSON Schema drafts, translated for the model: %s — the connector should update its schema", server, tool, issue.Detail)
	}
	metrics.RecordToolSchemaIssue(server, issue.Status)
}

// ToolSchemaIssues returns the runtime record, sorted by server then tool.
func ToolSchemaIssues() []ToolSchemaIssue {
	toolSchemaRecord.Lock()
	out := make([]ToolSchemaIssue, 0, len(toolSchemaRecord.issues))
	for _, issue := range toolSchemaRecord.issues {
		out = append(out, issue)
	}
	toolSchemaRecord.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Tool < out[j].Tool
	})
	return out
}

// resetToolSchemaRecordForTest empties the process-wide record.
func resetToolSchemaRecordForTest() {
	toolSchemaRecord.Lock()
	toolSchemaRecord.issues = map[string]ToolSchemaIssue{}
	toolSchemaRecord.Unlock()
}
