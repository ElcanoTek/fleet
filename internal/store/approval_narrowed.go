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
	// Name the smaller side exactly by its 1-based positions in the staged
	// list, which the call's recorded arguments hold. Positions only, never
	// the values: the values are model-authored, so a line built from them
	// could carry what output governance must scrub or block, and this line
	// must reach the model whole (it is also replayed at restart recovery).
	var approvedPos, leftPos []int
	for i, v := range staged {
		if picked[v] {
			approvedPos = append(approvedPos, i+1)
		} else {
			leftPos = append(leftPos, i+1)
		}
	}
	side, pos, other := "were approved", approvedPos, fmt.Sprintf("the other %d were left out", len(leftPos))
	if len(leftPos) < len(approvedPos) {
		side, pos, other = "were left out", leftPos, fmt.Sprintf("the other %d were approved", len(approvedPos))
	}
	// The decision only: whether the approved ones ran is the call's own
	// outcome (it may have failed before it was sent, or a restart may have
	// left it unknown), so the line never claims it.
	return fmt.Sprintf("[fleet] The person narrowed this call: of the %d staged %s in its arguments, only positions %s %s; %s. The call's %s held only the approved ones; the result says whether they ran. Do not run the left-out ones unless the person asks. (Written by fleet, not the tool.)",
		len(staged), arg, PositionRanges(pos), side, other, arg)
}

// NarrowingNoteCompact is NarrowingNote without the position list, for a
// result whose output limit leaves no room for it: the counts and the rule,
// and where the positions are (the approval card). Positions only ever, never
// the values, as NarrowingNote.
func NarrowingNoteCompact(arg string, staged, approved []string) string {
	return fmt.Sprintf("[fleet] The person narrowed this call: of the %d staged %s in its arguments, %d were approved and %d were left out (the approval card lists which; too many to name within this result's size limit). The call's %s held only the approved ones; the result says whether they ran. Do not run the left-out ones unless the person asks. (Written by fleet, not the tool.)",
		len(staged), arg, len(approved), len(staged)-len(approved), arg)
}

// PositionRanges writes ascending 1-based positions compactly: "1-3, 5, 8-10".
// The narrowing note and the chat server's audit line share it.
func PositionRanges(pos []int) string {
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
