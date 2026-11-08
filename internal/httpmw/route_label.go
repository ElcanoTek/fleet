package httpmw

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
)

// RouteLabel names r's route for a log line or a panic event without its
// variable parts: a path segment can be a live capability (/shared/<token>,
// an invite or reset token) and a query can carry an OAuth code, a state or
// ?token=. It is the route pattern the router matched — chi's RoutePattern,
// else net/http's Request.Pattern — and, when no router has matched (a panic
// before routing), SanitizePath of the path. Never the query.
func RouteLabel(r *http.Request) string {
	if r == nil {
		return ""
	}
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if p := rctx.RoutePattern(); p != "" && p != "/*" {
			return p
		}
	}
	if r.Pattern != "" {
		// A ServeMux pattern may name the method and host ("GET
		// example.com/x/{id}"); the path part is what identifies the route.
		p := r.Pattern
		if i := strings.IndexByte(p, ' '); i >= 0 {
			p = strings.TrimSpace(p[i+1:])
		}
		if i := strings.IndexByte(p, '/'); i > 0 {
			p = p[i:]
		}
		return p
	}
	if r.URL == nil {
		return ""
	}
	return SanitizePath(r.URL.Path)
}

// staticSegment is a path segment kept by SanitizePath: a short lower-case
// word (letters and hyphens), the shape of a route's fixed part. Anything
// else — digits, mixed case, base64, a long run — may be an id or a token.
var staticSegment = regexp.MustCompile(`^[a-z][a-z-]{0,23}$`)

// SanitizePath keeps the leading fixed segments of path (staticSegment) and
// replaces the rest with "{…}" from the first segment that could be an id or
// a token: "/shared/AbC123…" → "/shared/{…}". Only the first segment is ever
// kept as is when it is static, so a token made only of lower-case letters
// in a later position is not kept either. The query is never part of it.
func SanitizePath(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		return "/"
	}
	if !staticSegment.MatchString(segs[0]) {
		return "/{…}"
	}
	if len(segs) == 1 {
		return "/" + segs[0]
	}
	return "/" + segs[0] + "/{…}"
}
