package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// credentialValuer renders itself as its bare credential.
type credentialValuer struct{ Credential string }

func (c credentialValuer) LogValue() slog.Value { return slog.StringValue(c.Credential) }

// wrapperValuer has no secret-named field itself, but returns a group
// holding a valuer that does: the resolved value is checked in turn.
type wrapperValuer struct{ payload string }

func (w wrapperValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.String("kind", "wrapped"), slog.Any("inner", credentialValuer{Credential: w.payload}))
}

// mapValuer hides its secret behind a runtime map key.
type mapValuer struct{ m map[string]any }

func (v mapValuer) LogValue() slog.Value { return slog.AnyValue(v.m["api_key"]) }

// plainValuer is an innocent valuer: it still logs by its own value.
type plainValuer struct{ n int }

func (p plainValuer) LogValue() slog.Value { return slog.IntValue(p.n) }

// A slog.LogValuer whose method can read a secret-named field fails closed,
// at the top level, in a group, under WithAttrs, and returned by another
// valuer — slog resolves valuers before ReplaceAttr, so the attribute rules
// never saw the field (#2074 review).
func TestLogValuerWithASecretFieldFailsClosed(t *testing.T) {
	const cred = "valuerCredPlain-Tq83Ns" // matches no pattern, not registered
	for _, format := range []string{"json", "text"} {
		var buf bytes.Buffer
		lg := slog.New(NewHandler(format, &buf, slog.LevelDebug)).With("bound", credentialValuer{Credential: cred})
		lg.Info("x",
			"top", credentialValuer{Credential: cred},
			slog.Group("g", "member", credentialValuer{Credential: cred}),
			"nested", wrapperValuer{payload: cred},
			"bymap", mapValuer{m: map[string]any{"api_key": cred}},
			"plain", plainValuer{n: 42},
		)
		out := buf.String()
		if strings.Contains(out, cred) {
			t.Fatalf("%s: a LogValuer leaked its secret field:\n%s", format, out)
		}
		if strings.Count(out, "unloggable value") < 5 || !strings.Contains(out, "42") || !strings.Contains(out, "wrapped") {
			t.Fatalf("%s: want every secret-bearing valuer as unloggable and the innocent ones kept:\n%s", format, out)
		}
	}
}
