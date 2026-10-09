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

// canaryRecordArgs are the call arguments that only say WHICH records a call
// touches, or how it reports or transports its work, so a one-record canary
// and the multi-record batch it proves legitimately differ in them: the record
// addressing (deal_ids, every callRecordIDKeys key, IX deal_references), the
// value set (values_sha256, and an inline values list or a digest-bound
// values_file, all bound separately by canaryShape's digest), the output verbosity,
// and a per-record optimistic-concurrency etag. EVERY other argument is part
// of the operation's shape. The shape is an exclusion list, not an allowlist
// of operation words: a connector's own mode argument (is_excluded, a seat
// such as member_id or logged_in_owner_id, dry_run, a per-dimension
// countries_include list) must never let a canary of one operation unlock a
// batch of another (Codex on #1712; Cutlass enumerates mode words instead).
// The engine cannot know every bundle's vocabulary, so an unknown argument
// fails closed: it needs its own canary.
var canaryRecordArgs = func() map[string]bool {
	m := map[string]bool{
		"deal_ids":        true,
		"deal_references": true,
		"values_sha256":   true,
		"verbose":         true,
		"etag":            true,
	}
	for _, k := range callRecordIDKeys {
		m[k] = true
	}
	return m
}()

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

// canaryShape returns the call's values digest plus a canonical encoding of
// every argument that is not record addressing or value transport
// (canaryRecordArgs) — the canary binding for a batch call. With no
// values_sha256, inline values bind by a canonical digest of the value set
// itself, so a canary proven on ["safe.example"] cannot clear a batch carrying
// different inline values (cutlass#1081 pass 2).
func canaryShape(rawInput string) string {
	digest := valuesDigestArg(rawInput)
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return digest
	}
	fileDigest := digest != ""
	inlineDigest := false
	if digest == "" {
		if vals, ok := args["values"].([]any); ok && len(vals) > 0 {
			norm := make([]string, 0, len(vals))
			for _, v := range vals {
				norm = append(norm, strings.ToLower(strings.TrimSpace(fmt.Sprint(v))))
			}
			sort.Strings(norm)
			sum := sha256.Sum256([]byte(strings.Join(norm, "\n")))
			digest = "inline:" + hex.EncodeToString(sum[:])
			inlineDigest = true
		}
	}
	shape := make(map[string]any, len(args))
	for k, v := range args {
		if v == nil || canaryRecordArgs[k] {
			continue
		}
		// The value set is bound by the digest when there is one: a
		// values_file by its values_sha256, an inline values list by its
		// canonical digest. Otherwise the argument itself is the binding.
		if (k == "values_file" && fileDigest) || (k == "values" && inlineDigest) {
			continue
		}
		shape[k] = v
	}
	var b strings.Builder
	b.WriteString(digest)
	if len(shape) > 0 {
		// json.Marshal writes map keys sorted, so the encoding is canonical.
		// Keys and values keep their case: a bundle argument may be
		// case-sensitive ("TenantA" vs "tenanta"), and folding it would let
		// one operation's canary clear another's batch (Codex on #1712).
		if enc, err := json.Marshal(shape); err == nil {
			b.WriteString("\x00")
			b.Write(enc)
		}
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
