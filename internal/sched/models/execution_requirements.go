package models

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
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

// ValidateExecutionRequirements reports whether a task prompt's optional
// EXECUTION REQUIREMENTS declaration is well-formed (#1601): at most one
// marker, followed by one bounded JSON object whose mcp_servers and
// required_tools are identifier arrays within their limits. A prompt with no
// marker is valid — the declaration is optional.
//
// It is the one grammar for the declaration. The scheduled runner calls it
// first at dispatch (scheduledrun.parseExecutionRequirements), and every
// task write path calls it before saving — POST /tasks and everything that
// funnels into validateTaskCreate (edit, clone, rerun, HTTP import, batch,
// estimate) and the CLI imports — because a malformed declaration is a property
// of the prompt text: it would fail every run identically, and at dispatch it
// used to be found only by a $0 dead-letter. Lives in models so storage (the
// ADR-0070 parking breaker) and the handlers can use it without importing the
// runner.
//
// It checks shape only. Whether the named servers and tools exist in a run's
// roster, and what they mean, is decided at dispatch; Fleet interprets
// nothing about them. Keys it does not know are ignored for forward
// compatibility, exactly as at dispatch.
func ValidateExecutionRequirements(prompt string) error {
	lines := strings.Split(prompt, "\n")
	found := false
	for i, line := range lines {
		if strings.TrimSpace(line) != ExecutionRequirementsMarker {
			continue
		}
		if found || i+1 == len(lines) || len(lines[i+1]) > 16384 {
			return fmt.Errorf("execution requirements: expected one bounded JSON object after the marker")
		}
		var req *struct {
			Servers []string `json:"mcp_servers"`
			Tools   []string `json:"required_tools"`
			Network bool     `json:"network"`
		}
		if err := json.Unmarshal([]byte(lines[i+1]), &req); err != nil || req == nil {
			detail := "not a JSON object"
			if err != nil {
				detail = err.Error()
			}
			return fmt.Errorf("execution requirements: invalid JSON object after the marker (%s)", detail)
		}
		if len(req.Servers) > 100 || len(req.Tools) > 200 {
			return fmt.Errorf("execution requirements: too many servers or tools (at most 100 mcp_servers and 200 required_tools)")
		}
		for _, name := range append(append([]string{}, req.Servers...), req.Tools...) {
			if !executionRequirementName.MatchString(name) {
				return fmt.Errorf("execution requirements: invalid server or tool identifier %q; allowed %s", name, ExecutionRequirementNamePattern)
			}
		}
		found = true
	}
	return nil
}
