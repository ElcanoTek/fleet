// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"regexp"
	"strings"
	"testing"
)

// logicalLines joins backslash-continued lines of a shell script and returns
// each logical line with the 1-based number of its first physical line.
func logicalLines(src string) (lines []string, nums []int) {
	var cur strings.Builder
	start := 0
	for i, l := range strings.Split(src, "\n") {
		if cur.Len() == 0 {
			start = i + 1
		}
		if strings.HasSuffix(l, "\\") {
			cur.WriteString(strings.TrimSuffix(l, "\\"))
			cur.WriteByte(' ')
			continue
		}
		cur.WriteString(l)
		lines = append(lines, cur.String())
		nums = append(nums, start)
		cur.Reset()
	}
	return lines, nums
}

// TestUpdateNeverGatesReadinessWhileItsDrainMarkerCanExist pins the order of
// the update drain marker against the restart in update.sh (v2026.11.07.21
// rollout: every non --force `fleet update` rolled back). fleet_wait_for_idle
// leaves <env>.update.drain on disk so the OLD process admits no new work; the
// NEW process reads the same file at start and answers /readyz 503
// "draining". So once the drain is held, update.sh must not
//
//   - restart or start the service itself (the new process would start
//     draining), or
//   - run a readiness-polling `fleet release install` that is not handed the
//     drain (FLEET_UPDATE_DRAIN_FILE + FLEET_DRAIN_OWNER_TOKEN), which it
//     clears after stopping the old process and before starting the new one.
//
// The scan is linear over the script: the drain is held from a
// fleet_wait_for_idle call until the next fleet_clear_drain.
func TestUpdateNeverGatesReadinessWhileItsDrainMarkerCanExist(t *testing.T) {
	src := readFile(t, repoRoot(t), "scripts/update.sh")
	lines, nums := logicalLines(src)
	restartSvc := regexp.MustCompile(`systemctl\s+(restart|start|try-restart|reload-or-restart)\s+"\$SERVICE_NAME"`)
	notCode := regexp.MustCompile(`^(#|info |warn |say |die |ok |printf |echo |trap )`)
	oneLineIf := regexp.MustCompile(`[;\s]fi$`)
	// if/elif/else/fi are tracked so a clear in one branch does not count for
	// a sibling branch: each branch starts from the state at its `if`, and
	// after `fi` the drain is held if any branch left it held.
	type frame struct{ entry, anyHeld bool }
	var stack []frame
	held := false
	gated := 0
	for i, raw := range lines {
		l := strings.TrimSpace(raw)
		if l == "" || notCode.MatchString(l) {
			continue
		}
		switch {
		case strings.HasPrefix(l, "if ") && !oneLineIf.MatchString(l):
			stack = append(stack, frame{entry: held})
		case (l == "else" || strings.HasPrefix(l, "else ") || strings.HasPrefix(l, "elif ")) && len(stack) > 0:
			top := &stack[len(stack)-1]
			top.anyHeld = top.anyHeld || held
			held = top.entry
			continue
		case (l == "fi" || strings.HasPrefix(l, "fi ")) && len(stack) > 0:
			top := stack[len(stack)-1]
			held = top.anyHeld || held
			stack = stack[:len(stack)-1]
			continue
		}
		if strings.Contains(l, `fleet_wait_for_idle "`) {
			held = true
			continue
		}
		if strings.HasPrefix(l, "fleet_clear_drain ") {
			held = false
			continue
		}
		if !held {
			continue
		}
		if restartSvc.MatchString(l) {
			t.Errorf("update.sh:%d restarts %s while the update drain marker is still on disk — the new process starts draining (stop it, fleet_clear_drain, then start):\n  %s", nums[i], "$SERVICE_NAME", l)
		}
		if strings.Contains(l, "release install") && !strings.Contains(l, "--no-restart") {
			gated++
			for _, want := range []string{"FLEET_UPDATE_DRAIN_FILE=", "FLEET_DRAIN_OWNER_TOKEN="} {
				if !strings.Contains(l, want) {
					t.Errorf("update.sh:%d runs a readiness-gated `fleet release install` while the drain marker is held without handing it the drain (%s missing): /readyz answers 503 draining and the install rolls back:\n  %s", nums[i], want, l)
				}
			}
		}
	}
	if gated == 0 {
		t.Fatal("found no readiness-gated `release install` inside the drain fence in update.sh — the scan no longer matches the script; update this test")
	}
}
