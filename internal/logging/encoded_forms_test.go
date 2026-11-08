package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// trickyLiterals each encode differently in a JSON string and a logfmt value
// than as raw bytes: line and paragraph separators, HTML characters, control
// bytes, quotes, backslashes, invalid UTF-8, spaces and '='.
var trickyLiterals = []string{
	"lineSep Secret-Aq1",
	"paraSep Secret-Bw2",
	"<html>&amp;Secret-Ce3",
	"ctl\x01\x7f\x1bSecret-Dr4",
	`quo"te\back\slash-Ft5`,
	"badUtf8\xff\xfeSecret-Gy6",
	"space and=eq Secret-Hu7",
	"tab\there nbsp-Ji8",
}

// quotedToken is one Go-quoted token of a logfmt line.
var quotedToken = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// A registered literal is gone from the encoded line whatever it holds and
// wherever it sits — group name, key, value, inside a longer value — through
// both handlers: no form a reader could decode back to it remains (#2074
// review: the encoded forms were computed only for an ASCII special set).
func TestRegisteredLiteralIsUnrecoverableInEveryEncoding(t *testing.T) {
	for _, lit := range trickyLiterals {
		RegisterSecretLiterals("", false, lit)
	}
	for _, lit := range trickyLiterals {
		recoverable := []string{lit, strings.ToValidUTF8(lit, "�")}
		for _, format := range []string{"json", "text"} {
			var buf bytes.Buffer
			slog.New(NewHandler(format, &buf, slog.LevelDebug)).WithGroup(lit).Info("x",
				lit, "plain",
				"v", lit,
				"embedded", "before "+lit+" after",
			)
			out := buf.String()
			for _, s := range decodedStrings(t, format, out) {
				for _, r := range recoverable {
					if strings.Contains(s, r) {
						t.Fatalf("%s: %q is recoverable from %s", format, lit, out)
					}
				}
			}
		}
	}
}

// decodedStrings is every string a reader recovers from one line: the raw
// line itself, and each decoded JSON string (keys too) or Go-quoted token.
func decodedStrings(t *testing.T, format, line string) []string {
	t.Helper()
	out := []string{line}
	if format == "json" {
		var v any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("invalid JSON line %q: %v", line, err)
		}
		var walk func(any)
		walk = func(x any) {
			switch x := x.(type) {
			case string:
				out = append(out, x)
			case map[string]any:
				for k, e := range x {
					out = append(out, k)
					walk(e)
				}
			case []any:
				for _, e := range x {
					walk(e)
				}
			}
		}
		walk(v)
		return out
	}
	for _, tok := range quotedToken.FindAllString(line, -1) {
		if s, err := strconv.Unquote(tok); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// With `level=INFO` registered, text-format records still log normally —
// their own level field is untouched — and the literal inside a value is
// redacted (#2074 review).
func TestTextRecordsSurviveASyntaxShapedLiteral(t *testing.T) {
	RegisterSecretLiterals("", false, "level=INFO")
	var buf bytes.Buffer
	lg := slog.New(NewHandler("text", &buf, slog.LevelDebug))
	lg.Info("ordinary", "n", 1)
	lg.Info("x", "body", "saw level=INFO here")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], " level=INFO msg=ordinary n=1") {
		t.Fatalf("an ordinary text record changed:\n%s", buf.String())
	}
	if !strings.Contains(lines[1], " level=INFO msg=x ") || strings.Contains(lines[1], "saw level=INFO") ||
		!strings.Contains(lines[1], `body="saw [REDACTED] here"`) {
		t.Fatalf("the literal inside a value: %s", lines[1])
	}
}
