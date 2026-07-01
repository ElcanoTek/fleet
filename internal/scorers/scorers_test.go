package scorers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestRegex(t *testing.T) {
	cases := []struct {
		name, pattern, text string
		pass                bool
		label               string
	}{
		{"match", `(?i)done`, "All DONE here", true, "regex:matched"},
		{"no match", `absent`, "nothing to see", false, "regex:no_match"},
		{"invalid pattern fails closed", `([`, "anything", false, "regex:invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pass, label := Regex(tc.pattern, tc.text)
			if pass != tc.pass || label != tc.label {
				t.Fatalf("Regex(%q, %q) = (%v, %q); want (%v, %q)", tc.pattern, tc.text, pass, label, tc.pass, tc.label)
			}
		})
	}
}

func TestContains(t *testing.T) {
	if pass, label := Contains("needle", "hay needle stack"); !pass || label != "contains:matched" {
		t.Fatalf("got (%v, %q)", pass, label)
	}
	if pass, label := Contains("needle", "haystack"); pass || label != "contains:no_match" {
		t.Fatalf("got (%v, %q)", pass, label)
	}
	if pass, label := Contains("", "anything"); pass || label != "contains:empty_needle" {
		t.Fatalf("empty needle must fail closed, got (%v, %q)", pass, label)
	}
}

func TestEquals(t *testing.T) {
	if pass, _ := Equals("42", "  42\n"); !pass {
		t.Fatal("surrounding whitespace must not fail an exact-match golden")
	}
	if pass, label := Equals("42", "43"); pass || label != "equals:no_match" {
		t.Fatalf("got (%v, %q)", pass, label)
	}
}

// fakeBash is a scorers.BashRunner double so Shell is testable without podman.
type fakeBash struct {
	res sandbox.BashResult
	err error
}

func (f fakeBash) RunBash(_ context.Context, _ sandbox.BashRequest) (sandbox.BashResult, error) {
	return f.res, f.err
}

func TestShell(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		fake  fakeBash
		pass  bool
		label string
	}{
		{"exit 0 passes", fakeBash{res: sandbox.BashResult{ExitCode: 0}}, true, "shell:passed"},
		{"non-zero exit fails with code", fakeBash{res: sandbox.BashResult{ExitCode: 3}}, false, "shell:exit_3"},
		{"timeout fails", fakeBash{res: sandbox.BashResult{TimedOut: true}}, false, "shell:timeout"},
		{"runner error fails", fakeBash{err: errors.New("boom")}, false, "shell:error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pass, label := Shell(ctx, tc.fake, "true", time.Second)
			if pass != tc.pass || label != tc.label {
				t.Fatalf("got (%v, %q); want (%v, %q)", pass, label, tc.pass, tc.label)
			}
		})
	}
}

func TestFirstWordIsYes(t *testing.T) {
	yes := []string{"YES", "yes, it did", " **Yes** — complete", "> yes"}
	no := []string{"NO", "not yet", "The answer is yes", ""}
	for _, s := range yes {
		if !FirstWordIsYes(s) {
			t.Errorf("FirstWordIsYes(%q) = false; want true", s)
		}
	}
	for _, s := range no {
		if FirstWordIsYes(s) {
			t.Errorf("FirstWordIsYes(%q) = true; want false", s)
		}
	}
}

func TestLastAssistantMessage(t *testing.T) {
	if got := LastAssistantMessage(nil); got != "" {
		t.Fatalf("nil session: got %q", got)
	}
	s := &models.LogSession{Messages: []models.LogMessage{
		{Role: "user", Content: "do it"},
		{Role: "assistant", Content: "first"},
		{Role: "tool", Content: "result"},
		{Role: "assistant", Content: "final answer"},
	}}
	if got := LastAssistantMessage(s); got != "final answer" {
		t.Fatalf("got %q; want %q", got, "final answer")
	}
	empty := &models.LogSession{Messages: []models.LogMessage{{Role: "user", Content: "hi"}}}
	if got := LastAssistantMessage(empty); got != "" {
		t.Fatalf("no assistant messages: got %q", got)
	}
}
