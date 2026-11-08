package logging

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"sort"
	"strconv"
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

// seedEnvLiterals registers this process's own secret-named env values
// (Configure calls it first thing). In the MCP broker that is the connector
// credentials; in fleet serve, whatever the env file set — before the
// broker boot path hands connector values over and unsets them.
func seedEnvLiterals() {
	RegisterEnvLiterals(os.Environ())
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
// Groups recurse: slog calls replaceAttr for each member with the group path,
// and a member under a secret-named group is treated as secret-keyed.
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
		if secretAttrKey(a.Key) || secretGroupPath(groups) {
			return slog.String(a.Key, redactedValue)
		}
		return slog.String(a.Key, Redact(v.String()))
	case slog.KindAny:
		if secretAttrKey(a.Key) || secretGroupPath(groups) {
			return slog.String(a.Key, redactedValue)
		}
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, Redact(errorText(x)))
		case fmt.Stringer:
			if !stringerHidesSecretKeys(x) {
				return slog.String(a.Key, Redact(stringText(x)))
			}
		case []byte:
			return slog.String(a.Key, Redact(string(x)))
		}
		return slog.Any(a.Key, redactComposite(v.Any()))
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindDuration:
		// A number is a credential as easily as a string ({"password":
		// 12345678}, a numeric PIN or account secret). Under a secret-named
		// key or group it is redacted unless the key is count-like
		// (prompt_tokens, retry_count, latency_ms — countLikeKey).
		if (secretAttrKey(a.Key) || secretGroupPath(groups)) && !countLikeKey(a.Key) {
			return slog.String(a.Key, redactedValue)
		}
		return a
	default:
		// Bool/Time/Group/LogValuer: no secret to carry (a bool is one bit;
		// a group's members are visited individually).
		return a
	}
}

// redactComposite scrubs every string a composite attribute value holds —
// a slice, map, struct, pointer, or any nesting of them (a model-authored
// []string of review issues, a decoded JSON object) — keeping its structure.
// Scalars pass through. Common shapes are walked directly; anything else is
// round-tripped through JSON (marshal, walk, keep the decoded structure),
// which is also how the JSON handler would have rendered it. A value that
// cannot be marshalled (a cycle, a chan or func field) is logged as a fixed
// placeholder naming its type, never as its fmt text.
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
		for _, k := range redactedKeys(x) {
			if secretAttrKey(k.orig) {
				out[k.out] = redactedValue
				continue
			}
			out[k.out] = Redact(x[k.orig])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for _, k := range redactedKeys(x) {
			e := x[k.orig]
			if secretAttrKey(k.orig) && !keepUnderSecretKey(k.orig, e) {
				out[k.out] = redactedValue
				continue
			}
			out[k.out] = redactComposite(e)
		}
		return out
	case error:
		return Redact(errorText(x))
	case fmt.Stringer:
		if !stringerHidesSecretKeys(x) {
			return Redact(stringText(x))
		}
		// A composite whose fields the secret-key rule names: walked by
		// structure below, never flattened by its own String().
		if marshalsItself(x) {
			return unloggable(x)
		}
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
	// encoding/json hands a value with its own MarshalJSON/MarshalText the
	// form that code chooses — at any depth — and the key rule then never
	// sees the fields. A self-marshalling value whose type has a
	// secret-named field (exported or not: its own code can print either)
	// fails closed, wherever in v it sits.
	if hasSecretMarshaler(reflect.ValueOf(v)) {
		return unloggable(v)
	}
	raw, err := safeMarshal(v)
	if err != nil {
		return unloggable(v)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var walked any
	if err := dec.Decode(&walked); err != nil {
		return unloggable(v)
	}
	return redactComposite(walked)
}

// keyPair is one map key and the form it is logged under.
type keyPair struct{ orig, out string }

// redactedKeys is the one key routine for every map the redactor rebuilds
// (redactComposite, and through it child JSON and slog.Any values): each key
// scrubbed like a value — a key can be a secret too (a registered token used
// as a map key, a header line used as a field name) — in sorted order, with
// collisions made unique deterministically: when two keys redact to the same
// text, the later one in sorted order gets "#2", "#3", … so neither entry is
// lost and the output does not depend on map iteration order. The secret-KEY
// rule is applied by the caller to the ORIGINAL key.
func redactedKeys[V any](m map[string]V) []keyPair {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	seen := make(map[string]bool, len(keys))
	out := make([]keyPair, 0, len(keys))
	for _, k := range keys {
		out = append(out, keyPair{orig: k, out: uniqueKey(seen, Redact(k))})
	}
	return out
}

// uniqueKey returns k, or k#2, k#3 … — the first not yet in seen — and marks
// it seen.
func uniqueKey(seen map[string]bool, k string) string {
	c := k
	for i := 2; seen[c]; i++ {
		c = k + "#" + strconv.Itoa(i)
	}
	seen[c] = true
	return c
}

// stringerHidesSecretKeys reports a fmt.Stringer that must not be logged by
// its String(): a struct, map, slice or array (behind any pointers) whose
// type carries a field or key the secret-key rule names — a Credential
// field, a map keyed "api_key" — so String() could print that value with no
// key left to redact it by. Such a value is walked structurally like any
// composite. Every other Stringer (time.Time, a UUID, net.IP: values whose
// String() is their meaning) keeps String(), which is still scrubbed by
// pattern and literal.
func stringerHidesSecretKeys(v any) bool {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			for _, k := range rv.MapKeys() {
				if secretAttrKey(k.String()) {
					return true
				}
			}
		}
		return typeHasSecretPath(rv.Type().Elem())
	case reflect.Struct, reflect.Slice, reflect.Array:
		return typeHasSecretPath(rv.Type())
	default:
		// scalars, strings, funcs, chans: String() is all there is
		return false
	}
}

