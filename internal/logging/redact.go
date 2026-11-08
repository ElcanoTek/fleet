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
	redactorOnce.Do(func() {
		logRedactor = redact.NewRedactor(nil)
		logRedactor.SetReservedKeys(ReservedKeys...)
	})
	return logRedactor
}

// ReservedKeys are the field names this package's handlers write themselves:
// slog's built-ins, the correlation keys, and the keys the package adds to
// re-logged or bridged records. A registered secret equal to one is still
// registered and scrubbed everywhere but in key position, where it is the
// record's own field name (the line backstop would otherwise rewrite that key
// in every record). A secret-carrying key a call site supplies is still
// redacted like any other.
var ReservedKeys = []string{
	slog.TimeKey, slog.LevelKey, slog.MessageKey, slog.SourceKey,
	KeyConvID, KeyTurnID, KeyTaskID, KeyRunID, KeyUserID, KeyRequestID, KeyTraceID, KeySpanID,
	StdlibLogKey, childKey, lineKey, truncatedKey,
}

// reservedKeySet is ReservedKeys as a set.
var reservedKeySet = func() map[string]bool {
	m := make(map[string]bool, len(ReservedKeys))
	for _, k := range ReservedKeys {
		m[k] = true
	}
	return m
}()

// scrubKey is Redact for an attribute key: a reserved key (ReservedKeys) is
// the record's own field name and is kept as is, even if a registered
// secret happens to equal it; its VALUE is scrubbed as usual.
func scrubKey(k string) string {
	if reservedKeySet[k] {
		return k
	}
	return Redact(k)
}

// RegisterSecretLiterals is THE registration point for a secret literal in
// this process: it adds the VALUES to the log redactor and hands them to every
// other redactor subscribed with OnSecretLiterals — the tool-output redactor
// (agentcore), the broker's (mcpbroker) — so a value scrubbed from the log is
// scrubbed from tool results reaching the model too, and vice versa, whichever
// package learned it (#2074 review). Semantics are those of
// redact.Redactor.RegisterSecrets: an empty scope registers permanently, a
// scope with rotated=true replaces that scope's previous generation after the
// redactor's grace window. The values are kept only as scrub targets and are
// never emitted.
func RegisterSecretLiterals(scope string, rotated bool, values ...string) {
	redactor().RegisterSecrets(scope, rotated, values...)
	for _, fn := range secretSubscribers() {
		fn(scope, rotated, values)
	}
}

// RegisterEnvLiterals registers the values of secret-named variables in
// environ (os.Environ form) — redact.IsSecretEnvName, the name rule every
// redactor seeds itself with — through RegisterSecretLiterals, so the
// subscribed redactors learn them too.
func RegisterEnvLiterals(environ []string) {
	var values []string
	for _, kv := range environ {
		name, val, ok := strings.Cut(kv, "=")
		if ok && name != "" && redact.IsSecretEnvName(name) {
			values = append(values, val)
		}
	}
	if len(values) > 0 {
		RegisterSecretLiterals("", false, values...)
	}
}

var (
	subscribersMu sync.Mutex
	subscribers   []func(scope string, rotated bool, values []string)
)

// OnSecretLiterals subscribes another redactor to every RegisterSecretLiterals
// (and RegisterEnvLiterals) call in this process, from now on. fn must not
// call RegisterSecretLiterals itself, and must not block. Call it from the
// subscribing package's init, before anything registers.
func OnSecretLiterals(fn func(scope string, rotated bool, values []string)) {
	subscribersMu.Lock()
	defer subscribersMu.Unlock()
	subscribers = append(subscribers, fn)
}

func secretSubscribers() []func(string, bool, []string) {
	subscribersMu.Lock()
	defer subscribersMu.Unlock()
	return append([]func(string, bool, []string){}, subscribers...)
}

