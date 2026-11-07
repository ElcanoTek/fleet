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
// back to its redacted fmt text, never the raw value.
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
