// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

// Package a2a translates between fleet's task model and the A2A (Agent2Agent)
// protocol's wire types (#1279). It is protocol translation ONLY: no HTTP
// handlers, no storage, no policy — the dispatcher in internal/sched/handlers
// owns auth and calls fleet's existing governed seams, and this package owns
// what those results look like on the A2A wire.
//
// The wire types come from the official Go SDK's pure-types packages
// (github.com/a2aproject/a2a-go/v2/a2a + errordetails, stdlib+uuid imports
// only), which carry the marshalling for the three shapes hand-rolling gets
// wrong: the oneof Part, the oneof StreamResponse, and the SCREAMING_SNAKE
// enum spellings. The SDK's a2asrv server framework is deliberately NOT used —
// its AgentExecutor is a competing execution loop, and fleet has exactly one
// governed loop (ADR-0001).
//
// The normative source for every mapping here is specification/a2a.proto at
// the pinned release (SpecVersion): the prose spec's §4 tables are
// macro-generated and its migration doc has confirmed factual bugs, so the
// proto is the only authority worth citing. See docs/A2A.md.
package a2a

import (
	"encoding/json"
	"mime"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	wire "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// SpecVersion is the exact A2A specification release this implementation is
// written against. Bumping it is a deliberate PR that re-verifies every
// mapping in this package against the new specification/a2a.proto — never
// drift. (The protocol version declared on the wire is wire.Version, the
// Major.Minor form the spec requires in AgentInterface.protocolVersion.)
const SpecVersion = "v1.0.1"

// TaskStateFor maps a fleet task status to its A2A TaskState. The boolean
// reports whether the status was recognized; the exhaustiveness test ranges
// models.AllTaskStatuses so a new fleet status cannot silently map to nothing
// (it would return TASK_STATE_UNSPECIFIED, which this package never emits for
// a known status).
//
// The two deliberate compressions, both documented in docs/A2A.md:
//   - paused_awaiting_wake → WORKING. A2A has no "sleeping" state, and the
//     interrupted pair (INPUT_REQUIRED / AUTH_REQUIRED) means "the CALLER must
//     act". A self-wake park needs nothing from the caller, so from the
//     caller's seat the task is still in progress.
//   - dead_lettered → FAILED. Retry exhaustion is an implementation detail;
//     the caller-visible fact is that the task failed terminally.
func TaskStateFor(status models.TaskStatus) (wire.TaskState, bool) {
	switch status {
	case models.TaskStatusPending, models.TaskStatusScheduled, models.TaskStatusLeased:
		return wire.TaskStateSubmitted, true
	case models.TaskStatusRunning, models.TaskStatusPausedAwaitingWake:
		return wire.TaskStateWorking, true
	case models.TaskStatusPausedAwaitingInput:
		return wire.TaskStateInputRequired, true
	case models.TaskStatusSuccess:
		return wire.TaskStateCompleted, true
	case models.TaskStatusError, models.TaskStatusDeadLettered:
		return wire.TaskStateFailed, true
	case models.TaskStatusCancelled:
		return wire.TaskStateCanceled, true
	default:
		// models.TaskLifecycleStart (the lifecycle table's pre-creation
		// pseudo-status) and anything genuinely new land here; the
		// exhaustiveness test over AllTaskStatuses is what keeps "new" loud.
		return wire.TaskStateUnspecified, false
	}
}

// FleetStatusesFor is the reverse mapping, for the ListTasks status filter:
// the set of fleet statuses that report as the given A2A state. An empty
// slice means no fleet status ever reports that state (REJECTED — refusals
// happen at admission, before a row exists — and AUTH_REQUIRED, which fleet
// has no equivalent of), so a filter on it legitimately matches nothing.
func FleetStatusesFor(state wire.TaskState) ([]string, bool) {
	switch state {
	case wire.TaskStateSubmitted:
		return []string{string(models.TaskStatusPending), string(models.TaskStatusScheduled), string(models.TaskStatusLeased)}, true
	case wire.TaskStateWorking:
		return []string{string(models.TaskStatusRunning), string(models.TaskStatusPausedAwaitingWake)}, true
	case wire.TaskStateInputRequired:
		return []string{string(models.TaskStatusPausedAwaitingInput)}, true
	case wire.TaskStateCompleted:
		return []string{string(models.TaskStatusSuccess)}, true
	case wire.TaskStateFailed:
		return []string{string(models.TaskStatusError), string(models.TaskStatusDeadLettered)}, true
	case wire.TaskStateCanceled:
		return []string{string(models.TaskStatusCancelled)}, true
	case wire.TaskStateRejected, wire.TaskStateAuthRequired:
		return nil, true
	default:
		// TaskStateUnspecified and unknown strings: refused, so the handler
		// answers InvalidParams instead of silently matching nothing.
		return nil, false
	}
}

// BuildTask renders a fleet task as an A2A Task.
//
// contextId is the task id: fleet tasks are hermetically isolated (no
// cross-task context to group), so each task is its own context in v1.
// History is deliberately empty — fleet run transcripts are tool-call logs,
// not A2A Messages, and pretending otherwise would be dishonest (docs/A2A.md
// "Honest scope"). publicBaseURL prefixes artifact file URLs; empty yields
// server-relative URLs. includeArtifacts=false (the ListTasks default per the
// spec) skips artifact construction entirely.
func BuildTask(t *models.Task, publicBaseURL string, includeArtifacts bool) *wire.Task {
	state, _ := TaskStateFor(t.Status)
	out := &wire.Task{
		ID:        wire.TaskID(t.ID.String()),
		ContextID: t.ID.String(),
		Status: wire.TaskStatus{
			State:     state,
			Message:   statusMessage(t),
			Timestamp: statusTimestamp(t),
		},
	}
	if includeArtifacts {
		out.Artifacts = BuildArtifacts(t, publicBaseURL)
	}
	return out
}

// statusTimestamp picks the most recent recorded lifecycle instant: terminal
// completion, else run start, else creation. Spec §5.6.1 wants ISO 8601 UTC;
// the SDK marshals time.Time via RFC 3339, so normalize to UTC here.
func statusTimestamp(t *models.Task) *time.Time {
	var ts time.Time
	switch {
	case t.CompletedAt != nil:
		ts = t.CompletedAt.UTC()
	case t.StartedAt != nil:
		ts = t.StartedAt.UTC()
	default:
		ts = t.CreatedAt.UTC()
	}
	return &ts
}

// statusMessage carries the human-readable status detail the spec allows on
// TaskStatus: the pending question for INPUT_REQUIRED (the caller needs it to
// answer), the error text for FAILED, and the who-stopped-it attribution for
// CANCELED (fleet records it in Result on cancel). A COMPLETED task's result
// travels as an artifact, not a status message.
func statusMessage(t *models.Task) *wire.Message {
	var text string
	switch t.Status {
	case models.TaskStatusPausedAwaitingInput:
		text = t.PendingQuestion
	case models.TaskStatusError, models.TaskStatusDeadLettered:
		if t.ErrorMessage != nil {
			text = *t.ErrorMessage
		}
	case models.TaskStatusCancelled:
		if t.Result != nil {
			text = *t.Result
		}
	default:
		// Every other status has no status-message payload to carry.
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return agentMessage(t.ID.String(), "status", text)
}

// agentMessage builds a single-text-part agent message with a deterministic
// id derived from the task and slot — stable across repeated GetTask calls
// (and golden tests) where a random id would churn.
func agentMessage(taskID, slot, text string) *wire.Message {
	return &wire.Message{
		ID:        "fleet-" + taskID + "-" + slot,
		TaskID:    wire.TaskID(taskID),
		ContextID: taskID,
		Role:      wire.MessageRoleAgent,
		Parts:     wire.ContentParts{wire.NewTextPart(text)},
	}
}

// BuildArtifacts renders a terminal task's outputs as A2A artifacts, primary
// deliverables first:
//
//   - one artifact per PUBLISHED workspace file (the run's explicit
//     deliverables), as a URL part pointing at the AUTHENTICATED workspace
//     endpoint (same X-API-Key the caller already holds; the URL is not a
//     bearer capability);
//   - "output": the schema-validated structured result, as a data part;
//   - "result": the free-form final answer (success only — on cancel the
//     Result column holds the stop attribution, which statusMessage carries),
//     as a text part, LAST: it is fleet's synthesized supplement, and the
//     runner substitutes a boilerplate value when the run ends without final
//     text, so it must never displace an explicitly published deliverable
//     from the front of the list.
//
// Artifact ids are stable, not random: repeated reads of the same task must
// describe the same artifacts.
func BuildArtifacts(t *models.Task, publicBaseURL string) []*wire.Artifact {
	var out []*wire.Artifact
	var files []models.TaskArtifact
	if len(t.Artifacts) > 0 {
		// Best-effort: an unparsable manifest yields no file artifacts rather
		// than failing the whole task render.
		_ = json.Unmarshal(t.Artifacts, &files)
	}
	for _, f := range files {
		if f.Path == "" {
			continue
		}
		out = append(out, &wire.Artifact{
			ID:          wire.ArtifactID("file:" + f.Path),
			Name:        f.Name,
			Description: f.Description,
			Parts: wire.ContentParts{&wire.Part{
				Content:   wire.URL(workspaceFileURL(publicBaseURL, t.ID.String(), f.Path)),
				Filename:  f.Name,
				MediaType: mediaTypeFor(f.Name),
			}},
		})
	}
	if len(t.OutputJSON) > 0 {
		var decoded any
		if err := json.Unmarshal(t.OutputJSON, &decoded); err == nil {
			out = append(out, &wire.Artifact{
				ID:          "output",
				Name:        "output",
				Description: "Structured output validated against the task's declared output_schema.",
				Parts:       wire.ContentParts{&wire.Part{Content: wire.Data{Value: decoded}, MediaType: "application/json"}},
			})
		}
	}
	if t.Status == models.TaskStatusSuccess && t.Result != nil && strings.TrimSpace(*t.Result) != "" {
		out = append(out, &wire.Artifact{
			ID:    "result",
			Name:  "result",
			Parts: wire.ContentParts{&wire.Part{Content: wire.Text(*t.Result), MediaType: "text/plain"}},
		})
	}
	return out
}

// workspaceFileURL builds the authenticated download URL for a published
// workspace file: <base>/v1/tasks/{id}/workspace/{path}. Path segments are
// escaped individually so a nested path keeps its separators. An empty base
// yields a server-relative URL (documented: set FLEET_PUBLIC_BASE_URL for
// absolute artifact URLs).
func workspaceFileURL(base, taskID, p string) string {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.TrimRight(base, "/") + "/v1/tasks/" + taskID + "/workspace/" + strings.Join(segs, "/")
}

// mediaTypeFor guesses a published file's media type from its extension,
// defaulting to application/octet-stream. Only the extension is consulted —
// the file body stays in the workspace and is never read here. Parameters
// (";" onward — Go's mime table appends "; charset=utf-8" to text types) are
// stripped: the A2A Part.mediaType is a plain media type, and receivers (the
// official TCK included) compare it exactly.
func mediaTypeFor(name string) string {
	mt := mime.TypeByExtension(filepath.Ext(name))
	if mt == "" {
		return "application/octet-stream"
	}
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	return mt
}
