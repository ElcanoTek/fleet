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

	// tck-complete / tck-ask: the two SUT scenarios the official A2A TCK
	// drives (#1279). The TCK carries the scenario in the request's messageId
	// (tck-complete-task-* / tck-input-required-*), which fleet's A2A adapter
	// rightly never puts into a prompt — so the conformance harness's shim
	// (scripts/a2a-tck-shim) appends the matching [[scenario:…]] marker to the
	// message TEXT, and these scripts do the rest. A resumed run's answer text
	// lands in the system prompt, which selectScenario reads FIRST — so a
	// follow-up's marker naturally overrides the original task prompt's, which
	// is exactly the multi-turn semantics the TCK expects (ask → answer with a
	// complete marker → the resumed run completes; answer with another ask
	// marker → it asks again).
	tckAuditArgs := `{` +
		`"success":true,` +
		`"reasoning":"TCK conformance scenario: the delegated request is acknowledged and complete.",` +
		`"artifacts_checked":["assistant reply text"],` +
		`"workflow_sections_checked":["acknowledge","reply"],` +
		`"critical_actions":[],` +
		`"critical_actions_being_unblocked":["none: reply-only conformance task"],` +
		`"send_contract_checked":true,` +
		`"attachments_checked":[],` +
		`"remaining_risks":[]` +
		`}`
	s.Scenario("tck-complete", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.ToolStep(fakellm.ToolCall{ID: "call_tck_audit", Name: "confirm_audit", Arguments: tckAuditArgs}),
		fakellm.TextStep("TCK task complete."),
	}})
	s.Scenario("tck-ask", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.ToolStep(fakellm.ToolCall{ID: "call_tck_ask", Name: "ask",
			Arguments: `{"question":"TCK scenario: reply to continue this task."}`}),
	}})
	// tck-artifact-text / tck-artifact-file: the TCK's data-model artifact
	// scenarios. The text variant's EXACT final text becomes the task Result
	// and therefore the "result" text artifact the test inspects; the file
	// variants really produce output.txt in the sandbox and publish it — the
	// empty final text keeps the file as artifacts[0], which is the slot the
	// test asserts on.
	s.Scenario("tck-artifact-text", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.ToolStep(fakellm.ToolCall{ID: "call_tck_at_audit", Name: "confirm_audit", Arguments: tckAuditArgs}),
		fakellm.TextStep("Generated text content"),
	}})
	s.Scenario("tck-artifact-file", fakellm.Scenario{Steps: []fakellm.Step{
		// One turn for create+publish (tools run in order within a turn):
		// the TCK inspects the BLOCKING send's response, so the run must
		// finish inside the unary wait — every saved round trip counts.
		fakellm.ToolStep(
			fakellm.ToolCall{ID: "call_tck_af_bash", Name: "bash",
				Arguments: `{"command":"printf 'Generated file content' > output.txt"}`},
			fakellm.ToolCall{ID: "call_tck_af_pub", Name: "publish_artifact",
				Arguments: `{"path":"output.txt","description":"TCK artifact scenario file"}`},
			fakellm.ToolCall{ID: "call_tck_af_audit", Name: "confirm_audit", Arguments: tckAuditArgs},
		),
		fakellm.TextStep(""),
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
