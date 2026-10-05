package models

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/truncate"
)

// ExecutionRequirementsMarker is the literal line a prompt producer puts
// before its one-line EXECUTION REQUIREMENTS JSON object
// (docs/CONDITIONAL-TASK-COMPLETION.md). The scheduled runner parses the same
// marker at dispatch.
const ExecutionRequirementsMarker = "EXECUTION REQUIREMENTS (JSON):"

// ExecutionRequirementNamePattern is the identifier rule for the declaration's
// server and tool names, quoted in rejection messages so the author sees the
// rule their name broke.
const ExecutionRequirementNamePattern = `^[a-zA-Z0-9_.-]{1,200}$`

var executionRequirementName = regexp.MustCompile(ExecutionRequirementNamePattern)

// ExecutionRequirementsRosterRequiredToolsOnly is the one supported roster
// value (#1603); any other value, the empty string included, is refused.
const ExecutionRequirementsRosterRequiredToolsOnly = "required_tools_only"

// Bounds of the declaration, stated in the rejection messages.
const (
	maxExecutionRequirementsLine    = 16384
	maxExecutionRequirementsServers = 100
	maxExecutionRequirementsTools   = 200
	// maxQuotedRequirementIdentifier clamps the identifier a rejection quotes:
	// the message reaches the task editor, the run log, the dead-letter
	// notification and the parked schedule's reason, and an invalid name can
	// be as long as the whole JSON line.
	maxQuotedRequirementIdentifier = 120
)

// ExecutionRequirements is a task prompt's EXECUTION REQUIREMENTS declaration
// (docs/CONDITIONAL-TASK-COMPLETION.md) as the one parser reads it. The
// scheduled runner embeds it and adds only what the run's roster resolves, so
// dispatch and every save path read one grammar into one type. Keys it does
// not know are ignored for forward compatibility.
type ExecutionRequirements struct {
	Servers []string `json:"mcp_servers"`
	Tools   []string `json:"required_tools"`
	Network bool     `json:"network"`
	// Completion is the producer's deterministic completion predicate (#1602);
	// nil or an empty list declares none.
	Completion *ExecutionCompletion `json:"completion"`
	// Roster is the optional roster narrowing (#1603): nil when absent or
	// null, otherwise ExecutionRequirementsRosterRequiredToolsOnly — the
	// parser refuses every other value. Read it through RosterNarrowing.
	Roster *string `json:"roster"`
	// SerializationKey is the producer's declared mutual-exclusion key
	// (docs/TASK-SERIALIZATION.md), e.g. "pages:<slug>": nil when absent or
	// null, otherwise a value matching SerializationKeyPattern. A task created
	// without an explicit serialization_key takes this one (NewTask), so a task
	// saved from chat or pasted into the task form is serialized like one the
	// producer created through the API. Fleet never interprets it.
	SerializationKey *string `json:"serialization_key"`
}

// SerializationKeyPattern is the rule for a DECLARED serialization_key,
// quoted in the rejection: bounded, and limited to the characters a key such
// as "pages:<slug>" or "client/<id>" needs. (An explicit task field keeps its
// own, opaque contract.)
const SerializationKeyPattern = `^[A-Za-z0-9_.:/-]{1,200}$`

var declaredSerializationKey = regexp.MustCompile(SerializationKeyPattern)

// ExecutionCompletion is the completion clause (#1602): the run is complete
// once ANY listed tool has a successful execution. Names take the forms
// required_tools accepts. Unknown sibling keys are ignored.
type ExecutionCompletion struct {
	AnySucceeded []string `json:"any_succeeded"`
	// BlockedWhen optionally marks a predicate-completed run as BLOCKED rather
	// than plain success (nil = never). See CompletionBlockedWhen.
	BlockedWhen *CompletionBlockedWhen `json:"blocked_when"`
}

// CompletionBlockedWhen is the completion clause's optional blocked rule: a
// run that completed through the predicate, in which no any_succeeded tool
// other than Tool succeeded, and whose LAST successful execution of Tool
// passed a top-level string argument named Argument with a value listed in
// In, finishes successfully but with run outcome "blocked" — shown as Blocked
// in the Operations Center, not plain green. It is how a producer says "this
// recording call means the run correctly decided not to publish"; Fleet
// assigns no meaning to the tool, the argument or the values beyond that
// match. DetailArgument optionally names a top-level string argument of the
// same call whose (bounded) text explains the outcome.
//
// It changes nothing about retry, dead-letter or recurrence: a blocked run is
// a success. Unknown keys are ignored, so an older build that does not know
// the rule ignores it entirely.
type CompletionBlockedWhen struct {
	Tool           string   `json:"tool"`
	Argument       string   `json:"argument"`
	In             []string `json:"in"`
	DetailArgument string   `json:"detail_argument"`
}

