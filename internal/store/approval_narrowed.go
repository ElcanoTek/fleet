package store

import (
	"encoding/json"
	"fmt"
	"strings"
)

// NarrowingNote is fleet's labelled line for a narrowed approval
// (docs/APPROVAL-NARROWING.md): what the person approved and which staged
// values they left out, so the model neither reports the left-out ones as
// done nor runs them unasked. The chat server appends it after the call's
// result; RecoverStrandedApprovals appends it to an interrupted call's
// breadcrumb, so the context survives a restart too.
func NarrowingNote(arg string, staged, approved []string) string {
	picked := make(map[string]bool, len(approved))
	for _, v := range approved {
		picked[v] = true
	}
	var left []string
	more := 0
	for _, v := range staged {
		if picked[v] {
			continue
		}
		if len(left) == narrowingNoteMaxNamed {
			more++
			continue
		}
		left = append(left, fmt.Sprintf("%q", v))
	}
	named := strings.Join(left, ", ")
	if more > 0 {
		named += fmt.Sprintf(" and %d more", more)
	}
	return fmt.Sprintf("[fleet] The person approved %d of the %d staged %s and left out %s. Only the approved ones were sent to the tool. Do not run the left-out ones unless the person asks. (Written by fleet, not the tool.)",
		len(approved), len(staged), arg, named)
}

// narrowingNoteMaxNamed bounds how many left-out values the note names; the
// rest are counted. The values are model-authored, so the note stays small
// whatever the staged list held.
const narrowingNoteMaxNamed = 20

// narrowingNoteFor is NarrowingNote for a stored narrowed_json record, or ""
// when there is none or it does not read.
func narrowingNoteFor(narrowedJSON string) string {
	if narrowedJSON == "" {
		return ""
	}
	var n struct {
		Arg      string   `json:"arg"`
		Staged   []string `json:"staged"`
		Approved []string `json:"approved"`
	}
	if json.Unmarshal([]byte(narrowedJSON), &n) != nil || n.Arg == "" || len(n.Approved) == 0 {
		return ""
	}
	return NarrowingNote(n.Arg, n.Staged, n.Approved)
}
