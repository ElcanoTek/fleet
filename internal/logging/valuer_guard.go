package logging

import (
	"context"
	"log/slog"
)

// valuerGuard closes the last self-rendering path. slog resolves a
// slog.LogValuer BEFORE ReplaceAttr runs, so replaceAttr only ever sees what
// the valuer chose to return — a credentialValuer{Credential} whose LogValue
// is the bare credential would reach the line under an innocent key, past the
// secret-field checks a Stringer, an error or a marshaler gets
// (hidesSecretFields, hasSecretMarshaler).
//
// So every handler this package builds sits behind this guard. Before the
// record (or a WithAttrs set) reaches the slog handler, each LogValuer —
// top-level, inside a group, or returned by another valuer — is checked like
// the other self-renderers: when anything its LogValue method could read
// carries a secret-named field or map key (the same owned walk), the
// attribute fails closed as [unloggable value: T]; otherwise it is resolved
// here and its result checked in turn, then handed on to ReplaceAttr as an
// ordinary value.
type valuerGuard struct{ inner slog.Handler }

func guardValuers(h slog.Handler) slog.Handler { return valuerGuard{inner: h} }

func (g valuerGuard) Enabled(ctx context.Context, l slog.Level) bool {
	return g.inner.Enabled(ctx, l)
}

func (g valuerGuard) Handle(ctx context.Context, r slog.Record) error {
	changed := false
	r.Attrs(func(a slog.Attr) bool {
		if holdsValuer(a.Value) {
			changed = true
			return false
		}
		return true
	})
	if !changed {
		return g.inner.Handle(ctx, r)
	}
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(guardAttr(a, 0))
		return true
	})
	return g.inner.Handle(ctx, nr)
}

func (g valuerGuard) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = guardAttr(a, 0)
	}
	return valuerGuard{inner: g.inner.WithAttrs(out)}
}

func (g valuerGuard) WithGroup(name string) slog.Handler {
	return valuerGuard{inner: g.inner.WithGroup(name)}
}

// holdsValuer reports a LogValuer at v or anywhere in its groups.
func holdsValuer(v slog.Value) bool {
	switch v.Kind() {
	case slog.KindLogValuer:
		return true
	case slog.KindGroup:
		for _, a := range v.Group() {
			if holdsValuer(a.Value) {
				return true
			}
		}
	default:
		// every other kind is already a plain value
	}
	return false
}

// maxValuerDepth bounds a chain of valuers returning valuers (slog itself
// stops at 100).
const maxValuerDepth = 32

// guardAttr returns a with every LogValuer in it checked and resolved, or
// replaced by the unloggable placeholder.
func guardAttr(a slog.Attr, depth int) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindLogValuer:
		valuer := a.Value.LogValuer()
		if depth >= maxValuerDepth || hidesSecretFields(valuer) {
			return slog.String(a.Key, unloggable(valuer))
		}
		resolved := resolveOne(valuer)
		return guardAttr(slog.Attr{Key: a.Key, Value: resolved}, depth+1)
	case slog.KindGroup:
		members := a.Value.Group()
		out := make([]slog.Attr, len(members))
		for i, m := range members {
			out[i] = guardAttr(m, depth)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	default:
		return a
	}
}