// BlockedWhenArgumentPattern is the rule for blocked_when's argument and
// detail_argument: a top-level JSON argument name, quoted in rejections.
const BlockedWhenArgumentPattern = `^[A-Za-z_][A-Za-z0-9_]{0,63}$`

var blockedWhenArgument = regexp.MustCompile(BlockedWhenArgumentPattern)

// Bounds of blocked_when's value list.
const (
	maxBlockedWhenValues     = 20
	maxBlockedWhenValueRunes = 100
)

// RosterNarrowing is the declared roster narrowing: "" for none (no
// declaration, or no roster key), else ExecutionRequirementsRosterRequiredToolsOnly.
func (r *ExecutionRequirements) RosterNarrowing() string {
	if r == nil || r.Roster == nil {
		return ""
	}
	return *r.Roster
}

// ParseExecutionRequirements is the one parser of a task prompt's optional
// EXECUTION REQUIREMENTS declaration (#1601): at most one marker line, followed
// on the very next line by one bounded JSON object whose mcp_servers,
// required_tools and completion.any_succeeded are identifier arrays within
// their limits, and whose roster, when present, is "required_tools_only".
// (nil, nil) means the prompt declares nothing — the declaration is optional.
//
// The scheduled runner parses the declaration with it at dispatch, and every
// task write path validates with it (ValidateExecutionRequirements) before
// saving, because a malformed declaration is a property of the prompt text:
// it would fail every run identically, and at dispatch it used to be found
// only by a $0 dead-letter. It lives in models so storage (the ADR-0073
// parking breaker and the replay guard) and the handlers use it without
// importing the runner.
//
// It checks shape only. Whether the named servers and tools exist in a run's
// roster, and what they mean, is decided at dispatch; Fleet interprets
// nothing about them.
func ParseExecutionRequirements(prompt string) (*ExecutionRequirements, error) {
	lines := strings.Split(prompt, "\n")
	var found *ExecutionRequirements
	seen := false
	for i, line := range lines {
		if strings.TrimSpace(line) != ExecutionRequirementsMarker {
			continue
		}
		if seen {
			return nil, fmt.Errorf("execution requirements: the %q marker appears more than once; keep exactly one", ExecutionRequirementsMarker)
		}
		seen = true
		if i+1 == len(lines) {
			return nil, fmt.Errorf("execution requirements: nothing follows the marker; put the JSON object on the line directly after it")
		}
		body := lines[i+1]
		if strings.TrimSpace(body) == "" {
			return nil, fmt.Errorf("execution requirements: the line after the marker is blank; put the JSON object on the line directly after it")
		}
		if len(body) > maxExecutionRequirementsLine {
			return nil, fmt.Errorf("execution requirements: the JSON line after the marker is %d bytes; at most %d", len(body), maxExecutionRequirementsLine)
		}
		req, err := decodeExecutionRequirements(body)
		if err != nil {
			return nil, err
		}
		found = req
	}
	return found, nil
}

// DeclaredSerializationKey is the serialization_key a prompt's EXECUTION
// REQUIREMENTS declare, or "" when it declares none — no declaration, no key,
// or a malformed declaration (which every save path refuses on its own).
func DeclaredSerializationKey(prompt string) string {
	req, err := ParseExecutionRequirements(prompt)
	if err != nil || req == nil || req.SerializationKey == nil {
		return ""
	}
	return *req.SerializationKey
}

// ValidateExecutionRequirements reports whether a task prompt's optional
// EXECUTION REQUIREMENTS declaration is well-formed: ParseExecutionRequirements
// without the result. A prompt with no marker is valid.
func ValidateExecutionRequirements(prompt string) error {
	_, err := ParseExecutionRequirements(prompt)
	return err
}