// secretPathTypes caches typeHasSecretPath per type.
var secretPathTypes sync.Map // reflect.Type -> bool

// typeHasSecretPath reports whether t (a struct, or a pointer, slice, array
// or map reaching one) has an exported field whose name — or json tag name —
// the secret-key rule matches, at any depth. Interface-typed fields cannot be
// known statically and count as no match.
func typeHasSecretPath(t reflect.Type) bool {
	if hit, ok := secretPathTypes.Load(t); ok {
		return hit.(bool)
	}
	hit := typeHasSecretPathVisit(t, map[reflect.Type]bool{})
	secretPathTypes.Store(t, hit)
	return hit
}

func typeHasSecretPathVisit(t reflect.Type, seen map[reflect.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return typeHasSecretPathVisit(t.Elem(), seen)
	case reflect.Struct:
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := f.Name
			if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag != "" && tag != "-" {
				name = tag
			}
			if secretAttrKey(name) || secretAttrKey(f.Name) || typeHasSecretPathVisit(f.Type, seen) {
				return true
			}
		}
		return false
	default:
		// scalars, strings and interfaces carry no static field names
		return false
	}
}

// marshalsItself reports a value whose JSON form its own code chooses
// (json.Marshaler or encoding.TextMarshaler): the structural walk goes
// through encoding/json, which would hand it that form — as likely the
// String() text as its fields — so a secret-keyed one fails closed instead.
func marshalsItself(v any) bool {
	switch v.(type) {
	case json.Marshaler, encoding.TextMarshaler:
		return true
	}
	return false
}

// unloggable is what a composite value that cannot be walked logs as: its
// type only. It fails closed — formatting the raw value with %v and scrubbing
// the text would apply only the value patterns, skipping the secret-KEY
// check the walk does, so a struct whose chan or func field (or a cycle)
// defeats json.Marshal would print its Credential/Token fields verbatim.
func unloggable(v any) string {
	return fmt.Sprintf("[unloggable value: %T]", v)
}

// keepUnderSecretKey reports whether a decoded value under the secret-named
// key may be logged as is: a boolean (one bit, never a credential), or a
// number under a count-like key (countLikeKey). Every other value under a
// secret-named key — strings, objects, arrays and numbers alike — is
// redacted whole.
func keepUnderSecretKey(key string, v any) bool {
	switch v.(type) {
	case bool:
		return true
	case json.Number, int, int64, float64:
		return countLikeKey(key)
	}
	return false
}

// countLikeSuffixes are the key endings (separators removed, lowercased) that
// name a measurement rather than a credential. It is an explicit allowlist,
// matched as a SUFFIX: "prompt_tokens" and "token_count" are counts, while
// "token", "api_token" and "password" are not — a bare substring rule would
// let "token" match "prompt_tokens" the wrong way round.
var countLikeSuffixes = []string{
	"tokens", "count", "ms", "bytes", "seconds", "secs", "len", "length",
	"size", "ttl", "limit", "total", "num",
}

// countLikeKey reports a key whose number is a count, size or duration.
func countLikeKey(key string) bool {
	k := keySeparators.Replace(strings.ToLower(key))
	for _, s := range countLikeSuffixes {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	return false
}

// errorText is err.Error() with a nil-pointer error guarded, so a typed-nil
// error logged by mistake renders instead of panicking the handler.
func errorText(err error) (s string) {
	defer func() {
		if recover() != nil {
			s = userMethodFailed(err)
		}
	}()
	if isNilPointer(err) {
		return fmt.Sprintf("%T(nil)", err)
	}
	return err.Error()
}

// stringText is s.String(), protected like errorText: a typed-nil pointer
// renders as "T(nil)" and a String() that panics as a placeholder — a
// caller's method must never take the log handler down with it.
func stringText(s fmt.Stringer) (out string) {
	defer func() {
		if recover() != nil {
			out = userMethodFailed(s)
		}
	}()
	if isNilPointer(s) {
		return fmt.Sprintf("%T(nil)", s)
	}
	return s.String()
}

// safeMarshal is json.Marshal with a panicking MarshalJSON/MarshalText (or
// any method it reaches) turned into an error, which the caller logs as
// unloggable.
func safeMarshal(v any) (raw []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			raw, err = nil, fmt.Errorf("marshal panicked")
		}
	}()
	return json.Marshal(v)
}

