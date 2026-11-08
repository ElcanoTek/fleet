package logging

import (
	"bytes"
	"encoding/json"
	"io"
)

// scrubWriter is the line-level backstop under every handler this package
// builds (JSON and text, the default, bridge, panic and emergency handlers,
// and so every sink and every re-logged child record): each fully encoded
// record is passed through the redactor — registered literals, in their raw
// and their JSON- and Go-quoted forms, and the vendor-shape patterns — before
// it is written. The attribute-level rules (secret-named keys, structural
// walks, fail-closed self-describing types) decide most cases; this layer
// guarantees that a registered credential or a recognised vendor key cannot
// reach a sink by any path those rules did not anticipate: a key, a group
// name, a built-in field, a MarshalJSON, an Error() or a String().
//
// The placeholders carry no quote or backslash, so a replacement inside a
// JSON string keeps the line valid. Should a pattern still span JSON
// structure (a PEM block whose BEGIN and END sit in different fields), the
// line is replaced by a fixed, valid notice rather than written broken or
// half-redacted.
type scrubWriter struct {
	w io.Writer
	// json is the encoding of the lines written (JSON records, else logfmt):
	// each literal is matched in that encoding's form only.
	json bool
}

// brokenLineNotice replaces a JSON line the redactor's patterns made invalid.
const brokenLineNotice = `{"level":"ERROR","msg":"logging: a log line was withheld: redacting it broke its JSON encoding"}` + "\n"

func (s scrubWriter) Write(p []byte) (int, error) {
	out := redactor().ScrubLine(string(p), s.json)
	if out != string(p) {
		if trimmed := bytes.TrimSpace(p); len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed) &&
			!json.Valid(bytes.TrimSpace([]byte(out))) {
			out = brokenLineNotice
		}
	}
	if _, err := io.WriteString(s.w, out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// scrubbed wraps w in the backstop once, for lines of the given encoding.
func scrubbed(w io.Writer, json bool) io.Writer {
	if _, ok := w.(scrubWriter); ok {
		return w
	}
	return scrubWriter{w: w, json: json}
}
