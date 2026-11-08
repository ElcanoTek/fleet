package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestRunStartsTheSchedulerAfterTheLastFallibleStep: run() wires the chat
// server in as the scheduler's chat schedule deliverer and starts the
// scheduler only after every boot step that can fail, so a failed boot never
// leaves a chat turn (or a promoted task) running while fleet reports the
// failure. The check is on run()'s source: after the scheduler start, run()
// has no early error return before it serves.
func TestRunStartsTheSchedulerAfterTheLastFallibleStep(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "\nfunc run(")
	if start < 0 {
		t.Fatal("run() not found")
	}
	end := strings.Index(text[start+1:], "\n}\n")
	body := text[start : start+1+end]

	deliverer := strings.Index(body, "sch.SetChatScheduleDeliverer(chatSrv)")
	started := strings.Index(body, "sch.Start()")
	if deliverer < 0 || started < 0 || started < deliverer {
		t.Fatalf("run() must attach the deliverer and then start the scheduler (deliverer at %d, start at %d)", deliverer, started)
	}
	if strings.Count(body, "sch.Start()") != 1 || strings.Count(body, "sch.SetChatScheduleDeliverer(") != 1 {
		t.Fatal("run() must start the scheduler and attach the deliverer exactly once")
	}
	// No boot error return after the start: anything that can fail runs first.
	errReturn := regexp.MustCompile(`(?m)^\treturn (err|fmt\.Errorf|errors\.New)`)
	for _, loc := range errReturn.FindAllStringIndex(body, -1) {
		if loc[0] > started {
			line := strings.Count(body[:loc[0]], "\n")
			t.Fatalf("run() has a boot error return after the scheduler starts (run() line %d): a failed boot could leave chat turns running", line)
		}
	}
	for _, fallible := range []string{"wireA2A(", "buildWebServer("} {
		if i := strings.Index(body, fallible); i < 0 || i > started {
			t.Fatalf("%s must run before the scheduler starts", fallible)
		}
	}
}