// userMethodFailed is the placeholder for a String() or Error() that
// panicked: the type only, never anything the method produced.
func userMethodFailed(v any) string {
	if isNilPointer(v) {
		return fmt.Sprintf("%T(nil)", v)
	}
	return fmt.Sprintf("[unloggable value: %T]", v)
}

func isNilPointer(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

var (
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// selfMarshals reports a type whose JSON form its own code chooses, through
// a value or a pointer receiver.
func selfMarshals(t reflect.Type) bool {
	if t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType) {
		return true
	}
	if t.Kind() != reflect.Pointer && t.Kind() != reflect.Interface {
		pt := reflect.PointerTo(t)
		return pt.Implements(jsonMarshalerType) || pt.Implements(textMarshalerType)
	}
	return false
}

// selfMarshalSecretTypes caches typeHasAnySecretField per type.
var selfMarshalSecretTypes sync.Map // reflect.Type -> bool

// typeHasAnySecretField is typeHasSecretPath counting unexported fields too:
// a type's own MarshalJSON can print those.
func typeHasAnySecretField(t reflect.Type) bool {
	if hit, ok := selfMarshalSecretTypes.Load(t); ok {
		return hit.(bool)
	}
	key := t
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	hit := typeHasSecretPath(t)
	if !hit && t.Kind() == reflect.Struct {
		for i := range t.NumField() {
			if secretAttrKey(t.Field(i).Name) {
				hit = true
				break
			}
		}
	}
	selfMarshalSecretTypes.Store(key, hit)
	return hit
}

// maxMarshalScan bounds hasSecretMarshaler's walk (depth and elements per
// container): a value deeper or wider than this is not logged structurally.
const maxMarshalScan = 4096

// hasSecretMarshaler reports whether v, or any value reachable from it
// through exported fields, elements, map values, pointers and interfaces,
// marshals itself and has a secret-named field. Pointer cycles are cut, and
// a walk that runs past maxMarshalScan reports true (fail closed).
func hasSecretMarshaler(v reflect.Value) bool {
	budget := maxMarshalScan
	return secretMarshalerVisit(v, 0, &budget, map[uintptr]bool{})
}

func secretMarshalerVisit(v reflect.Value, depth int, budget *int, seen map[uintptr]bool) bool {
	if !v.IsValid() {
		return false
	}
	*budget--
	if depth > 64 || *budget < 0 {
		return true
	}
	t := v.Type()
	if selfMarshals(t) && typeHasAnySecretField(t) {
		return true
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() || seen[v.Pointer()] {
			return false
		}
		seen[v.Pointer()] = true
		return secretMarshalerVisit(v.Elem(), depth+1, budget, seen)
	case reflect.Interface:
		if v.IsNil() {
			return false
		}
		return secretMarshalerVisit(v.Elem(), depth+1, budget, seen)
	case reflect.Struct:
		for i := range t.NumField() {
			if t.Field(i).IsExported() && secretMarshalerVisit(v.Field(i), depth+1, budget, seen) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return false // bytes: no fields
		}
		for i := range v.Len() {
			if secretMarshalerVisit(v.Index(i), depth+1, budget, seen) {
				return true
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if secretMarshalerVisit(iter.Value(), depth+1, budget, seen) {
				return true
			}
		}
	default:
		// scalars, strings, funcs, chans: nothing nested
	}
	return false
}

// redactedValue replaces a secret-keyed attribute's whole value; it is the
// placeholder internal/redact writes.
const redactedValue = "[REDACTED]"

// secretKeyMarkers are the attribute-key substrings that name a credential.
// The list is the one this package has always applied (#178), aligned with
// internal/redact's marker=value rule (api_key, secret, token, password,
// passwd, authorization) plus bearer/credential and cookie. The markers are
// matched against the key lowercased with its separators removed, so
// "X-Api-Key", "api_key", "api.key" and "APIKey" all name the same thing.
var secretKeyMarkers = []string{"secret", "token", "password", "passwd", "apikey", "authorization", "bearer", "credential", "cookie"}

// keySeparators are dropped from a key before it is matched.
var keySeparators = strings.NewReplacer("-", "", "_", "", ".", "", " ", "")

// secretGroupPath reports a group path with a secret-named element: slog
// hands replaceAttr only the member ("value" in slog.Group("credentials",
// "value", v), or under Logger.WithGroup("credentials")), never the group
// itself, so the group names are checked here. A string-like value under
// such a group is redacted whole, exactly as under a secret-named key.
func secretGroupPath(groups []string) bool {
	for _, g := range groups {
		if secretAttrKey(g) {
			return true
		}
	}
	return false
}

// secretAttrKey reports whether an attribute key names a likely secret.
func secretAttrKey(key string) bool {
	k := keySeparators.Replace(strings.ToLower(key))
	for _, s := range secretKeyMarkers {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}
