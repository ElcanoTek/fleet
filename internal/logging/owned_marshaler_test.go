package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// envelope marshals itself and prints whatever Data holds: its static type
// has no secret-named field, but the value inside the interface does.
type envelope struct{ Data any }

func (e envelope) MarshalJSON() ([]byte, error) {
	inner, err := json.Marshal(e.Data)
	if err != nil {
		return nil, err
	}
	// Re-keyed so the key rule could not recognise the field by name.
	return json.Marshal(map[string]string{"payload": string(inner)})
}

// A self-marshalling value owns everything below it: a secret-named field
// reachable only at run time — through an interface, a map, a pointer —
// fails the value closed, not just one in its static type (#2074 review).
func TestSelfMarshalerOwnsRuntimeReachableSecrets(t *testing.T) {
	const cred = "ownedMarshalCred-Kp52Vn" // matches no pattern, not registered
	type creds struct{ Credential string }
	cases := map[string]any{
		"interface": envelope{Data: creds{Credential: cred}},
		"pointer":   envelope{Data: &creds{Credential: cred}},
		"map value": envelope{Data: map[string]any{"c": creds{Credential: cred}}},
		"map key":   envelope{Data: map[string]string{"api_key": cred}},
		"nested":    struct{ Wrapped any }{Wrapped: envelope{Data: []any{creds{Credential: cred}}}},
	}
	for name, v := range cases {
		var buf bytes.Buffer
		slog.New(NewHandler("json", &buf, slog.LevelDebug)).Info("x", "v", v)
		if out := buf.String(); strings.Contains(out, cred) || !strings.Contains(out, "unloggable value") {
			t.Fatalf("%s: %s", name, out)
		}
	}
	// An owner over innocent data still logs by its own form.
	var buf bytes.Buffer
	slog.New(NewHandler("json", &buf, slog.LevelDebug)).Info("x", "v", envelope{Data: map[string]int{"n": 1}})
	if !strings.Contains(buf.String(), `payload`) {
		t.Fatalf("an innocent envelope was not logged: %s", buf.String())
	}
}
