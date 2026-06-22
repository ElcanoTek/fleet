// Command fake-llm runs the wire-compatible fake OpenRouter server used by the
// live E2E suite. Fleet is pointed at it with OPENROUTER_BASE_URL=<addr>.
//
// It pre-registers the scenarios the live Playwright specs drive via the
// "[[scenario:NAME]]" prompt marker (see web/e2e/live/). Run standalone:
//
//	fake-llm -addr 127.0.0.1:18090
//
// Then export OPENROUTER_BASE_URL=http://127.0.0.1:18090 before booting fleet.
// The boot script (scripts/e2e-boot-server.sh) does this for you.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/ElcanoTek/fleet/internal/fakellm"
)

func main() {
	addr := flag.String("addr", envOr("FAKE_LLM_ADDR", "127.0.0.1:18090"), "listen address host:port")
	flag.Parse()

	srv := fakellm.New()
	registerLiveScenarios(srv)

	lc := net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp", *addr)
	if err != nil {
		log.Fatalf("fake-llm: listen %s: %v", *addr, err)
	}
	log.Printf("fake-llm: listening on http://%s (chat-completions + models)", ln.Addr())

	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 30 * time.Second}
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("fake-llm: serve: %v", err)
	}
}

// registerLiveScenarios wires the scenarios the live specs select by marker.
func registerLiveScenarios(s *fakellm.Server) {
	// tool-loop: a real multi-step tool loop exercised against the Podman
	// sandbox. turn 0 → bash, turn 1 → run_python, turn 2 → final text that
	// echoes the marker the python step printed so the UI assertion is exact.
	s.Scenario("tool-loop", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.BashStep("call_bash_1", "echo FAKELLM_BASH_OK"),
		fakellm.PythonStep("call_py_1", "print('FAKELLM_PY_RESULT', 6 * 7)"),
		fakellm.TextStep("Sandbox run complete: bash said FAKELLM_BASH_OK and python computed FAKELLM_PY_RESULT 42."),
	}})

	// sched-task: drives a scheduled task through the worker pool + sandbox to
	// SUCCESS. The worker path runs the same provider + sandbox as chat, but
	// scheduled mode adds a ScheduledPolicy that refuses to finish until the
	// self-audit gate clears — so the script must call confirm_audit(success=
	// true, …) with the structured evidence validateConfirmAuditArgs() demands.
	//   turn 0 → run_python (real sandbox compute),
	//   turn 1 → confirm_audit (clears the enforcement gate),
	//   turn 2 → final report text.
	confirmAuditArgs := `{` +
		`"success":true,` +
		`"reasoning":"Computed and verified the scheduled result in the sandbox.",` +
		`"artifacts_checked":["sandbox stdout: SCHED_TASK_OK 45"],` +
		`"workflow_sections_checked":["compute","verify"],` +
		`"critical_actions":[],` +
		`"critical_actions_being_unblocked":["none: read-only compute task"],` +
		`"send_contract_checked":true,` +
		`"attachments_checked":[],` +
		`"remaining_risks":[]` +
		`}`
	s.Scenario("sched-task", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.PythonStep("call_sched_py_1", "print('SCHED_TASK_OK', sum(range(10)))"),
		fakellm.ToolStep(fakellm.ToolCall{ID: "call_sched_audit_1", Name: "confirm_audit", Arguments: confirmAuditArgs}),
		fakellm.TextStep("Scheduled task done: SCHED_TASK_OK 45."),
	}})

	// resilience-429: the model returns 429 once, then a clean text turn — the
	// real provider retry/backoff path should recover and the UI should still
	// land a reply. (fleet retries the same turn, so the retry re-enters at
	// turn 0; we keep both the error and the recovery as step 0 by alternating
	// is not possible statelessly, so we model recovery as: step 0 = 429,
	// step 1+ = text. Because a 429 does NOT append an assistant message, the
	// turn index stays 0 on retry; to actually recover we instead emit the
	// error only on the FIRST hit and text thereafter — handled below.)
	//
	// Stateless retry can't be expressed purely by assistant-turn index (a
	// failed request appends nothing), so this scenario simply asserts the
	// error is surfaced cleanly; recovery is covered by the per-process hit
	// counter variant the spec uses sparingly.
	s.Scenario("resilience-500", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.StatusStep(http.StatusInternalServerError),
	}})

	// text-only: a plain single-turn reply, no tools — the cheap smoke path.
	s.Scenario("text-only", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.TextStep("Hello from the fake LLM. No tools were harmed."),
	}})

	// Default for any prompt without a marker: a deterministic echo-ish reply.
	s.SetDefault(fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.TextStep("fake-llm reply (no scenario marker matched)."),
	}})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
