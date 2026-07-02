package models

import (
	"encoding/json"
	"testing"
)

func TestTriggerKind_IsValid(t *testing.T) {
	for _, k := range []TriggerKind{TriggerKindWebhook, TriggerKindEmail} {
		if !k.IsValid() {
			t.Errorf("%q should be valid", k)
		}
	}
	for _, k := range []TriggerKind{"", "sms", "cron", "EMAIL"} {
		if k.IsValid() {
			t.Errorf("%q should be invalid", k)
		}
	}
}

func TestTaskTrigger_KindOrWebhook(t *testing.T) {
	// A blank kind (a legacy row created before #511) defaults to webhook.
	if got := (&TaskTrigger{}).KindOrWebhook(); got != TriggerKindWebhook {
		t.Errorf("blank kind: got %q, want webhook", got)
	}
	if got := (&TaskTrigger{Kind: TriggerKindEmail}).KindOrWebhook(); got != TriggerKindEmail {
		t.Errorf("email kind: got %q, want email", got)
	}
}

func TestEmailTriggerPolicy_JSONRoundTrip(t *testing.T) {
	in := &EmailTriggerPolicy{
		ApprovedSenders:    []string{"alerts@corp.com", "ops.com"},
		RequireDKIM:        true,
		RequireSPF:         true,
		MaxAttachments:     3,
		MaxAttachmentBytes: 1024,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out EmailTriggerPolicy
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.ApprovedSenders) != 2 || out.ApprovedSenders[0] != "alerts@corp.com" {
		t.Errorf("approved senders lost: %+v", out.ApprovedSenders)
	}
	if !out.RequireDKIM || !out.RequireSPF || out.MaxAttachments != 3 || out.MaxAttachmentBytes != 1024 {
		t.Errorf("policy fields lost: %+v", out)
	}
}

func TestNewTask_AllowEventTriggers(t *testing.T) {
	// Off by default (the secure default), on when requested.
	if NewTask(TaskCreate{Prompt: "p"}).AllowEventTriggers {
		t.Error("allow_event_triggers should default to false")
	}
	if !NewTask(TaskCreate{Prompt: "p", AllowEventTriggers: true}).AllowEventTriggers {
		t.Error("allow_event_triggers should carry through NewTask when set")
	}
}

func TestTaskToCreate_PreservesAllowEventTriggers(t *testing.T) {
	// A re-run/clone must keep the security posture (#277 governance-recipe rule).
	task := NewTask(TaskCreate{Prompt: "p", AllowEventTriggers: true})
	if !TaskToCreate(task).AllowEventTriggers {
		t.Error("TaskToCreate dropped allow_event_triggers")
	}
}
