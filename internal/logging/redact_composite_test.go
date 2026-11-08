package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

type nestedIssue struct {
	Title  string            `json:"title"`
	Notes  []string          `json:"notes"`
	Extra  map[string]string `json:"extra"`
	Count  int               `json:"count"`
	hidden string
}

// secretValuer resolves to a composite holding a secret (slog.LogValuer).
type secretValuer struct{ v string }

func (s secretValuer) LogValue() slog.Value { return slog.AnyValue([]string{"x", s.v}) }

// unwalkable cannot be marshalled (a channel field); the redactor must fall
// back to a fixed placeholder, never the value's text.
type unwalkable struct {
	Note string
	Ch   chan int
}

// Composite attribute values — slices, maps, structs, their nestings, a
// LogValuer and a group — are scrubbed through every string they hold, by
// pattern and by registered literal, in both formats (#2052).
func TestRedactsCompositeValues(t *testing.T) {
	shaped := "sk-ant-" + strings.Repeat("B", 32)
	const literal = "Qm4zR8tLw2pXv7Ns"
	RegisterSecretLiterals("", false, literal)

	cases := []struct {
		name string
		attr slog.Attr
	}{
		{"[]string", slog.Any("issues", []string{"fine", "key " + shaped, "echo " + literal})},
		{"[]any", slog.Any("v", []any{1, "key " + shaped, []string{literal}})},
		{"map[string]any", slog.Any("v", map[string]any{"a": "key " + shaped, "b": map[string]any{"c": []any{literal}}})},
		{"map[string]string", slog.Any("v", map[string]string{"a": "key " + shaped, "b": literal})},
		{"secret-named nested key", slog.Any("v", map[string]any{"api_key": "plain-but-secret-keyed"})},
		{"struct", slog.Any("v", nestedIssue{Title: "key " + shaped, Notes: []string{literal}, Extra: map[string]string{"x": shaped}, Count: 3, hidden: literal})},
		{"pointer to struct", slog.Any("v", &nestedIssue{Title: literal})},
		{"[]struct", slog.Any("v", []nestedIssue{{Title: shaped}, {Notes: []string{literal}}})},
		{"LogValuer", slog.Any("v", secretValuer{v: literal})},
		{"group", slog.Group("g", slog.Any("inner", []string{shaped}), slog.String("s", literal))},
		{"unwalkable", slog.Any("v", unwalkable{Note: literal, Ch: make(chan int)})},
	}
	for _, format := range []string{"json", "text"} {
		for _, tc := range cases {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				var buf bytes.Buffer
				slog.New(NewHandler(format, &buf, slog.LevelDebug)).LogAttrs(t.Context(), slog.LevelInfo, "phone_a_friend: review result", tc.attr)
				out := buf.String()
				for _, leak := range []string{shaped, literal, "plain-but-secret-keyed"} {
					if strings.Contains(out, leak) {
						t.Errorf("leaked %q: %s", leak, out)
					}
				}
			})
		}
	}
}

// Structure survives: a []string stays a JSON array, numbers stay numbers,
// and an innocent value is unchanged.
func TestCompositeRedactionKeepsStructure(t *testing.T) {
	var buf bytes.Buffer
	withDefault(t, &buf)
	slog.Info("x", "issues", []string{"a", "b"}, "obj", nestedIssue{Title: "t", Count: 7})
	m := decodeLast(t, &buf)
	issues, ok := m["issues"].([]any)
	if !ok || len(issues) != 2 || issues[0] != "a" || issues[1] != "b" {
		t.Fatalf("issues = %#v, want [a b]", m["issues"])
	}
	obj, ok := m["obj"].(map[string]any)
	if !ok || obj["title"] != "t" || obj["count"] != float64(7) {
		t.Fatalf("obj = %#v, want title=t count=7", m["obj"])
	}
}

// credentialHolder cannot be walked (its chan field defeats json.Marshal) and
// holds a credential under a secret-named FIELD whose value matches no
// pattern — only the key check the walk applies would catch it.
type credentialHolder struct {
	Name       string
	Credential string
	Done       chan struct{}
}

// cyclic refers to itself, which json.Marshal also refuses.
type cyclic struct {
	Token string
	Next  *cyclic
}

// An unwalkable composite fails closed: it logs as a placeholder naming its
// type, never as fmt text that skips the secret-key check (#2074 review).
func TestUnwalkableCompositeFailsClosed(t *testing.T) {
	const cred = "plain-looking-credential-value"
	loop := &cyclic{Token: cred}
	loop.Next = loop
	cases := []struct {
		name string
		v    any
		typ  string
	}{
		{"chan field", credentialHolder{Name: "acme", Credential: cred, Done: make(chan struct{})}, "logging.credentialHolder"},
		{"cycle", loop, "*logging.cyclic"},
		{"func field", struct {
			Credential string
			F          func()
		}{Credential: cred, F: func() {}}, "struct"},
	}
	for _, format := range []string{"json", "text"} {
		for _, tc := range cases {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				var buf bytes.Buffer
				slog.New(NewHandler(format, &buf, slog.LevelDebug)).Info("x", "v", tc.v)
				out := buf.String()
				if strings.Contains(out, cred) {
					t.Fatalf("credential leaked: %s", out)
				}
				if !strings.Contains(out, "[unloggable value: ") || !strings.Contains(out, tc.typ) {
					t.Fatalf("want the unloggable placeholder naming %s, got: %s", tc.typ, out)
				}
			})
		}
	}
}
