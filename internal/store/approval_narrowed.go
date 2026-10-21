package store

import (
	"encoding/json"
	"fmt"
	"strconv"
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
	// Name the smaller side exactly, by its 1-based position in the staged
	// list (the call's recorded arguments hold that list), and by value too
	// when it is short. Positions keep the note small and exact whatever the
	// values are: at most 250 positions.
	var approvedPos, leftPos []int
	var approvedVals, leftVals []string
	for i, v := range staged {
		if picked[v] {
			approvedPos = append(approvedPos, i+1)
			approvedVals = append(approvedVals, fmt.Sprintf("%q", v))
		} else {
			leftPos = append(leftPos, i+1)
			leftVals = append(leftVals, fmt.Sprintf("%q", v))
		}
	}
	side, pos, vals, other := "were approved", approvedPos, approvedVals, fmt.Sprintf("the other %d were left out", len(leftPos))
	if len(leftPos) < len(approvedPos) {
		side, pos, vals, other = "were left out", leftPos, leftVals, fmt.Sprintf("the other %d were approved", len(approvedPos))
	}
	named := ""
	if len(vals) <= narrowingNoteMaxNamed {
		named = " (" + strings.Join(vals, ", ") + ")"
	}
	// The note records the person's decision only. Whether the approved ones
	// ran is the call's own outcome (it may have failed before it was sent,
	// or a restart may have left it unknown), so the note never claims it.
	return fmt.Sprintf("[fleet] The person narrowed this call: of the %d staged %s in its arguments, only positions %s%s %s; %s. Only the approved ones could be sent to the tool; the result says whether they ran. Do not run the left-out ones unless the person asks. (Written by fleet, not the tool.)",
		len(staged), arg, positionRanges(pos), named, side, other)
}

// positionRanges writes ascending 1-based positions compactly: "1-3, 5, 8-10".
func positionRanges(pos []int) string {
	var parts []string
	for i := 0; i < len(pos); {
		j := i
		for j+1 < len(pos) && pos[j+1] == pos[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", pos[i], pos[j]))
		} else {
			parts = append(parts, strconv.Itoa(pos[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// narrowingNoteMaxNamed bounds how many values the note also names by value;
// above it the positions alone identify them. The values are model-authored,
// so the note stays small whatever the staged list held.
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
