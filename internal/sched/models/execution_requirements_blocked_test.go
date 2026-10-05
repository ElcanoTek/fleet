package models

import (
	"strings"
	"testing"
)

func requirementsPrompt(body string) string {
	return "Refresh the page.\n" + ExecutionRequirementsMarker + "\n" + body + "\nFinish."
}

// pagesBlockedWhen is the Pages refresh producer's completion clause.
const pagesBlockedWhen = `{"completion":{"any_succeeded":["mcp_pages_record_refresh_check","mcp_pages_update_page_data","mcp_pages_update_page_data_upload"],` +
	`"blocked_when":{"tool":"mcp_pages_record_refresh_check","argument":"outcome","in":["blocked","failed","source_unreachable"],"detail_argument":"detail"}}}`

func TestParseCompletionBlockedWhen(t *testing.T) {
	req, err := ParseExecutionRequirements(requirementsPrompt(pagesBlockedWhen))
	if err != nil {
		t.Fatal(err)
	}
	bw := req.Completion.BlockedWhen
	if bw == nil || bw.Tool != "mcp_pages_record_refresh_check" || bw.Argument != "outcome" ||
		strings.Join(bw.In, ",") != "blocked,failed,source_unreachable" || bw.DetailArgument != "detail" {
		t.Fatalf("blocked_when = %+v", bw)
	}
	for _, ok := range []string{
		`{"completion":{"any_succeeded":["record"],"blocked_when":{"tool":"record","argument":"outcome","in":["blocked"]}}}`, // no detail_argument
		`{"completion":{"any_succeeded":["record"],"blocked_when":null}}`,
		`{"completion":{"any_succeeded":["record"],"blocked_when":{"tool":"record","argument":"outcome","in":["blocked"],"future_key":1}}}`,
	} {
		if err := ValidateExecutionRequirements(requirementsPrompt(ok)); err != nil {
			t.Fatalf("valid blocked_when refused: %v\n%s", err, ok)
		}
	}
}

func TestValidateCompletionBlockedWhenRejections(t *testing.T) {
	anySucceeded := `"any_succeeded":["mcp_pages_record_refresh_check","mcp_pages_update_page_data"]`
	clause := func(blockedWhen string) string {
		return `{"completion":{` + anySucceeded + `,"blocked_when":` + blockedWhen + `}}`
	}
	for _, tc := range []struct{ body, want string }{
		{`{"completion":{"blocked_when":{"tool":"x","argument":"outcome","in":["blocked"]}}}`, "completion.blocked_when needs completion.any_succeeded"},
		{clause(`{"argument":"outcome","in":["blocked"]}`), `invalid tool identifier "" in completion.blocked_when.tool`},
		{clause(`{"tool":"pages record","argument":"outcome","in":["blocked"]}`), `invalid tool identifier "pages record" in completion.blocked_when.tool; allowed ^[a-zA-Z0-9_.-]{1,200}$`},
		{clause(`{"tool":"mcp_pages_get_page_data","argument":"outcome","in":["blocked"]}`), `completion.blocked_when.tool "mcp_pages_get_page_data" is not listed in completion.any_succeeded`},
		{clause(`{"tool":"mcp_pages_record_refresh_check","argument":"result.outcome","in":["blocked"]}`), `invalid argument name "result.outcome" in completion.blocked_when.argument`},
		{clause(`{"tool":"mcp_pages_record_refresh_check","argument":"outcome","in":["blocked"],"detail_argument":"why not"}`), `invalid argument name "why not" in completion.blocked_when.detail_argument`},
		{clause(`{"tool":"mcp_pages_record_refresh_check","argument":"outcome","in":[]}`), "completion.blocked_when.in must list 1 to 20 values (got 0)"},
		{clause(`{"tool":"mcp_pages_record_refresh_check","argument":"outcome"}`), "completion.blocked_when.in must list 1 to 20 values (got 0)"},
		{clause(`{"tool":"mcp_pages_record_refresh_check","argument":"outcome","in":["` + strings.Repeat("v", 101) + `"]}`), "in completion.blocked_when.in[0]; each value is 1 to 100 printable characters"},
		{clause(`{"tool":"mcp_pages_record_refresh_check","argument":"outcome","in":["blocked",""]}`), `invalid value "" in completion.blocked_when.in[1]`},
		{clause(`{"tool":"mcp_pages_record_refresh_check","argument":"outcome","in":["a\u0007b"]}`), "in completion.blocked_when.in[0]"},
		{clause(`{"tool":"mcp_pages_record_refresh_check","argument":"outcome","in":"blocked"}`), "invalid JSON object after the marker"},
	} {
		err := ValidateExecutionRequirements(requirementsPrompt(tc.body))
		if err == nil || !strings.HasPrefix(err.Error(), "execution requirements: ") || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s\ngot %v, want an error containing %q", tc.body, err, tc.want)
		}
	}
}

