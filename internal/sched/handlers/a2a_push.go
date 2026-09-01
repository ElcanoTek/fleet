// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// A2A push-notification config CRUD (#1279 Phase 2): the four JSON-RPC
// methods that manage per-task webhook registrations, multiplexed over
// POST /a2a like every other A2A method. Delivery itself lives in
// internal/sched/push; this file only persists and reads configurations.
//
// Two wire realities shape the decoding here, both TCK-verified:
//   - The official TCK sends SNAKE_CASE JSON-RPC params (task_id, page_size)
//     despite the spec's camelCase MUST, so every parameter is accepted in
//     both spellings — a camelCase-only struct would silently see empty ids
//     and fail all four methods.
//   - A client-supplied config id MUST round-trip (the caller manages
//     multiple configs per task under its own ids); the server mints an id
//     only when none is given.
//
// Authorization mirrors the other mutating A2A surfaces (answer, cancel):
// the task must be visible (invisible ⇒ TaskNotFound, spec §3.3.2) and the
// principal must be the task's creator or hold operator cancel scope — a
// push config steers where task state announcements flow, so registering one
// on someone else's task is exactly as sensitive as cancelling it.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	wire "github.com/a2aproject/a2a-go/v2/a2a"

	a2abridge "github.com/ElcanoTek/fleet/internal/a2a"
	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// a2aPushParams decodes the params of all four push-config methods, accepting
// the spec's camelCase and the TCK's snake_case side by side. Create's params
// are the flat config itself (there is no nested wrapper in v1.0).
type a2aPushParams struct {
	Tenant string `json:"tenant"`
	ID     string `json:"id"`

	TaskID      string `json:"taskId"`
	TaskIDSnake string `json:"task_id"`

	URL   string `json:"url"`
	Token string `json:"token"`

	Authentication *struct {
		Scheme      string `json:"scheme"`
		Credentials string `json:"credentials"`
	} `json:"authentication"`

	PageSize       int    `json:"pageSize"`
	PageSizeSnake  int    `json:"page_size"`
	PageToken      string `json:"pageToken"`
	PageTokenSnake string `json:"page_token"`
}

func (p a2aPushParams) taskID() string {
	if p.TaskID != "" {
		return p.TaskID
	}
	return p.TaskIDSnake
}

// a2aPushRefusal is the spec §3.3.4 capability error every push method (and
// the inline SendMessage registration) answers while the capability is off.
func a2aPushRefusal(id json.RawMessage) a2abridge.Response {
	return a2abridge.NewErrorResponse(id, wire.ErrPushNotificationNotSupported,
		"push notifications are not supported by this server (capabilities.pushNotifications is false — the operator "+
			"must set FLEET_MCP_OAUTH_ENCRYPTION_KEY to enable storing push configs); poll GetTask or use SubscribeToTask", nil)
}

// a2aValidatePushURL bounds what can be registered as a webhook target: an
// absolute http(s) URL with a host. Reachability (and the SSRF posture) is
// the dispatcher's dial-time concern; this is the create-time shape check.
func a2aValidatePushURL(raw string) error {
	u, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%w: url must be an absolute http(s) URL", wire.ErrInvalidParams)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: url must be an absolute http(s) URL with a host", wire.ErrInvalidParams)
	}
	return nil
}

// a2aPushWireConfig renders a stored config in the spec's wire shape.
// Token and credentials belong to the creator, who supplied them; the reads
// are creator-scoped, so echoing them back is returning the caller's own data.
func a2aPushWireConfig(cfg *models.A2APushConfig) *wire.PushConfig {
	out := &wire.PushConfig{
		TaskID: wire.TaskID(cfg.TaskID.String()),
		ID:     cfg.ID,
		URL:    cfg.URL,
		Token:  cfg.Token,
	}
	if cfg.AuthScheme != "" || cfg.AuthCredentials != "" {
		out.Auth = &wire.PushAuthInfo{Scheme: cfg.AuthScheme, Credentials: cfg.AuthCredentials}
	}
	return out
}

// a2aPushTask loads the task behind a push-config call and enforces the
// mutating-surface gate. Invisible (or malformed id) ⇒ -32001; visible but
// neither creator nor operator ⇒ transport-layer 403, like answer/cancel.
func (h *Handlers) a2aPushTask(w http.ResponseWriter, p principal, req a2abridge.Request, rawTaskID string) (*models.Task, bool) {
	task, err := h.a2aVisibleTask(p, wire.TaskID(rawTaskID))
	if err != nil {
		a2aWrite(w, a2aErrorFrom(req.ID, err))
		return nil, false
	}
	if !taskCreatedByPrincipal(p, task) && !p.hasPermission(models.PermissionCancelTask) {
		writeError(w, http.StatusForbidden, "push-notification configs are limited to the task's creator or an operator credential")
		return nil, false
	}
	return task, true
}

// a2aCreatePushConfig implements CreateTaskPushNotificationConfig. The
// request params ARE the config; a repeated create for the same id is an
// update (reference-implementation semantics), and configs are creatable on
// terminal tasks — the spec keeps them until task deletion or explicit
// delete, and the TCK registers configs on completed tasks.
func (h *Handlers) a2aCreatePushConfig(w http.ResponseWriter, r *http.Request, p principal, req a2abridge.Request) {
	if !h.a2a.PushEnabled {
		a2aWrite(w, a2aPushRefusal(req.ID))
		return
	}
	var params a2aPushParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "params must be a TaskPushNotificationConfig: "+err.Error(), nil))
		return
	}
	if params.Tenant != "" {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "this agent declares no tenant; omit the tenant field", nil))
		return
	}
	if params.taskID() == "" {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "taskId is required", nil))
		return
	}
	if err := a2aValidatePushURL(params.URL); err != nil {
		a2aWrite(w, a2aErrorFrom(req.ID, err))
		return
	}
	task, ok := h.a2aPushTask(w, p, req, params.taskID())
	if !ok {
		return
	}
	cfg := models.A2APushConfig{
		TaskID: task.ID,
		ID:     strings.TrimSpace(params.ID),
		URL:    strings.TrimSpace(params.URL),
		Token:  params.Token,
	}
	if params.Authentication != nil {
		cfg.AuthScheme = params.Authentication.Scheme
		cfg.AuthCredentials = params.Authentication.Credentials
	}
	stored, err := h.storage.UpsertA2APushConfig(r.Context(), cfg)
	if err != nil {
		a2aWrite(w, a2aPushStoreError(req.ID, err))
		return
	}
	a2aWrite(w, a2abridge.NewResponse(req.ID, a2aPushWireConfig(stored)))
}

