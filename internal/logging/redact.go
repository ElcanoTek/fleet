package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/redact"
)

// The log redactor reuses internal/redact — the canonical vendor-key, bearer,
// PEM and marker=value patterns that scrub tool output (agentcore/redact.go)
// and broker errors (mcpbroker/redact.go) — rather than growing a third rule
// set. Its LITERAL set (exact secret values, any shape) is fed by the same
// boot and runtime registrations those two packages already receive:
// agentcore.RegisterSecretLiterals and mcpbroker.RegisterSecretLiteral(s)
// forward here, and each package's lazily-built redactor hands this one the
// secret-named env values it snapshots (RegisterEnvLiterals). So a connector
// credential the broker scrubs from an error string is scrubbed from a log
// attribute too, by value, whatever its shape.
//
// The redactor is built lazily so a process that never logs never compiles
// the patterns, and so tests can reset it.
var (
	redactorOnce sync.Once
	logRedactor  *redact.Redactor
)

func redactor() *redact.Redactor {
	redactorOnce.Do(func() { logRedactor = redact.NewRedactor(nil) })
	return logRedactor
}

// RegisterSecretLiterals adds secret VALUES to the log redactor, with the
// scope/rotation semantics of redact.Redactor.RegisterSecrets: an empty scope
// registers permanently, a scope with rotated=true replaces that scope's
// previous generation after the redactor's grace window. The values are kept
// only as scrub targets and are never emitted.
func RegisterSecretLiterals(scope string, rotated bool, values ...string) {
	redactor().RegisterSecrets(scope, rotated, values...)
}

// RegisterEnvLiterals adds the values of secret-named variables in environ
// (os.Environ form) to the log redactor — redact.Redactor.RegisterEnvLiterals,
// the same name rule the tool-output and broker redactors seed themselves with.
func RegisterEnvLiterals(environ []string) {
	redactor().RegisterEnvLiterals(environ)
}

// Redact scrubs s with the log redactor. Exported for the rare call site that
// must compose a string before logging it (a multi-line diagnostic).
func Redact(s string) string {
	if s == "" {
		return s
	}
	return redactor().Redact(s)
}

// replaceAttr is the slog ReplaceAttr hook both handlers install. It runs on
// every attribute — including ones bound with Logger.With, which slog
// pre-formats through the same hook — and on the built-in message.
//
//   - time and level pass through untouched (they cannot carry a secret);
//   - the message is scrubbed by value;
//   - a STRING-like value whose key names a secret (token, password, api_key,
//     authorization, …) is replaced whole with "[REDACTED]". Numeric, boolean
//     and duration values are left alone: "prompt_tokens=1234" is a count, not
//     a credential;
//   - every other string value, error and fmt.Stringer is scrubbed by value
//     (patterns + registered literals), so a token inside an error message or
//     a URL is caught even under an innocent key.
//
// Groups recurse: slog calls replaceAttr for each member with the group path.
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 {
		switch a.Key {
		case slog.TimeKey, slog.LevelKey, slog.SourceKey:
			return a
		case slog.MessageKey:
			if a.Value.Kind() == slog.KindString {
				return slog.String(a.Key, Redact(a.Value.String()))
			}
			return a
		}
	}
	v := a.Value
	switch v.Kind() {
	case slog.KindString:
		if secretAttrKey(a.Key) {
			return slog.String(a.Key, redactedValue)
		}
		return slog.String(a.Key, Redact(v.String()))
	case slog.KindAny:
		if secretAttrKey(a.Key) {
			return slog.String(a.Key, redactedValue)
		}
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, Redact(errorText(x)))
		case fmt.Stringer:
			return slog.String(a.Key, Redact(x.String()))
		case []byte:
			return slog.String(a.Key, Redact(string(x)))
		}
		return slog.Any(a.Key, redactComposite(v.Any()))
	default:
		// Int64/Uint64/Float64/Bool/Duration/Time/Group/LogValuer: no text to
		// scrub (a group's members are visited individually).
		return a
	}
}

// redactComposite scrubs every string a composite attribute value holds —
// a slice, map, struct, pointer, or any nesting of them (a model-authored
// []string of review issues, a decoded JSON object) — keeping its structure.
// Scalars pass through. Common shapes are walked directly; anything else is
// round-tripped through JSON (marshal, walk, keep the decoded structure),
// which is also how the JSON handler would have rendered it. A value that
// cannot be marshalled is logged as its redacted fmt text, never raw.
func redactComposite(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case bool, json.Number,
		int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr,
		float32, float64, complex64, complex128:
		// json.Number is a Stringer; it must stay a number, not become text.
		return x
	case string:
		return Redact(x)
	case []string:
		out := make([]string, len(x))
		for i, s := range x {
			out[i] = Redact(s)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = redactComposite(e)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(x))
		for k, s := range x {
			if secretAttrKey(k) {
				out[k] = redactedValue
				continue
			}
			out[k] = Redact(s)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if secretAttrKey(k) && !isNumberOrBool(e) {
				out[k] = redactedValue
				continue
			}
			out[k] = redactComposite(e)
		}
		return out
	case error:
		return Redact(errorText(x))
	case fmt.Stringer:
		return Redact(x.String())
	case []byte:
		return Redact(string(x))
	}
	// A named scalar type (type Status int) has no text to scrub.
	switch reflect.ValueOf(v).Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return v
	case reflect.String:
		return Redact(reflect.ValueOf(v).String())
	default:
		// Composite or other kinds: walked through JSON below.
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return Redact(fmt.Sprintf("%+v", v))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var walked any
	if err := dec.Decode(&walked); err != nil {
		return Redact(fmt.Sprintf("%+v", v))
	}
	return redactComposite(walked)
}

// isNumberOrBool reports a decoded JSON scalar that carries no text, so a
// secret-named key holding a count ("max_tokens": 4096) keeps its value.
func isNumberOrBool(v any) bool {
	switch v.(type) {
	case json.Number, bool, int, int64, float64:
		return true
	}
	return false
}

// errorText is err.Error() with a nil-pointer error guarded, so a typed-nil
// error logged by mistake renders instead of panicking the handler.
func errorText(err error) (s string) {
	defer func() {
		if recover() != nil {
			s = fmt.Sprintf("%T(nil)", err)
		}
	}()
	return err.Error()
}

// redactedValue replaces a secret-keyed attribute's whole value; it is the
// placeholder internal/redact writes.
const redactedValue = "[REDACTED]"

// secretKeyMarkers are the attribute-key substrings that name a credential.
// The list is the one this package has always applied (#178), aligned with
// internal/redact's marker=value rule (api_key, secret, token, password,
// passwd, authorization) plus bearer/credential.
var secretKeyMarkers = []string{"secret", "token", "password", "passwd", "apikey", "api_key", "authorization", "bearer", "credential"}

// secretAttrKey reports whether an attribute key names a likely secret.
func secretAttrKey(key string) bool {
	k := strings.ToLower(key)
	for _, s := range secretKeyMarkers {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}