// seedEnvLiterals registers this process's own secret-named env values
// (Configure calls it first thing). In the MCP broker that is the connector
// credentials; in fleet serve, whatever the env file set — before the
// broker boot path hands connector values over and unsets them. It also
// registers every setting the configuration registry marks Secret, whatever
// its name (registerConfigSecrets).
func seedEnvLiterals() {
	RegisterEnvLiterals(os.Environ())
	registerConfigSecrets()
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
	if builtIn, out := builtInAttr(groups, a); builtIn {
		return out
	}
	// A key can carry a secret as well as a value (a token used as a field
	// name); the secret-key rule below reads the key as written.
	name := a.Key
	a.Key = scrubKey(a.Key)
	v := a.Value
	switch v.Kind() {
	case slog.KindString:
		if secretAttrKey(name) || secretGroupPath(groups) {
			return slog.String(a.Key, redactedValue)
		}
		return slog.String(a.Key, Redact(v.String()))
	case slog.KindAny:
		if secretAttrKey(name) || secretGroupPath(groups) {
			return slog.String(a.Key, redactedValue)
		}
		switch x := v.Any().(type) {
		case error:
			if !hidesSecretFields(x) {
				if wrapsSecretError(x) {
					return slog.String(a.Key, unloggable(x))
				}
				return slog.String(a.Key, Redact(errorText(x)))
			}
		case fmt.Stringer:
			if !hidesSecretFields(x) {
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
		if (secretAttrKey(name) || secretGroupPath(groups)) && !countLikeKey(name) {
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
		if !hidesSecretFields(x) {
			if wrapsSecretError(x) {
				return unloggable(x)
			}
			return Redact(errorText(x))
		}
		// An error type carrying a secret-named field: its Error() could
		// print it with no key left to redact it by. Walked by structure
		// below (exported fields, key-checked), or fails closed.
		if marshalsItself(x) {
			return unloggable(x)
		}
	case fmt.Stringer:
		if !hidesSecretFields(x) {
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

// wrapsSecretError reports an error whose chain (errors.Unwrap, and
// Unwrap() []error for errors.Join) holds an error type with a secret-named
// field: fmt.Errorf("…: %w", err) has already formatted that error's Error()
// into its own text, so the wrapper's message is as unsafe as the wrapped
// value and the whole error fails closed. The walk is bounded; past the
// bound it fails closed too.
func wrapsSecretError(err error) bool {
	budget := 64
	var visit func(error) bool
	visit = func(e error) bool {
		if e == nil {
			return false
		}
		budget--
		if budget < 0 {
			return true
		}
		if hidesSecretFields(e) {
			return true
		}
		for _, inner := range wrappedErrors(e) {
			if visit(inner) {
				return true
			}
		}
		return false
	}
	for _, inner := range wrappedErrors(err) {
		if visit(inner) {
			return true
		}
	}
	return false
}

// wrappedErrors is the errors e directly wraps — Unwrap() error, or Unwrap()
// []error for errors.Join — with a panicking or typed-nil Unwrap tolerated.
func wrappedErrors(e error) (out []error) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	if isNilPointer(e) {
		return nil
	}
	//nolint:errorlint // walking the chain one link at a time is the point: errors.As would skip the links this checks
	switch u := e.(type) {
	case interface{ Unwrap() error }:
		if inner := u.Unwrap(); inner != nil {
			return []error{inner}
		}
	case interface{ Unwrap() []error }:
		return u.Unwrap()
	}
	return nil
}

// builtInAttr handles the handler's own fields: time, level, source and msg
// at the top level, and only with the value kind the handler itself emits
// (a time, a slog.Level, a *slog.Source, a string message). A call site's
// attribute that merely reuses one of those names — "level": "<secret>" in a
// group or as a string — is not a built-in and is redacted like any other.
func builtInAttr(groups []string, a slog.Attr) (bool, slog.Attr) {
	if len(groups) != 0 {
		return false, a
	}
	v := a.Value
	switch a.Key {
	case slog.TimeKey:
		return v.Kind() == slog.KindTime, a
	case slog.LevelKey:
		if v.Kind() == slog.KindAny {
			if _, ok := v.Any().(slog.Level); ok {
				return true, a
			}
		}
	case slog.SourceKey:
		if v.Kind() == slog.KindAny {
			if _, ok := v.Any().(*slog.Source); ok {
				return true, a
			}
		}
	case slog.MessageKey:
		if v.Kind() == slog.KindString {
			return true, slog.String(a.Key, Redact(v.String()))
		}
	}
	return false, a
}

// hidesSecretFields reports a fmt.Stringer or error that must not be logged
// by its own String()/Error(): anything its method can reach (walkReachable:
// fields exported or not, elements, map keys AND values, pointers, the
// dynamic value of an interface) carries a secret-named field or map key —
// a struct with a Credential field, a map keyed "api_key", a map whose KEY
// type has a Token field — so the method could print that value with no key
// left to redact it by. Such a value is walked structurally like any
// composite, or fails closed. Every other Stringer or error (time.Time, a
// UUID, net.IP, *url.Error: values whose text is their meaning) keeps its
// method, whose text is still scrubbed by pattern and literal.
func hidesSecretFields(v any) bool {
	// The method owns everything it can reach.
	return walkReachable(reflect.ValueOf(v), true, nil)
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

// resolveOne calls v.LogValue once, a panicking method turning into the
// unloggable placeholder (as slog itself would, but never with the panic
// value).
func resolveOne(v slog.LogValuer) (out slog.Value) {
	defer func() {
		if recover() != nil {
			out = slog.StringValue(userMethodFailed(v))
		}
	}()
	if isNilPointer(v) {
		return slog.StringValue(userMethodFailed(v))
	}
	return v.LogValue()
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

// typeHasAnySecretField reports a secret-named field — by name or json tag —
// on EVERY field, exported or not, at any depth (through pointers, slices, arrays, maps and nested
// structs): a type's own MarshalJSON, MarshalText, String() or Error() can
// read and print an unexported one, nested or not.
func typeHasAnySecretField(t reflect.Type) bool {
	if hit, ok := selfMarshalSecretTypes.Load(t); ok {
		return hit.(bool)
	}
	hit := anySecretFieldVisit(t, map[reflect.Type]bool{})
	selfMarshalSecretTypes.Store(t, hit)
	return hit
}

func anySecretFieldVisit(t reflect.Type, seen map[reflect.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		if t.Kind() == reflect.Map && anySecretFieldVisit(t.Key(), seen) {
			return true
		}
		return anySecretFieldVisit(t.Elem(), seen)
	case reflect.Struct:
		for i := range t.NumField() {
			f := t.Field(i)
			name := f.Name
			if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag != "" && tag != "-" {
				name = tag
			}
			if secretAttrKey(name) || secretAttrKey(f.Name) || anySecretFieldVisit(f.Type, seen) {
				return true
			}
		}
		return false
	default:
		// scalars, strings and interfaces carry no static field names
		return false
	}
}

// maxMarshalScan bounds hasSecretMarshaler's walk (depth and elements per
// container): a value deeper or wider than this is not logged structurally.
const maxMarshalScan = 4096

// hasSecretMarshaler reports whether v holds a value that marshals itself
// (json.Marshaler, encoding.TextMarshaler, at any depth, a map key included)
// over a secret: a secret-named field or map key anywhere its own code can
// reach — in its static type, or at run time below it, through an interface
// (an outer MarshalJSON with Data any holding a struct{Credential string}),
// a map, a pointer, an element. That code chooses the JSON form, so the key
// rule never sees those fields: it fails closed instead.
func hasSecretMarshaler(v reflect.Value) bool {
	return walkReachable(v, false, selfMarshals)
}

// walkReachable is the ONE runtime walk the Stringer, error and marshaler
// checks share. It visits v and every value reachable from it — struct
// fields (exported or not), slice and array elements, map keys and values,
// pointer targets and interfaces' dynamic values — and reports whether a
// value OWNED by a custom renderer carries a secret (secretBearing). A value
// is owned when owned is true (the whole value is a Stringer or error that
// prints itself) or when it sits at or below a value for which renders
// reports true (a self-marshalling type); ownership propagates down through
// every interface, map and pointer. Pointer cycles are cut, and a walk that
// runs past maxMarshalScan (or 64 levels) reports true: fail closed.
func walkReachable(v reflect.Value, owned bool, renders func(reflect.Type) bool) bool {
	budget := maxMarshalScan
	w := reachableWalk{renders: renders, budget: &budget, seen: map[uintptr]bool{}}
	return w.visit(v, false, owned, 0)
}

type reachableWalk struct {
	renders func(reflect.Type) bool
	budget  *int
	// seen records each pointer visited, with whether it was visited owned:
	// a pointer reached again under an owner is walked again.
	seen map[uintptr]bool
}

func (w reachableWalk) visit(v reflect.Value, mapKey, owned bool, depth int) bool {
	if !v.IsValid() {
		return false
	}
	*w.budget--
	if depth > 64 || *w.budget < 0 {
		return true
	}
	t := v.Type()
	if !owned && w.renders != nil && w.renders(t) {
		owned = true
	}
	if owned {
		if secretBearing(v, mapKey) {
			return true
		}
		// The static check above already covers a type whose contents hold
		// no map key or interface value to read at run time.
		if !typeHoldsDynamic(t) {
			return false
		}
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return false
		}
		if wasOwned, ok := w.seen[v.Pointer()]; ok && (wasOwned || !owned) {
			return false
		}
		w.seen[v.Pointer()] = owned
		return w.visit(v.Elem(), false, owned, depth+1)
	case reflect.Interface:
		if v.IsNil() {
			return false
		}
		return w.visit(v.Elem(), mapKey, owned, depth+1)
	case reflect.Struct:
		for i := range t.NumField() {
			if w.visit(v.Field(i), false, owned, depth+1) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return false // bytes: no fields
		}
		for i := range v.Len() {
			if w.visit(v.Index(i), false, owned, depth+1) {
				return true
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if w.visit(iter.Key(), true, owned, depth+1) || w.visit(iter.Value(), false, owned, depth+1) {
				return true
			}
		}
	default:
		// scalars, strings, funcs, chans: nothing nested
	}
	return false
}

// secretBearing reports a value that carries a secret by NAME: its type has
// a secret-named field, or a map key/elem type with one, at any depth
// (typeHasAnySecretField), or it is a runtime map key the secret-key rule
// names ("api_key" in a map[string]any).
func secretBearing(v reflect.Value, mapKey bool) bool {
	switch v.Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array, reflect.Pointer:
		if typeHasAnySecretField(v.Type()) {
			return true
		}
	case reflect.Interface:
		if mapKey && !v.IsNil() {
			v = v.Elem()
		}
	default:
		// scalars, strings, funcs, chans: the key check below
	}
	return mapKey && v.Kind() == reflect.String && secretAttrKey(v.String())
}

// dynamicTypes caches typeHoldsDynamic per type.
var dynamicTypes sync.Map // reflect.Type -> bool

// typeHoldsDynamic reports a type whose values can hold something only a
// runtime walk sees: a map (its keys) or an interface (its dynamic value),
// at any depth.
func typeHoldsDynamic(t reflect.Type) bool {
	if hit, ok := dynamicTypes.Load(t); ok {
		return hit.(bool)
	}
	hit := holdsDynamicVisit(t, map[reflect.Type]bool{})
	dynamicTypes.Store(t, hit)
	return hit
}

func holdsDynamicVisit(t reflect.Type, seen map[reflect.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Map, reflect.Interface:
		return true
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return holdsDynamicVisit(t.Elem(), seen)
	case reflect.Struct:
		for i := range t.NumField() {
			if holdsDynamicVisit(t.Field(i).Type, seen) {
				return true
			}
		}
	default:
		// scalars, strings, funcs, chans
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