// a2aGetPushConfig implements GetTaskPushNotificationConfig.
func (h *Handlers) a2aGetPushConfig(w http.ResponseWriter, r *http.Request, p principal, req a2abridge.Request) {
	if !h.a2a.PushEnabled {
		a2aWrite(w, a2aPushRefusal(req.ID))
		return
	}
	var params a2aPushParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "params must name taskId and id: "+err.Error(), nil))
		return
	}
	task, ok := h.a2aPushTask(w, p, req, params.taskID())
	if !ok {
		return
	}
	cfg, err := h.storage.GetA2APushConfig(r.Context(), task.ID, params.ID)
	if errors.Is(err, sql.ErrNoRows) {
		a2aWrite(w, a2aErrorFrom(req.ID, fmt.Errorf("%w: task %s has no push-notification config %q",
			wire.ErrTaskNotFound, task.ID, params.ID)))
		return
	}
	if err != nil {
		a2aWrite(w, a2aPushStoreError(req.ID, err))
		return
	}
	a2aWrite(w, a2abridge.NewResponse(req.ID, a2aPushWireConfig(cfg)))
}

// a2aListPushConfigs implements ListTaskPushNotificationConfigs. Pagination
// params are accepted and ignored (spec: pagination is MAY; the reference
// implementation does the same) — configs is never null, nextPageToken is
// always the empty string.
func (h *Handlers) a2aListPushConfigs(w http.ResponseWriter, r *http.Request, p principal, req a2abridge.Request) {
	if !h.a2a.PushEnabled {
		a2aWrite(w, a2aPushRefusal(req.ID))
		return
	}
	var params a2aPushParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "params must name taskId: "+err.Error(), nil))
		return
	}
	task, ok := h.a2aPushTask(w, p, req, params.taskID())
	if !ok {
		return
	}
	configs, err := h.storage.ListA2APushConfigs(r.Context(), task.ID)
	if err != nil {
		a2aWrite(w, a2aPushStoreError(req.ID, err))
		return
	}
	out := make([]*wire.PushConfig, 0, len(configs))
	for _, cfg := range configs {
		out = append(out, a2aPushWireConfig(cfg))
	}
	a2aWrite(w, a2abridge.NewResponse(req.ID, map[string]any{
		"configs":       out,
		"nextPageToken": "",
	}))
}

// a2aDeletePushConfig implements DeleteTaskPushNotificationConfig —
// idempotent by spec §3.1.10; the result is the Empty object.
func (h *Handlers) a2aDeletePushConfig(w http.ResponseWriter, r *http.Request, p principal, req a2abridge.Request) {
	if !h.a2a.PushEnabled {
		a2aWrite(w, a2aPushRefusal(req.ID))
		return
	}
	var params a2aPushParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		a2aWrite(w, a2abridge.NewErrorResponse(req.ID, wire.ErrInvalidParams, "params must name taskId and id: "+err.Error(), nil))
		return
	}
	task, ok := h.a2aPushTask(w, p, req, params.taskID())
	if !ok {
		return
	}
	if err := h.storage.DeleteA2APushConfig(r.Context(), task.ID, params.ID); err != nil {
		a2aWrite(w, a2aPushStoreError(req.ID, err))
		return
	}
	a2aWrite(w, a2abridge.NewResponse(req.ID, struct{}{}))
}

// a2aStorePushConfigInline persists a SendMessage-inline push config against
// the task the send just created or resumed — the registration path the
// TCK's delivery tests use exclusively. The body's own taskId is ignored per
// spec ("should be empty when sending this configuration in a SendMessage").
// Refusals surface as the send's error: a caller who asked for notifications
// it cannot have must hear that, not silently miss every callback.
func (h *Handlers) a2aStorePushConfigInline(r *http.Request, taskID uuid.UUID, inline *wire.PushConfig) error {
	if err := a2aValidatePushURL(inline.URL); err != nil {
		return err
	}
	cfg := models.A2APushConfig{
		TaskID: taskID,
		ID:     strings.TrimSpace(inline.ID),
		URL:    strings.TrimSpace(inline.URL),
		Token:  inline.Token,
	}
	if inline.Auth != nil {
		cfg.AuthScheme = inline.Auth.Scheme
		cfg.AuthCredentials = inline.Auth.Credentials
	}
	_, err := h.storage.UpsertA2APushConfig(r.Context(), cfg)
	return err
}

// a2aPushStoreError maps storage failures: the missing-cipher fail-closed
// case reports as the capability refusal (the capability genuinely is not
// available), everything else as an internal error with no row detail.
func a2aPushStoreError(id json.RawMessage, err error) a2abridge.Response {
	if errors.Is(err, db.ErrA2APushCipherMissing) {
		return a2aPushRefusal(id)
	}
	return a2abridge.NewErrorResponse(id, wire.ErrInternalError, "push-notification config storage failed", nil)
}
