package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// sneakyMarshaler has a secret-named field and a MarshalJSON that prints it
// under an innocent key, so the key rule on the output never sees it.
type sneakyMarshaler struct {
	Name       string
	Credential string
}

func (s sneakyMarshaler) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"name": s.Name, "note": s.Credential})
}

// hiddenTextMarshaler keeps its secret unexported and prints it via MarshalText.
type hiddenTextMarshaler struct{ token string }

func (h hiddenTextMarshaler) MarshalText() ([]byte, error) { return []byte(h.token), nil }

// plainWithSecret has a secret-named field and no custom marshaler: walked.
type plainWithSecret struct {
	Name     string
	Password string
}

type outer struct {
	ID    int
	Inner any
}

// A composite that marshals itself and has a secret-named field — exported
// or not, at the top or nested anywhere — logs as a placeholder; one without
// a custom marshaler is walked, its secret field redacted by key; a
// self-marshalling type with no secret field (time.Time) is untouched
// (#2074 review).
func TestSelfMarshallingSecretsFailClosed(t *testing.T) {
	const secret = "marshalSecretPlain-Wq72"
	var buf bytes.Buffer
	slog.New(NewHandler("json", &buf, slog.LevelDebug)).Info("x",
		"top", sneakyMarshaler{Name: "a", Credential: secret},
		"text", hiddenTextMarshaler{token: secret},
		"nested", outer{ID: 1, Inner: []any{map[string]any{"k": &sneakyMarshaler{Credential: secret}}}},
		"plain", plainWithSecret{Name: "b", Password: secret},
		"when", struct{ At time.Time }{At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
	)
	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("a self-marshalled secret leaked: %s", out)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"top", "text", "nested"} {
		if s, _ := m[k].(string); !strings.HasPrefix(s, "[unloggable value: ") {
			t.Errorf("%s = %v, want the unloggable placeholder", k, m[k])
		}
	}
	plain, _ := m["plain"].(map[string]any)
	if plain["Name"] != "b" || plain["Password"] != redactedValue {
		t.Errorf("plain = %v, want walked with Password redacted", m["plain"])
	}
	when, _ := m["when"].(map[string]any)
	if when["At"] != "2026-01-02T03:04:05Z" {
		t.Errorf("when = %v, want time.Time's own JSON", m["when"])
	}
}

type panickyStringer struct{}

func (panickyStringer) String() string { panic("boom in String") }

type nilSafeless struct{ name string }

func (n *nilSafeless) String() string { return n.name } // dereferences n

type panickyError struct{}

func (panickyError) Error() string { panic("boom in Error") }

type panickyMarshaler struct{ A int }

func (panickyMarshaler) MarshalJSON() ([]byte, error) { panic("boom in MarshalJSON") }

// A user method that panics — String(), Error(), MarshalJSON — or a typed-nil
// Stringer never takes the handler down: each renders as a placeholder and
// the line is still written (#2074 review).
func TestPanickingUserMethodsDoNotEscapeTheHandler(t *testing.T) {
	var typedNil *nilSafeless
	var buf bytes.Buffer
	slog.New(NewHandler("json", &buf, slog.LevelDebug)).Info("still logged",
		"s", panickyStringer{},
		"nilp", typedNil,
		"e", panickyError{},
		"m", panickyMarshaler{A: 1},
		"inslice", []any{panickyStringer{}},
	)
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("no line written: %v: %q", err, buf.String())
	}
	if m["msg"] != "still logged" {
		t.Fatalf("record = %v", m)
	}
	if m["s"] != "[unloggable value: logging.panickyStringer]" || m["nilp"] != "*logging.nilSafeless(nil)" || m["e"] != "[unloggable value: logging.panickyError]" {
		t.Fatalf("placeholders = s:%v nilp:%v e:%v", m["s"], m["nilp"], m["e"])
	}
	if s, _ := m["m"].(string); !strings.HasPrefix(s, "[unloggable value: ") {
		t.Fatalf("m = %v", m["m"])
	}
}
