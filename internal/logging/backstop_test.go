package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// The line-level backstop: a registered credential cannot reach a sink by a
// path the attribute rules do not cover — here a group name, which
// ReplaceAttr never sees — in its raw form or its JSON-escaped form (a secret
// holding a quote and a backslash), and the line stays valid JSON (#2074
// review).
func TestBackstopScrubsEveryEncodedLine(t *testing.T) {
	const plain = "groupNameSecret-7Hq2Lw"
	const quoted = `quo"te\back-Secret9Zk`
	RegisterSecretLiterals("", false, plain, quoted)
	for _, format := range []string{"json", "text"} {
		var buf bytes.Buffer
		slog.New(NewHandler(format, &buf, slog.LevelDebug)).Info("x",
			slog.Group(plain, "k", "v"),
			slog.Group(quoted, "k", "v"),
		)
		out := buf.String()
		escapedJSON, _ := json.Marshal(quoted)
		for _, leak := range []string{plain, quoted, string(escapedJSON[1 : len(escapedJSON)-1]), fmt.Sprintf("%q", quoted)[1 : len(fmt.Sprintf("%q", quoted))-1]} {
			if strings.Contains(out, leak) {
				t.Fatalf("%s: a registered secret in a group name reached the sink as %q: %s", format, leak, out)
			}
		}
		if format == "json" && !json.Valid(bytes.TrimSpace(buf.Bytes())) {
			t.Fatalf("the scrubbed line is not valid JSON: %s", out)
		}
	}
}

// A map key whose type marshals itself (MarshalText) and has a secret field
// fails closed like a value would (#2074 review).
type secretKey struct {
	Name  string
	token string
}

func (k secretKey) MarshalText() ([]byte, error) { return []byte(k.Name + ":" + k.token), nil }

// A self-marshalling type whose secret sits in an unexported nested struct.
type cfgInner struct{ Password string }

type cfgWrapper struct{ cfg cfgInner }

func (w cfgWrapper) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"note": w.cfg.Password})
}

// A Stringer whose secret field is unexported.
type hiddenStringer struct {
	name   string
	apiKey string
}

func (h hiddenStringer) String() string { return h.name + " " + h.apiKey }

// An error type carrying a secret field its Error() prints.
type authError struct{ Token string }

func (e authError) Error() string { return "denied for " + e.Token }

func TestSelfDescribingSecretsAtEveryDepth(t *testing.T) {
	const secret = "unregisteredOpaque-Pz31"
	// Not registered: only the structural rules can catch it.
	var buf bytes.Buffer
	slog.New(NewHandler("json", &buf, slog.LevelDebug)).Info("x",
		"bykey", map[secretKey]string{{Name: "a", token: secret}: "v"},
		"wrapped", cfgWrapper{cfg: cfgInner{Password: secret}},
		"stringer", hiddenStringer{name: "n", apiKey: secret},
		"err", authError{Token: secret},
		"wrappederr", fmt.Errorf("call: %w", authError{Token: secret}),
	)
	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("a secret field leaked through a self-describing type: %s", out)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"bykey", "wrapped"} {
		if s, _ := m[k].(string); !strings.HasPrefix(s, "[unloggable value: ") {
			t.Errorf("%s = %v, want the unloggable placeholder", k, m[k])
		}
	}
	if e, _ := m["err"].(map[string]any); e["Token"] != redactedValue {
		t.Errorf("err = %v, want walked with Token redacted", m["err"])
	}
	_ = errors.New
}

// Attribute keys are redacted like values, and the built-in exemption covers
// only the handler's own time/level/source/msg at the top level with the
// kind the handler emits — a call site's "level" string, or "msg" inside a
// group, is scrubbed (#2074 review).
func TestAttributeKeysAndBuiltInNames(t *testing.T) {
	const literal = "attrKeySecret-Mw82Qe"
	RegisterSecretLiterals("", false, literal)
	shaped := "sk-ant-" + strings.Repeat("Q", 32)
	if a := replaceAttr(nil, slog.String(literal, "v")); a.Key == literal {
		t.Fatalf("a registered secret as an attribute key survived: %v", a)
	}
	if a := replaceAttr(nil, slog.String(slog.LevelKey, shaped)); strings.Contains(a.Value.String(), shaped) {
		t.Fatalf("a top-level string \"level\" attribute was treated as the built-in: %v", a)
	}
	if a := replaceAttr([]string{"g"}, slog.String(slog.MessageKey, shaped)); strings.Contains(a.Value.String(), shaped) {
		t.Fatalf("\"msg\" inside a group was treated as the built-in: %v", a)
	}
	if a := replaceAttr(nil, slog.Any(slog.LevelKey, slog.LevelWarn)); a.Value.Any() != slog.LevelWarn {
		t.Fatalf("the real built-in level was altered: %v", a)
	}
}

// BenchmarkBackstopLine measures the line-level backstop on a typical JSON
// record with a realistic literal set registered.
func BenchmarkBackstopLine(b *testing.B) {
	for i := range 64 {
		RegisterSecretLiterals("", false, fmt.Sprintf("benchLiteral-%04d-Xq9Zk2", i))
	}
	line := []byte(`{"time":"2026-11-08T05:12:46.123456789Z","level":"INFO","msg":"Task created","task_id":"0b7a5f0e-1b8c-4c1f-9a7e-3d2c1b0a9f8e","prompt":"Refresh the weekly pages from the source data","user_id":"apikey/key_4f6a9394ceaf08ac","request_id":"4bf92f3577b34da6a3ce929d0e0e4736"}` + "\n")
	var sink bytes.Buffer
	w := scrubWriter{w: &sink, json: true}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sink.Reset()
		_, _ = w.Write(line)
	}
}
