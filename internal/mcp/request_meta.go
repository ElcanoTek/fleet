package mcp

import (
	"context"
	"strings"
)

// Request metadata fleet adds to a tools/call (docs/APPROVAL-NARROWING.md).
//
// The MCP spec lets a request carry `params._meta`, a map of keys outside the
// tool's arguments. Fleet uses it to tell a server facts about an approved
// call that are not arguments, such as "the person left some of the staged
// deal_ids out" (`fleet/narrowed`). A caller attaches the entries with
// WithRequestMeta; Server.callTool merges them into the request's `_meta`
// next to the progress token. Like WithProgress, the entries are a context
// value and do not cross a process boundary: the out-of-process broker sends
// them on its own wire and re-attaches them on the side that reaches
// Server.callTool.

// RequestMetaPrefix is the only key prefix WithRequestMeta accepts. Every
// fleet entry lives under it, so an entry can never replace a key the spec
// or the transport owns (`progressToken`), and a server reads one namespace.
const RequestMetaPrefix = "fleet/"

type requestMetaKey struct{}

// WithRequestMeta attaches `_meta` entries to the tools/call made under ctx.
// Keys outside RequestMetaPrefix are dropped. An empty or all-dropped map
// attaches nothing, so the request is the one it always was.
func WithRequestMeta(ctx context.Context, meta map[string]any) context.Context {
	kept := FleetRequestMeta(meta)
	if len(kept) == 0 {
		return ctx
	}
	if prev := RequestMeta(ctx); len(prev) > 0 {
		for k, v := range prev {
			if _, ok := kept[k]; !ok {
				kept[k] = v
			}
		}
	}
	return context.WithValue(ctx, requestMetaKey{}, kept)
}

// RequestMeta returns the entries WithRequestMeta attached to ctx, or nil.
// The out-of-process broker reads it to forward the entries.
func RequestMeta(ctx context.Context) map[string]any {
	m, _ := ctx.Value(requestMetaKey{}).(map[string]any)
	return m
}

// FleetRequestMeta is meta restricted to RequestMetaPrefix keys (a copy), or
// nil when none remain. The broker child applies it to what the parent sent,
// so a parent cannot set any other `_meta` key through it.
func FleetRequestMeta(meta map[string]any) map[string]any {
	var out map[string]any
	for k, v := range meta {
		if !strings.HasPrefix(k, RequestMetaPrefix) || len(k) == len(RequestMetaPrefix) {
			continue
		}
		if out == nil {
			out = make(map[string]any, len(meta))
		}
		out[k] = v
	}
	return out
}
