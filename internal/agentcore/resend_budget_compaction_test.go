// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import (
	"context"
	"strings"
	"testing"
)

// payloadObserver keeps every event's payload so a test can assert WHY a
// compaction fired, not only that one did.
type payloadObserver struct {
	events   []string
	payloads []map[string]any
}

func (o *payloadObserver) Observe(eventType string, payload map[string]any) {
	o.events = append(o.events, eventType)
	o.payloads = append(o.payloads, payload)
}

func (o *payloadObserver) payloadOf(eventType string) map[string]any {
	for i, e := range o.events {
		if e == eventType {
			return o.payloads[i]
		}
	}
	return nil
}

// A 1M-window model never reaches the window-pressure threshold in a 100-turn
// run, yet every turn resends and pays for the whole transcript (#1534). A
// SCHEDULED run therefore compacts once the resent prompt exceeds the resend
// budget — without the SCHEDULED_AUTO_COMPACT opt-in, which guards the
// window-pressure path — and says why in the event and the session log.
func TestRun_ResendBudget_ScheduledCompactsWithoutOptIn(t *testing.T) {
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "")
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "1000")
	model := newStopModel("ctx1534-sched-budget")
	obs := &payloadObserver{}
	session := NewLogSession()
	// ~6K estimated tokens: far below any window threshold, above the budget.
	_, err := Run(context.Background(), ModeScheduled, RunConfig{EnvPrefix: CanonicalEnvPrefix, RequireCompactionOptIn: true}, Deps{
		Input:      historyInput{system: "s", msgs: fillerMessages(6, 4_000), label: "sched"},
		Observer:   obs,
		Policy:     finishableScheduledPolicy(session),
		Model:      model,
		LogSession: session,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	payload := obs.payloadOf(evtContextCompacted)
	if payload == nil {
		t.Fatalf("expected %s from the resend budget, got %v", evtContextCompacted, obs.events)
	}
	if payload[evtFieldTrigger] != "resend_budget" || payload[evtFieldResendBudget] != 1000 {
		t.Errorf("compaction payload must name the trigger and budget, got %v", payload)
	}
	var crumb bool
	for _, m := range session.Messages {
		if strings.Contains(m.Content, "[context_compacted] trigger=resend_budget") && strings.Contains(m.Content, "budget=1000") {
			crumb = true
		}
	}
	if !crumb {
		t.Error("expected a [context_compacted] trigger=resend_budget breadcrumb in the session log")
	}
}

// The budget is a scheduled-run cost control: an interactive engine (no
// requireCompactionOptIn) keeps the window-pressure rule only — a chat's
// history is the user's to keep.
func TestCheckContextPressure_ResendBudgetIsScheduledOnly(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "1000")
	slug := "ctx1534-interactive"
	recordContextMax(slug, testContextWindow)
	model := &namedMockModel{name: slug}
	msgs := fillerMessages(6, 4_000)

	interactive := newMockEngine(t, model)
	obs := &payloadObserver{}
	interactive.logSession.LastStepPromptTokens = 6_000 // above the budget, 6% of the window
	if res := interactive.checkContextPressure(context.Background(), msgs, model, newStreamSink(obs), false); len(obs.events) != 0 || len(res.messages) != len(msgs) {
		t.Fatalf("an interactive engine must ignore the resend budget, events=%v", obs.events)
	}

	scheduled := newMockEngine(t, model)
	scheduled.requireCompactionOptIn = true
	obs2 := &payloadObserver{}
	scheduled.logSession.LastStepPromptTokens = 6_000
	res := scheduled.checkContextPressure(context.Background(), msgs, model, newStreamSink(obs2), false)
	if obs2.payloadOf(evtContextCompacted) == nil || len(res.messages) >= len(msgs) {
		t.Fatalf("a scheduled engine over the resend budget must compact, events=%v len=%d", obs2.events, len(res.messages))
	}

	// 0 disables the trigger even for a scheduled engine, and the window path
	// below its threshold stays silent.
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "0")
	off := newMockEngine(t, model)
	off.requireCompactionOptIn = true
	obs3 := &payloadObserver{}
	off.logSession.LastStepPromptTokens = 6_000
	if res := off.checkContextPressure(context.Background(), msgs, model, newStreamSink(obs3), false); len(obs3.events) != 0 || len(res.messages) != len(msgs) {
		t.Fatalf("budget 0 must disable the trigger, events=%v", obs3.events)
	}
}

// The window-pressure compaction now names its trigger too, so a log reader
// can tell the two apart.
func TestCheckContextPressure_WindowCompactionNamesTrigger(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "0")
	slug := "ctx1534-window"
	recordContextMax(slug, testContextWindow)
	model := &namedMockModel{name: slug}
	e := newMockEngine(t, model)
	obs := &payloadObserver{}
	e.logSession.LastStepPromptTokens = 95_000
	e.checkContextPressure(context.Background(), fillerMessages(6, 20), model, newStreamSink(obs), false)
	if p := obs.payloadOf(evtContextCompacted); p == nil || p[evtFieldTrigger] != "window" {
		t.Fatalf("window compaction must carry trigger=window, got %v", p)
	}
}

func TestContextResendBudgetTokensKnob(t *testing.T) {
	p := EnvPrefix(CanonicalEnvPrefix)
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "")
	if got := contextResendBudgetTokens(p); got != defaultContextResendBudgetTokens {
		t.Fatalf("default = %d, want %d", got, defaultContextResendBudgetTokens)
	}
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "0")
	if got := contextResendBudgetTokens(p); got != 0 {
		t.Fatalf("0 must disable, got %d", got)
	}
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "-5")
	if got := contextResendBudgetTokens(p); got != defaultContextResendBudgetTokens {
		t.Fatalf("negative must fall back to the default, got %d", got)
	}
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "lots")
	if got := contextResendBudgetTokens(p); got != defaultContextResendBudgetTokens {
		t.Fatalf("unparseable must fall back to the default, got %d", got)
	}
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "")
	t.Setenv("CHAT_CONTEXT_RESEND_BUDGET_TOKENS", "12345")
	if got := contextResendBudgetTokens(p); got != 12345 {
		t.Fatalf("legacy alias must be honoured, got %d", got)
	}
}