// decodeExecutionRequirements decodes and checks the one JSON line after the
// marker.
func decodeExecutionRequirements(body string) (*ExecutionRequirements, error) {
	var req *ExecutionRequirements
	if err := json.Unmarshal([]byte(body), &req); err != nil || req == nil {
		detail := "not a JSON object"
		if err != nil {
			detail = err.Error()
		}
		return nil, fmt.Errorf("execution requirements: invalid JSON object after the marker (%s)", detail)
	}
	if len(req.Servers) > maxExecutionRequirementsServers || len(req.Tools) > maxExecutionRequirementsTools {
		return nil, fmt.Errorf("execution requirements: too many servers or tools (at most %d mcp_servers and %d required_tools)",
			maxExecutionRequirementsServers, maxExecutionRequirementsTools)
	}
	var completion []string
	if req.Completion != nil {
		completion = req.Completion.AnySucceeded
	}
	if len(completion) > maxExecutionRequirementsTools {
		return nil, fmt.Errorf("execution requirements: too many completion tools (at most %d)", maxExecutionRequirementsTools)
	}
	for _, field := range []struct {
		name  string
		names []string
	}{
		{"mcp_servers", req.Servers},
		{"required_tools", req.Tools},
		{"completion.any_succeeded", completion},
	} {
		for i, name := range field.names {
			if !executionRequirementName.MatchString(name) {
				return nil, fmt.Errorf("execution requirements: invalid server or tool identifier %q in %s[%d]; allowed %s",
					truncate.Clamp(name, maxQuotedRequirementIdentifier, "…"), field.name, i, ExecutionRequirementNamePattern)
			}
		}
	}
	if req.SerializationKey != nil && !declaredSerializationKey.MatchString(*req.SerializationKey) {
		return nil, fmt.Errorf("execution requirements: invalid serialization_key %q; allowed %s",
			truncate.Clamp(*req.SerializationKey, maxQuotedRequirementIdentifier, "…"), SerializationKeyPattern)
	}
	if req.Completion != nil && req.Completion.BlockedWhen != nil {
		if err := validateBlockedWhen(req.Completion); err != nil {
			return nil, err
		}
	}
	if req.Roster != nil && *req.Roster != ExecutionRequirementsRosterRequiredToolsOnly {
		// An explicitly empty value is refused like any other: an unset
		// template variable must not silently turn off an opt-in that exists
		// to constrain a run.
		return nil, fmt.Errorf("execution requirements: unknown roster %q (supported: %q)",
			truncate.Clamp(*req.Roster, maxQuotedRequirementIdentifier, "…"), ExecutionRequirementsRosterRequiredToolsOnly)
	}
	return req, nil
}

// validateBlockedWhen checks completion.blocked_when's shape: a tool that the
// same clause's any_succeeded lists (the rule qualifies how the predicate
// completed the run, so a tool that cannot complete it would never match), a
// top-level argument name, and 1–20 non-empty, bounded, printable values.
func validateBlockedWhen(c *ExecutionCompletion) error {
	bw := c.BlockedWhen
	quote := func(v string) string { return truncate.Clamp(v, maxQuotedRequirementIdentifier, "…") }
	if len(c.AnySucceeded) == 0 {
		return fmt.Errorf("execution requirements: completion.blocked_when needs completion.any_succeeded; it qualifies how that predicate completed the run")
	}
	if !executionRequirementName.MatchString(bw.Tool) {
		return fmt.Errorf("execution requirements: invalid tool identifier %q in completion.blocked_when.tool; allowed %s",
			quote(bw.Tool), ExecutionRequirementNamePattern)
	}
	listed := false
	for _, name := range c.AnySucceeded {
		listed = listed || name == bw.Tool
	}
	if !listed {
		return fmt.Errorf("execution requirements: completion.blocked_when.tool %q is not listed in completion.any_succeeded; list it there too, spelled the same way", quote(bw.Tool))
	}
	if !blockedWhenArgument.MatchString(bw.Argument) {
		return fmt.Errorf("execution requirements: invalid argument name %q in completion.blocked_when.argument; a top-level argument name, allowed %s",
			quote(bw.Argument), BlockedWhenArgumentPattern)
	}
	if bw.DetailArgument != "" && !blockedWhenArgument.MatchString(bw.DetailArgument) {
		return fmt.Errorf("execution requirements: invalid argument name %q in completion.blocked_when.detail_argument; a top-level argument name, allowed %s",
			quote(bw.DetailArgument), BlockedWhenArgumentPattern)
	}
	if len(bw.In) == 0 || len(bw.In) > maxBlockedWhenValues {
		return fmt.Errorf("execution requirements: completion.blocked_when.in must list 1 to %d values (got %d)", maxBlockedWhenValues, len(bw.In))
	}
	for i, v := range bw.In {
		if strings.TrimSpace(v) == "" || utf8.RuneCountInString(v) > maxBlockedWhenValueRunes || strings.IndexFunc(v, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
			return fmt.Errorf("execution requirements: invalid value %q in completion.blocked_when.in[%d]; each value is 1 to %d printable characters",
				quote(v), i, maxBlockedWhenValueRunes)
		}
	}
	return nil
}
