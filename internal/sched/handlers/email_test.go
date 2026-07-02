package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestSenderApproved(t *testing.T) {
	approved := []string{"alerts@corp.com", "ops.io"}
	cases := []struct {
		from string
		want bool
	}{
		{"alerts@corp.com", true},          // exact
		{"Alerts@Corp.com", true},          // case-insensitive
		{"Ops Team <anyone@ops.io>", true}, // domain match + display name
		{"someone@ops.io", true},           // domain match
		{"stranger@corp.com", false},       // wrong local-part, exact entry
		{"attacker@evil.com", false},       // no match
		{"", false},                        // empty
		{"not-an-email", false},            // unparseable, no domain
	}
	for _, c := range cases {
		if got := senderApproved(c.from, approved); got != c.want {
			t.Errorf("senderApproved(%q) = %v, want %v", c.from, got, c.want)
		}
	}
	// An empty allowlist approves no one.
	if senderApproved("alerts@corp.com", nil) {
		t.Error("empty allowlist should approve no one")
	}
}

func TestCheckEmailPolicy(t *testing.T) {
	base := InboundEmail{From: "alerts@corp.com", DKIM: "pass", SPF: "pass"}
	policy := &models.EmailTriggerPolicy{ApprovedSenders: []string{"corp.com"}, RequireDKIM: true}

	if _, ok := checkEmailPolicy(policy, base); !ok {
		t.Error("valid email should pass")
	}
	// Unapproved sender.
	bad := base
	bad.From = "x@evil.com"
	if _, ok := checkEmailPolicy(policy, bad); ok {
		t.Error("unapproved sender should be rejected")
	}
	// DKIM required but failing.
	dkimFail := base
	dkimFail.DKIM = "fail"
	if _, ok := checkEmailPolicy(policy, dkimFail); ok {
		t.Error("DKIM=fail should be rejected when required")
	}
	// SPF required but failing.
	spfPolicy := &models.EmailTriggerPolicy{ApprovedSenders: []string{"corp.com"}, RequireDKIM: false, RequireSPF: true}
	spfFail := base
	spfFail.SPF = "softfail"
	if _, ok := checkEmailPolicy(spfPolicy, spfFail); ok {
		t.Error("SPF!=pass should be rejected when required")
	}
	// A nil policy is the most-restrictive posture: reject all (no approved senders).
	if _, ok := checkEmailPolicy(nil, base); ok {
		t.Error("nil policy should reject (fail closed)")
	}
}

func TestAttachmentsExceedLimits(t *testing.T) {
	// nil policy / zero MaxAttachments disallows attachments entirely.
	if bad, _ := attachmentsExceedLimits(nil, []InboundAttachment{{Filename: "x", Size: 1}}); !bad {
		t.Error("nil policy should reject any attachment")
	}
	if bad, _ := attachmentsExceedLimits(nil, nil); bad {
		t.Error("nil policy with no attachments should pass")
	}
	pol := &models.EmailTriggerPolicy{MaxAttachments: 2, MaxAttachmentBytes: 100}
	if bad, _ := attachmentsExceedLimits(pol, []InboundAttachment{{Size: 50}, {Size: 90}}); bad {
		t.Error("within limits should pass")
	}
	if bad, _ := attachmentsExceedLimits(pol, []InboundAttachment{{}, {}, {}}); !bad {
		t.Error("too many attachments should be rejected")
	}
	if bad, _ := attachmentsExceedLimits(pol, []InboundAttachment{{Size: 200}}); !bad {
		t.Error("oversized attachment should be rejected")
	}
}

func TestEmailAddress(t *testing.T) {
	cases := map[string]string{
		"alerts@corp.com":        "alerts@corp.com",
		"Ops <ops@corp.com>":     "ops@corp.com",
		"  MixedCase@Corp.COM  ": "mixedcase@corp.com",
		"garbage":                "garbage",
	}
	for in, want := range cases {
		if got := emailAddress(in); got != want {
			t.Errorf("emailAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEmailIdempotencyKey(t *testing.T) {
	withID := InboundEmail{MessageID: "  <abc@mail>  ", From: "a@b.com", Subject: "s", Text: "t"}
	if got := emailIdempotencyKey(withID); got != "<abc@mail>" {
		t.Errorf("message-id key = %q, want <abc@mail>", got)
	}
	// No Message-ID → deterministic content hash; identical content → identical key.
	noID := InboundEmail{From: "a@b.com", Subject: "s", Text: "t"}
	k1 := emailIdempotencyKey(noID)
	k2 := emailIdempotencyKey(noID)
	if k1 != k2 || k1 == "" {
		t.Errorf("content-hash key should be stable & non-empty: %q vs %q", k1, k2)
	}
	// Different content → different key.
	other := noID
	other.Text = "different"
	if emailIdempotencyKey(other) == k1 {
		t.Error("different content should yield a different dedup key")
	}
}

func TestRenderEmailPrompt(t *testing.T) {
	email := InboundEmail{From: "a@b.com", Subject: "Deploy?", Text: "Please deploy v2."}

	// Empty template → a useful default prompt carrying the email content.
	def, err := renderEmailPrompt("", email)
	if err != nil {
		t.Fatalf("default render: %v", err)
	}
	for _, want := range []string{"a@b.com", "Deploy?", "Please deploy v2."} {
		if !strings.Contains(def, want) {
			t.Errorf("default prompt missing %q:\n%s", want, def)
		}
	}

	// A configured template renders against the email fields.
	got, err := renderEmailPrompt("Subject was: {{.Subject}} / body: {{.Text}}", email)
	if err != nil {
		t.Fatalf("template render: %v", err)
	}
	if got != "Subject was: Deploy? / body: Please deploy v2." {
		t.Errorf("unexpected render: %q", got)
	}

	// A broken template surfaces an error (handler maps it to 400).
	if _, err := renderEmailPrompt("{{ .Nope", email); err == nil {
		t.Error("malformed template should error")
	}
}