func TestDeclaredSerializationKey(t *testing.T) {
	if got := DeclaredSerializationKey(requirementsPrompt(`{"mcp_servers":["pages"],"serialization_key":"pages:acme-seller-view"}`)); got != "pages:acme-seller-view" {
		t.Fatalf("declared key = %q", got)
	}
	for _, none := range []string{
		"An ordinary prompt",
		requirementsPrompt(`{"mcp_servers":["pages"]}`),
		requirementsPrompt(`{"serialization_key":null}`),
		requirementsPrompt(`{"serialization_key":"has space"}`), // malformed: refused at save, never applied
	} {
		if got := DeclaredSerializationKey(none); got != "" {
			t.Fatalf("%q declared key %q, want none", none, got)
		}
	}
	for _, tc := range []struct{ body, want string }{
		{`{"serialization_key":""}`, `invalid serialization_key ""; allowed ^[A-Za-z0-9_.:/-]{1,200}$`},
		{`{"serialization_key":"pages: acme"}`, `invalid serialization_key "pages: acme"`},
		{`{"serialization_key":"` + strings.Repeat("k", 201) + `"}`, "invalid serialization_key"},
		{`{"serialization_key":["pages:acme"]}`, "invalid JSON object after the marker"},
	} {
		err := ValidateExecutionRequirements(requirementsPrompt(tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %v, want %q", tc.body, err, tc.want)
		}
	}
}

// TestNewTaskTakesTheDeclaredSerializationKey: a task created with no
// explicit key — from chat, the task form, an import — is serialized by the
// key its prompt declares; an explicit key always wins.
func TestNewTaskTakesTheDeclaredSerializationKey(t *testing.T) {
	prompt := requirementsPrompt(`{"mcp_servers":["pages"],"serialization_key":"pages:acme"}`)
	task := NewTask(TaskCreate{Prompt: prompt})
	if task.SerializationKey == nil || *task.SerializationKey != "pages:acme" {
		t.Fatalf("serialization_key = %v, want the declared pages:acme", task.SerializationKey)
	}
	blank := "  "
	if task := NewTask(TaskCreate{Prompt: prompt, SerializationKey: &blank}); task.SerializationKey == nil || *task.SerializationKey != "pages:acme" {
		t.Fatalf("a blank explicit key is no key: got %v", task.SerializationKey)
	}
	explicit := "client:42"
	if task := NewTask(TaskCreate{Prompt: prompt, SerializationKey: &explicit}); task.SerializationKey == nil || *task.SerializationKey != "client:42" {
		t.Fatalf("an explicit key must win: got %v", task.SerializationKey)
	}
	if task := NewTask(TaskCreate{Prompt: "no declaration"}); task.SerializationKey != nil {
		t.Fatalf("no declaration, no key: got %v", *task.SerializationKey)
	}
	// A recurrence successor is created from the current definition, so an
	// occurrence created before the declaration gains the key on the next spawn.
	legacy := NewTask(TaskCreate{Prompt: "old prompt"})
	legacy.Prompt = prompt // as a prompt edit leaves it; the row's own key is immutable
	if next := NewTask(TaskToCreate(legacy)); next.SerializationKey == nil || *next.SerializationKey != "pages:acme" {
		t.Fatalf("successor key = %v, want the declared key", next.SerializationKey)
	}
}
