package agentcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Single-deal canary before a multi-deal batch (ported from cutlass
// internal/agent/orchestration.go canaryKey / canaryShape / creditBatchCanary,
// cutlass#740.7, 3d1546bf, 9c532cee).
//
// The update protocol's canary_gate mandates proving each (SSP, account,
// operation) on ONE record — and inspecting its read-back — before touching
// the rest. The Go-enforceable core: a deal_ids batch of MORE than one record
// is refused until a single-record application of the same critical action
// (same server/variant prefix and alias class, critical_tool_aliases #1604)
// with the same value set and operation shape has succeeded this run. The
// diff-inspection half stays prompt-level. Per-run scope: a resumed run must
// re-prove its canary (the merge tools are idempotent RMW, so re-applying the
// canary record is a no-op).

// canaryOpShapeArgs are the call arguments that change WHAT a merge does (the
// operation's direction / side / mode / dimension), as opposed to which
// records or which values it touches. They join the canary key so a batch
// whose operation differs from its proven canary — e.g. a canary run as
// list_type=allowlist, merge_mode=add then a batch as blocklist/remove with
// the same values_sha256 — needs its own canary (cutlass#1081). Cross-SSP:
// absent keys contribute nothing.
var canaryOpShapeArgs = []string{
	"merge_mode", "mode", "operation", "operator", "action", "list_type", "direction", "exclude",
	"include", "side", "segment_type", "level", "dimension", "kind", "group_index", "add_group",
	"remove_group", "switch_geo_type", "media_lists",
}

// canaryKey is the canarySucceeded key: the call's critical action
// (CriticalActionKey — server/variant prefix + alias class, so alias twins on
// one server share a canary and two servers or seats never do) plus its
// canaryShape. NUL never appears in a tool name or an alias class, and the
// shape comes last, so distinct (action, shape) pairs never collide.
func canaryKey(toolName, rawInput string) string {
	action, ok := CriticalActionKey(toolName)
	if !ok {
		action = CriticalAction{Class: toolName}
	}
	return action.Prefix + "\x00" + action.Class + "\x00" + canaryShape(rawInput)
}

// canaryShape returns the call's values digest plus a canonical encoding of its
// operation-shape arguments — the canary binding for a batch call. With no
// values_sha256, inline values bind by a canonical digest of the value set
// itself, so a canary proven on ["safe.example"] cannot clear a batch carrying
// different inline values (cutlass#1081 pass 2).
func canaryShape(rawInput string) string {
	digest := valuesDigestArg(rawInput)
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return digest
	}
	if digest == "" {
		if vals, ok := args["values"].([]any); ok && len(vals) > 0 {
			norm := make([]string, 0, len(vals))
			for _, v := range vals {
				norm = append(norm, strings.ToLower(strings.TrimSpace(fmt.Sprint(v))))
			}
			sort.Strings(norm)
			sum := sha256.Sum256([]byte(strings.Join(norm, "\n")))
			digest = "inline:" + hex.EncodeToString(sum[:])
		}
	}
	var b strings.Builder
	b.WriteString(digest)
	for _, k := range canaryOpShapeArgs {
		v, ok := args[k]
		if !ok || v == nil {
			continue
		}
		enc, err := json.Marshal(v)
		if err != nil {
			continue
		}
		b.WriteString("\x00")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(strings.ToLower(string(enc)))
	}
	return b.String()
}

// creditCanary records a proven single-record application of this call's
// action and shape. Callers must hold o.mu.
func (o *orchestrationState) creditCanary(toolName, rawInput string) {
	if o.canarySucceeded == nil {
		o.canarySucceeded = make(map[string]bool)
	}
	o.canarySucceeded[canaryKey(toolName, rawInput)] = true
}

// creditSingleRecordCanary grants canary credit for a successful single-call
// critical tool (no per-record results[]) that named one record: the call's
// OWN success, with no results[] to drift, proves this action and shape.
// Callers must hold o.mu.
func (o *orchestrationState) creditSingleRecordCanary(toolName, rawInput string) {
	if callDealID(rawInput) != "" {
		o.creditCanary(toolName, rawInput)
	}
}

// creditBatchCanary grants canary credit for a per-record results[] call that
// TARGETED exactly one record — a deal_ids batch of one, or a single-record
// call — only when that requested record itself reported success. Matching the
// requested id (not "any success in results[]") keeps a drifted results[] whose
// success row names a DIFFERENT record from crediting a canary whose own record
// failed. Callers must hold o.mu.
func (o *orchestrationState) creditBatchCanary(toolName, rawInput string, outcomes []dealOutcome) {
	ids, isBatch := batchDealIDs(rawInput)
	var canaryDeal string
	switch {
	case isBatch && len(ids) == 1:
		canaryDeal = strings.TrimSpace(ids[0])
	case !isBatch:
		canaryDeal = callDealID(rawInput)
	}
	if canaryDeal == "" {
		return
	}
	for _, oc := range outcomes {
		if oc.success && strings.TrimSpace(oc.dealID) == canaryDeal {
			o.creditCanary(toolName, rawInput)
			return
		}
	}
}

// checkBatchCanary refuses a deal_ids batch of more than one record until its
// canary has succeeded (see the file comment). Callers must hold o.mu.
func (o *orchestrationState) checkBatchCanary(toolName, rawInput string, dealIDs []string) (bool, string) {
	if len(dealIDs) <= 1 || o.canarySucceeded[canaryKey(toolName, rawInput)] {
		return false, ""
	}
	return true, fmt.Sprintf("BLOCKED: batch '%s' targets %d records but no single-record CANARY of this "+
		"tool with this value set and operation shape has succeeded in this run. The update protocol's "+
		"canary_gate requires proving each (SSP, account, operation) on ONE record first: issue this same "+
		"call with deal_ids containing exactly one approved record (same values_sha256 and the same "+
		"operation arguments) — a one-record batch with the matching digest IS the canary, no re-audit "+
		"needed — inspect the returned read-back/diff, and only then apply the remaining records.",
		toolName, len(dealIDs))
}
