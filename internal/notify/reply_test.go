package notify

import (
	"context"
	"strings"
	"testing"
)

func TestReplySubject(t *testing.T) {
	cases := map[string]string{
		"Weekly report": "Re: Weekly report",
		"Re: existing":  "Re: existing",
		"RE: shouty":    "RE: shouty",
		"  ":            "Re:",
		"":              "Re:",
	}
	for in, want := range cases {
		if got := replySubject(in); got != want {
			t.Errorf("replySubject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderReplyEmail_HeadersAndThreading(t *testing.T) {
	msg := string(renderReplyEmail("bot@fleet.example", "sender@corp.com", "Status?", "All green.", "<abc@mail>"))

	for _, want := range []string{
		"From: bot@fleet.example\r\n",
		"To: sender@corp.com\r\n",
		"Subject: Re: Status?\r\n",
		"In-Reply-To: <abc@mail>\r\n",
		"References: <abc@mail>\r\n",
		"Content-Type: text/plain; charset=\"utf-8\"\r\n",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("reply missing header %q\n---\n%s", want, msg)
		}
	}
	// The body sits after the header/body separator.
	if !strings.HasSuffix(msg, "All green.\r\n") {
		t.Errorf("reply body not at end:\n%s", msg)
	}
}

func TestRenderReplyEmail_NoInReplyToWhenBlank(t *testing.T) {
	msg := string(renderReplyEmail("bot@fleet.example", "sender@corp.com", "Hi", "body", "  "))
	if strings.Contains(msg, "In-Reply-To:") || strings.Contains(msg, "References:") {
		t.Errorf("blank message-id should omit threading headers:\n%s", msg)
	}
}

// TestRenderReplyEmail_HeaderInjection: the untrusted inbound sender/subject/
// message-id must not be able to smuggle extra headers via CR/LF.
func TestRenderReplyEmail_HeaderInjection(t *testing.T) {
	msg := string(renderReplyEmail(
		"bot@fleet.example",
		"evil@corp.com\r\nBcc: victim@corp.com",
		"subject\r\nX-Injected: 1",
		"body",
		"<id>\r\nX-Evil: 2",
	))
	// The security property is that CR/LF is stripped so the injected text can
	// never START A NEW HEADER LINE — the "\r\n<Header>:" form must be absent
	// (the text may survive harmlessly inline on the sanitized line).
	for _, bad := range []string{"\r\nBcc:", "\r\nX-Injected:", "\r\nX-Evil:"} {
		if strings.Contains(msg, bad) {
			t.Errorf("header injection not neutralized: found new header line %q in\n%s", bad, msg)
		}
	}
}

// TestReplyToEmailEvent_NoOpWhenUnconfigured: with no SMTP configured, reply-back
// is an inert no-op (returns nil) so it can be wired unconditionally.
func TestReplyToEmailEvent_NoOpWhenUnconfigured(t *testing.T) {
	n := New(Config{}) // nothing configured
	if n.ReplyEnabled() {
		t.Fatal("ReplyEnabled should be false with no SMTP")
	}
	if err := n.ReplyToEmailEvent(context.Background(), "a@b.com", "s", "body", "<id>"); err != nil {
		t.Errorf("expected no-op nil, got %v", err)
	}
}

func TestReplyEnabled(t *testing.T) {
	// Needs host AND from — but NOT a recipient list (a reply targets the sender).
	if !New(Config{SMTPHost: "smtp.x", SMTPFrom: "bot@x"}).ReplyEnabled() {
		t.Error("host+from should enable reply")
	}
	if New(Config{SMTPHost: "smtp.x"}).ReplyEnabled() {
		t.Error("host without from should not enable reply")
	}
	if New(Config{SMTPFrom: "bot@x"}).ReplyEnabled() {
		t.Error("from without host should not enable reply")
	}
}
