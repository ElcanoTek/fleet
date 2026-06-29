package observability

import (
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"
)

// fakeRedactor scrubs any occurrence of "secret-token" (a stand-in high-entropy
// value) so the BeforeSend test asserts the injected Redactor is applied to
// every breadcrumb message + data field and the "extra" context bucket.
func fakeRedactor(s string) string {
	return strings.ReplaceAll(s, "secret-token", "[REDACTED]")
}

func TestRedactEvent_NilReturnsNil(t *testing.T) {
	if got := RedactEvent(nil, nil); got != nil {
		t.Fatalf("RedactEvent(nil) = %v, want nil", got)
	}
}

func TestRedactEvent_ScrubsBreadcrumbMessageAndData(t *testing.T) {
	prev := redactor
	redactor = fakeRedactor
	t.Cleanup(func() { redactor = prev })

	event := &sentry.Event{
		Message: "boom: secret-token",
		Exception: []sentry.Exception{
			{Type: "*fmt.wrapError", Value: "dial postgres: password=secret-token failed"},
		},
		Breadcrumbs: []*sentry.Breadcrumb{
			{
				Message: "mcp call with api_key=secret-token",
				Data: map[string]any{
					"args":   "token=secret-token",
					"count":  3,
					"nested": "bearer secret-token",
				},
			},
		},
		Contexts: map[string]sentry.Context{
			"extra": {
				"note":  "leaked: secret-token",
				"count": 7,
			},
		},
		Request: &sentry.Request{
			QueryString: "token=secret-token&page=2",
			Cookies:     "session=secret-token",
			Headers: map[string]string{
				"authorization": "Bearer secret-token",
				"x-fleet-token": "secret-token",
				"x-api-key":     "secret-token",
				"cookie":        "session=secret-token",
				"x-other":       "untouched",
			},
		},
	}

	got := RedactEvent(event, nil)
	if got == nil {
		t.Fatal("RedactEvent returned nil event")
	}

	if strings.Contains(got.Message, "secret-token") {
		t.Errorf("top-level message not redacted: %q", got.Message)
	}
	if strings.Contains(got.Exception[0].Value, "secret-token") {
		t.Errorf("exception value not redacted: %q", got.Exception[0].Value)
	}
	if strings.Contains(got.Request.QueryString, "secret-token") {
		t.Errorf("request query string not redacted: %q", got.Request.QueryString)
	}
	if strings.Contains(got.Request.Cookies, "secret-token") {
		t.Errorf("request cookies not redacted: %q", got.Request.Cookies)
	}

	bc := got.Breadcrumbs[0]
	if strings.Contains(bc.Message, "secret-token") {
		t.Errorf("breadcrumb message not redacted: %q", bc.Message)
	}
	if v, ok := bc.Data["args"].(string); !ok || strings.Contains(v, "secret-token") {
		t.Errorf("breadcrumb data[args] not redacted: %v", bc.Data["args"])
	}
	if v, ok := bc.Data["nested"].(string); !ok || strings.Contains(v, "secret-token") {
		t.Errorf("breadcrumb data[nested] not redacted: %v", bc.Data["nested"])
	}
	if _, ok := bc.Data["count"].(int); !ok {
		t.Errorf("breadcrumb non-string data[count] was altered: %v", bc.Data["count"])
	}

	if v, ok := got.Contexts["extra"]["note"].(string); !ok || strings.Contains(v, "secret-token") {
		t.Errorf("extra context[note] not redacted: %v", got.Contexts["extra"]["note"])
	}
	if _, ok := got.Contexts["extra"]["count"].(int); !ok {
		t.Errorf("extra non-string context[count] was altered: %v", got.Contexts["extra"]["count"])
	}

	for _, h := range []string{"authorization", "x-fleet-token", "x-api-key", "cookie"} {
		if got.Request.Headers[h] != "[Filtered]" {
			t.Errorf("request header %s = %q, want [Filtered]", h, got.Request.Headers[h])
		}
	}
	if got.Request.Headers["x-other"] != "untouched" {
		t.Errorf("non-sensitive header x-other altered: %q", got.Request.Headers["x-other"])
	}
}

func TestRedactEvent_NoPanicOnNilRequestAndBreadcrumbs(t *testing.T) {
	prev := redactor
	redactor = fakeRedactor
	t.Cleanup(func() { redactor = prev })

	event := &sentry.Event{}
	got := RedactEvent(event, nil)
	if got != event {
		t.Fatalf("RedactEvent returned a different event pointer")
	}
}

func TestInit_DisabledWhenDSNEmpty(t *testing.T) {
	if Init(Options{}) {
		t.Fatal("Init returned true for empty DSN; want false (disabled)")
	}
}
